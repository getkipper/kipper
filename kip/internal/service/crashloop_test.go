package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// For three and a half days `kip service list` showed this service as running.
// Its phase came from the Service CR, which said running because the CR was
// fine; readiness came from the StatefulSet, which said 0/1. Nothing said the
// container had died nine hundred and ninety-six times.
//
// An operator reading "running 0/1" cannot tell a service that is starting from
// one that has been failing since Tuesday. That distinction is what these
// tests are about.

func crashLoopingPod(namespace, service, container string, restarts int32) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service + "-0",
			Namespace: namespace,
			Labels: map[string]string{
				"app":                     service,
				"kipper.run/service-type": "postgres",
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:         container,
				RestartCount: restarts,
				State: corev1.ContainerState{
					Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
				},
				LastTerminationState: corev1.ContainerState{
					Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
				},
			}},
		},
	}
}

func healthyPod(namespace, service string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      service + "-0",
			Namespace: namespace,
			Labels: map[string]string{
				"app":                     service,
				"kipper.run/service-type": "postgres",
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  service,
				Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
		},
	}
}

func TestListNamesACrashLoopingService(t *testing.T) {
	client := fake.NewSimpleClientset(crashLoopingPod("shop-test", "db", "postgres", 996)) //nolint:staticcheck
	dynClient := fakeDynamicForServices(t, "shop-test", map[string]string{"db": "postgres"})
	mgr := &Manager{Client: client, Dynamic: dynClient}

	services, err := mgr.List(context.Background(), "shop-test")
	require.NoError(t, err)
	require.Len(t, services, 1)

	assert.Equal(t, "crash-looping", services[0].Status,
		"a service whose container keeps dying must not read as running")
	assert.Contains(t, services[0].BlockedMessage, "996",
		"the restart count is the number that says how long this has been going on")
	assert.Contains(t, services[0].BlockedMessage, "kip service restart",
		"say how to recover it, since a container restart cannot clear a read-only mount")
}

func TestListLeavesAHealthyServiceAlone(t *testing.T) {
	client := fake.NewSimpleClientset(healthyPod("default", "db")) //nolint:staticcheck
	dynClient := fakeDynamicForServices(t, "default", map[string]string{"db": "postgres"})
	mgr := &Manager{Client: client, Dynamic: dynClient}

	services, err := mgr.List(context.Background(), "default")
	require.NoError(t, err)
	require.Len(t, services, 1)

	assert.NotEqual(t, "crash-looping", services[0].Status)
	assert.Empty(t, services[0].BlockedReason, "a healthy service raises nothing")
}

// Reading pods is best-effort. A cluster that refuses the list must still
// produce the service table it produced before, or a permissions problem turns
// into no output at all.
func TestListSurvivesAPodListFailure(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, assert.AnError
	})
	dynClient := fakeDynamicForServices(t, "default", map[string]string{"db": "postgres"})
	mgr := &Manager{Client: client, Dynamic: dynClient}

	services, err := mgr.List(context.Background(), "default")
	require.NoError(t, err, "a failed pod list must not fail the whole listing")
	require.Len(t, services, 1)
	assert.NotEqual(t, "crash-looping", services[0].Status)
}

// An app and a service can carry the same name in one namespace, and both their
// pods carry app=<name>. Only the service controller sets a service type, so
// that is what tells them apart. Without it, a crash-looping app pod would be
// reported as a broken database.
func TestCrashLoopingServicesIgnoresAppPods(t *testing.T) {
	appPod := crashLoopingPod("default", "db", "web", 12)
	delete(appPod.Labels, "kipper.run/service-type")
	client := fake.NewSimpleClientset(appPod) //nolint:staticcheck

	assert.Empty(t, CrashLoopingServices(context.Background(), client, "default"),
		"a pod with no service type belongs to an app, not to a service")
}

// A cluster with no client at all (an unconfigured manager) must not panic.
func TestCrashLoopingServicesWithoutAClient(t *testing.T) {
	assert.Nil(t, CrashLoopingServices(context.Background(), nil, "default"))
}

// kip status is not scoped to a project, so it needs every crash loop on the
// cluster rather than one namespace's.
func TestCrashLoopsEverywhereCoversEveryNamespace(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		crashLoopingPod("shop-test", "db", "postgres", 996),
		crashLoopingPod("shop-prod", "cache", "redis", 4),
		healthyPod("shop-prod", "queue"),
	)

	loops := CrashLoopsEverywhere(context.Background(), client)
	require.Len(t, loops, 2, "a healthy service is not a crash loop")

	seen := map[string]bool{}
	for _, l := range loops {
		seen[l.Namespace+"/"+l.Service] = true
		assert.NotEmpty(t, l.Message)
	}
	assert.True(t, seen["shop-test/db"])
	assert.True(t, seen["shop-prod/cache"])
}

// Two projects can each run a service called db, so "db is crash-looping" says
// nothing about which one an operator has to go and fix.
func TestCrashLoopsEverywhereTellsTwoServicesOfTheSameNameApart(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		crashLoopingPod("shop-test", "db", "postgres", 996),
		crashLoopingPod("shop-prod", "db", "postgres", 4),
	)

	loops := CrashLoopsEverywhere(context.Background(), client)
	require.Len(t, loops, 2)

	seen := map[string]bool{}
	for _, l := range loops {
		seen[l.Namespace+"/"+l.Service] = true
	}
	assert.True(t, seen["shop-test/db"])
	assert.True(t, seen["shop-prod/db"])
}

func TestCrashLoopsEverywhereIsQuietOnAHealthyCluster(t *testing.T) {
	client := fake.NewSimpleClientset(healthyPod("default", "db")) //nolint:staticcheck
	assert.Empty(t, CrashLoopsEverywhere(context.Background(), client))
}

// A service's init container mounts the same data volume the database does, so
// a read-only volume can stop the pod before the main container ever starts.
// Kubernetes then records the failure in InitContainerStatuses and leaves
// ContainerStatuses empty, so a listing that reads only the latter shows the
// stale CR phase and 0/1 while the pod cannot start at all.
func TestListNamesAServiceStuckInItsInitContainer(t *testing.T) {
	pod := crashLoopingPod("shop-test", "db", "remove-lost-found", 40)
	pod.Status.InitContainerStatuses = pod.Status.ContainerStatuses
	pod.Status.ContainerStatuses = nil

	client := fake.NewSimpleClientset(pod) //nolint:staticcheck
	dynClient := fakeDynamicForServices(t, "shop-test", map[string]string{"db": "postgres"})
	mgr := &Manager{Client: client, Dynamic: dynClient}

	services, err := mgr.List(context.Background(), "shop-test")
	require.NoError(t, err)
	require.Len(t, services, 1)

	assert.Equal(t, "crash-looping", services[0].Status,
		"a service whose init container keeps dying has not started and must not read as running")
	assert.Contains(t, services[0].BlockedMessage, "remove-lost-found",
		"and the message has to name the container that is failing")
}
