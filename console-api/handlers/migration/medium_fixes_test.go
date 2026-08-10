package migration

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/runtime"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func gitAppSpecMap(t *testing.T, app *kipperv1.App) map[string]interface{} {
	t.Helper()
	raw, err := json.Marshal(app.Spec)
	if err != nil {
		t.Fatalf("marshalling spec: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshalling spec: %v", err)
	}
	return m
}

// A git app rebuilds on the target. If its build only fits because this
// cluster raised the default, that limit has to travel with it: the target has
// never heard of this cluster's deployment config and the build would OOM.
func TestMigrationCarriesClusterBuildDefault(t *testing.T) {
	t.Setenv("BUILD_MEMORY_LIMIT", "6Gi")
	t.Setenv("BUILD_CPU_LIMIT", "4")

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "website", Namespace: "hrportal-prod"},
		Spec:       kipperv1.AppSpec{Git: &kipperv1.AppGitSource{URL: "https://example.com/repo.git"}},
	}
	spec := gitAppSpecMap(t, app)
	materialiseBuildDefaults(app, spec)

	git, _ := spec["git"].(map[string]interface{})
	res, _ := git["buildResources"].(map[string]interface{})
	if res["memory"] != "6Gi" {
		t.Errorf("memory = %v, want the source cluster's 6Gi carried across", res["memory"])
	}
	if res["cpu"] != "4" {
		t.Errorf("cpu = %v, want the source cluster's 4 carried across", res["cpu"])
	}
}

// An app that sets its own limit keeps it: that already travels in the spec,
// and the cluster default must not override the operator's explicit choice.
func TestMigrationKeepsPerAppBuildLimit(t *testing.T) {
	t.Setenv("BUILD_MEMORY_LIMIT", "6Gi")

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "hrportal-prod"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			URL:            "https://example.com/repo.git",
			BuildResources: &kipperv1.BuildResources{Memory: "2Gi"},
		}},
	}
	spec := gitAppSpecMap(t, app)
	materialiseBuildDefaults(app, spec)

	git, _ := spec["git"].(map[string]interface{})
	res, _ := git["buildResources"].(map[string]interface{})
	if res["memory"] != "2Gi" {
		t.Errorf("memory = %v, want the app's own 2Gi preserved", res["memory"])
	}
}

// With no cluster default there is nothing to carry, and the migrated spec
// must not gain a limit that pins the app against future defaults.
func TestMigrationAddsNothingWithoutClusterDefault(t *testing.T) {
	t.Setenv("BUILD_MEMORY_LIMIT", "")
	t.Setenv("BUILD_CPU_LIMIT", "")

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "website", Namespace: "hrportal-prod"},
		Spec:       kipperv1.AppSpec{Git: &kipperv1.AppGitSource{URL: "https://example.com/repo.git"}},
	}
	spec := gitAppSpecMap(t, app)
	materialiseBuildDefaults(app, spec)

	git, _ := spec["git"].(map[string]interface{})
	if _, present := git["buildResources"]; present {
		t.Error("a migrated app gained build limits it never had")
	}
}

// A non-git app has no build to carry limits for.
func TestMigrationSkipsNonGitApp(t *testing.T) {
	t.Setenv("BUILD_MEMORY_LIMIT", "6Gi")
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "docuseal", Namespace: "docuseal-prod"},
		Spec:       kipperv1.AppSpec{Image: "docuseal:latest"},
	}
	spec := gitAppSpecMap(t, app)
	materialiseBuildDefaults(app, spec) // must not panic on a nil Git
	if _, present := spec["git"]; present {
		t.Error("a non-git app gained a git block")
	}
}

// The transfer retries by relaunching the exporter. If the importer died, the
// receiver has to be rebuilt or every remaining attempt uploads to nothing.
func TestEnsureImportPodReplacesFinishedReceiver(t *testing.T) {
	ctx := context.Background()
	dead := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "dt-tr1-import", Namespace: "shop-prod"},
		Status:     corev1.PodStatus{Phase: corev1.PodFailed},
	}
	client := fake.NewSimpleClientset(dead)
	h := &Handler{Client: client}

	fresh := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dt-tr1-import", Namespace: "shop-prod"}}
	if err := h.ensureImportPod(ctx, fresh); err != nil {
		t.Fatalf("ensuring receiver: %v", err)
	}
	got, err := client.CoreV1().Pods("shop-prod").Get(ctx, "dt-tr1-import", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading receiver: %v", err)
	}
	if got.Status.Phase == corev1.PodFailed {
		t.Error("the failed receiver was left in place, so the next attempt would upload to a dead backend")
	}
}

// A receiver that is still running may be mid-write to the destination, so it
// must never be replaced underneath itself.
func TestEnsureImportPodLeavesRunningReceiverAlone(t *testing.T) {
	ctx := context.Background()
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "dt-tr1-import", Namespace: "shop-prod", UID: "original"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewSimpleClientset(running)
	h := &Handler{Client: client}

	replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "dt-tr1-import", Namespace: "shop-prod", UID: "replacement"}}
	if err := h.ensureImportPod(ctx, replacement); err != nil {
		t.Fatalf("ensuring receiver: %v", err)
	}
	got, _ := client.CoreV1().Pods("shop-prod").Get(ctx, "dt-tr1-import", metav1.GetOptions{})
	if got.UID != "original" {
		t.Error("a running receiver was replaced — it may have been mid-write to the destination")
	}
}

// The builder skips a malformed or non-positive per-app limit and falls through
// to the cluster default, so the app is really building on that default. If
// migration treats the key as "already set", the target drops to the built-in
// default and the rebuild OOMs — the finding this fix exists for.
func TestMigrationReplacesUnusablePerAppBuildLimit(t *testing.T) {
	t.Setenv("BUILD_MEMORY_LIMIT", "6Gi")
	t.Setenv("BUILD_CPU_LIMIT", "4")

	for _, bad := range []string{"0", "-1Gi", "not-a-quantity"} {
		t.Run(bad, func(t *testing.T) {
			app := &kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "website", Namespace: "hrportal-prod"},
				Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
					URL:            "https://example.com/repo.git",
					BuildResources: &kipperv1.BuildResources{Memory: bad},
				}},
			}
			spec := gitAppSpecMap(t, app)
			materialiseBuildDefaults(app, spec)

			git, _ := spec["git"].(map[string]interface{})
			res, _ := git["buildResources"].(map[string]interface{})
			if res["memory"] != "6Gi" {
				t.Errorf("memory = %v, want the cluster default the source build was actually using", res["memory"])
			}
		})
	}
}

// Repairing a receiver must never disturb a live one, and must report honestly
// when there is nothing to repair.
func TestEnsureImportPodIsIdempotentOnAHealthyReceiver(t *testing.T) {
	ctx := context.Background()
	running := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "dt-tr1-import", Namespace: "shop-prod", UID: "live"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewSimpleClientset(running)
	h := &Handler{Client: client}

	for i := 0; i < 3; i++ {
		if err := h.ensureImportPod(ctx, running.DeepCopy()); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
	}
	got, _ := client.CoreV1().Pods("shop-prod").Get(ctx, "dt-tr1-import", metav1.GetOptions{})
	if got.UID != "live" {
		t.Error("a live receiver was disturbed by a repeated repair")
	}
}

// A receiver evicted because its node died must come back schedulable. Cloning
// the failed pod would carry the scheduler's node assignment with it and pin
// the replacement to the node that just failed.
func TestReceiverRepairDoesNotPinToTheFailedNode(t *testing.T) {
	dead := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dt-tr1-import", Namespace: "shop-prod",
			UID: "old", ResourceVersion: "42",
		},
		Spec: corev1.PodSpec{
			NodeName:     "node-that-died",
			NodeSelector: map[string]string{"disktype": "ssd"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodFailed},
	}

	fresh := schedulableCopy(dead)
	if fresh.Spec.NodeName != "" {
		t.Errorf("NodeName = %q, want the replacement left for the scheduler to place", fresh.Spec.NodeName)
	}
	if fresh.Spec.NodeSelector["disktype"] != "ssd" {
		t.Error("the receiver's own placement requirements were dropped")
	}
	if fresh.UID != "" || fresh.ResourceVersion != "" || fresh.Status.Phase != "" {
		t.Error("stale identity or status carried into the replacement")
	}
}

// Exercised through the HTTP handler: repair refuses when there is nothing to
// repair, leaves a live receiver and its Service alone, and refuses a namespace
// outside the migration's accepted scope.
func TestEnsureReceiverHandler(t *testing.T) {
	newHandler := func(objs ...runtime.Object) (*Handler, *chi.Mux) {
		base := []runtime.Object{
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: "shop-prod", Labels: map[string]string{"kipper.run/project": "shop"},
			}},
			&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
				Name: "payments-prod", Labels: map[string]string{"kipper.run/project": "payments"},
			}},
		}
		client := fake.NewSimpleClientset(append(base, objs...)...)
		h := &Handler{Client: client, Sessions: NewSessionStore()}
		h.Sessions.Put(&Session{ID: "s1", Projects: []string{"shop"}, Secret: "test-session-secret"})
		r := chi.NewRouter()
		r.Post("/{session}/transfer/{transfer}/ensure", h.EnsureReceiverHandler)
		return h, r
	}
	post := func(router *chi.Mux, transfer string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/s1/transfer/"+transfer+"/ensure", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	receiverFor := func(ns, id string, phase corev1.PodPhase) []runtime.Object {
		// The namespace is resolved from the import Service, which carries both
		// the transfer and the session that stood it up.
		labels := map[string]string{transferIDLabel: id, "kipper.run/migration-session": "s1"}
		return []runtime.Object{
			&corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name: importServiceName(id), Namespace: ns, Labels: labels,
			}},
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{
				Name: "dt-" + id + "-token", Namespace: ns, Labels: labels,
			}},
			&corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: importPodName(id), Namespace: ns, UID: "live", Labels: labels},
				Status:     corev1.PodStatus{Phase: phase},
			},
		}
	}

	t.Run("no receiver to repair", func(t *testing.T) {
		_, router := newHandler()
		if rec := post(router, "missing"); rec.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 for a transfer with no receiver", rec.Code)
		}
	})

	t.Run("live receiver is left alone", func(t *testing.T) {
		h, router := newHandler(receiverFor("shop-prod", "tr1", corev1.PodRunning)...)
		rec := post(router, "tr1")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"replaced":false`) {
			t.Errorf("body = %s, want replaced:false for a running receiver", rec.Body.String())
		}
		got, err := h.Client.CoreV1().Pods("shop-prod").Get(context.Background(), importPodName("tr1"), metav1.GetOptions{})
		if err != nil || got.UID != "live" {
			t.Error("a running receiver was disturbed by repair")
		}
	})

	t.Run("finished receiver is replaced", func(t *testing.T) {
		_, router := newHandler(receiverFor("shop-prod", "tr1", corev1.PodFailed)...)
		rec := post(router, "tr1")
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"replaced":true`) {
			t.Errorf("status = %d body = %s, want a replaced receiver", rec.Code, rec.Body.String())
		}
	})

	t.Run("namespace outside the migration scope is refused", func(t *testing.T) {
		_, router := newHandler(receiverFor("payments-prod", "tr2", corev1.PodFailed)...)
		if rec := post(router, "tr2"); rec.Code != http.StatusForbidden {
			t.Errorf("status = %d, want 403 for a namespace this migration never accepted", rec.Code)
		}
	})
}

// The per-dimension contract holds for CPU as well as memory.
func TestMigrationReplacesUnusablePerAppCPU(t *testing.T) {
	t.Setenv("BUILD_CPU_LIMIT", "4")
	t.Setenv("BUILD_MEMORY_LIMIT", "")

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "website", Namespace: "hrportal-prod"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			URL:            "https://example.com/repo.git",
			BuildResources: &kipperv1.BuildResources{CPU: "0", Memory: "2Gi"},
		}},
	}
	spec := gitAppSpecMap(t, app)
	materialiseBuildDefaults(app, spec)

	git, _ := spec["git"].(map[string]interface{})
	res, _ := git["buildResources"].(map[string]interface{})
	if res["cpu"] != "4" {
		t.Errorf("cpu = %v, want the cluster default replacing the unusable 0", res["cpu"])
	}
	if res["memory"] != "2Gi" {
		t.Errorf("memory = %v, want the app's own valid value preserved", res["memory"])
	}
}
