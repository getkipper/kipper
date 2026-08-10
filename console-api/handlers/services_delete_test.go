package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func testCRScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = kipperv1.AddToScheme(scheme)
	return scheme
}

func TestServicesDelete_DeletesCR(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mydb",
			Namespace: "default",
			Labels: map[string]string{
				kipperLabel: kipperValue,
				"app":       "mydb",
			},
		},
		Spec: kipperv1.ServiceSpec{
			Type:    "postgres",
			Storage: "5Gi",
		},
	}

	scheme := testCRScheme()
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).Build()
	k8sClient := fake.NewClientset() //nolint:staticcheck

	handler := &Services{Client: k8sClient, CRClient: crClient}

	r := chi.NewRouter()
	r.Delete("/services/{name}", handler.Delete)

	req := httptest.NewRequest("DELETE", "/services/mydb?namespace=default&confirm=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected status %d, got %d; body: %s", http.StatusNoContent, rec.Code, rec.Body.String())
	}
}

func TestServicesDelete_WithoutConfirmReturns400(t *testing.T) {
	handler := &Services{Client: fake.NewClientset()} //nolint:staticcheck

	r := chi.NewRouter()
	r.Delete("/services/{name}", handler.Delete)

	req := httptest.NewRequest("DELETE", "/services/mydb", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected status %d, got %d; body: %s", http.StatusBadRequest, rec.Code, rec.Body.String())
	}
}

func TestServicesDelete_NonexistentServiceReturns404(t *testing.T) {
	scheme := testCRScheme()
	crClient := crfake.NewClientBuilder().WithScheme(scheme).Build()
	k8sClient := fake.NewClientset() //nolint:staticcheck

	handler := &Services{Client: k8sClient, CRClient: crClient}

	r := chi.NewRouter()
	r.Delete("/services/{name}", handler.Delete)

	req := httptest.NewRequest("DELETE", "/services/nonexistent?namespace=default&confirm=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected status %d, got %d; body: %s", http.StatusNotFound, rec.Code, rec.Body.String())
	}
}

// Deleting a service used to strip its bindings from Apps only. A Function was
// left declaring a binding that can never resolve — which now fails its
// reconcile outright — and its derived credentials outlived the service they
// were copied from.
func TestServicesDelete_ClearsBindingsOnBothWorkloadKinds(t *testing.T) {
	ctx := context.Background()

	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-test"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-api")},
		Spec: kipperv1.AppSpec{Image: "api:1", ServiceBindings: []kipperv1.ServiceBinding{
			{Name: "db", Prefix: "DB_", Database: "api_db"},
			{Name: "cache", Prefix: "REDIS_"},
		}},
	}
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "shop-test", UID: types.UID("uid-resize")},
		Spec: kipperv1.FunctionSpec{ServiceBindings: []kipperv1.ServiceBinding{
			{Name: "db", Prefix: "DB_", Database: "resize_db"},
		}},
	}
	controller := true
	owned := func(name, kind, workload, uid string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop-test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: kind,
				Name: workload, UID: types.UID(uid), Controller: &controller,
			}},
		}}
	}
	appDerived := owned("db-app-api-credentials", "App", "api", "uid-api")
	fnDerived := owned("db-function-resize-credentials", "Function", "resize", "uid-resize")

	crClient := testCRClient(svc, app, fn, appDerived, fnDerived)
	handler := &Services{Client: fake.NewClientset(), CRClient: crClient}

	r := chi.NewRouter()
	r.Delete("/services/{name}", handler.Delete)
	req := httptest.NewRequest(http.MethodDelete, "/services/db?namespace=shop-test&confirm=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	var gotApp kipperv1.App
	require.NoError(t, handler.CRClient.Get(ctx, crclient.ObjectKey{Namespace: "shop-test", Name: "api"}, &gotApp))
	require.Len(t, gotApp.Spec.ServiceBindings, 1, "only the binding to the deleted service goes")
	assert.Equal(t, "cache", gotApp.Spec.ServiceBindings[0].Name)

	var gotFn kipperv1.Function
	require.NoError(t, handler.CRClient.Get(ctx, crclient.ObjectKey{Namespace: "shop-test", Name: "resize"}, &gotFn))
	assert.Empty(t, gotFn.Spec.ServiceBindings,
		"a Function's binding to the deleted service must go too, or its reconcile fails on a service that cannot come back")

	// The projections outlive the service on purpose. A workload still serving
	// from a retained revision reads one on every container restart, so each is
	// retired by its own workload's reconcile once nothing names it, under the
	// same grace the published environments use.
	assert.NoError(t, crClient.Get(ctx, crclient.ObjectKey{Namespace: "shop-test", Name: "db-app-api-credentials"}, &corev1.Secret{}),
		"the App's projection is the reconcile's to retire, not this endpoint's")
	assert.NoError(t, crClient.Get(ctx, crclient.ObjectKey{Namespace: "shop-test", Name: "db-function-resize-credentials"}, &corev1.Secret{}),
		"and so is the Function's")
}

// A cleanup that cannot finish must not leave the service deleted. The endpoint
// answers 404 once the CR is gone, so a swallowed failure here is unretryable —
// and every workload still bound to the vanished service stops reconciling.
func TestServicesDelete_FailsWithoutDeletingWhenUnbindFails(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-test"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	controller := true
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-test", UID: types.UID("uid-api")},
		Spec: kipperv1.AppSpec{Image: "api:1", ServiceBindings: []kipperv1.ServiceBinding{
			{Name: "db", Prefix: "DB_", Database: "api_db"},
		}},
	}
	derived := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "db-app-api-credentials", Namespace: "shop-test",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
			Name: "api", UID: types.UID("uid-api"), Controller: &controller,
		}},
	}}

	// Clearing the binding writes the workload, and that write fails. The
	// endpoint used to fail here on deleting a derived Secret; that deletion
	// moved to the workload's reconcile, so the failure to provoke is the one
	// unbinding still owns.
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, app, derived).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, isApp := obj.(*kipperv1.App); isApp {
					return apierrors.NewInternalError(fmt.Errorf("etcd unavailable"))
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	handler := &Services{Client: fake.NewClientset(), CRClient: crClient}

	r := chi.NewRouter()
	r.Delete("/services/{name}", handler.Delete)
	req := httptest.NewRequest(http.MethodDelete, "/services/db?namespace=shop-test&confirm=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code, "a failed unbind must be reported, not swallowed")

	var still kipperv1.Service
	assert.NoError(t, handler.CRClient.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-test", Name: "db"}, &still),
		"the service must still exist so the caller can retry")
}
