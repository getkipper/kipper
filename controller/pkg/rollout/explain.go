package rollout

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Reason names why a rollout has not finished.
type Reason string

const (
	Complete         Reason = ""
	Unschedulable    Reason = "Unschedulable"
	QuotaExceeded    Reason = "QuotaExceeded"
	PodsRefused      Reason = "PodsRefused"
	PodsNotStarting  Reason = "PodsNotStarting"
	NotBecomingReady Reason = "NotBecomingReady"
	DeadlineExceeded Reason = "DeadlineExceeded"
	InProgress       Reason = "InProgress"
	// Stopping and Stopped describe an app that was stopped on purpose: its
	// pods are draining, or all gone.
	Stopping Reason = "Stopping"
	Stopped  Reason = "Stopped"
)

const keepServing = "Any healthy current pods continue serving."

// Explain reports rollout progress or the first matching problem, prioritizing
// scheduling and admission errors over startup and readiness failures.
// newPods must belong to the newest ReplicaSet; budget is the time allowed
// from pod creation to readiness.
func Explain(dep *appsv1.Deployment, newPods []corev1.Pod, budget time.Duration, now time.Time) (Reason, string) {
	if Settled(dep) {
		return Complete, ""
	}
	for i := range newPods {
		if c := podCondition(&newPods[i], corev1.PodScheduled); c != nil &&
			c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return Unschedulable, fmt.Sprintf("A new pod cannot be placed: %s %s Lower the CPU or memory request, or add capacity.", c.Message, keepServing)
		}
	}
	if c := replicaFailure(dep); c != nil {
		if quotaRefusal(c) {
			return QuotaExceeded, fmt.Sprintf("The project quota prevents new pods from being created: %s %s", c.Message, keepServing)
		}
		return PodsRefused, fmt.Sprintf("The cluster rejected new pods: %s %s", c.Message, keepServing)
	}
	newPods = activePods(newPods)
	// Check scheduling and admission before completion because scale-ups
	// can retain NewReplicaSetAvailable. After completion, report startup
	// failures only for newer pods with no recorded container execution.
	if Finished(dep) {
		finishedAt := finishedTime(dep)
		for i := range newPods {
			if !newPods[i].CreationTimestamp.After(finishedAt) {
				continue
			}
			if detail := cannotStart(&newPods[i]); detail != "" && neverRan(&newPods[i]) {
				return PodsNotStarting, fmt.Sprintf("A new pod cannot start: %s %s", detail, keepServing)
			}
		}
		return Complete, ""
	}
	for i := range newPods {
		if detail := cannotStart(&newPods[i]); detail != "" {
			return PodsNotStarting, fmt.Sprintf("A new pod cannot start: %s %s", detail, keepServing)
		}
	}
	for i := range newPods {
		p := &newPods[i]
		ready := podCondition(p, corev1.PodReady)
		since := podSince(p)
		if (ready == nil || ready.Status != corev1.ConditionTrue) && !since.IsZero() && now.Sub(since) > budget {
			if p.Status.Phase != corev1.PodRunning {
				return PodsNotStarting, fmt.Sprintf("A new pod has not started running within %s. Its phase is %s; check the pod status and events for details. %s", budget, p.Status.Phase, keepServing)
			}
			return NotBecomingReady, fmt.Sprintf("New pods have not become ready within %s. %s", budget, keepServing)
		}
	}
	if Failed(dep) {
		return DeadlineExceeded, fmt.Sprintf("The rollout stopped making progress: %s %s", Message(dep), keepServing)
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	if dep.Status.UpdatedReplicas > want {
		return InProgress, fmt.Sprintf("Scaling to %d pod(s).", want)
	}
	return InProgress, fmt.Sprintf("Replacing pods: %d of %d updated.", dep.Status.UpdatedReplicas, want)
}

// QuotaBlocked reports whether the project quota is refusing the
// Deployment's new pods.
func QuotaBlocked(dep *appsv1.Deployment) bool {
	c := replicaFailure(dep)
	return c != nil && quotaRefusal(c)
}

// replicaFailure returns the Deployment's active ReplicaFailure condition, or
// nil when no ReplicaFailure condition is True.
func replicaFailure(dep *appsv1.Deployment) *appsv1.DeploymentCondition {
	if dep == nil {
		return nil
	}
	for i := range dep.Status.Conditions {
		c := &dep.Status.Conditions[i]
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

func quotaRefusal(c *appsv1.DeploymentCondition) bool {
	return strings.Contains(c.Message, "exceeded quota")
}

// stuckWaitingReasons identifies startup problems worth reporting before
// the timeout. CrashLoopBackOff also requires at least two restarts.
var stuckWaitingReasons = map[string]bool{
	"ImagePullBackOff": true, "InvalidImageName": true,
	"CreateContainerConfigError": true, "CreateContainerError": true,
	"RunContainerError": true, "CrashLoopBackOff": true,
}

// cannotStart describes the first recognized startup problem, checking init
// containers before app containers. It returns an empty string if none qualify.
func cannotStart(p *corev1.Pod) string {
	statuses := append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...)
	for _, cs := range statuses {
		w := cs.State.Waiting
		if w == nil || !stuckWaitingReasons[w.Reason] {
			continue
		}
		if w.Reason == "CrashLoopBackOff" {
			// Wait for at least two restarts before reporting repeated crashes.
			if cs.RestartCount < 2 {
				continue
			}
			detail := fmt.Sprintf("container %s keeps crashing", cs.Name)
			if t := cs.LastTerminationState.Terminated; t != nil {
				detail += fmt.Sprintf(" (last exit %d", t.ExitCode)
				if t.Reason != "" {
					detail += ", " + t.Reason
				}
				detail += ")"
			}
			return detail + "."
		}
		detail := fmt.Sprintf("container %s is in %s", cs.Name, w.Reason)
		if w.Message != "" {
			detail += ": " + w.Message
		}
		return strings.TrimSuffix(detail, ".") + "."
	}
	return ""
}

// neverRan checks for recorded evidence that any init or app container has
// run. Missing container status counts as no evidence.
func neverRan(p *corev1.Pod) bool {
	for _, cs := range append(append([]corev1.ContainerStatus{}, p.Status.InitContainerStatuses...), p.Status.ContainerStatuses...) {
		if cs.RestartCount > 0 || cs.State.Running != nil || cs.State.Terminated != nil || cs.LastTerminationState.Terminated != nil {
			return false
		}
	}
	return true
}

// finishedTime returns the update time of the NewReplicaSetAvailable
// condition, or zero if that condition is absent.
func finishedTime(dep *appsv1.Deployment) time.Time {
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentProgressing && c.Reason == "NewReplicaSetAvailable" {
			return c.LastUpdateTime.Time
		}
	}
	return time.Time{}
}

// activePods excludes terminating and completed pods from startup diagnostics.
func activePods(pods []corev1.Pod) []corev1.Pod {
	var out []corev1.Pod
	for i := range pods {
		p := &pods[i]
		if p.DeletionTimestamp != nil || p.Status.Phase == corev1.PodFailed || p.Status.Phase == corev1.PodSucceeded {
			continue
		}
		out = append(out, *p)
	}
	return out
}

// podSince uses creation time for the startup timeout, including time spent
// pulling images. It falls back to status.startTime when creation time is absent.
func podSince(p *corev1.Pod) time.Time {
	if !p.CreationTimestamp.IsZero() {
		return p.CreationTimestamp.Time
	}
	if p.Status.StartTime != nil {
		return p.Status.StartTime.Time
	}
	return time.Time{}
}

// NewestReplicaSet returns the Deployment's highest-revision ReplicaSet,
// or nil if no owned ReplicaSet has a valid revision.
func NewestReplicaSet(dep *appsv1.Deployment, sets []appsv1.ReplicaSet) *appsv1.ReplicaSet {
	var newest *appsv1.ReplicaSet
	best := -1
	for i := range sets {
		if !ownedBy(&sets[i], dep) {
			continue
		}
		rev, err := strconv.Atoi(sets[i].Annotations["deployment.kubernetes.io/revision"])
		if err != nil {
			continue
		}
		if rev > best {
			best, newest = rev, &sets[i]
		}
	}
	return newest
}

func ownedBy(rs *appsv1.ReplicaSet, dep *appsv1.Deployment) bool {
	for _, ref := range rs.OwnerReferences {
		if ref.UID == dep.UID {
			return true
		}
	}
	return false
}

func podCondition(p *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// PodsOf returns the pods rs controls, or nil when rs is nil.
func PodsOf(rs *appsv1.ReplicaSet, pods []corev1.Pod) []corev1.Pod {
	if rs == nil {
		return nil
	}
	var out []corev1.Pod
	for i := range pods {
		if ref := metav1.GetControllerOf(&pods[i]); ref != nil && ref.UID == rs.UID {
			out = append(out, pods[i])
		}
	}
	return out
}
