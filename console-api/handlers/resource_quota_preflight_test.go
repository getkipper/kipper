package handlers

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func tightQuota() *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: kipperv1.ProjectQuotaName, Namespace: "shop"},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("2")}},
		Status:     corev1.ResourceQuotaStatus{Used: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("1")}},
	}
}

func containerDeployment() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{
			Replicas: func() *int32 { r := int32(1); return &r }(),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:      "api",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
			}}}},
		},
	}
}

func TestResources_UpdateBlockedByQuota(t *testing.T) {
	client := fake.NewClientset(containerDeployment(), tightQuota())
	res := &Resources{Client: client, CRClient: testCRClient()}

	r := chi.NewRouter()
	r.Put("/api/v1/projects/{name}/apps/{app}/resources", res.Update)

	// Raising to 3 CPU needs ~2.9 more than the current 100m, well past the
	// 2-core cap with 1 already used.
	req := httptest.NewRequest("PUT", "/api/v1/projects/shop/apps/api/resources", strings.NewReader(`{"cpu_request":"3"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 for an over-quota app resource change, got %d; body %s", rec.Code, rec.Body.String())
	}
}

func TestServices_UpdateResourcesBlockedByQuota(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: func() *int32 { r := int32(1); return &r }(),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:      "db",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}},
			}}}},
		},
	}
	client := fake.NewClientset(sts, tightQuota())
	s := &Services{Client: client}

	r := chi.NewRouter()
	r.Put("/api/v1/services/{name}/resources", s.UpdateResources)

	req := httptest.NewRequest("PUT", "/api/v1/services/db/resources?namespace=shop", strings.NewReader(`{"cpu_request":"3"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 for an over-quota service resource change, got %d; body %s", rec.Code, rec.Body.String())
	}
}

// A CPU-only Function change omits memory, but the Function reconciler defaults
// unpinned memory to 64Mi. The preflight must project that default, so a change
// whose CPU fits but whose implied memory default does not still returns 409.
func TestResources_FunctionUpdateProjectsDefaultedMemory(t *testing.T) {
	fnDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "fn1", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{
			Replicas: func() *int32 { r := int32(1); return &r }(),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "fn1",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("50m"),
					corev1.ResourceMemory: resource.MustParse("16Mi"),
				}},
			}}}},
		},
	}
	// Room for the small CPU bump, but not for memory rising from 16Mi to the
	// 64Mi function default (used 40Mi + 48Mi delta = 88Mi over the 64Mi cap).
	rq := &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: kipperv1.ProjectQuotaName, Namespace: "shop"},
		Spec: corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{
			corev1.ResourceRequestsCPU:    resource.MustParse("4"),
			corev1.ResourceRequestsMemory: resource.MustParse("64Mi"),
		}},
		Status: corev1.ResourceQuotaStatus{Used: corev1.ResourceList{
			corev1.ResourceRequestsCPU:    resource.MustParse("100m"),
			corev1.ResourceRequestsMemory: resource.MustParse("40Mi"),
		}},
	}
	client := fake.NewClientset(fnDeploy, rq)
	res := &Resources{Client: client, CRClient: testCRClient()}

	r := chi.NewRouter()
	r.Put("/api/v1/projects/{name}/functions/{fn}/resources", res.UpdateByParam("fn", ResourceKindFunction))

	req := httptest.NewRequest("PUT", "/api/v1/projects/shop/functions/fn1/resources", strings.NewReader(`{"cpu_request":"100m"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 from the memory default a CPU-only function change implies, got %d; body %s", rec.Code, rec.Body.String())
	}
}

func TestResources_UpdateWithinQuotaNotBlocked(t *testing.T) {
	client := fake.NewClientset(containerDeployment(), tightQuota())
	res := &Resources{Client: client, CRClient: testCRClient()}

	r := chi.NewRouter()
	r.Put("/api/v1/projects/{name}/apps/{app}/resources", res.Update)

	// 500m is within the 2-core cap (1 used + ~0.4 delta), so the preflight
	// must not return 409. The app CR is absent, so the write then 404s — the
	// point is that the request cleared the quota gate rather than being
	// rejected at it.
	req := httptest.NewRequest("PUT", "/api/v1/projects/shop/apps/api/resources", strings.NewReader(`{"cpu_request":"500m"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code == http.StatusConflict {
		t.Errorf("within-quota change must not be blocked by the quota preflight; body %s", rec.Body.String())
	}
}
