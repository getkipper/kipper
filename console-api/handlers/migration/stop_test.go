package migration

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func TestOutgoingStop(t *testing.T) {
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	at := metav1.NewTime(now.Add(-time.Hour))
	operator := &kipperv1.AppStopped{Reason: "parked", By: "alice@example.com", At: &at}
	freeze := &kipperv1.AppStopped{Reason: "migration freeze", By: "kip (alice)", At: &at, ForMigration: true}
	bound := []kipperv1.ServiceBinding{{Name: "db"}}
	leftBehind := func(name string) bool { return name == "db" }
	nothingLeft := func(string) bool { return false }

	for _, tc := range []struct {
		name     string
		stop     *kipperv1.AppStopped
		bindings []kipperv1.ServiceBinding
		left     func(string) bool
		want     *kipperv1.AppStopped
	}{
		{"a running app arrives running", nil, bound, nothingLeft, nil},
		{"an operator's stop is carried as it is", operator, nil, nothingLeft, operator},
		{"the write freeze stays behind", freeze, bound, nothingLeft, nil},
		{"an app whose database stayed behind arrives stopped", nil, bound, leftBehind,
			&kipperv1.AppStopped{Reason: waitingForRestore, By: "migration", At: &metav1.Time{Time: now}}},
		{"so does a frozen one", freeze, bound, leftBehind,
			&kipperv1.AppStopped{Reason: waitingForRestore, By: "migration", At: &metav1.Time{Time: now}}},
		{"an operator's stop keeps its record when its database stayed behind", operator, bound, leftBehind, operator},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := &kipperv1.App{Spec: kipperv1.AppSpec{Stopped: tc.stop, ServiceBindings: tc.bindings}}
			got := outgoingStop(app, tc.left, now)
			switch {
			case tc.want == nil && got != nil:
				t.Fatalf("got %+v, want none", got)
			case tc.want != nil && (got == nil || got.Reason != tc.want.Reason || got.By != tc.want.By || got.ForMigration || !got.At.Equal(tc.want.At)):
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

type capturedSends struct {
	mu   sync.Mutex
	apps map[string]map[string]interface{}
}

func captureTarget(t *testing.T) (*httptest.Server, *capturedSends) {
	t.Helper()
	got := &capturedSends{apps: map[string]map[string]interface{}{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if strings.HasSuffix(r.URL.Path, "/resource") && body["kind"] == "App" {
			got.mu.Lock()
			spec, _ := body["spec"].(map[string]interface{})
			got.apps[body["name"].(string)] = spec
			got.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func stopTestHandler(apps ...crclient.Object) *Handler {
	ns := projectNamespace("shop-prod", "shop")
	ns.Labels["kipper.run/environment"] = "prod"
	return &Handler{
		Client:   fake.NewSimpleClientset(ns),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(append(apps, ownerOf("shop-prod"))...).Build(),
	}
}

func shopApp(name string, stop *kipperv1.AppStopped, bindings ...kipperv1.ServiceBinding) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Image: "registry.example.com/shop/" + name + ":v1", Port: 8080, Stopped: stop, ServiceBindings: bindings},
	}
}

func TestMigrateApps_SendsTheStopEachAppCarries(t *testing.T) {
	srv, got := captureTarget(t)
	h := stopTestHandler(
		shopApp("web", nil),
		shopApp("api", &kipperv1.AppStopped{Reason: "parked"}),
		shopApp("frozen", &kipperv1.AppStopped{ForMigration: true}),
		shopApp("worker", nil, kipperv1.ServiceBinding{Name: "db"}),
	)
	session := &Session{SavedRoutes: map[string]map[string]interface{}{}, TargetKeepsStops: true}
	session.MarkDataLeftBehind("shop-prod", "db")

	if err := h.migrateApps(context.Background(), session, &Token{Endpoint: srv.URL}, "shop-prod", nil); err != nil {
		t.Fatal(err)
	}

	if _, stopped := got.apps["web"]["stopped"]; stopped {
		t.Error("a running app arrived stopped")
	}
	if st, _ := got.apps["api"]["stopped"].(map[string]interface{}); st["reason"] != "parked" {
		t.Errorf("the operator's stop did not travel: %v", got.apps["api"]["stopped"])
	}
	if _, stopped := got.apps["frozen"]["stopped"]; stopped {
		t.Error("the write freeze travelled to the target")
	}
	if st, _ := got.apps["worker"]["stopped"].(map[string]interface{}); st["reason"] != waitingForRestore {
		t.Errorf("an app whose database stayed behind arrived running: %v", got.apps["worker"]["stopped"])
	}
	var listed *Step
	for i := range session.Steps {
		if strings.HasPrefix(session.Steps[i].Name, "Apps that arrive stopped") {
			listed = &session.Steps[i]
		}
	}
	if listed == nil || !strings.Contains(strings.Join(listed.ManualSteps, "\n"), "kip app start worker --project shop --environment prod") {
		t.Errorf("the completion screen does not say how to start them: %+v", listed)
	}
}

// An older target decodes the App into a spec without the field and starts
// the app. The source refuses before sending such an app.
func TestMigrateApps_RefusesToSendAStopTheTargetWouldDrop(t *testing.T) {
	srv, got := captureTarget(t)
	h := stopTestHandler(shopApp("api", &kipperv1.AppStopped{Reason: "parked"}))
	session := &Session{SavedRoutes: map[string]map[string]interface{}{}}

	err := h.migrateApps(context.Background(), session, &Token{Endpoint: srv.URL}, "shop-prod", nil)

	if err == nil || !strings.Contains(err.Error(), "Upgrade the target") {
		t.Fatalf("got %v, want a refusal naming the upgrade", err)
	}
	if _, sent := got.apps["api"]; sent {
		t.Error("the app was sent anyway")
	}
}

func TestCreateApp_KeepsTheStop(t *testing.T) {
	h := stopTestHandler()
	spec := map[string]interface{}{"image": "registry.example.com/shop/web:v1", "port": 8080, "stopped": map[string]interface{}{"reason": "parked"}}
	if err := h.createApp(context.Background(), "web", "shop-prod", spec, nil); err != nil {
		t.Fatal(err)
	}
	var app kipperv1.App
	if err := h.CRClient.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-prod", Name: "web"}, &app); err != nil {
		t.Fatal(err)
	}
	if app.Spec.Stopped == nil || app.Spec.Stopped.Reason != "parked" {
		t.Fatalf("the stop was dropped: %+v", app.Spec.Stopped)
	}
}

func planWithTarget(t *testing.T, keepsStops *bool, apps ...crclient.Object) *planResponse {
	t.Helper()
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/capacity"):
			body := map[string]interface{}{
				"allocatable_cpu_millis": 8000, "allocatable_memory_bytes": 16 * 1024 * 1024 * 1024,
				"allocatable_storage_bytes": 100 * 1024 * 1024 * 1024,
				"requested_cpu_millis":      0, "requested_memory_bytes": 0, "requested_storage_bytes": 0,
				"target_version": "dev",
			}
			if keepsStops != nil {
				body["keeps_stops"] = *keepsStops
			}
			_ = json.NewEncoder(w).Encode(body)
		case strings.HasSuffix(r.URL.Path, "/projects"):
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(target.Close)
	objs := append([]crclient.Object{ownerOf("shop-prod")}, apps...)
	h := &Handler{
		Client:   fakeClientWithProject(t),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(objs...).Build(),
		Sessions: NewSessionStore(),
		Domain:   "source.example.com",
	}
	tok, _ := testToken(t, target.URL)
	return h.buildPlan(context.Background(), planClaims("admin@example.com"), tok, []string{"shop"}, nil, nil, false)
}

func blockerMentioning(resp *planResponse, text string) bool {
	for _, b := range resp.Blockers {
		if strings.Contains(b, text) {
			return true
		}
	}
	return false
}

func TestPlan_AnOlderTargetCannotTakeAStoppedApp(t *testing.T) {
	keeps, drops := true, false
	stopped := shopApp("api", &kipperv1.AppStopped{Reason: "parked"})

	if resp := planWithTarget(t, nil, stopped); !blockerMentioning(resp, "api") || !blockerMentioning(resp, "Upgrade the target") {
		t.Errorf("a target that does not say it keeps stops was accepted: %v", resp.Blockers)
	}
	if resp := planWithTarget(t, &drops, stopped); !blockerMentioning(resp, "Upgrade the target") {
		t.Errorf("a target that drops stops was accepted: %v", resp.Blockers)
	}
	if resp := planWithTarget(t, &keeps, stopped); blockerMentioning(resp, "Upgrade the target") {
		t.Errorf("a target that keeps stops was refused: %v", resp.Blockers)
	}
	frozen := shopApp("api", &kipperv1.AppStopped{ForMigration: true})
	if resp := planWithTarget(t, nil, frozen); blockerMentioning(resp, "Upgrade the target") {
		t.Errorf("the write freeze stays behind and needs nothing from the target: %v", resp.Blockers)
	}
}

func TestFetchTargetCapacity_ReadsWhetherTheTargetKeepsStops(t *testing.T) {
	for _, tc := range []struct {
		body string
		want bool
	}{
		{`"keeps_stops":true`, true},
		{`"keeps_stops":false`, false},
		{`"target_version":"v0.22.0"`, false},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"allocatable_cpu_millis":1,"allocatable_memory_bytes":1,"allocatable_storage_bytes":1,` +
				`"requested_cpu_millis":0,"requested_memory_bytes":0,"requested_storage_bytes":0,` + tc.body + `}`))
		}))
		got, _, err := (&Handler{}).fetchTargetCapacity(&Token{Endpoint: srv.URL}, nil)
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got.KeepsStops != tc.want {
			t.Errorf("%s: KeepsStops = %v", tc.body, got.KeepsStops)
		}
	}
}

func pruningStops() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok {
				unstructured.RemoveNestedField(u.Object, "spec", "stopped")
			}
			if app, ok := obj.(*kipperv1.App); ok {
				app.Spec.Stopped = nil
			}
			return c.Create(ctx, obj, opts...)
		},
	}
}

// Probe schema support even when the running image supports stops.
func TestKeepsStops(t *testing.T) {
	builder := func() *crfake.ClientBuilder { return crfake.NewClientBuilder().WithScheme(migrationScheme()) }

	if !(&Handler{CRClient: builder().Build()}).keepsStops(context.Background()) {
		t.Error("a schema that keeps the field was reported as dropping it")
	}
	if (&Handler{CRClient: builder().WithInterceptorFuncs(pruningStops()).Build()}).keepsStops(context.Background()) {
		t.Error("a schema that drops the field was reported as keeping it")
	}
	failing := builder().WithInterceptorFuncs(interceptor.Funcs{
		Create: func(context.Context, crclient.WithWatch, crclient.Object, ...crclient.CreateOption) error {
			return errors.New("forbidden")
		},
	}).Build()
	if (&Handler{CRClient: failing}).keepsStops(context.Background()) {
		t.Error("a check that could not run was reported as keeping stops")
	}
}

func TestCreateApp_RefusesToReportAStopTheSchemaDropped(t *testing.T) {
	h := &Handler{CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithInterceptorFuncs(pruningStops()).Build()}
	spec := map[string]interface{}{"image": "registry.example.com/shop/web:v1", "port": 8080, "stopped": map[string]interface{}{"reason": "parked"}}

	err := h.createApp(context.Background(), "web", "shop-prod", spec, nil)

	if err == nil || !strings.Contains(err.Error(), "stopping") {
		t.Fatalf("got %v, want a refusal naming the missing support for stopping", err)
	}
}
