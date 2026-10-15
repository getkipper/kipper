package k8s

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var readyNodes = map[string]bool{"node-1": true}

func pod(phase corev1.PodPhase, init []corev1.ContainerStatus, containers ...corev1.ContainerStatus) corev1.Pod {
	return corev1.Pod{Spec: corev1.PodSpec{NodeName: "node-1"}, Status: corev1.PodStatus{Phase: phase, InitContainerStatuses: init, ContainerStatuses: containers}}
}

func waiting(reason string) corev1.ContainerStatus {
	return corev1.ContainerStatus{State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
}

func runningNotReady() corev1.ContainerStatus {
	return corev1.ContainerStatus{State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}
}

func restartedAfter(exitCode int32) corev1.ContainerStatus {
	s := runningNotReady()
	s.RestartCount = 3
	s.LastTerminationState = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: exitCode}}
	return s
}

func completedAfterRetries() corev1.ContainerStatus {
	s := restartedAfter(1)
	s.State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	return s
}

func TestPodsStartingRecognisesPodsOnTheirWayUp(t *testing.T) {
	cases := map[string][]corev1.Pod{
		"running, probe not passed yet": {pod(corev1.PodRunning, nil, runningNotReady())},
		"creating its container":        {pod(corev1.PodPending, nil, waiting("ContainerCreating"))},
		"running its init container": {pod(corev1.PodPending,
			[]corev1.ContainerStatus{runningNotReady()}, waiting("PodInitializing"))},
		"pending before any status": {pod(corev1.PodPending, nil)},
		"replacing an evicted pod": {
			pod(corev1.PodFailed, nil),
			pod(corev1.PodPending, nil, waiting("ContainerCreating")),
		},
		"init container done after retries": {pod(corev1.PodPending,
			[]corev1.ContainerStatus{completedAfterRetries()}, waiting("PodInitializing"))},
		"replacing a completed pod": {
			pod(corev1.PodSucceeded, nil),
			pod(corev1.PodRunning, nil, runningNotReady()),
		},
	}
	for name, pods := range cases {
		if !podsStarting(pods, readyNodes) {
			t.Errorf("%s: reported as down, want starting", name)
		}
	}
}

func TestPodsStartingKeepsBrokenComponentsFailed(t *testing.T) {
	terminating := pod(corev1.PodRunning, nil, runningNotReady())
	now := metav1.NewTime(time.Now())
	terminating.DeletionTimestamp = &now
	unschedulable := pod(corev1.PodPending, nil)
	unschedulable.Status.Conditions = []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
	}}

	onLostNode := pod(corev1.PodRunning, nil, runningNotReady())
	onLostNode.Spec.NodeName = "node-2"

	cases := map[string][]corev1.Pod{
		"no pods":                      nil,
		"on a NotReady node":           {onLostNode},
		"only finished pods":           {pod(corev1.PodFailed, nil), pod(corev1.PodSucceeded, nil)},
		"crash looping":                {pod(corev1.PodRunning, nil, waiting("CrashLoopBackOff"))},
		"image pull failing":           {pod(corev1.PodPending, nil, waiting("ImagePullBackOff"))},
		"bad config":                   {pod(corev1.PodPending, nil, waiting("CreateContainerConfigError"))},
		"init container loop":          {pod(corev1.PodPending, []corev1.ContainerStatus{waiting("CrashLoopBackOff")}, waiting("PodInitializing"))},
		"failed pod":                   {pod(corev1.PodFailed, nil)},
		"only terminating":             {terminating},
		"unschedulable":                {unschedulable},
		"running between crashes":      {pod(corev1.PodRunning, nil, restartedAfter(1))},
		"restarting after clean exits": {pod(corev1.PodRunning, nil, restartedAfter(0))},
		"one of two broken": {
			pod(corev1.PodRunning, nil, runningNotReady()),
			pod(corev1.PodRunning, nil, waiting("CrashLoopBackOff")),
		},
	}
	for name, pods := range cases {
		if podsStarting(pods, readyNodes) {
			t.Errorf("%s: reported as starting, want down", name)
		}
	}
}
