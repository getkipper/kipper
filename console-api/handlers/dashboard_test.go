package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestDashboard_ReturnsComponentStatus(t *testing.T) {
	replicas := int32(1)

	objects := []runtime.Object{
		// k3s — a ready node
		&corev1.Node{
			ObjectMeta: metav1.ObjectMeta{Name: "node-1"},
			Status: corev1.NodeStatus{
				Conditions: []corev1.NodeCondition{
					{Type: corev1.NodeReady, Status: corev1.ConditionTrue},
				},
			},
		},
		// Traefik deployment
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "traefik", Namespace: "traefik"},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas},
			Status:     appsv1.DeploymentStatus{AvailableReplicas: 1},
		},
		// Loki statefulset
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "loki", Namespace: "monitoring"},
			Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
			Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
		},
		// Promtail daemonset
		&appsv1.DaemonSet{
			ObjectMeta: metav1.ObjectMeta{Name: "promtail", Namespace: "monitoring"},
			Status:     appsv1.DaemonSetStatus{NumberReady: 1, DesiredNumberScheduled: 1},
		},
	}

	client := fake.NewClientset(objects...)
	handler := &Dashboard{Client: client}

	req := httptest.NewRequest("GET", "/api/v1/dashboard", nil)
	w := httptest.NewRecorder()
	handler.Status(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp dashboardResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	// Verify we get all 13 components
	assert.Len(t, resp.Components, 13)

	// Check specific components that we seeded
	componentMap := make(map[string]componentStatus)
	for _, c := range resp.Components {
		componentMap[c.Name] = c
	}

	assert.True(t, componentMap["k3s"].Healthy)
	assert.Equal(t, "1 node(s)", componentMap["k3s"].Message)

	assert.True(t, componentMap["Traefik"].Healthy)
	assert.Equal(t, "1/1 available", componentMap["Traefik"].Message)

	assert.True(t, componentMap["Loki"].Healthy)
	assert.Equal(t, "1/1 ready", componentMap["Loki"].Message)

	assert.True(t, componentMap["Promtail"].Healthy)
	assert.Equal(t, "1/1 ready", componentMap["Promtail"].Message)

	// Components not seeded should be unhealthy
	assert.False(t, componentMap["cert-manager"].Healthy)
	assert.Equal(t, "not found", componentMap["cert-manager"].Message)

	// Verify nodes section
	assert.Len(t, resp.Nodes, 1)
	assert.Equal(t, "node-1", resp.Nodes[0].Name)
	assert.Equal(t, "Ready", resp.Nodes[0].Status)
}

func TestDashboard_DetectsOOMKills(t *testing.T) {
	oomTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))

	objects := []runtime.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "loki-0", Namespace: "monitoring"},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name: "loki",
						LastTerminationState: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{
								Reason:     "OOMKilled",
								FinishedAt: oomTime,
							},
						},
					},
				},
			},
		},
	}

	client := fake.NewClientset(objects...)
	handler := &Dashboard{Client: client}

	req := httptest.NewRequest("GET", "/api/v1/dashboard", nil)
	w := httptest.NewRecorder()
	handler.Status(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp dashboardResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	require.Len(t, resp.OOMKills, 1)
	assert.Equal(t, "loki-0", resp.OOMKills[0].Pod)
	assert.Equal(t, "monitoring", resp.OOMKills[0].Namespace)
}

func TestDashboard_NoOOMKills(t *testing.T) {
	// Pod with a normal termination (not OOM)
	objects := []runtime.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "worker-1", Namespace: "default"},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name: "app",
						LastTerminationState: corev1.ContainerState{
							Terminated: &corev1.ContainerStateTerminated{
								Reason:     "Completed",
								FinishedAt: metav1.NewTime(time.Now().Add(-30 * time.Minute)),
							},
						},
					},
				},
			},
		},
	}

	client := fake.NewClientset(objects...)
	handler := &Dashboard{Client: client}

	req := httptest.NewRequest("GET", "/api/v1/dashboard", nil)
	w := httptest.NewRecorder()
	handler.Status(w, req)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp dashboardResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	assert.Empty(t, resp.OOMKills)
}

func TestDashboard_OOMKillsCachedWithinTTL(t *testing.T) {
	client := fake.NewClientset()
	var lists int32
	client.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		atomic.AddInt32(&lists, 1)
		return false, nil, nil // fall through to the default tracker
	})

	h := &Dashboard{Client: client}
	req := httptest.NewRequest("GET", "/api/v1/dashboard", nil)

	for i := 0; i < 3; i++ {
		h.getOOMKills(req.Context(), req)
	}

	if got := atomic.LoadInt32(&lists); got != 1 {
		t.Fatalf("expected the pod list to be cached and run once within the TTL, got %d lists", got)
	}
}

func TestDashboard_OOMKillsFilteredPerRequestNotCached(t *testing.T) {
	withResolver(t)

	oomTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "blog-web-0", Namespace: "blog"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:                 "web",
			LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Reason: "OOMKilled", FinishedAt: oomTime}},
		}}},
	}
	h := &Dashboard{Client: fake.NewClientset(pod)}

	// An outsider's request populates the (unfiltered) cache but must not see
	// the blog OOM.
	outsider := reqAs("outsider@test.com", "")
	if got := h.getOOMKills(outsider.Context(), outsider); len(got) != 0 {
		t.Fatalf("outsider must not see the blog OOM, got %+v", got)
	}

	// An admin reading from the same cache does see it, proving filtering is
	// applied per request rather than baked into the cache.
	admin := reqAs("root@test.com", "")
	got := h.getOOMKills(admin.Context(), admin)
	if len(got) != 1 || got[0].Pod != "blog-web-0" {
		t.Fatalf("admin should see the cached blog OOM, got %+v", got)
	}
}
