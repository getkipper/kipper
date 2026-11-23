package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// imageRefused simulates an API server validation error for an App image.
func imageRefused(image string) error {
	return apierrors.NewInvalid(schema.GroupKind{Group: "kipper.run", Kind: "App"}, "web",
		field.ErrorList{field.Invalid(field.NewPath("spec", "image"), image, `spec.image in body should match '^\S*$'`)})
}

func refusingWorkloadWrites() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			switch w := obj.(type) {
			case *kipperv1.App:
				return imageRefused(w.Spec.Image)
			case *kipperv1.Job:
				return imageRefused(w.Spec.Image)
			case *kipperv1.Function:
				return imageRefused(w.Spec.Image)
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
			if app, isApp := obj.(*kipperv1.App); isApp {
				return imageRefused(app.Spec.Image)
			}
			return c.Update(ctx, obj, opts...)
		},
	}
}

// TestCreateApp_ReportsAnImageTheAPIServerRefuses verifies that a pasted image
// with a trailing space produces a validation response with the offending field,
// rather than a server error.
func TestCreateApp_ReportsAnImageTheAPIServerRefuses(t *testing.T) {
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(refusingWorkloadWrites()).Build()
	handler := &Apps{Client: fake.NewClientset(), CRClient: crClient, GitReach: gitAlwaysReachable}
	r := chi.NewRouter()
	r.Post("/projects/{name}/apps", handler.Create)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/projects/shop-test/apps", strings.NewReader(`{"name":"web","port":80,"image":"nginx:1.27 "}`)))

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "spec.image")
}

func TestUpdateImage_ReportsAnImageTheAPIServerRefuses(t *testing.T) {
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "staging", Labels: kipperLabels("web")},
		Spec:       kipperv1.AppSpec{Image: "nginx:1.25", Port: 80},
	}
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app).WithInterceptorFuncs(refusingWorkloadWrites()).Build()
	handler := &Apps{Client: fake.NewClientset(), CRClient: crClient, GitReach: gitAlwaysReachable}
	r := chi.NewRouter()
	r.Put("/projects/{name}/apps/{app}/image", handler.UpdateImage)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("PUT", "/projects/staging/apps/web/image", strings.NewReader(`{"image":"nginx:1.27 "}`)))

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "spec.image")
}

func TestCreateJob_ReportsAnImageTheAPIServerRefuses(t *testing.T) {
	withCollisionResolver(t, "deployer", "deployer")
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(refusingWorkloadWrites()).Build()
	h := &Jobs{Client: fake.NewClientset(), CRClient: crClient}

	rec := httptest.NewRecorder()
	h.Create(rec, jobRequest("POST", "/api/v1/jobs", "root@test.com", "", `{"name":"backup","image":"busybox x"}`))

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "spec.image")
}

func TestCreateFunction_ReportsAnImageTheAPIServerRefuses(t *testing.T) {
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).WithInterceptorFuncs(refusingWorkloadWrites()).Build()
	handler := &Functions{Client: fake.NewClientset(), CRClient: crClient}
	r := chi.NewRouter()
	r.Post("/projects/{name}/functions", handler.Create)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest("POST", "/projects/shop-test/functions", strings.NewReader(`{"name":"hello","image":"nginx:1.27 "}`)))

	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "spec.image")
}
