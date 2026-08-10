package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func TestComputeBumpedLimit(t *testing.T) {
	tests := []struct {
		name        string
		current     string
		ceiling     string
		want        string
		wantCeiling bool
	}{
		{"doubles 512Mi to 1Gi", "512Mi", "4Gi", "1Gi", false},
		{"doubles 1Gi to 2Gi", "1Gi", "4Gi", "2Gi", false},
		{"doubles 2Gi to 4Gi and hits ceiling", "2Gi", "4Gi", "4Gi", true},
		{"already at ceiling stays at ceiling", "4Gi", "4Gi", "4Gi", true},
		{"manual override above ceiling is preserved, not lowered", "6Gi", "4Gi", "6Gi", true},
		{"manual override well above ceiling is preserved", "16Gi", "4Gi", "16Gi", true},
		{"loki: 384Mi doubles to 768Mi", "384Mi", "2Gi", "768Mi", false},
		{"loki: 1Gi to 2Gi hits ceiling", "1Gi", "2Gi", "2Gi", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, atCeiling, err := computeBumpedLimit(tt.current, tt.ceiling)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.wantCeiling, atCeiling)
		})
	}
}

func TestComponentForPod(t *testing.T) {
	tests := []struct {
		name      string
		pod       *corev1.Pod
		want      string
		wantMatch bool
	}{
		{
			name: "prometheus in monitoring namespace matches",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "prometheus-kube-prometheus-stack-prometheus-0",
					Namespace: monitoringNamespace,
					Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
				},
			},
			want:      componentPrometheus,
			wantMatch: true,
		},
		{
			name: "loki in monitoring namespace matches",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "loki-0",
					Namespace: monitoringNamespace,
					Labels:    map[string]string{"app.kubernetes.io/name": "loki"},
				},
			},
			want:      componentLoki,
			wantMatch: true,
		},
		{
			name: "user app named prometheus in another namespace is ignored",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "prometheus",
					Namespace: "user-project-prod",
					Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
				},
			},
			wantMatch: false,
		},
		{
			name: "unrelated pod in monitoring is ignored",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "grafana-abc123",
					Namespace: monitoringNamespace,
					Labels:    map[string]string{"app.kubernetes.io/name": "grafana"},
				},
			},
			wantMatch: false,
		},
		{
			name: "pod without app.kubernetes.io/name is ignored",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "stray",
					Namespace: monitoringNamespace,
				},
			},
			wantMatch: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := componentForPod(tt.pod)
			assert.Equal(t, tt.wantMatch, ok)
			if tt.wantMatch {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestHasOOMKilledContainer(t *testing.T) {
	t.Run("returns true when a container's lastTerminationState is OOMKilled", func(t *testing.T) {
		pod := &corev1.Pod{
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name: "prometheus",
						LastTerminationState: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
						},
					},
				},
			},
		}
		assert.True(t, hasOOMKilledContainer(pod))
	})

	t.Run("returns false for normal terminations and clean pods", func(t *testing.T) {
		clean := &corev1.Pod{Status: corev1.PodStatus{}}
		assert.False(t, hasOOMKilledContainer(clean))

		errored := &corev1.Pod{
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						LastTerminationState: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{Reason: "Error", ExitCode: 1},
						},
					},
				},
			},
		}
		assert.False(t, hasOOMKilledContainer(errored))
	})
}

func TestRecentBumpDebounce(t *testing.T) {
	tenSeconds := metav1.NewTime(time.Now().Add(-10 * time.Second))
	hourAgo := metav1.NewTime(time.Now().Add(-1 * time.Hour))

	t.Run("debounces a bump from a few seconds ago", func(t *testing.T) {
		pc := &kipperv1.PlatformConfig{
			Status: kipperv1.PlatformConfigStatus{
				Components: []kipperv1.ComponentStatus{
					{Name: componentPrometheus, LastBumpAt: &tenSeconds},
				},
			},
		}
		assert.True(t, recentBump(pc, componentPrometheus))
	})

	t.Run("does not debounce a bump from an hour ago", func(t *testing.T) {
		pc := &kipperv1.PlatformConfig{
			Status: kipperv1.PlatformConfigStatus{
				Components: []kipperv1.ComponentStatus{
					{Name: componentPrometheus, LastBumpAt: &hourAgo},
				},
			},
		}
		assert.False(t, recentBump(pc, componentPrometheus))
	})

	t.Run("does not debounce when no prior bump is recorded", func(t *testing.T) {
		assert.False(t, recentBump(&kipperv1.PlatformConfig{}, componentPrometheus))
	})
}

func TestEffectiveLimit(t *testing.T) {
	t.Run("override wins over profile default", func(t *testing.T) {
		pc := &kipperv1.PlatformConfig{
			Spec: kipperv1.PlatformConfigSpec{
				Profile: platform.ProfileMedium,
				Components: []kipperv1.ComponentOverride{
					{Name: componentPrometheus, MemoryLimit: "3Gi"},
				},
			},
		}
		assert.Equal(t, "3Gi", effectiveLimit(pc, componentPrometheus))
	})

	t.Run("falls back to profile default when no override is set", func(t *testing.T) {
		pc := &kipperv1.PlatformConfig{Spec: kipperv1.PlatformConfigSpec{Profile: platform.ProfileXLarge}}
		assert.Equal(t, "2Gi", effectiveLimit(pc, componentPrometheus))
		assert.Equal(t, "1Gi", effectiveLimit(pc, componentLoki))
	})
}

func TestPodOOMReconciler_BumpsOnOOMKilled(t *testing.T) {
	scheme := testScheme()

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileSmall},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-stack-prometheus-0",
			Namespace: monitoringNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "prometheus",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137},
					},
				},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, pod).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PodOOMReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod.Name, Namespace: pod.Namespace,
	}})
	require.NoError(t, err)

	var got kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))

	require.Len(t, got.Spec.Components, 1, "spec should now carry the prometheus override")
	assert.Equal(t, componentPrometheus, got.Spec.Components[0].Name)
	assert.Equal(t, "1Gi", got.Spec.Components[0].MemoryLimit,
		"small profile default (512Mi) doubled to 1Gi")

	require.Len(t, got.Status.Components, 1)
	cs := got.Status.Components[0]
	assert.Equal(t, componentPrometheus, cs.Name)
	assert.Equal(t, "512Mi", cs.LastBumpFrom)
	assert.Equal(t, "1Gi", cs.LastBumpTo)
	assert.False(t, cs.AtCeiling)
	require.NotNil(t, cs.LastBumpAt)
	assert.Contains(t, cs.LastBumpReason, "OOMKilled")
}

func TestPodOOMReconciler_DebouncesRepeatOOMOnSamePod(t *testing.T) {
	scheme := testScheme()

	now := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileSmall,
			Components: []kipperv1.ComponentOverride{
				{Name: componentPrometheus, MemoryLimit: "1Gi"},
			},
		},
		Status: kipperv1.PlatformConfigStatus{
			Components: []kipperv1.ComponentStatus{
				{
					Name:         componentPrometheus,
					LastBumpAt:   &now,
					LastBumpFrom: "512Mi",
					LastBumpTo:   "1Gi",
				},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-stack-prometheus-0",
			Namespace: monitoringNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
					},
				},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, pod).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PodOOMReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod.Name, Namespace: pod.Namespace,
	}})
	require.NoError(t, err)

	var got kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))

	assert.Equal(t, "1Gi", got.Spec.Components[0].MemoryLimit,
		"recent bump must not turn into a runaway 1Gi -> 2Gi -> 4Gi sequence")
}

func TestPodOOMReconciler_StopsAtCeiling(t *testing.T) {
	scheme := testScheme()

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileXLarge,
			Components: []kipperv1.ComponentOverride{
				{Name: componentPrometheus, MemoryLimit: "4Gi"},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-stack-prometheus-0",
			Namespace: monitoringNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
					},
				},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, pod).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PodOOMReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod.Name, Namespace: pod.Namespace,
	}})
	require.NoError(t, err)

	var got kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))

	assert.Equal(t, "4Gi", got.Spec.Components[0].MemoryLimit,
		"limit should stay at ceiling, not double further")

	require.Len(t, got.Status.Components, 1)
	assert.True(t, got.Status.Components[0].AtCeiling,
		"status should flag the ceiling so the operator sees it needs attention")
}

func TestPodOOMReconciler_DedupsSameOOMEventAfterCooldown(t *testing.T) {
	// A pod's LastTerminationState is sticky after a restart. Without
	// event-identity dedup, every routine pod update after the cooldown
	// expires would re-trigger a bump and burn the limit to the ceiling.
	scheme := testScheme()
	finishedAt := metav1.NewTime(time.Now().Add(-30 * time.Minute))
	bumpedAt := metav1.NewTime(time.Now().Add(-20 * time.Minute)) // older than the cooldown

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-stack-prometheus-0",
			Namespace: monitoringNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "prometheus",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							Reason:     "OOMKilled",
							FinishedAt: finishedAt,
						},
					},
				},
			},
		},
	}
	eventID, _ := oomEventIdentity(pod)

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: PlatformConfigName,
			Annotations: map[string]string{
				lastOOMAnnotation(componentPrometheus): eventID,
			},
		},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileSmall,
			Components: []kipperv1.ComponentOverride{
				{Name: componentPrometheus, MemoryLimit: "1Gi"},
			},
		},
		Status: kipperv1.PlatformConfigStatus{
			Components: []kipperv1.ComponentStatus{
				{
					Name:         componentPrometheus,
					LastBumpAt:   &bumpedAt,
					LastBumpFrom: "512Mi",
					LastBumpTo:   "1Gi",
				},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, pod).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PodOOMReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod.Name, Namespace: pod.Namespace,
	}})
	require.NoError(t, err)

	var got kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))

	// Cooldown has expired, but the OOM event identity matches the
	// already-handled annotation, so the limit must not double again.
	assert.Equal(t, "1Gi", got.Spec.Components[0].MemoryLimit,
		"same OOM event must not bump twice even after the cooldown window")
}

func TestPodOOMReconciler_WritesEventIdentityAtomicallyWithBump(t *testing.T) {
	// Atomic write of override + event-identity annotation means a
	// subsequent status-write failure can't lead to a re-bump on retry.
	scheme := testScheme()
	finishedAt := metav1.NewTime(time.Now())

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileSmall},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-stack-prometheus-0",
			Namespace: monitoringNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "prometheus",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: finishedAt},
					},
				},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, pod).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PodOOMReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod.Name, Namespace: pod.Namespace,
	}})
	require.NoError(t, err)

	var got kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))

	expected, _ := oomEventIdentity(pod)
	assert.Equal(t, expected, got.Annotations[lastOOMAnnotation(componentPrometheus)],
		"event-identity annotation must land on the same Update as the spec bump")
	assert.Equal(t, "1Gi", got.Spec.Components[0].MemoryLimit)

	// Re-reconcile the same pod (simulating a stuck Status() update
	// returning error and controller-runtime requeuing the same pod). The
	// limit must stay at 1Gi, not double to 2Gi.
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod.Name, Namespace: pod.Namespace,
	}})
	require.NoError(t, err)
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))
	assert.Equal(t, "1Gi", got.Spec.Components[0].MemoryLimit,
		"a retry on the same OOM event must not re-bump")
}

func TestOomEventIdentity_PicksNewestAcrossContainers(t *testing.T) {
	// Multi-container pod: an older container's OOM stays sticky in
	// LastTerminationState. The reconciler must see a newer container's
	// OOM as a fresh event, not get deduped by the stale earlier one.
	older := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	newer := metav1.NewTime(time.Now().Add(-1 * time.Minute))

	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "sidecar",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: older},
					},
				},
				{
					Name: "prometheus",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: newer},
					},
				},
			},
		},
	}
	id, ok := oomEventIdentity(pod)
	require.True(t, ok)
	assert.Contains(t, id, "prometheus", "must use the newer container's identity")
	assert.NotContains(t, id, "sidecar", "must not be deceived by an older sticky OOM")
}

func TestPodOOMReconciler_CooldownAnnotationSurvivesStatusWriteFailure(t *testing.T) {
	// The cooldown lives on a metadata annotation that's written
	// atomically with the spec bump. A subsequent OOM (different event)
	// arriving inside the cooldown window must be debounced even if the
	// previous reconcile's status update never landed.
	scheme := testScheme()
	finishedAt1 := metav1.NewTime(time.Now().Add(-3 * time.Minute))

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileSmall},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-stack-prometheus-0",
			Namespace: monitoringNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "prometheus",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: finishedAt1},
					},
				},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, pod1).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PodOOMReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod1.Name, Namespace: pod1.Namespace,
	}})
	require.NoError(t, err)

	// Sanity: limit was bumped.
	var afterFirst kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &afterFirst))
	require.Equal(t, "1Gi", afterFirst.Spec.Components[0].MemoryLimit)

	// Simulate the situation the finding described: status got lost
	// after the spec write. Clear status.components and update the
	// resource to mimic a status-update-loss path.
	cleared := afterFirst.DeepCopy()
	cleared.Status.Components = nil
	require.NoError(t, fakeClient.Status().Update(context.Background(), cleared))

	// A SECOND pod with a DIFFERENT OOM event (newer FinishedAt) arrives
	// inside the cooldown window. The cooldown lives in the annotation,
	// not in status, so this must not produce a second bump.
	finishedAt2 := metav1.NewTime(time.Now())
	pod2 := pod1.DeepCopy()
	pod2.Status.ContainerStatuses[0].LastTerminationState.Terminated.FinishedAt = finishedAt2
	require.NoError(t, fakeClient.Update(context.Background(), pod2))

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod2.Name, Namespace: pod2.Namespace,
	}})
	require.NoError(t, err)

	var afterSecond kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &afterSecond))
	assert.Equal(t, "1Gi", afterSecond.Spec.Components[0].MemoryLimit,
		"cooldown annotation must prevent a re-bump within the 10-minute window even if status was lost")
}

func TestPodOOMReconciler_AuditRepairPreservesAtCeiling(t *testing.T) {
	// recordCeilingHit ran but its Status().Patch failed. Annotations
	// landed on spec (event handled, cooldown set). On retry, dedup
	// short-circuits Reconcile to repairStatusAudit. That repair must
	// recover the AtCeiling flag from the live limit, otherwise the user
	// loses the warning that auto-bump has run out of room.
	scheme := testScheme()
	finishedAt := metav1.NewTime(time.Now())
	bumpedAtStr := time.Now().UTC().Format(time.RFC3339)

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-stack-prometheus-0",
			Namespace: monitoringNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "prometheus",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: finishedAt},
					},
				},
			},
		},
	}
	eventID, _ := oomEventIdentity(pod)

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: PlatformConfigName,
			Annotations: map[string]string{
				lastOOMAnnotation(componentPrometheus):    eventID,
				lastBumpAtAnnotation(componentPrometheus): bumpedAtStr,
			},
		},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileXLarge,
			Components: []kipperv1.ComponentOverride{
				// Override at the ceiling — auto-bump can't help further.
				{Name: componentPrometheus, MemoryLimit: "4Gi"},
			},
		},
		// Status.Components is empty: a previous Status().Patch failed
		// and left the audit blank. This is what the retry sees.
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, pod).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PodOOMReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod.Name, Namespace: pod.Namespace,
	}})
	require.NoError(t, err)

	var got kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))

	require.Len(t, got.Status.Components, 1, "audit recovery should write the component status entry")
	cs := got.Status.Components[0]
	assert.Equal(t, componentPrometheus, cs.Name)
	assert.True(t, cs.AtCeiling,
		"audit repair must recover AtCeiling=true when the live limit equals the component ceiling")
	require.NotNil(t, cs.LastBumpAt, "audit timestamp must be filled from the annotation")
}

func TestPodOOMReconciler_AuditRepairOverwritesStalePriorBump(t *testing.T) {
	// First bump succeeded long ago. Then a new OOM bumped to the
	// ceiling but the follow-up Status().Patch failed, leaving status
	// stale (still showing the older 512Mi->1Gi bump with AtCeiling=false).
	// The retry must detect the staleness and repair, not exit early
	// just because LastBumpAt is non-nil.
	scheme := testScheme()

	oldBump := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	newBumpTS := time.Now().Add(-30 * time.Second).UTC().Format(time.RFC3339)
	finishedAt := metav1.NewTime(time.Now().Add(-1 * time.Minute))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-stack-prometheus-0",
			Namespace: monitoringNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "prometheus",
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: finishedAt},
					},
				},
			},
		},
	}
	eventID, _ := oomEventIdentity(pod)

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name: PlatformConfigName,
			Annotations: map[string]string{
				lastOOMAnnotation(componentPrometheus):    eventID,
				lastBumpAtAnnotation(componentPrometheus): newBumpTS,
			},
		},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileXLarge,
			Components: []kipperv1.ComponentOverride{
				// The new (ceiling) override is already in spec — it landed
				// with the annotations.
				{Name: componentPrometheus, MemoryLimit: "4Gi"},
			},
		},
		Status: kipperv1.PlatformConfigStatus{
			Components: []kipperv1.ComponentStatus{
				{
					Name:               componentPrometheus,
					LastBumpAt:         &oldBump,
					LastBumpFrom:       "512Mi",
					LastBumpTo:         "1Gi",
					LastBumpReason:     "OOMKilled (earlier event)",
					AtCeiling:          false,
					CurrentMemoryLimit: "1Gi",
				},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, pod).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PodOOMReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod.Name, Namespace: pod.Namespace,
	}})
	require.NoError(t, err)

	var got kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))

	var prom *kipperv1.ComponentStatus
	for i := range got.Status.Components {
		if got.Status.Components[i].Name == componentPrometheus {
			prom = &got.Status.Components[i]
		}
	}
	require.NotNil(t, prom)
	require.NotNil(t, prom.LastBumpAt)
	assert.True(t, prom.LastBumpAt.After(oldBump.Time),
		"status LastBumpAt must move forward to the new bump time, not stay on the older one")
	assert.True(t, prom.AtCeiling,
		"AtCeiling must be repaired to reflect that the new override sits at the 4Gi ceiling")
}

func TestPodOOMReconciler_SkipsWhenOOMedContainerIsRunning(t *testing.T) {
	// A pod with OOMKilled in lastTerminationState but state.Running != nil
	// has already recovered. Bumping in that state over-reacts to a
	// historical event and can drive the limit past what the workload
	// actually needs. The migration-day scenario: a pre-upgrade 512Mi OOM
	// stays sticky on the pod after the upgrade raises the limit to 2Gi
	// and the pod restarts cleanly.
	scheme := testScheme()
	oldFinishedAt := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	startedAt := metav1.NewTime(time.Now().Add(-30 * time.Minute))

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileXLarge},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-stack-prometheus-0",
			Namespace: monitoringNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "prometheus",
					// Container has been Running since startedAt.
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{StartedAt: startedAt},
					},
					// But still carries the stale OOMKilled in lastTerminationState.
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: oldFinishedAt},
					},
				},
			},
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, pod).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PodOOMReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod.Name, Namespace: pod.Namespace,
	}})
	require.NoError(t, err)

	var got kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))
	assert.Empty(t, got.Spec.Components,
		"a running pod with a historical OOM must not produce a bump")
}

func TestOomedContainerStabilized(t *testing.T) {
	t.Run("OOMed and currently Running", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
					},
				},
			},
		}}
		assert.True(t, oomedContainerStabilized(pod))
	})

	t.Run("OOMed and currently Waiting (CrashLoopBackOff)", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
					},
				},
			},
		}}
		assert.False(t, oomedContainerStabilized(pod), "an active CrashLoop must still trigger a bump")
	})

	t.Run("OOMed and currently Terminated", func(t *testing.T) {
		pod := &corev1.Pod{Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"}},
					LastTerminationState: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled"},
					},
				},
			},
		}}
		assert.False(t, oomedContainerStabilized(pod), "just-died counts as not stabilized")
	})
}

func TestPodOOMReconciler_IgnoresNonOOMPodEvents(t *testing.T) {
	scheme := testScheme()

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileSmall},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "prometheus-kube-prometheus-stack-prometheus-0",
			Namespace: monitoringNamespace,
			Labels:    map[string]string{"app.kubernetes.io/name": "prometheus"},
		},
		// Status with no OOMKilled - either a clean restart or no termination.
		Status: corev1.PodStatus{},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, pod).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PodOOMReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Name: pod.Name, Namespace: pod.Namespace,
	}})
	require.NoError(t, err)

	var got kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))
	assert.Empty(t, got.Spec.Components, "no bump expected for a healthy pod")
}
