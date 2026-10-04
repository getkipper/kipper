package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
)

func stoppableApp(stopped *kipperv1.AppStopped) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop"},
		Spec:       kipperv1.AppSpec{Image: "web:1", Port: 8080, Replicas: int32Ptr(2), Stopped: stopped},
	}
}

func postAppAction(t *testing.T, handler *Apps, action, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Post("/api/v1/projects/{name}/apps/{app}/stop", handler.Stop)
	r.Post("/api/v1/projects/{name}/apps/{app}/start", handler.Start)
	r.Post("/api/v1/projects/{name}/apps/{app}/restart", handler.Restart)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/shop/apps/web/"+action, strings.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: "alice@example.com"}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func storedApp(t *testing.T, c crclient.Client) *kipperv1.App {
	t.Helper()
	var app kipperv1.App
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop", Name: "web"}, &app))
	return &app
}

func TestStop_RecordsWhoWhenAndWhy(t *testing.T) {
	c := testCRClient(stoppableApp(nil))
	h := &Apps{Client: fake.NewClientset(), CRClient: c}

	rec := postAppAction(t, h, "stop", `{"reason":"freeing memory"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := storedApp(t, c)
	require.NotNil(t, got.Spec.Stopped)
	assert.Equal(t, "freeing memory", got.Spec.Stopped.Reason)
	assert.Equal(t, "alice@example.com", got.Spec.Stopped.By)
	assert.NotNil(t, got.Spec.Stopped.At)
	assert.Equal(t, int32(2), *got.Spec.Replicas, "a stop leaves the replica count alone")
}

func TestStop_WithoutABodyStopsWithoutAReason(t *testing.T) {
	c := testCRClient(stoppableApp(nil))
	h := &Apps{Client: fake.NewClientset(), CRClient: c}

	rec := postAppAction(t, h, "stop", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NotNil(t, storedApp(t, c).Spec.Stopped)
}

func TestStop_AgainChangesOnlyTheReason(t *testing.T) {
	at := metav1.Now()
	c := testCRClient(stoppableApp(&kipperv1.AppStopped{Reason: "old", By: "bob@example.com", At: &at}))
	h := &Apps{Client: fake.NewClientset(), CRClient: c}

	rec := postAppAction(t, h, "stop", `{"reason":"new"}`)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := storedApp(t, c).Spec.Stopped
	assert.Equal(t, "new", got.Reason)
	assert.Equal(t, "bob@example.com", got.By, "who stopped it stays the person who did")
	assert.Equal(t, at.Unix(), got.At.Unix(), "and so does when")
}

func TestStop_RefusesAReasonOver500Characters(t *testing.T) {
	c := testCRClient(stoppableApp(nil))
	h := &Apps{Client: fake.NewClientset(), CRClient: c}

	rec := postAppAction(t, h, "stop", `{"reason":"`+strings.Repeat("r", 501)+`"}`)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Nil(t, storedApp(t, c).Spec.Stopped)
}

func TestStart_RemovesTheStop(t *testing.T) {
	c := testCRClient(stoppableApp(&kipperv1.AppStopped{Reason: "x"}))
	h := &Apps{Client: fake.NewClientset(), CRClient: c}

	rec := postAppAction(t, h, "start", "")

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Nil(t, storedApp(t, c).Spec.Stopped)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "starting", body["status"])
}

func TestStart_OnARunningAppIsANoOp(t *testing.T) {
	c := testCRClient(stoppableApp(nil))
	h := &Apps{Client: fake.NewClientset(), CRClient: c}

	rec := postAppAction(t, h, "start", "")

	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, "running", body["status"])
}

// A non-autoscaled app stopped at zero replicas runs no pods after the start,
// and the response says so rather than letting the operator wait for them.
func TestStart_SaysWhenTheAppWillRunNoPods(t *testing.T) {
	app := stoppableApp(&kipperv1.AppStopped{})
	app.Spec.Replicas = int32Ptr(0)
	c := testCRClient(app)
	h := &Apps{Client: fake.NewClientset(), CRClient: c}

	rec := postAppAction(t, h, "start", "")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), "no pods")
}

func TestRestart_RefusesAStoppedApp(t *testing.T) {
	c := testCRClient(stoppableApp(&kipperv1.AppStopped{}))
	h := &Apps{Client: fake.NewClientset(), CRClient: c}

	rec := postAppAction(t, h, "restart", "")

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "is stopped")
	assert.Empty(t, storedApp(t, c).Annotations["kipper.run/restartedAt"])
}

func TestAppResponseCarriesTheStop(t *testing.T) {
	at := metav1.Now()
	app := stoppableApp(&kipperv1.AppStopped{Reason: "freeing memory", By: "alice@example.com", At: &at})
	app.Status.Phase = "Stopped"

	resp := appCRToResponse(*app)

	assert.Equal(t, "stopped", resp.Status)
	require.NotNil(t, resp.Stopped)
	assert.Equal(t, "freeing memory", resp.Stopped.Reason)
	assert.Equal(t, "alice@example.com", resp.Stopped.By)
	assert.Equal(t, at.UTC().Format(time.RFC3339), resp.Stopped.At)

	assert.Nil(t, appCRToResponse(*stoppableApp(nil)).Stopped)
}

func TestRouteHealthOfAStoppedApp(t *testing.T) {
	h := stoppedRouteHealth(RouteHealth{IngressReady: true, TLSReady: true, Message: "Live."})

	assert.True(t, h.Stopped)
	assert.True(t, h.IngressReady, "the route itself is still in place")
	assert.Contains(t, h.Message, "stopped")
}

func TestScale_OnAStoppedAppAppliesWhenStarted(t *testing.T) {
	c := testCRClient(stoppableApp(&kipperv1.AppStopped{}))
	h := &Apps{Client: fake.NewClientset(), CRClient: c}
	r := chi.NewRouter()
	r.Put("/api/v1/projects/{name}/apps/{app}/scale", h.Scale)
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/shop/apps/web/scale", strings.NewReader(`{"replicas":4}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := storedApp(t, c)
	assert.Equal(t, int32(4), *got.Spec.Replicas)
	assert.NotNil(t, got.Spec.Stopped, "a scale must not start a stopped app")
	assert.Contains(t, rec.Body.String(), "applies when the app is started")
}

func TestAutoscale_OnAStoppedAppAppliesWhenStarted(t *testing.T) {
	for _, tc := range []struct{ method, body string }{
		{http.MethodPut, `{"min_replicas":2,"max_replicas":5,"cpu_target":80}`},
		{http.MethodDelete, ""},
	} {
		t.Run(tc.method, func(t *testing.T) {
			app := stoppableApp(&kipperv1.AppStopped{})
			// A disable only writes, and so only notes, when a policy is on.
			app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](1), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}
			c := testCRClient(app)
			h := &Autoscale{Client: fake.NewClientset(), CRClient: c}
			r := chi.NewRouter()
			r.Put("/api/v1/projects/{name}/apps/{app}/autoscale", h.Set)
			r.Delete("/api/v1/projects/{name}/apps/{app}/autoscale", h.Delete)
			req := httptest.NewRequest(tc.method, "/api/v1/projects/shop/apps/web/autoscale", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.NotNil(t, storedApp(t, c).Spec.Stopped)
			assert.Contains(t, rec.Body.String(), "applies when the app is started")
		})
	}
}

func TestImageChangesOnAStoppedAppApplyWhenStarted(t *testing.T) {
	t.Run("image update", func(t *testing.T) {
		c := testCRClient(stoppableApp(&kipperv1.AppStopped{}))
		h := &Apps{Client: fake.NewClientset(), CRClient: c}
		r := chi.NewRouter()
		r.Put("/api/v1/projects/{name}/apps/{app}/image", h.UpdateImage)
		req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/shop/apps/web/image", strings.NewReader(`{"image":"web:2"}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "web:2", storedApp(t, c).Spec.Image)
		assert.Contains(t, rec.Body.String(), "applies when the app is started")
	})

	t.Run("rollback", func(t *testing.T) {
		app := stoppableApp(&kipperv1.AppStopped{})
		app.Annotations = map[string]string{historyAnnotation: `[{"revision":2,"image":"web:2"},{"revision":1,"image":"web:1"}]`}
		c := testCRClient(app)
		h := &Webhooks{Client: fake.NewClientset(), CRClient: c}
		r := chi.NewRouter()
		r.Post("/api/v1/projects/{name}/apps/{app}/rollback", h.Rollback)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/shop/apps/web/rollback", strings.NewReader(`{}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)

		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotNil(t, storedApp(t, c).Spec.Stopped)
		assert.Contains(t, rec.Body.String(), "applies when the app is started")
	})
}

// An App schema from before stopping drops the field on write, and the app
// keeps running. The console must not report it stopped.
func TestStop_ACLusterThatDropsTheStopIsReported(t *testing.T) {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(stoppableApp(nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if app, ok := obj.(*kipperv1.App); ok {
					app.Spec.Stopped = nil
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	h := &Apps{Client: fake.NewClientset(), CRClient: c}

	rec := postAppAction(t, h, "stop", `{"reason":"x"}`)

	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Contains(t, rec.Body.String(), "Upgrade Kipper")
}
