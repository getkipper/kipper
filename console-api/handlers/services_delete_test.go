package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/controller/pkg/datavolume"
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

// The volume outlives the CR, so the delete has to ask for it. The finalizer
// destroys it, because a browser cannot hold a request open while a database
// stops and this endpoint answers 404 once the CR has gone, leaving a cleanup
// the handler did itself with no way to retry.
func TestServicesDelete_AsksForTheDataToGo(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mydb", Namespace: "default",
			Finalizers: []string{"kipper.run/test-hold"},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	claim := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-mydb-0", Namespace: "default", Labels: map[string]string{"app": "mydb"},
	}}

	crClient := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(svc).Build()
	k8sClient := fake.NewClientset(claim) //nolint:staticcheck
	handler := &Services{Client: k8sClient, CRClient: crClient}

	r := chi.NewRouter()
	r.Delete("/services/{name}", handler.Delete)
	req := httptest.NewRequest("DELETE", "/services/mydb?namespace=default&confirm=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	var deleting kipperv1.Service
	require.NoError(t, crClient.Get(context.Background(),
		types.NamespacedName{Name: "mydb", Namespace: "default"}, &deleting))
	assert.Equal(t, "true", deleting.Annotations[datavolume.DeleteAnnotation],
		"nothing asked for the data to go, so the finalizer will leave the volume")
	assert.False(t, deleting.DeletionTimestamp.IsZero(), "the service itself was not deleted")

	_, err := k8sClient.CoreV1().PersistentVolumeClaims("default").
		Get(context.Background(), "data-mydb-0", metav1.GetOptions{})
	assert.NoError(t, err,
		"the handler deleted the volume itself, which cannot wait for the workload and cannot be retried")
}

// Destroying the data takes as long as the workload takes to stop, and the
// service stays in the list until it has. Showing it as running the whole time
// reads as a delete that did nothing.
func TestServicesList_SaysAServiceIsDeleting(t *testing.T) {
	now := metav1.Now()
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mydb", Namespace: "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{"kipper.run/test-hold"},
		},
		Spec:   kipperv1.ServiceSpec{Type: "postgres"},
		Status: kipperv1.ServiceStatus{Phase: "Running"},
	}
	handler := &Services{
		Client:   fake.NewClientset(), //nolint:staticcheck
		CRClient: crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(svc).Build(),
	}

	req := httptest.NewRequest("GET", "/services?namespace=default", nil)
	rec := httptest.NewRecorder()
	handler.List(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var listed []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	require.Len(t, listed, 1)
	assert.Equal(t, "deleting", listed[0]["status"], "a service on its way out was shown as though nothing had happened")
	assert.Equal(t, "0/1", listed[0]["ready"])
}

// The mark and the delete both have to name the service that was read. A
// service somebody creates under the same name between the read and either call
// would otherwise be marked for data deletion and deleted, on a confirmation
// nobody gave for it.
func TestServicesDelete_PinsBothCallsToTheServiceItRead(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mydb", Namespace: "default",
			UID:        types.UID("the-service-being-deleted"),
			Finalizers: []string{"kipper.run/test-hold"},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}

	var markedWithLock, deletedWithUID bool
	crClient := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(svc).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, patch crclient.Patch, opts ...crclient.PatchOption) error {
				data, err := patch.Data(obj)
				require.NoError(t, err)
				markedWithLock = strings.Contains(string(data), "resourceVersion")
				return c.Patch(ctx, obj, patch, opts...)
			},
			Delete: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
				var options crclient.DeleteOptions
				options.ApplyOptions(opts)
				deletedWithUID = options.Preconditions != nil && options.Preconditions.UID != nil &&
					*options.Preconditions.UID == types.UID("the-service-being-deleted")
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()

	handler := &Services{Client: fake.NewClientset(), CRClient: crClient} //nolint:staticcheck
	r := chi.NewRouter()
	r.Delete("/services/{name}", handler.Delete)
	req := httptest.NewRequest("DELETE", "/services/mydb?namespace=default&confirm=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	assert.True(t, markedWithLock, "the mark was not pinned to the service that was read")
	assert.True(t, deletedWithUID, "the delete was not pinned to the service that was read")
}

// A service the controller has never reconciled carries no finalizer, so the API
// server would take it away the moment it was deleted and nothing would ever
// destroy its data. The mark puts the finalizer on with the request.
func TestServicesDelete_HoldsAServiceThatHasNoFinalizerYet(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "mydb", Namespace: "default"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(svc).Build()
	handler := &Services{Client: fake.NewClientset(), CRClient: crClient} //nolint:staticcheck

	r := chi.NewRouter()
	r.Delete("/services/{name}", handler.Delete)
	req := httptest.NewRequest("DELETE", "/services/mydb?namespace=default&confirm=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())

	var held kipperv1.Service
	require.NoError(t, crClient.Get(context.Background(),
		types.NamespacedName{Name: "mydb", Namespace: "default"}, &held),
		"the service went without anything destroying its data")
	assert.Contains(t, held.Finalizers, controllers.DataFinalizer)
	assert.Equal(t, "true", held.Annotations[datavolume.DeleteAnnotation])
}

// A delete that has stopped on something no retry clears sits at deleting for
// good. The reason is on the object; the list is where an operator reads it.
func TestServicesList_SaysWhyADeleteIsStuck(t *testing.T) {
	now := metav1.Now()
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mydb", Namespace: "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{"kipper.run/test-hold"},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
		Status: kipperv1.ServiceStatus{
			Phase: "Running",
			Conditions: []metav1.Condition{{
				Type:    kipperv1.ConditionCleanupComplete,
				Status:  metav1.ConditionFalse,
				Reason:  "DataNotDestroyed",
				Message: "the workload named mydb in default is not Kipper's",
			}},
		},
	}
	handler := &Services{
		Client:   fake.NewClientset(), //nolint:staticcheck
		CRClient: crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(svc).Build(),
	}

	req := httptest.NewRequest("GET", "/services?namespace=default", nil)
	rec := httptest.NewRecorder()
	handler.List(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	var listed []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	require.Len(t, listed, 1)
	assert.Equal(t, "deleting", listed[0]["status"])
	assert.Equal(t, "DataNotDestroyed", listed[0]["blockedReason"], "nothing said why the delete is stuck")
	assert.Contains(t, listed[0]["blockedMessage"], "not Kipper's")
}

// A service on its way out still holds its name. "Already exists" would send an
// operator looking for a service they have just deleted.
func TestServicesCreate_SaysTheNameIsHeldByADeleteInProgress(t *testing.T) {
	now := metav1.Now()
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mydb", Namespace: "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{"kipper.run/test-hold"},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	handler := &Services{
		Client:   fake.NewClientset(), //nolint:staticcheck
		CRClient: crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(svc).Build(),
	}

	body := strings.NewReader(`{"name":"mydb","namespace":"default","type":"postgres"}`)
	req := httptest.NewRequest("POST", "/services", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.Create(rec, req)

	require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "still being deleted",
		"an operator is told the name is taken by a service they have just deleted")
}

// The API server rejects a finalizer added to an object that is being deleted,
// so a second delete would fail on the patch and report nothing an operator can
// use. What happens to the volume was settled by whoever deleted it first.
func TestServicesDelete_RefusesAServiceThatIsAlreadyGoing(t *testing.T) {
	now := metav1.Now()
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mydb", Namespace: "default",
			DeletionTimestamp: &now,
			Finalizers:        []string{"kipper.run/test-hold"},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	handler := &Services{
		Client:   fake.NewClientset(), //nolint:staticcheck
		CRClient: crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(svc).Build(),
	}

	r := chi.NewRouter()
	r.Delete("/services/{name}", handler.Delete)
	req := httptest.NewRequest("DELETE", "/services/mydb?namespace=default&confirm=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	assert.Contains(t, rec.Body.String(), "already being deleted")
}

// The mark and the delete are one decision. A writer who takes the mark off
// between them would otherwise leave this answering that the data went while the
// finalizer keeps the volume.
func TestServicesDelete_PinsTheDeleteToTheMarkItMade(t *testing.T) {
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mydb", Namespace: "default",
			UID: types.UID("the-service-being-deleted"), ResourceVersion: "1",
			Finalizers: []string{"kipper.run/test-hold"},
		},
		Spec: kipperv1.ServiceSpec{Type: "postgres"},
	}
	var pinned *metav1.Preconditions
	crClient := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(svc).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
				var options crclient.DeleteOptions
				options.ApplyOptions(opts)
				pinned = options.Preconditions
				return c.Delete(ctx, obj, opts...)
			},
		}).Build()

	handler := &Services{Client: fake.NewClientset(), CRClient: crClient} //nolint:staticcheck
	r := chi.NewRouter()
	r.Delete("/services/{name}", handler.Delete)
	req := httptest.NewRequest("DELETE", "/services/mydb?namespace=default&confirm=true", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusNoContent, rec.Code, "body: %s", rec.Body.String())
	require.NotNil(t, pinned, "the delete carried no preconditions at all")
	require.NotNil(t, pinned.UID)
	assert.Equal(t, types.UID("the-service-being-deleted"), *pinned.UID)
	require.NotNil(t, pinned.ResourceVersion,
		"the delete is pinned to the service but not to the mark, so a writer can undo the mark in between")
	assert.NotEmpty(t, *pinned.ResourceVersion)
}

// Interference gets one answer whichever call meets it. A conflict on the mark
// and a conflict on the delete are the same event to an operator: somebody wrote
// to the service in between, and nothing here happened.
func TestServicesDelete_AnswersAConflictTheSameWayWhicheverCallMeetsIt(t *testing.T) {
	for _, meets := range []string{"patch", "delete"} {
		t.Run(meets, func(t *testing.T) {
			svc := &kipperv1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name: "mydb", Namespace: "default",
					UID: types.UID("the-service"), ResourceVersion: "1",
					Finalizers: []string{"kipper.run/test-hold"},
				},
				Spec: kipperv1.ServiceSpec{Type: "postgres"},
			}
			conflict := func() error {
				return apierrors.NewConflict(
					schema.GroupResource{Group: "kipper.run", Resource: "services"}, "mydb",
					fmt.Errorf("the object has changed"))
			}
			funcs := interceptor.Funcs{}
			if meets == "patch" {
				funcs.Patch = func(context.Context, crclient.WithWatch, crclient.Object, crclient.Patch, ...crclient.PatchOption) error {
					return conflict()
				}
			} else {
				funcs.Delete = func(context.Context, crclient.WithWatch, crclient.Object, ...crclient.DeleteOption) error {
					return conflict()
				}
			}
			crClient := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(svc).
				WithInterceptorFuncs(funcs).Build()

			handler := &Services{Client: fake.NewClientset(), CRClient: crClient} //nolint:staticcheck
			r := chi.NewRouter()
			r.Delete("/services/{name}", handler.Delete)
			req := httptest.NewRequest("DELETE", "/services/mydb?namespace=default&confirm=true", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
			assert.Contains(t, rec.Body.String(), "did not delete it")
			assert.NotContains(t, rec.Body.String(), "nothing was deleted")
		})
	}
}
