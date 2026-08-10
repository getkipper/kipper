package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestSettings_APIKeyGatePending(t *testing.T) {
	appWith := func(require bool, cond *metav1.Condition) *kipperv1.App {
		a := &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
			Spec:       kipperv1.AppSpec{Route: &kipperv1.AppRoute{RequireAPIKey: require}},
		}
		if cond != nil {
			a.Status.Conditions = []metav1.Condition{*cond}
		}
		return a
	}
	ready := &metav1.Condition{Type: kipperv1.ConditionAPIKeyGateReady, Status: metav1.ConditionTrue, Reason: "GateEngaged", LastTransitionTime: metav1.Now()}
	failed := &metav1.Condition{Type: kipperv1.ConditionAPIKeyGateReady, Status: metav1.ConditionFalse, Reason: "MiddlewareReconcileFailed", LastTransitionTime: metav1.Now()}

	tests := []struct {
		name        string
		app         *kipperv1.App
		wantPending bool
	}{
		{"toggle on, gate not yet confirmed", appWith(true, nil), true},
		{"toggle on, gate reconcile failed", appWith(true, failed), true},
		{"toggle on, gate engaged", appWith(true, ready), false},
		{"toggle off", appWith(false, nil), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Settings{Client: fake.NewClientset(), CRClient: testCRClient(tt.app)}
			r := chi.NewRouter()
			r.Get("/api/v1/projects/{name}/apps/{app}/settings", s.Get)
			req := httptest.NewRequest(http.MethodGet, "/api/v1/projects/shop/apps/web/settings", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			var got appSettings
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got.APIKeyGatePending != tt.wantPending {
				t.Errorf("api_key_gate_pending = %v, want %v", got.APIKeyGatePending, tt.wantPending)
			}
		})
	}
}

func TestIngressReferencesApp(t *testing.T) {
	tests := []struct {
		name     string
		ingress  networkingv1.Ingress
		app      string
		expected bool
	}{
		{
			name: "matches by app label",
			ingress: networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "web"},
				},
			},
			app:      "web",
			expected: true,
		},
		{
			name: "matches by backend service name",
			ingress: networkingv1.Ingress{
				Spec: networkingv1.IngressSpec{
					Rules: []networkingv1.IngressRule{
						{
							IngressRuleValue: networkingv1.IngressRuleValue{
								HTTP: &networkingv1.HTTPIngressRuleValue{
									Paths: []networkingv1.HTTPIngressPath{
										{
											Backend: networkingv1.IngressBackend{
												Service: &networkingv1.IngressServiceBackend{
													Name: "api",
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			app:      "api",
			expected: true,
		},
		{
			name: "no match when different app",
			ingress: networkingv1.Ingress{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": "web"},
				},
				Spec: networkingv1.IngressSpec{
					Rules: []networkingv1.IngressRule{
						{
							IngressRuleValue: networkingv1.IngressRuleValue{
								HTTP: &networkingv1.HTTPIngressRuleValue{
									Paths: []networkingv1.HTTPIngressPath{
										{
											Backend: networkingv1.IngressBackend{
												Service: &networkingv1.IngressServiceBackend{
													Name: "web",
												},
											},
										},
									},
								},
							},
						},
					},
				},
			},
			app:      "api",
			expected: false,
		},
		{
			name:     "no match with empty ingress",
			ingress:  networkingv1.Ingress{},
			app:      "web",
			expected: false,
		},
		{
			name: "no match when rule has no HTTP section",
			ingress: networkingv1.Ingress{
				Spec: networkingv1.IngressSpec{
					Rules: []networkingv1.IngressRule{
						{Host: "example.com"},
					},
				},
			},
			app:      "web",
			expected: false,
		},
		{
			name: "no match when backend has no service",
			ingress: networkingv1.Ingress{
				Spec: networkingv1.IngressSpec{
					Rules: []networkingv1.IngressRule{
						{
							IngressRuleValue: networkingv1.IngressRuleValue{
								HTTP: &networkingv1.HTTPIngressRuleValue{
									Paths: []networkingv1.HTTPIngressPath{
										{Backend: networkingv1.IngressBackend{}},
									},
								},
							},
						},
					},
				},
			},
			app:      "web",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := ingressReferencesApp(tt.ingress, tt.app)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}
