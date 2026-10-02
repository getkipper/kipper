package rollout

import (
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func deployment(replicas, updated, available int32, conds ...appsv1.DeploymentCondition) *appsv1.Deployment {
	total := replicas
	if updated < replicas {
		total = replicas + 1
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Generation: 2},
		Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  2,
			Replicas:            total,
			UpdatedReplicas:     updated,
			AvailableReplicas:   available,
			UnavailableReplicas: replicas - available,
			Conditions:          conds,
		},
	}
}

func pendingPod(message string) corev1.Pod {
	return corev1.Pod{Status: corev1.PodStatus{
		Phase: corev1.PodPending,
		Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason: corev1.PodReasonUnschedulable, Message: message,
		}},
	}}
}

func runningNotReady(started time.Time) corev1.Pod {
	st := metav1.NewTime(started)
	return corev1.Pod{Status: corev1.PodStatus{
		Phase:     corev1.PodRunning,
		StartTime: &st,
		Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		},
	}}
}

func TestExplain(t *testing.T) {
	budget := 5 * time.Minute
	quota := appsv1.DeploymentCondition{
		Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate",
		Message: `pods "shop-abc" is forbidden: exceeded quota: project-quota, requested: cpu=3, used: cpu=6, limited: cpu=8`,
	}
	deadline := appsv1.DeploymentCondition{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded",
		Message: `ReplicaSet "shop-abc" has timed out progressing.`,
	}

	tests := []struct {
		name        string
		dep         *appsv1.Deployment
		pods        []corev1.Pod
		wantReason  Reason
		wantMessage []string
	}{
		{
			name:       "complete",
			dep:        deployment(2, 2, 2),
			wantReason: Complete,
		},
		{
			name:        "a new pod cannot be placed",
			dep:         deployment(2, 1, 2),
			pods:        []corev1.Pod{pendingPod("0/1 nodes are available: 1 Insufficient cpu.")},
			wantReason:  Unschedulable,
			wantMessage: []string{"A new pod cannot be placed", "Insufficient cpu", "healthy current pods continue serving"},
		},
		{
			name:        "the quota refuses new pods",
			dep:         deployment(2, 0, 2, quota),
			wantReason:  QuotaExceeded,
			wantMessage: []string{"project quota", "exceeded quota", "healthy current pods continue serving"},
		},
		{
			// Kubernetes keeps NewReplicaSetAvailable through a scale-up, so the
			// record of a finished rollout must not hide the added pod.
			name: "a scaled-up pod that cannot be placed after the rollout finished",
			dep: func() *appsv1.Deployment {
				d := deployment(3, 3, 2, appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"})
				d.Status.Replicas = 3
				return d
			}(),
			pods:        []corev1.Pod{pendingPod("0/1 nodes are available: 1 Insufficient cpu.")},
			wantReason:  Unschedulable,
			wantMessage: []string{"Insufficient cpu"},
		},
		{
			name: "a pod crashing after the rollout finished is not a rollout",
			dep: func() *appsv1.Deployment {
				d := deployment(2, 2, 1, appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable"})
				d.Status.Replicas = 2
				return d
			}(),
			pods:       []corev1.Pod{runningNotReady(now.Add(-time.Hour))},
			wantReason: Complete,
		},
		{
			name: "a refusal that is not the quota is not called the quota",
			dep: deployment(2, 0, 2, appsv1.DeploymentCondition{
				Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate",
				Message: `pods "shop-abc" is forbidden: error looking up service account shop/runner: serviceaccount "runner" not found`,
			}),
			wantReason:  PodsRefused,
			wantMessage: []string{"The cluster rejected new pods", "serviceaccount", "healthy current pods continue serving"},
		},
		{
			name:        "a new pod runs but never becomes ready",
			dep:         deployment(2, 1, 2),
			pods:        []corev1.Pod{runningNotReady(now.Add(-6 * time.Minute))},
			wantReason:  NotBecomingReady,
			wantMessage: []string{"have not become ready within 5m", "healthy current pods continue serving"},
		},
		{
			name:        "a new pod still inside its startup budget is just in progress",
			dep:         deployment(2, 1, 2),
			pods:        []corev1.Pod{runningNotReady(now.Add(-2 * time.Minute))},
			wantReason:  InProgress,
			wantMessage: []string{"1 of 2"},
		},
		{
			name:        "Kubernetes gave up for another reason",
			dep:         deployment(2, 1, 2, deadline),
			wantReason:  DeadlineExceeded,
			wantMessage: []string{"stopped making progress", "timed out progressing"},
		},
		{
			name:        "the scheduler's reason wins over the deadline",
			dep:         deployment(2, 1, 2, deadline),
			pods:        []corev1.Pod{pendingPod("0/1 nodes are available: 1 Insufficient memory.")},
			wantReason:  Unschedulable,
			wantMessage: []string{"Insufficient memory"},
		},
		{
			name: "scaling down",
			dep: func() *appsv1.Deployment {
				d := deployment(1, 3, 3)
				d.Status.Replicas = 3
				return d
			}(),
			wantReason:  InProgress,
			wantMessage: []string{"Scaling to 1 pod"},
		},
		{
			name:        "replacing pods",
			dep:         deployment(3, 1, 3),
			wantReason:  InProgress,
			wantMessage: []string{"Replacing pods: 1 of 3 updated"},
		},
		{
			name:        "a status left over from the previous spec is not complete",
			dep:         func() *appsv1.Deployment { d := deployment(2, 2, 2); d.Generation = 3; return d }(),
			wantReason:  InProgress,
			wantMessage: []string{"Replacing pods"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, message := Explain(tt.dep, tt.pods, budget, now)
			if reason != tt.wantReason {
				t.Errorf("reason = %q, want %q (message %q)", reason, tt.wantReason, message)
			}
			for _, want := range tt.wantMessage {
				if !strings.Contains(message, want) {
					t.Errorf("message %q does not mention %q", message, want)
				}
			}
			if tt.wantReason == Complete && message != "" {
				t.Errorf("a complete rollout has message %q", message)
			}
		})
	}
}

func TestNewestReplicaSet(t *testing.T) {
	dep := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "shop", UID: "dep-uid"}}
	rs := func(name, revision, owner string) appsv1.ReplicaSet {
		return appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Annotations:     map[string]string{"deployment.kubernetes.io/revision": revision},
			OwnerReferences: []metav1.OwnerReference{{UID: types.UID(owner)}},
		}}
	}
	sets := []appsv1.ReplicaSet{rs("shop-1", "1", "dep-uid"), rs("shop-10", "10", "dep-uid"), rs("shop-9", "9", "dep-uid"), rs("other-99", "99", "other-uid")}

	got := NewestReplicaSet(dep, sets)
	if got == nil || got.Name != "shop-10" {
		t.Fatalf("NewestReplicaSet = %v, want shop-10 (numeric revision, owned by the Deployment)", got)
	}
	if NewestReplicaSet(dep, nil) != nil {
		t.Error("no ReplicaSets must give nil")
	}
}

func TestSettledWaitsForTheOldPodsToGo(t *testing.T) {
	// The old ReplicaSet has been told to scale to zero but its pod is still
	// counted: every new pod is updated and available, yet the rollout is not
	// done until the total matches the desired count.
	two := int32(2)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Generation: 3},
		Spec:       appsv1.DeploymentSpec{Replicas: &two},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 3, Replicas: 3, UpdatedReplicas: 2, AvailableReplicas: 2, UnavailableReplicas: 0,
		},
	}
	if !Ready(dep) {
		t.Fatal("setup: Ready should already hold here")
	}
	if Settled(dep) {
		t.Error("Settled while an old pod is still counted")
	}
	dep.Status.Replicas = 2
	if !Settled(dep) {
		t.Error("not Settled once only the new pods remain")
	}
}

func TestPodsOf(t *testing.T) {
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "shop-2", UID: "rs-2"}}
	pod := func(name, owner string, isController bool) corev1.Pod {
		c := isController
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, OwnerReferences: []metav1.OwnerReference{{UID: types.UID(owner), Controller: &c}}}}
	}
	pods := []corev1.Pod{pod("new", "rs-2", true), pod("old", "rs-1", true), pod("not-controlled", "rs-2", false)}

	got := PodsOf(rs, pods)
	if len(got) != 1 || got[0].Name != "new" {
		t.Fatalf("PodsOf = %v, want only the pod rs-2 controls", got)
	}
	if PodsOf(nil, pods) != nil {
		t.Error("no ReplicaSet has no pods")
	}
}

func TestFinished(t *testing.T) {
	progressing := func(reason string) appsv1.DeploymentCondition {
		return appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: reason}
	}
	two := int32(2)
	dep := func(replicas, updated, available int32, gen, observed int64, conds ...appsv1.DeploymentCondition) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Generation: gen},
			Spec:       appsv1.DeploymentSpec{Replicas: &two},
			Status: appsv1.DeploymentStatus{
				ObservedGeneration: observed, Replicas: replicas, UpdatedReplicas: updated, AvailableReplicas: available,
				UnavailableReplicas: 2 - available, Conditions: conds,
			},
		}
	}
	tests := []struct {
		name string
		dep  *appsv1.Deployment
		want bool
	}{
		{name: "settled", dep: dep(2, 2, 2, 3, 3), want: true},
		{name: "a pod crashing after the rollout finished", dep: dep(2, 2, 1, 3, 3, progressing("NewReplicaSetAvailable")), want: true},
		{name: "still replacing pods", dep: dep(3, 1, 2, 3, 3, progressing("ReplicaSetUpdated"))},
		{name: "new pods all created but not yet available", dep: dep(2, 2, 1, 3, 3, progressing("ReplicaSetUpdated"))},
		{name: "a finished condition left over from the previous spec", dep: dep(2, 2, 1, 4, 3, progressing("NewReplicaSetAvailable"))},
		{name: "an old pod still counted", dep: dep(3, 2, 2, 3, 3, progressing("NewReplicaSetAvailable"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Finished(tt.dep); got != tt.want {
				t.Errorf("Finished = %v, want %v", got, tt.want)
			}
		})
	}
}

func waitingPod(created time.Time, init bool, state corev1.ContainerState, last *corev1.ContainerState) corev1.Pod {
	cs := corev1.ContainerStatus{Name: "shop", State: state}
	if state.Waiting != nil && state.Waiting.Reason == "CrashLoopBackOff" {
		cs.RestartCount = 2
	}
	if last != nil {
		cs.LastTerminationState = *last
	}
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "shop-2-a", CreationTimestamp: metav1.NewTime(created)},
		Status: corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{
			{Type: corev1.PodScheduled, Status: corev1.ConditionTrue},
			{Type: corev1.PodReady, Status: corev1.ConditionFalse},
		}},
	}
	if init {
		p.Status.InitContainerStatuses = []corev1.ContainerStatus{cs}
	} else {
		p.Status.ContainerStatuses = []corev1.ContainerStatus{cs}
	}
	return p
}

func waiting(reason, message string) corev1.ContainerState {
	return corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason, Message: message}}
}

// Report recognized startup failures before the timeout, with the available
// container details.
func TestExplainNamesPodsThatCannotStart(t *testing.T) {
	budget := 5 * time.Minute
	recent := now.Add(-time.Minute)
	oom := &corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 137, Reason: "OOMKilled"}}
	crashing := waitingPod(recent, false, waiting("CrashLoopBackOff", "back-off 40s restarting failed container"), oom)
	crashing.Status.Phase = corev1.PodRunning

	tests := []struct {
		name        string
		pod         corev1.Pod
		wantReason  Reason
		wantMessage []string
	}{
		{name: "image cannot be pulled", pod: waitingPod(recent, false, waiting("ImagePullBackOff", `Back-off pulling image "registry.example.com/shop:2"`), nil),
			wantReason: PodsNotStarting, wantMessage: []string{"cannot start", "shop", "ImagePullBackOff", "registry.example.com/shop:2", "current pods continue serving"}},
		{name: "a broken container config", pod: waitingPod(recent, false, waiting("CreateContainerConfigError", `secret "shop-env" not found`), nil),
			wantReason: PodsNotStarting, wantMessage: []string{"CreateContainerConfigError", `secret "shop-env" not found`}},
		{name: "crash loop names the last exit", pod: crashing,
			wantReason: PodsNotStarting, wantMessage: []string{"keeps crashing", "exit 137", "OOMKilled"}},
		{name: "an init container crash loop", pod: waitingPod(recent, true, waiting("CrashLoopBackOff", "back-off"), nil),
			wantReason: PodsNotStarting, wantMessage: []string{"keeps crashing"}},
		{name: "still being created past the startup time", pod: waitingPod(now.Add(-6*time.Minute), false, waiting("ContainerCreating", ""), nil),
			wantReason: PodsNotStarting, wantMessage: []string{"not started running within 5m", "Pending"}},
		{name: "still being created inside the startup time", pod: waitingPod(recent, false, waiting("ContainerCreating", ""), nil),
			wantReason: InProgress},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, message := Explain(deployment(2, 1, 2), []corev1.Pod{tt.pod}, budget, now)
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q (message %q)", reason, tt.wantReason, message)
			}
			for _, want := range tt.wantMessage {
				if !strings.Contains(message, want) {
					t.Errorf("message %q does not mention %q", message, want)
				}
			}
		})
	}
}

// Startup diagnostics should describe active pods, excluding failed or
// terminating pods that are being replaced.
func TestExplainIgnoresPodsOnTheirWayOut(t *testing.T) {
	budget := 5 * time.Minute
	evicted := waitingPod(now.Add(-time.Hour), false, waiting("ContainerCreating", ""), nil)
	evicted.Status.Phase = corev1.PodFailed
	deleting := waitingPod(now.Add(-time.Minute), false, waiting("ImagePullBackOff", "back-off"), nil)
	deletedAt := metav1.NewTime(now)
	deleting.DeletionTimestamp = &deletedAt
	replacement := waitingPod(now.Add(-time.Minute), false, waiting("ContainerCreating", ""), nil)

	for name, pods := range map[string][]corev1.Pod{
		"an evicted pod older than the budget": {evicted, replacement},
		"a deleting pod in a waiting reason":   {replacement, deleting},
	} {
		t.Run(name, func(t *testing.T) {
			if reason, message := Explain(deployment(2, 1, 2), pods, budget, now); reason != InProgress {
				t.Errorf("reason = %q, want InProgress (message %q)", reason, message)
			}
		})
	}
}

func TestExplainCountsFromCreationNotStart(t *testing.T) {
	p := runningNotReady(now.Add(-time.Minute))
	p.CreationTimestamp = metav1.NewTime(now.Add(-6 * time.Minute))
	if reason, _ := Explain(deployment(2, 1, 2), []corev1.Pod{p}, 5*time.Minute, now); reason != NotBecomingReady {
		t.Errorf("reason = %q, want NotBecomingReady: the pod was created 6 minutes ago", reason)
	}
}

func TestExplainNamesEveryStuckWaitingReason(t *testing.T) {
	for reason := range stuckWaitingReasons {
		for _, init := range []bool{false, true} {
			p := waitingPod(now.Add(-time.Minute), init, waiting(reason, "detail"), nil)
			if got, message := Explain(deployment(2, 1, 2), []corev1.Pod{p}, 5*time.Minute, now); got != PodsNotStarting || !strings.Contains(message, reason) && reason != "CrashLoopBackOff" {
				t.Errorf("%s (init %v): reason = %q, message %q", reason, init, got, message)
			}
		}
	}
}

func finishedAt(at time.Time, replicas, available int32) *appsv1.Deployment {
	d := deployment(replicas, replicas, available, appsv1.DeploymentCondition{
		Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "NewReplicaSetAvailable",
		LastUpdateTime: metav1.NewTime(at),
	})
	d.Status.Replicas = replicas
	return d
}

// Distinguish transient startup states from persistent failures, including
// new pods created after a completed rollout.
func TestExplainRound2(t *testing.T) {
	budget := 5 * time.Minute
	recent := now.Add(-time.Minute)
	crashOnce := waitingPod(recent, false, waiting("CrashLoopBackOff", "back-off 10s"), nil)
	crashOnce.Status.Phase = corev1.PodRunning
	crashOnce.Status.ContainerStatuses[0].RestartCount = 1
	crashTwice := crashOnce.DeepCopy()
	crashTwice.Status.ContainerStatuses[0].RestartCount = 2
	oldCrash := crashTwice.DeepCopy()
	oldCrash.CreationTimestamp = metav1.NewTime(now.Add(-3 * time.Hour))

	tests := []struct {
		name       string
		dep        *appsv1.Deployment
		pod        corev1.Pod
		wantReason Reason
		want       string
	}{
		{name: "a first ErrImagePull may clear on retry", dep: deployment(2, 1, 2), pod: waitingPod(recent, false, waiting("ErrImagePull", "timeout"), nil), wantReason: InProgress},
		{name: "one crash is not a crash loop yet", dep: deployment(2, 1, 2), pod: crashOnce, wantReason: InProgress},
		{name: "two crashes are", dep: deployment(2, 1, 2), pod: *crashTwice, wantReason: PodsNotStarting, want: "keeps crashing"},
		{name: "a pod still pulling its image past the startup time", dep: deployment(2, 1, 2), pod: waitingPod(now.Add(-6*time.Minute), false, waiting("ContainerCreating", ""), nil),
			wantReason: PodsNotStarting, want: "has not started running within 5m"},
		{name: "a scale-up pod that cannot start after the rollout finished", dep: finishedAt(now.Add(-time.Hour), 3, 2),
			pod: waitingPod(recent, false, waiting("ImagePullBackOff", "back-off"), nil), wantReason: PodsNotStarting, want: "ImagePullBackOff"},
		{name: "a pod crashing that existed when the rollout finished", dep: finishedAt(now.Add(-time.Hour), 2, 1), pod: *oldCrash, wantReason: Complete},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reason, message := Explain(tt.dep, []corev1.Pod{tt.pod}, budget, now)
			if reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q (message %q)", reason, tt.wantReason, message)
			}
			if tt.want != "" && !strings.Contains(message, tt.want) {
				t.Errorf("message %q does not mention %q", message, tt.want)
			}
		})
	}
}

// A pod that ran and then crashes after rollout completion affects app health
// without reopening the rollout.
func TestExplainAScaleUpPodCrashingLaterIsNotARollout(t *testing.T) {
	recent := now.Add(-time.Minute)
	crashing := waitingPod(recent, false, waiting("CrashLoopBackOff", "back-off"), nil)
	crashing.Status.Phase = corev1.PodRunning
	if reason, message := Explain(finishedAt(now.Add(-time.Hour), 3, 2), []corev1.Pod{crashing}, 5*time.Minute, now); reason != Complete {
		t.Errorf("reason = %q, want Complete (message %q)", reason, message)
	}
}
