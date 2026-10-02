package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func healthCheckApp(health *kipperv1.AppHealth) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/api:1", Port: 8080, Health: health},
		Status: kipperv1.AppStatus{HealthCheck: &kipperv1.AppHealthCheckStatus{
			Type: "tcp", Port: 8080, Source: "inferred",
		}},
	}
}

func serveHealthCheck(t *testing.T, c crclient.Client, method, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := &HealthCheck{CRClient: c}
	r := chi.NewRouter()
	r.Get("/api/v1/projects/{name}/apps/{app}/health-check", h.Get)
	r.Put("/api/v1/projects/{name}/apps/{app}/health-check", h.Update)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/projects/shop-prod/apps/api/health-check", bytes.NewBufferString(body)))
	return rec
}

func storedHealth(t *testing.T, c crclient.Client) *kipperv1.App {
	t.Helper()
	var app kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-prod", Name: "api"}, &app))
	return &app
}

func TestHealthCheckGet(t *testing.T) {
	t.Run("automatic says what Kipper chose", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(healthCheckApp(nil)).Build()
		rec := serveHealthCheck(t, c, http.MethodGet, "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.JSONEq(t, `{"health":{"type":"auto"},"status":{"type":"tcp","port":8080,"source":"inferred"}}`, rec.Body.String())
	})
	t.Run("declared", func(t *testing.T) {
		port := int32(8081)
		c := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(healthCheckApp(&kipperv1.AppHealth{Type: "http", Path: "/ready", Port: &port})).Build()
		rec := serveHealthCheck(t, c, http.MethodGet, "")
		require.Equal(t, http.StatusOK, rec.Code)
		var got healthCheckResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		assert.Equal(t, "http", got.Health.Type)
		assert.Equal(t, "/ready", got.Health.Path)
		require.NotNil(t, got.Health.Port)
		assert.Equal(t, int32(8081), *got.Health.Port)
	})
	t.Run("a missing app", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(testCRScheme()).Build()
		assert.Equal(t, http.StatusNotFound, serveHealthCheck(t, c, http.MethodGet, "").Code)
	})
}

func TestHealthCheckUpdate(t *testing.T) {
	t.Run("declares a check, normalised", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(healthCheckApp(nil)).Build()
		rec := serveHealthCheck(t, c, http.MethodPut, `{"type":"tcp","path":"/left-over","startup_timeout_seconds":600}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got := storedHealth(t, c).Spec.Health
		require.NotNil(t, got)
		assert.Equal(t, "tcp", got.Type)
		assert.Empty(t, got.Path, "a tcp check takes no path")
		require.NotNil(t, got.StartupTimeoutSeconds)
		assert.Equal(t, int32(600), *got.StartupTimeoutSeconds)
	})
	t.Run("auto clears it", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(healthCheckApp(&kipperv1.AppHealth{Type: "tcp"})).Build()
		rec := serveHealthCheck(t, c, http.MethodPut, `{"type":"auto"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Nil(t, storedHealth(t, c).Spec.Health)
	})
	t.Run("a route-less app stays route-less", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(healthCheckApp(nil)).Build()
		require.Equal(t, http.StatusOK, serveHealthCheck(t, c, http.MethodPut, `{"type":"none"}`).Code)
		assert.Nil(t, storedHealth(t, c).Spec.Route, "saving a check must not give a worker a route")
	})
	t.Run("a check Kipper would refuse is explained", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(healthCheckApp(nil)).Build()
		rec := serveHealthCheck(t, c, http.MethodPut, `{"type":"http"}`)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		assert.Contains(t, rec.Body.String(), "needs a path")
		assert.Nil(t, storedHealth(t, c).Spec.Health)
	})
	t.Run("the API server's refusal is passed on", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(healthCheckApp(nil)).
			WithInterceptorFuncs(interceptor.Funcs{
				Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
					return apierrors.NewInvalid(schema.GroupKind{Group: "kipper.run", Kind: "App"}, "api", field.ErrorList{
						field.Invalid(field.NewPath("spec", "health"), nil, "a check of type none takes no port or timeout"),
					})
				},
			}).Build()
		rec := serveHealthCheck(t, c, http.MethodPut, `{"type":"tcp"}`)
		assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
		assert.Contains(t, rec.Body.String(), "takes no port or timeout")
	})
	t.Run("an older App schema that drops the check is reported", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(healthCheckApp(nil)).
			WithInterceptorFuncs(interceptor.Funcs{
				// The cluster stores the App without the field, while the
				// caller's object keeps it: decoding the response into the
				// same struct does not clear a field the response leaves out.
				Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
					app, ok := obj.(*kipperv1.App)
					if !ok {
						return c.Update(ctx, obj, opts...)
					}
					pruned := app.DeepCopy()
					pruned.Spec.Health = nil
					if err := c.Update(ctx, pruned, opts...); err != nil {
						return err
					}
					app.ResourceVersion = pruned.ResourceVersion
					return nil
				},
			}).Build()
		rec := serveHealthCheck(t, c, http.MethodPut, `{"type":"tcp"}`)
		assert.Equal(t, http.StatusConflict, rec.Code)
		assert.Contains(t, rec.Body.String(), "kip upgrade")
	})
	t.Run("a body that is not JSON", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(testCRScheme()).WithObjects(healthCheckApp(nil)).Build()
		assert.Equal(t, http.StatusBadRequest, serveHealthCheck(t, c, http.MethodPut, `{`).Code)
	})
	t.Run("a missing app", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(testCRScheme()).Build()
		assert.Equal(t, http.StatusNotFound, serveHealthCheck(t, c, http.MethodPut, `{"type":"tcp"}`).Code)
	})
}
