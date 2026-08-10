package migration

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Both clusters must derive the same per-transfer token from the session
// secret, and different transfers must never share one.
func TestDeriveTransferToken(t *testing.T) {
	a1, err := deriveTransferToken("session-secret", "abc12345-shop-prod-uploads")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := deriveTransferToken("session-secret", "abc12345-shop-prod-uploads")
	b, _ := deriveTransferToken("session-secret", "abc12345-shop-prod-files")
	if a1 != a2 {
		t.Fatal("derivation must be deterministic")
	}
	if a1 == b {
		t.Fatal("different transfers must get different tokens")
	}
	if len(a1) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(a1))
	}
}

// Transfer IDs feed pod and label names, so they must be DNS-safe,
// bounded, and collision-free even when the readable slug truncates.
func TestTransferID(t *testing.T) {
	id := transferID("0d2f9c11-aaaa-bbbb", "shop-prod", "Uploads_2024")
	if !strings.HasPrefix(id, "shop-prod-uploads-2024-") || len(id) > 48 {
		t.Fatalf("id = %q", id)
	}
	if id != transferID("0d2f9c11-aaaa-bbbb", "shop-prod", "Uploads_2024") {
		t.Fatal("id must be deterministic")
	}
	// Truncated slugs must still be distinct through the hash suffix.
	a := transferID("0d2f9c11", "a-very-long-namespace-name-for-a-project", "an-equally-long-volume-name-alpha")
	b := transferID("0d2f9c11", "a-very-long-namespace-name-for-a-project", "an-equally-long-volume-name-beta")
	if a == b {
		t.Fatal("distinct items must never share an id")
	}
	if len(a) > 48 || len(b) > 48 {
		t.Fatalf("id lengths = %d, %d, want <= 48", len(a), len(b))
	}
}

func transferSession(t *testing.T, h *Handler) *Session {
	t.Helper()
	session := &Session{
		ID:       "sess-transfer-test",
		Secret:   "sess-secret",
		Projects: []string{"shop"},
	}
	h.Sessions.Put(session)
	return session
}

func postTransfer(t *testing.T, h *Handler, session *Session, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migrate-target/"+session.ID+"/transfer", bytes.NewReader(payload))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", session.ID)
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.CreateTransferHandler(rec, req)
	return rec
}

// The receiver must only ever be created inside the accepted scope.
func TestCreateTransferScopeEnforced(t *testing.T) {
	h := &Handler{
		Client:         fake.NewSimpleClientset(projectNamespace("other-prod", "other")),
		Sessions:       NewSessionStore(),
		DatamoverImage: "img",
	}
	session := transferSession(t, h)

	rec := postTransfer(t, h, session, map[string]interface{}{
		"transferID": "sess-other-prod-uploads",
		"namespace":  "other-prod",
		"kind":       "volume",
		"target":     map[string]string{"volume": "uploads"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

// A volume transfer stands up pod, service, and token secret; the pod
// carries the lease label and mounts the target claim without a
// service-account token.
func TestCreateTransferCreatesReceiver(t *testing.T) {
	h := &Handler{
		Client:         fake.NewSimpleClientset(projectNamespace("shop-prod", "shop")),
		Sessions:       NewSessionStore(),
		DatamoverImage: "img",
	}
	session := transferSession(t, h)

	rec := postTransfer(t, h, session, map[string]interface{}{
		"transferID": "sess-shop-prod-uploads",
		"namespace":  "shop-prod",
		"kind":       "volume",
		"target":     map[string]string{"volume": "uploads"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	pod, err := h.Client.CoreV1().Pods("shop-prod").Get(context.Background(), "dt-sess-shop-prod-uploads-import", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("import pod: %v", err)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("import pod must not mount a service-account token")
	}
	if pod.Labels[transferLeaseLabel] == "" {
		t.Fatal("import pod must carry a lease")
	}
	claim := ""
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			claim = v.PersistentVolumeClaim.ClaimName
		}
	}
	if claim != "shared-uploads" {
		t.Fatalf("claim = %q, want shared-uploads", claim)
	}
	if _, err := h.Client.CoreV1().Services("shop-prod").Get(context.Background(), "dt-sess-shop-prod-uploads", metav1.GetOptions{}); err != nil {
		t.Fatalf("import service: %v", err)
	}
	secret, err := h.Client.CoreV1().Secrets("shop-prod").Get(context.Background(), "dt-sess-shop-prod-uploads-token", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("token secret: %v", err)
	}
	want, _ := deriveTransferToken(session.Secret, "sess-shop-prod-uploads")
	if secret.StringData["token"] != want {
		t.Fatal("token secret must hold the HKDF-derived per-transfer token")
	}
}

// A servicePVC transfer stops the target statefulset before handing its
// claim to the import mover.
func TestCreateTransferStopsServiceStatefulSet(t *testing.T) {
	one := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "files", Namespace: "shop-prod"},
		Spec:       appsv1.StatefulSetSpec{Replicas: &one},
	}
	h := &Handler{
		Client:         fake.NewSimpleClientset(projectNamespace("shop-prod", "shop"), sts),
		Sessions:       NewSessionStore(),
		DatamoverImage: "img",
	}
	session := transferSession(t, h)

	rec := postTransfer(t, h, session, map[string]interface{}{
		"transferID": "sess-shop-prod-svc-files",
		"namespace":  "shop-prod",
		"kind":       "servicePVC",
		"target":     map[string]string{"service": "files"},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", rec.Code, rec.Body.String())
	}

	got, err := h.Client.AppsV1().StatefulSets("shop-prod").Get(context.Background(), "files", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
		t.Fatal("target statefulset must be scaled to zero for the PVC transfer")
	}
	pod, err := h.Client.CoreV1().Pods("shop-prod").Get(context.Background(), "dt-sess-shop-prod-svc-files-import", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Labels["kipper.run/transfer-service"] != "files" {
		t.Fatal("import pod must record the paused service for teardown")
	}
}

// The lease reaper collects expired receivers and restarts their paused
// service, and leaves live receivers alone.
func TestReapExpiredTransfers(t *testing.T) {
	zero := int32(0)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "files", Namespace: "shop-prod"},
		Spec:       appsv1.StatefulSetSpec{Replicas: &zero},
	}
	expiredSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "dt-old-token", Namespace: "shop-prod",
		Labels: map[string]string{
			transferIDLabel:              "old",
			transferLeaseLabel:           strconv.FormatInt(time.Now().Add(-time.Hour).Unix(), 10),
			transferServiceLabel:         "files",
			transferServiceReplicasLabel: "2",
		},
	}}
	expiredPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "dt-old-import", Namespace: "shop-prod",
		Labels: map[string]string{transferIDLabel: "old"},
	}}
	liveSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "dt-new-token", Namespace: "shop-prod",
		Labels: map[string]string{
			transferIDLabel:    "new",
			transferLeaseLabel: strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10),
		},
	}}
	h := &Handler{Client: fake.NewSimpleClientset(sts, expiredSecret, expiredPod, liveSecret)}

	h.reapExpiredTransfers(context.Background())

	if _, err := h.Client.CoreV1().Pods("shop-prod").Get(context.Background(), "dt-old-import", metav1.GetOptions{}); err == nil {
		t.Fatal("expired receiver must be reaped")
	}
	if _, err := h.Client.CoreV1().Secrets("shop-prod").Get(context.Background(), "dt-old-token", metav1.GetOptions{}); err == nil {
		t.Fatal("expired token secret must be reaped")
	}
	if _, err := h.Client.CoreV1().Secrets("shop-prod").Get(context.Background(), "dt-new-token", metav1.GetOptions{}); err != nil {
		t.Fatal("live receiver must be kept")
	}
	// A receiver only reaches its lease expiry because its transfer was
	// abandoned, and the importer rewrites the destination in place, so the
	// volume may hold a partly-applied copy. The service therefore stays
	// stopped and records what an operator needs to decide, rather than being
	// started on data nothing can vouch for.
	got, _ := h.Client.AppsV1().StatefulSets("shop-prod").Get(context.Background(), "files", metav1.GetOptions{})
	if got.Spec.Replicas == nil || *got.Spec.Replicas != 0 {
		t.Fatalf("replicas = %v, want the abandoned transfer's service left stopped", got.Spec.Replicas)
	}
	if got.Annotations["kipper.run/held-down-reason"] == "" {
		t.Error("a service held down by reaping must record why")
	}
	if got.Annotations["kipper.run/held-down-restore-replicas"] != "2" {
		t.Errorf("restore hint = %q, want the pre-pause count 2", got.Annotations["kipper.run/held-down-restore-replicas"])
	}
}

func proxyRequest(t *testing.T, h *Handler, sessionID, bearer string) *httptest.ResponseRecorder {
	const transferID = "ghost"
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/migrate-target/"+sessionID+"/transfer/"+transferID+"/chunk/0", nil)
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("session", sessionID)
	rctx.URLParams.Add("transfer", transferID)
	rctx.URLParams.Add("*", "chunk/0")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rec := httptest.NewRecorder()
	h.TransferProxyHandler(rec, req)
	return rec
}

// The proxy authenticates the derived token before doing any cluster-wide
// work: no or wrong token means 401, a valid token for a transfer without a
// receiver means 404.
func TestTransferProxyAuth(t *testing.T) {
	h := &Handler{Client: fake.NewSimpleClientset(), Sessions: NewSessionStore()}
	session := transferSession(t, h)

	if rec := proxyRequest(t, h, session.ID, ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", rec.Code)
	}
	if rec := proxyRequest(t, h, session.ID, "wrong-token"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: status = %d, want 401", rec.Code)
	}
	if rec := proxyRequest(t, h, "no-such-session", "anything"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown session: status = %d, want 401", rec.Code)
	}
	valid, _ := deriveTransferToken(session.Secret, "ghost")
	if rec := proxyRequest(t, h, session.ID, valid); rec.Code != http.StatusNotFound {
		t.Fatalf("valid token, no receiver: status = %d, want 404", rec.Code)
	}
}

// The proxy bypasses RequireMigrationSecret, so it must reject an expired
// accepted session itself — a receiver outliving its session cannot keep
// serving a still-valid derived bearer.
func TestTransferProxyRejectsExpiredSession(t *testing.T) {
	h := &Handler{Client: fake.NewSimpleClientset(), Sessions: NewSessionStore()}
	session := &Session{ID: "expired-sess", Secret: "sess-secret", Projects: []string{"shop"}, ExpiresAt: time.Now().Add(-time.Minute)}
	h.Sessions.Put(session)

	valid, _ := deriveTransferToken(session.Secret, "ghost")
	if rec := proxyRequest(t, h, session.ID, valid); rec.Code != http.StatusUnauthorized {
		t.Fatalf("expired session: status = %d, want 401", rec.Code)
	}
	if _, ok := h.Sessions.Get(session.ID); ok {
		t.Fatal("an expired session must be evicted on rejection")
	}
}
