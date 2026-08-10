package migration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func migrationToken(t *testing.T) *corev1.Secret {
	t.Helper()
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenSecretName, Namespace: tokenSecretNamespace},
		Data: map[string][]byte{
			"secret":  []byte("valid-secret"),
			"expires": []byte(time.Now().Add(time.Hour).Format(time.RFC3339)),
		},
	}
}

func postAccept(t *testing.T, h *Handler, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/accept", bytes.NewReader(payload))
	rec := httptest.NewRecorder()
	h.AcceptHandler(rec, req)
	return rec
}

// A conflicting accept must report the conflicts without consuming the
// single-use token: the source retries the same token with the overwrites
// confirmed, and that retry has to authenticate.
func TestAcceptHandler_ConflictsKeepTokenUsable(t *testing.T) {
	existing := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}}
	h := &Handler{
		Sessions: NewSessionStore(),
		Client:   fake.NewSimpleClientset(migrationToken(t)),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithStatusSubresource(&kipperv1.App{}).WithObjects(existing).Build(),
	}

	rec := postAccept(t, h, map[string]interface{}{
		"secret":   "valid-secret",
		"projects": []string{"shop", "blog"},
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "shop") {
		t.Fatalf("conflict response should name the project, got: %s", rec.Body.String())
	}

	// The token must still exist: the confirmed retry authenticates with it.
	if _, err := h.Client.CoreV1().Secrets(tokenSecretNamespace).Get(context.Background(), tokenSecretName, metav1.GetOptions{}); err != nil {
		t.Fatalf("token must survive a conflict response: %v", err)
	}

	rec = postAccept(t, h, map[string]interface{}{
		"secret":               "valid-secret",
		"projects":             []string{"shop", "blog"},
		"confirmed_overwrites": []string{"shop"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirmed accept status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	// Now the token is consumed: the accept succeeded.
	if _, err := h.Client.CoreV1().Secrets(tokenSecretNamespace).Get(context.Background(), tokenSecretName, metav1.GetOptions{}); err == nil {
		t.Fatal("token must be consumed once the accept succeeds")
	}
}

// A retried migration replays resources that already exist on the target.
// Every creator must converge on the incoming spec instead of failing with
// AlreadyExists, or "restart the migration" is a dead end.
func TestTargetCreators_ReplayConverges(t *testing.T) {
	h := &Handler{
		Client:   fake.NewSimpleClientset(),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithStatusSubresource(&kipperv1.App{}).Build(),
		Sessions: NewSessionStore(),
	}
	// The replayed service handover overwrites the credentials its first pass
	// wrote, and an overwrite is journalled, which fails closed without a
	// session to record the copy against.
	h.Sessions.Put(&Session{
		ID:        "sess-replay",
		Projects:  []string{"shop"},
		Status:    SessionRunning,
		Secret:    "s",
		ExpiresAt: time.Now().Add(time.Hour),
	})
	ctx := context.Background()

	if err := h.createProject(ctx, "shop", map[string]interface{}{"displayName": "Shop"}); err != nil {
		t.Fatalf("first createProject: %v", err)
	}
	if err := h.createProject(ctx, "shop", map[string]interface{}{"displayName": "Shop v2"}); err != nil {
		t.Fatalf("replayed createProject must not fail: %v", err)
	}
	var project kipperv1.Project
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Name: "shop"}, &project); err != nil {
		t.Fatalf("getting project: %v", err)
	}
	if project.Spec.DisplayName != "Shop v2" {
		t.Fatalf("displayName = %q, want the replayed spec to win", project.Spec.DisplayName)
	}

	appSpec := map[string]interface{}{
		"image": "registry.example.com/shop/web:v1",
		"port":  8080,
	}
	if err := h.createApp(ctx, "web", "shop-prod", appSpec); err != nil {
		t.Fatalf("first createApp: %v", err)
	}
	appSpec["image"] = "registry.example.com/shop/web:v2"
	if err := h.createApp(ctx, "web", "shop-prod", appSpec); err != nil {
		t.Fatalf("replayed createApp must not fail: %v", err)
	}
	var app kipperv1.App
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: "shop-prod", Name: "web"}, &app); err != nil {
		t.Fatalf("getting app: %v", err)
	}
	if app.Spec.Image != "registry.example.com/shop/web:v2" {
		t.Fatalf("image = %q, want the replayed spec to win", app.Spec.Image)
	}

	svcSpec := map[string]interface{}{"type": "postgres"}
	creds := &transferredCredentials{Data: map[string]string{
		"PASSWORD": base64.StdEncoding.EncodeToString([]byte("from-the-source")),
	}}
	if err := h.createService(ctx, "sess-replay", "db", "shop-prod", svcSpec, creds); err != nil {
		t.Fatalf("first createService: %v", err)
	}
	if err := h.createService(ctx, "sess-replay", "db", "shop-prod", svcSpec, creds); err != nil {
		t.Fatalf("replayed createService must not fail: %v", err)
	}
	// The replay must leave the credentials owned by the one Service, not
	// stacked with a second reference or detached by the overwrite.
	replayed, err := h.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("getting credentials: %v", err)
	}
	if owner := metav1.GetControllerOf(replayed); owner == nil || owner.Name != "db" {
		t.Fatalf("credentials must stay owned by their service after a replay, got %+v", replayed.OwnerReferences)
	}

	volSpec := map[string]interface{}{"size": "1Gi"}
	if err := h.createVolume(ctx, "uploads", "shop-prod", volSpec); err != nil {
		t.Fatalf("first createVolume: %v", err)
	}
	if err := h.createVolume(ctx, "uploads", "shop-prod", volSpec); err != nil {
		t.Fatalf("replayed createVolume must not fail: %v", err)
	}
}

// The capacity precheck must refuse a target whose disk cannot take the
// selected projects' volumes, and must not refuse on storage when the target
// cannot report its disk capacity.
func TestCheckTargetCapacity_Storage(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		Labels: map[string]string{"kipper.run/project": "shop"},
	}}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "shop-prod"},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
	}
	h := &Handler{
		Client:   fake.NewSimpleClientset(ns, pvc),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).Build(),
	}

	serve := func(capacity clusterCapacity) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(capacity)
		}))
	}

	tooSmall := serve(clusterCapacity{
		AllocatableCPU:     16000,
		AllocatableMemory:  32 << 30,
		AllocatableStorage: 5 << 30,
	})
	defer tooSmall.Close()

	err := h.checkTargetCapacity(context.Background(), &Token{Endpoint: tooSmall.URL}, []string{"shop"})
	if err == nil || !strings.Contains(err.Error(), "disk") {
		t.Fatalf("expected a disk refusal, got: %v", err)
	}

	unknownDisk := serve(clusterCapacity{
		AllocatableCPU:    16000,
		AllocatableMemory: 32 << 30,
	})
	defer unknownDisk.Close()

	if err := h.checkTargetCapacity(context.Background(), &Token{Endpoint: unknownDisk.URL}, []string{"shop"}); err != nil {
		t.Fatalf("unknown target disk capacity must not refuse the migration: %v", err)
	}
}

// A frozen app must still count toward the capacity demand: the guide tells
// operators to scale writes to zero before migrating, and live-pod
// measurement would vanish exactly then. A running app's live pods must not
// double-count on top of its spec.
func TestNamespacesResourceRequests_CountsFrozenApps(t *testing.T) {
	zero, three := int32(0), int32(3)
	frozen := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Replicas: &zero, Resources: kipperv1.AppResources{Profile: "standard"}},
	}
	running := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Replicas: &three, Resources: kipperv1.AppResources{Profile: "standard"}},
	}
	webPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web-1", Namespace: "shop-prod",
			Labels: map[string]string{"app": "web"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "web",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
			},
		}}},
	}
	h := &Handler{
		Client:   fake.NewSimpleClientset(webPod),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(frozen, running).Build(),
	}

	cpu, mem, _, err := h.namespacesResourceRequests(context.Background(), []string{"shop-prod"})
	if err != nil {
		t.Fatalf("namespacesResourceRequests: %v", err)
	}
	// The standard profile requests 100m/128Mi per replica: one replica for
	// the frozen app's floor plus three for the running app.
	if cpu != 400 {
		t.Fatalf("cpu = %dm, want 400m", cpu)
	}
	if want := int64(4) * 128 * 1024 * 1024; mem != want {
		t.Fatalf("mem = %d, want %d", mem, want)
	}
}

// A frozen app that shares its name with a Function keeps its spec-based
// demand: the ambiguity fallback must over-reserve, never under-count.
func TestNamespacesResourceRequests_AmbiguousNameKeepsSpecDemand(t *testing.T) {
	zero := int32(0)
	frozen := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Replicas: &zero, Resources: kipperv1.AppResources{Profile: "standard"}},
	}
	fn := &kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "shop-prod"}}
	fnPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "resize-fn-1", Namespace: "shop-prod",
			Labels: map[string]string{"app": "resize"},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "fn",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
			},
		}}},
	}
	h := &Handler{
		Client:   fake.NewSimpleClientset(fnPod),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(frozen, fn).Build(),
	}

	cpu, _, _, err := h.namespacesResourceRequests(context.Background(), []string{"shop-prod"})
	if err != nil {
		t.Fatalf("namespacesResourceRequests: %v", err)
	}
	// The frozen app's one-replica floor (100m) plus the function's live pod
	// (50m): both sides count for an ambiguous name.
	if cpu != 150 {
		t.Fatalf("cpu = %dm, want 150m", cpu)
	}
}

// A target's capacity report must leave out the projects a migration will
// overwrite: their workloads get replaced by the incoming ones, and counting
// both refuses overwrites and retries that actually fit.
func TestCapacity_ExcludesOverwrittenProjects(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name:   "shop-prod",
		Labels: map[string]string{"kipper.run/project": "shop"},
	}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: "shop-prod"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{
			Name: "web",
			Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
			},
		}}},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "shop-prod"},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")},
			},
		},
	}
	h := &Handler{Client: fake.NewSimpleClientset(ns, pod, pvc)}

	counted, err := h.capacity(context.Background(), nil)
	if err != nil {
		t.Fatalf("capacity: %v", err)
	}
	if counted.RequestedCPU != 500 || counted.RequestedStorage != 10<<30 {
		t.Fatalf("baseline capacity should count the project, got cpu=%d storage=%d", counted.RequestedCPU, counted.RequestedStorage)
	}

	excluded, err := h.capacity(context.Background(), []string{"shop"})
	if err != nil {
		t.Fatalf("capacity with exclusion: %v", err)
	}
	if excluded.RequestedCPU != 0 || excluded.RequestedStorage != 0 {
		t.Fatalf("overwritten project must not count, got cpu=%d storage=%d", excluded.RequestedCPU, excluded.RequestedStorage)
	}
}

// The streamed db-import endpoint takes its metadata from query parameters
// and must validate them and the scope before touching the body.
func TestReceiveDBImportHandler_ValidatesParams(t *testing.T) {
	h := &Handler{
		Sessions: NewSessionStore(),
		Client:   fake.NewSimpleClientset(projectNamespace("shop-prod", "shop")),
	}
	h.Sessions.Put(&Session{ID: "s1", Projects: []string{"shop"}})

	router := chi.NewRouter()
	router.Post("/{session}/db-import", h.ReceiveDBImportHandler)

	post := func(query string) int {
		req := httptest.NewRequest(http.MethodPost, "/s1/db-import"+query, strings.NewReader("dump-bytes"))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := post(""); code != http.StatusBadRequest {
		t.Fatalf("missing params status = %d, want 400", code)
	}
	if code := post("?service=db&namespace=payments-prod&type=postgres&database=app&tables=3"); code != http.StatusForbidden {
		t.Fatalf("out-of-scope namespace status = %d, want 403", code)
	}
	if code := post("?service=db&namespace=shop-prod&type=postgres&database=app"); code != http.StatusBadRequest {
		t.Fatalf("postgres import without a table count status = %d, want 400", code)
	}
}

// The transfer commands must move current data, not stale or clobbered
// snapshots: redis dumps with a blocking SAVE and reloads with NOSAVE so the
// transferred file is not overwritten before loading, and mysqldump carries
// routines, events, and triggers.
func TestBuildCommands_MoveCurrentData(t *testing.T) {
	dump, _ := buildDumpCommand("redis", "cache")
	dumpStr := strings.Join(dump, " ")
	if strings.Contains(dumpStr, "BGSAVE") || !strings.Contains(dumpStr, "SAVE") {
		t.Fatalf("redis dump must use blocking SAVE, got: %s", dumpStr)
	}

	imp, _ := buildImportCommand("redis", "cache")
	impStr := strings.Join(imp, " ")
	if !strings.Contains(impStr, "DEBUG RELOAD NOSAVE") {
		t.Fatalf("redis import must reload with NOSAVE, got: %s", impStr)
	}

	mysqlDump, _ := buildDumpCommand("mysql", "db")
	mysqlStr := strings.Join(mysqlDump, " ")
	for _, flag := range []string{"--routines", "--events", "--triggers"} {
		if !strings.Contains(mysqlStr, flag) {
			t.Fatalf("mysqldump must carry %s, got: %s", flag, mysqlStr)
		}
	}
}

// The target's app inventory must not report an older build's Succeeded
// status as this migration's outcome: the phase counts only when the status
// commit matches the build the migration triggered.
func TestTargetAppsHandler_BuildIdentity(t *testing.T) {
	staleBuild := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name: "web", Namespace: "shop-prod",
			Annotations: map[string]string{migrationBuildAnnotation: "migrate-200"},
		},
		Spec:   kipperv1.AppSpec{Git: &kipperv1.AppGitSource{URL: "https://github.com/shop/web"}},
		Status: kipperv1.AppStatus{Build: &kipperv1.AppBuildStatus{Phase: "Succeeded", Commit: "migrate-100"}},
	}
	currentBuild := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name: "api", Namespace: "shop-prod",
			Annotations: map[string]string{migrationBuildAnnotation: "migrate-200"},
		},
		Spec:   kipperv1.AppSpec{Git: &kipperv1.AppGitSource{URL: "https://github.com/shop/api"}},
		Status: kipperv1.AppStatus{Build: &kipperv1.AppBuildStatus{Phase: "Succeeded", Commit: "migrate-200"}},
	}
	h := &Handler{
		Sessions: NewSessionStore(),
		Client:   fake.NewSimpleClientset(projectNamespace("shop-prod", "shop")),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(staleBuild, currentBuild).Build(),
	}
	h.Sessions.Put(&Session{ID: "s1", Projects: []string{"shop"}})

	router := chi.NewRouter()
	router.Get("/{session}/apps", h.TargetAppsHandler)
	req := httptest.NewRequest(http.MethodGet, "/s1/apps", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Apps []struct {
			Name       string `json:"name"`
			BuildPhase string `json:"build_phase"`
		} `json:"apps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parsing response: %v", err)
	}
	phases := map[string]string{}
	for _, a := range resp.Apps {
		phases[a.Name] = a.BuildPhase
	}
	if phases["web"] != "Pending" {
		t.Fatalf("stale build commit must report Pending, got %q", phases["web"])
	}
	if phases["api"] != "Succeeded" {
		t.Fatalf("matching build commit must report Succeeded, got %q", phases["api"])
	}
}

// persist() marshals the session snapshot outside the lock; concurrent route
// saves during a persist must not trip the race detector or crash on
// concurrent map access.
func TestSessionStore_PersistDuringRouteSaves(t *testing.T) {
	store := NewPersistentSessionStore(fake.NewSimpleClientset(), "kipper-system")
	session := &Session{
		ID:          "race123",
		Status:      SessionRunning,
		StartedAt:   time.Now(),
		SavedRoutes: make(map[string]map[string]interface{}),
	}
	store.Put(session)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			session.SaveRoute(fmt.Sprintf("shop-prod/app-%d", i), map[string]interface{}{"host": "shop.example.com"})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			store.Save(session)
		}
	}()
	wg.Wait()
}

// callTarget must treat any 2xx as success. The transfer-receiver POST answers
// 201 Created; requiring exactly 200 read that as a failure and broke every
// volume/servicePVC transfer in a real migration.
func TestCallTargetAccepts2xx(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"200 ok", http.StatusOK, `{"ok":true}`, false},
		{"201 created", http.StatusCreated, `{"transferID":"x"}`, false},
		{"204 no content", http.StatusNoContent, ``, false},
		{"409 conflict", http.StatusConflict, `{"error":"conflict"}`, true},
		{"500 error", http.StatusInternalServerError, `boom`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			h := &Handler{}
			_, err := h.callTarget(&Token{Endpoint: srv.URL}, "POST", "/x", nil)
			if tc.wantErr && err == nil {
				t.Fatalf("status %d: expected an error", tc.status)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("status %d: unexpected error: %v", tc.status, err)
			}
		})
	}
}
