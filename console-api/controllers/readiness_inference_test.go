package controllers

import (
	"context"
	"fmt"
	"net"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var inferNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type podState struct {
	ip          string
	runningFor  time.Duration
	notReady    bool
	terminating bool
	pending     bool
	restarting  bool
	image       string
	foreign     bool
}

// inferenceWorld creates a Deployment, a ReplicaSet, and the requested pods.
// Foreign pods reference a different ReplicaSet UID to test ownership filtering.
func inferenceWorld(t *testing.T, pods ...podState) (crclient.Client, *appsv1.Deployment) {
	t.Helper()
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "shop", Namespace: "shop-prod", UID: "dep-uid"},
		Spec:       appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "shop"}}},
	}
	controller := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-1", Namespace: "shop-prod", UID: "rs-uid", Labels: map[string]string{"app": "shop"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: "shop", UID: "dep-uid", Controller: &controller}},
	}}
	objects := []crclient.Object{dep, rs}
	for i, p := range pods {
		owner := types.UID("rs-uid")
		if p.foreign {
			owner = "other-rs-uid"
		}
		image := p.image
		if image == "" {
			image = "registry.example.com/shop:1"
		}
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name: fmt.Sprintf("shop-1-%d", i), Namespace: "shop-prod", Labels: map[string]string{"app": "shop"},
				OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "shop-1", UID: owner, Controller: &controller}},
			},
			Spec:   corev1.PodSpec{Containers: []corev1.Container{{Name: "shop", Image: image}}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: p.ip},
		}
		if p.terminating {
			now := metav1.NewTime(inferNow)
			pod.DeletionTimestamp = &now
			pod.Finalizers = []string{"example.com/hold"}
		}
		if p.pending {
			pod.Status.Phase = corev1.PodPending
		}
		ready := corev1.ConditionTrue
		if p.notReady {
			ready = corev1.ConditionFalse
		}
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: ready}}
		started := metav1.NewTime(inferNow.Add(-p.runningFor))
		state := corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: started}}
		if p.restarting {
			state = corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}
		}
		pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "shop", State: state}}
		objects = append(objects, pod)
	}
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objects...).Build(), dep
}

type fakeDialer struct {
	mu      sync.Mutex
	answers map[string]error
	dialled []string
}

func (f *fakeDialer) dial(_ context.Context, addr string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dialled = append(f.dialled, addr)
	host, _, _ := net.SplitHostPort(addr)
	return f.answers[host]
}

var (
	refused  = &net.OpError{Op: "dial", Err: &osSyscallError{syscall.ECONNREFUSED}}
	timedOut = context.DeadlineExceeded
)

type osSyscallError struct{ errno syscall.Errno }

func (e *osSyscallError) Error() string { return e.errno.Error() }
func (e *osSyscallError) Unwrap() error { return e.errno }

func TestInferReadiness(t *testing.T) {
	young, settled := time.Minute, 11*time.Minute
	tests := []struct {
		name    string
		pods    []podState
		answers map[string]error
		app     func(*AppReconciler)
		want    inference
	}{
		{name: "a ready pod accepts", pods: []podState{{ip: "10.0.0.1", runningFor: settled}}, want: inferredTCP},
		{
			name: "a pod a minute old that accepts is evidence enough",
			pods: []podState{{ip: "10.0.0.1", runningFor: young}}, want: inferredTCP,
		},
		{
			name:    "one accepting pod outweighs a refusing one",
			pods:    []podState{{ip: "10.0.0.1", runningFor: settled}, {ip: "10.0.0.2", runningFor: settled}},
			answers: map[string]error{"10.0.0.2": refused}, want: inferredTCP,
		},
		{
			name: "a settled pod refuses",
			pods: []podState{{ip: "10.0.0.1", runningFor: settled}}, answers: map[string]error{"10.0.0.1": refused}, want: inferredNone,
		},
		{
			name: "a young pod refusing may still be starting",
			pods: []podState{{ip: "10.0.0.1", runningFor: 3 * time.Minute}}, answers: map[string]error{"10.0.0.1": refused}, want: inferencePending,
		},
		{
			name: "a refusal from a container that is restarting says nothing",
			pods: []podState{{ip: "10.0.0.1", restarting: true, notReady: true}}, answers: map[string]error{"10.0.0.1": refused}, want: inferencePending,
		},
		{
			name: "a timeout says nothing",
			pods: []podState{{ip: "10.0.0.1", runningFor: settled}}, answers: map[string]error{"10.0.0.1": timedOut}, want: inferencePending,
		},
		{
			name: "an accept from a pod that is not ready does not count",
			pods: []podState{{ip: "10.0.0.1", runningFor: settled, notReady: true}}, want: inferencePending,
		},
		{name: "a terminating pod is not asked", pods: []podState{{ip: "10.0.0.1", runningFor: settled, terminating: true}}, want: inferencePending},
		{name: "a pending pod is not asked", pods: []podState{{ip: "10.0.0.1", pending: true}}, want: inferencePending},
		{name: "the placeholder is not asked", pods: []podState{{ip: "10.0.0.1", runningFor: settled, image: "busybox:latest"}}, want: inferencePending},
		{name: "another workload's pod is not asked", pods: []podState{{ip: "10.0.0.1", runningFor: settled, foreign: true}}, want: inferencePending},
		{name: "no pods", want: inferencePending},
		{name: "no dialler", pods: []podState{{ip: "10.0.0.1", runningFor: settled}}, app: func(r *AppReconciler) { r.Dial = nil }, want: inferencePending},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, dep := inferenceWorld(t, tt.pods...)
			d := &fakeDialer{answers: tt.answers}
			r := &AppReconciler{Client: c, Scheme: testScheme(), Dial: d.dial}
			if tt.app != nil {
				tt.app(r)
			}
			app := healthApp(nil)
			app.Spec.Git = nil
			if len(tt.pods) > 0 && tt.pods[0].image == "busybox:latest" {
				app.Spec.Git = placeholderApp(nil).Spec.Git
			}
			assert.Equal(t, tt.want, r.inferReadiness(context.Background(), app, dep, inferNow))
		})
	}
}

func TestInferReadiness_DialsThePodIPOnTheAppPortAndAsksAtMostThree(t *testing.T) {
	pods := make([]podState, 5)
	for i := range pods {
		pods[i] = podState{ip: fmt.Sprintf("10.0.0.%d", i+1), runningFor: time.Hour}
	}
	c, dep := inferenceWorld(t, pods...)
	d := &fakeDialer{answers: map[string]error{}}
	for _, p := range pods {
		d.answers[p.ip] = refused
	}
	r := &AppReconciler{Client: c, Scheme: testScheme(), Dial: d.dial}

	assert.Equal(t, inferredNone, r.inferReadiness(context.Background(), healthApp(nil), dep, inferNow))
	require.Len(t, d.dialled, 3)
	for _, addr := range d.dialled {
		_, port, err := net.SplitHostPort(addr)
		require.NoError(t, err)
		assert.Equal(t, "8080", port)
	}
}

func TestTCPDial_TellsARefusalFromAnAccept(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	require.NoError(t, TCPDial(context.Background(), addr))
	require.NoError(t, ln.Close())

	err = TCPDial(context.Background(), addr)
	require.Error(t, err)
	assert.True(t, connectionRefused(err), "a closed port is a refusal: %v", err)
	assert.False(t, connectionRefused(context.DeadlineExceeded))
}

func TestInferenceCandidates_PrefersReadyAndLongRunningPods(t *testing.T) {
	app := healthApp(nil)
	c, dep := inferenceWorld(t,
		podState{ip: "10.0.0.1", restarting: true, notReady: true},
		podState{ip: "10.0.0.2", runningFor: time.Minute, notReady: true},
		podState{ip: "10.0.0.3", runningFor: time.Minute},
		podState{ip: "10.0.0.4", runningFor: time.Hour},
	)
	pods, err := ownedPods(context.Background(), c, dep)
	require.NoError(t, err)
	var ips []string
	for _, p := range inferenceCandidates(app, pods) {
		ips = append(ips, p.Status.PodIP)
	}
	assert.Equal(t, []string{"10.0.0.4", "10.0.0.3", "10.0.0.2"}, ips)
}
