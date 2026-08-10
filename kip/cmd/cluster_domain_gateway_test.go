package cmd

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/clusteridentity"
	"github.com/getkipper/kipper/kip/internal/domain"
)

// fakeGateway serves /register. A POST for conflictLabel returns 409; a POST for
// renewLabel returns 201 with an empty token; any other POST returns 201 with a
// fresh token. DELETE records the token and answers deleteStatus when set (0
// means 204).
//
// The tokenless answer models both of the real gateway's tokenless outcomes: a
// renewal it authorised, and a request against a name it will not hand over.
// They are identical on the wire except for the challenge, which the gateway
// issues only to a caller whose token it recognised — so this fake issues one
// only for the token it holds. Reading a tokenless reply as ownership, or
// reading "we sent a token" as ownership, are the two defects this fixture used
// to conceal.
func fakeGateway(t *testing.T, conflictLabel, renewLabel string, deleteStatus int, deregistered *[]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.Method {
		case http.MethodPost:
			if body["subdomain"] == conflictLabel {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "already registered"})
				return
			}
			reg := domain.Registration{Subdomain: body["subdomain"], Domain: body["subdomain"] + ".kipper.run"}
			if body["subdomain"] != renewLabel {
				reg.Token = "tok-" + body["subdomain"]
			} else if body["token"] == "tok-"+renewLabel {
				// A renewal the gateway authorised. It issues a challenge only
				// to a token holder, which is the caller's only way to tell this
				// apart from being turned away.
				reg.Challenge = "nonce-" + renewLabel
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(reg)
		case http.MethodDelete:
			if deleteStatus != 0 && deleteStatus != http.StatusNoContent {
				w.WriteHeader(deleteStatus)
				return
			}
			*deregistered = append(*deregistered, body["token"])
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return httptest.NewServer(mux)
}

func gatewaySecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayCredentialsSecret, Namespace: gatewayCredentialsNamespace},
		Data:       data,
	}
}

func gatewayData(t *testing.T, cs *fake.Clientset) map[string][]byte {
	t.Helper()
	data, err := readGatewayCredentials(context.Background(), cs)
	if err != nil {
		t.Fatalf("read gateway credentials: %v", err)
	}
	return data
}

func TestGatewayCredentialsRoundTrip(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()

	if data := gatewayData(t, cs); len(data) != 0 {
		t.Fatalf("absent secret should read empty, got %v", data)
	}
	if err := writeGatewayCredentials(ctx, cs, map[string][]byte{gatewayCredentialsKey: []byte("abc")}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if data := gatewayData(t, cs); string(data[gatewayCredentialsKey]) != "abc" {
		t.Fatalf("read back = %q, want abc", data[gatewayCredentialsKey])
	}
	// Overwrite in place.
	if err := writeGatewayCredentials(ctx, cs, map[string][]byte{gatewayCredentialsKey: []byte("def")}); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if data := gatewayData(t, cs); string(data[gatewayCredentialsKey]) != "def" {
		t.Fatalf("read back = %q, want def", data[gatewayCredentialsKey])
	}
}

func TestRegisterGatewayLabelConflictAborts(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var dereg []string
	srv := fakeGateway(t, "taken", "", 0, &dereg)
	defer srv.Close()
	cs := fake.NewSimpleClientset()
	current := &clusteridentity.ClusterIdentity{}

	if err := registerGatewayLabel(cs, srv.URL, "203.0.113.10", "c1", "taken.kipper.run", current); err == nil {
		t.Fatal("a conflicting label must abort registration")
	}
	// The stored credentials must be untouched on abort (nothing registered).
	if data := gatewayData(t, cs); len(data) != 0 {
		t.Fatalf("conflict must not write credentials, got %v", data)
	}
}

func TestRegisterRecordsMoveAndFinishCleansUp(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var dereg []string
	srv := fakeGateway(t, "", "", 0, &dereg)
	defer srv.Close()
	// The cluster already holds an old kipper.run label with a stored token.
	cs := fake.NewSimpleClientset(gatewaySecret(map[string][]byte{gatewayCredentialsKey: []byte("tok-old")}))
	current := &clusteridentity.ClusterIdentity{
		Spec: clusteridentity.Spec{Gateway: &clusteridentity.Gateway{KipperRunDomain: "old.kipper.run"}},
	}

	if err := registerGatewayLabel(cs, srv.URL, "203.0.113.10", "c1", "fresh.kipper.run", current); err != nil {
		t.Fatalf("register: %v", err)
	}
	// The new token and the pending cleanup are persisted in one write, so an
	// interrupted move loses neither.
	data := gatewayData(t, cs)
	if string(data[gatewayCredentialsKey]) != "tok-fresh" {
		t.Fatalf("registration should persist the new token immediately, got %q", data[gatewayCredentialsKey])
	}
	if string(data[gatewayCredentialsOldLabelKey]) != "old" || string(data[gatewayCredentialsOldTokenKey]) != "tok-old" {
		t.Fatalf("the move must be recorded durably, got %v", data)
	}
	if string(data[gatewayCredentialsNewLabelKey]) != "fresh" {
		t.Fatalf("the record must name the destination it belongs to, got %v", data)
	}

	if err := finishGatewayMove(cs, srv.URL); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(dereg) != 1 || dereg[0] != "tok-old" {
		t.Fatalf("finish should deregister the old label's token, got %v", dereg)
	}
	data = gatewayData(t, cs)
	if len(data[gatewayCredentialsOldLabelKey]) != 0 || len(data[gatewayCredentialsOldTokenKey]) != 0 || len(data[gatewayCredentialsNewLabelKey]) != 0 {
		t.Fatalf("finish must clear the move record, got %v", data)
	}
}

// The route-kill regression: a re-run after an interrupted move must not
// capture the new label's token as "old" and then deregister the label the
// cluster is moving to.
func TestRegisterRerunAfterInterruptionKeepsRecordedMove(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var dereg []string
	// The re-run's registration is a same-IP renewal: no token disclosed.
	srv := fakeGateway(t, "", "fresh", 0, &dereg)
	defer srv.Close()
	// State after run 1 died mid-move: token already rotated to the new label,
	// old label and token recorded.
	cs := fake.NewSimpleClientset(gatewaySecret(map[string][]byte{
		gatewayCredentialsKey:         []byte("tok-fresh"),
		gatewayCredentialsOldLabelKey: []byte("old"),
		gatewayCredentialsOldTokenKey: []byte("tok-old"),
		gatewayCredentialsNewLabelKey: []byte("fresh"),
	}))
	// Run 1 may or may not have patched the spec before dying; the recorded
	// move must win either way. This models the pre-patch crash, where the
	// spec still names the old label.
	current := &clusteridentity.ClusterIdentity{
		Spec: clusteridentity.Spec{Gateway: &clusteridentity.Gateway{KipperRunDomain: "old.kipper.run"}},
	}

	if err := registerGatewayLabel(cs, srv.URL, "203.0.113.10", "c1", "fresh.kipper.run", current); err != nil {
		t.Fatalf("register: %v", err)
	}
	data := gatewayData(t, cs)
	if string(data[gatewayCredentialsOldTokenKey]) != "tok-old" || string(data[gatewayCredentialsOldLabelKey]) != "old" {
		t.Fatalf("the recorded move must survive a re-run untouched, got %v", data)
	}
	if string(data[gatewayCredentialsKey]) != "tok-fresh" {
		t.Fatalf("the new label's token must survive a re-run, got %q", data[gatewayCredentialsKey])
	}

	if err := finishGatewayMove(cs, srv.URL); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if len(dereg) != 1 || dereg[0] != "tok-old" {
		t.Fatalf("finish must deregister the old label, never the new one, got %v", dereg)
	}
}

func TestRegisterRenewalKeepsExistingToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var dereg []string
	// The gateway returns an empty token for "retained" (a same-IP renewal).
	srv := fakeGateway(t, "", "retained", 0, &dereg)
	defer srv.Close()
	cs := fake.NewSimpleClientset(gatewaySecret(map[string][]byte{gatewayCredentialsKey: []byte("tok-retained")}))
	current := &clusteridentity.ClusterIdentity{
		Spec: clusteridentity.Spec{Gateway: &clusteridentity.Gateway{KipperRunDomain: "retained.kipper.run"}},
	}

	if err := registerGatewayLabel(cs, srv.URL, "203.0.113.10", "c1", "retained.kipper.run", current); err != nil {
		t.Fatalf("register: %v", err)
	}
	// A renewal returns no token; the stored token must be left intact, never
	// overwritten with empty. Same label means no move to record either.
	data := gatewayData(t, cs)
	if string(data[gatewayCredentialsKey]) != "tok-retained" {
		t.Fatalf("renewal must not clobber the stored token, got %q", data[gatewayCredentialsKey])
	}
	if len(data[gatewayCredentialsOldLabelKey]) != 0 {
		t.Fatalf("a same-label renewal must not record a move, got %v", data)
	}
}

func TestFinishGatewayMoveNoRecordIsNoOp(t *testing.T) {
	cs := fake.NewSimpleClientset(gatewaySecret(map[string][]byte{gatewayCredentialsKey: []byte("tok-current")}))
	// No gateway is contacted when nothing is recorded (192.0.2.1 is TEST-NET).
	if err := finishGatewayMove(cs, "http://192.0.2.1"); err != nil {
		t.Fatalf("finish with no recorded move must be a no-op, got %v", err)
	}
}

func TestFinishGatewayMoveKeepsRecordOnFailure(t *testing.T) {
	var dereg []string
	srv := fakeGateway(t, "", "", http.StatusInternalServerError, &dereg)
	defer srv.Close()
	cs := fake.NewSimpleClientset(gatewaySecret(map[string][]byte{
		gatewayCredentialsKey:         []byte("tok-fresh"),
		gatewayCredentialsOldLabelKey: []byte("old"),
		gatewayCredentialsOldTokenKey: []byte("tok-old"),
		gatewayCredentialsNewLabelKey: []byte("fresh"),
	}))

	if err := finishGatewayMove(cs, srv.URL); err == nil {
		t.Fatal("a failed deregistration must surface as an error")
	}
	data := gatewayData(t, cs)
	if string(data[gatewayCredentialsOldTokenKey]) != "tok-old" {
		t.Fatalf("a failed deregistration must keep the record for retry, got %v", data)
	}
}

// A second move to a different destination must finish the pending cleanup
// first, so the current label's token is captured as "old" instead of being
// clobbered while the earlier record still points at an even older label.
func TestSecondMoveFinishesPendingCleanupFirst(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var dereg []string
	srv := fakeGateway(t, "", "", 0, &dereg)
	defer srv.Close()
	// Converged on "fresh" with the cleanup for "old" still pending.
	cs := fake.NewSimpleClientset(gatewaySecret(map[string][]byte{
		gatewayCredentialsKey:         []byte("tok-fresh"),
		gatewayCredentialsOldLabelKey: []byte("old"),
		gatewayCredentialsOldTokenKey: []byte("tok-old"),
		gatewayCredentialsNewLabelKey: []byte("fresh"),
	}))
	current := &clusteridentity.ClusterIdentity{
		Spec: clusteridentity.Spec{Gateway: &clusteridentity.Gateway{KipperRunDomain: "fresh.kipper.run"}},
	}

	if err := registerGatewayLabel(cs, srv.URL, "203.0.113.10", "c1", "third.kipper.run", current); err != nil {
		t.Fatalf("register: %v", err)
	}
	if len(dereg) != 1 || dereg[0] != "tok-old" {
		t.Fatalf("the pending cleanup must run before the new move, got %v", dereg)
	}
	data := gatewayData(t, cs)
	if string(data[gatewayCredentialsKey]) != "tok-third" {
		t.Fatalf("the new label's token must be current, got %q", data[gatewayCredentialsKey])
	}
	if string(data[gatewayCredentialsOldLabelKey]) != "fresh" || string(data[gatewayCredentialsOldTokenKey]) != "tok-fresh" || string(data[gatewayCredentialsNewLabelKey]) != "third" {
		t.Fatalf("the new record must capture the label being left, got %v", data)
	}
}

// A second move must refuse to start when the pending cleanup cannot finish:
// proceeding would clobber the token that record still needs.
func TestSecondMoveRefusesWhenPendingCleanupFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var dereg []string
	srv := fakeGateway(t, "", "", http.StatusInternalServerError, &dereg)
	defer srv.Close()
	cs := fake.NewSimpleClientset(gatewaySecret(map[string][]byte{
		gatewayCredentialsKey:         []byte("tok-fresh"),
		gatewayCredentialsOldLabelKey: []byte("old"),
		gatewayCredentialsOldTokenKey: []byte("tok-old"),
		gatewayCredentialsNewLabelKey: []byte("fresh"),
	}))
	current := &clusteridentity.ClusterIdentity{
		Spec: clusteridentity.Spec{Gateway: &clusteridentity.Gateway{KipperRunDomain: "fresh.kipper.run"}},
	}

	if err := registerGatewayLabel(cs, srv.URL, "203.0.113.10", "c1", "third.kipper.run", current); err == nil {
		t.Fatal("a new move must refuse while the pending cleanup cannot finish")
	}
	data := gatewayData(t, cs)
	if string(data[gatewayCredentialsKey]) != "tok-fresh" || string(data[gatewayCredentialsOldTokenKey]) != "tok-old" {
		t.Fatalf("a refused move must leave the journal untouched, got %v", data)
	}
}

// A gateway that no longer knows the token (an earlier attempt already
// deregistered it) means the cleanup is done: the record clears instead of
// wedging every future retry.
func TestFinishGatewayMoveClearsRecordWhenAlreadyDeregistered(t *testing.T) {
	var dereg []string
	srv := fakeGateway(t, "", "", http.StatusNotFound, &dereg)
	defer srv.Close()
	cs := fake.NewSimpleClientset(gatewaySecret(map[string][]byte{
		gatewayCredentialsKey:         []byte("tok-fresh"),
		gatewayCredentialsOldLabelKey: []byte("old"),
		gatewayCredentialsOldTokenKey: []byte("tok-old"),
		gatewayCredentialsNewLabelKey: []byte("fresh"),
	}))

	if err := finishGatewayMove(cs, srv.URL); err != nil {
		t.Fatalf("an already-removed registration is completed cleanup, got %v", err)
	}
	data := gatewayData(t, cs)
	if len(data[gatewayCredentialsOldTokenKey]) != 0 || len(data[gatewayCredentialsNewLabelKey]) != 0 {
		t.Fatalf("the stale record must clear once the gateway confirms removal, got %v", data)
	}
}

// Moving onto a name that already exists, with no token for it, must be refused.
// The gateway answers such a request without a token — it calls that
// unauthenticated, not renewed — and continuing would carry the current label's
// token onto the new name. The cluster could then never prove the target, while
// the move goes on to deregister the label that currently works.
func TestDomainMoveRefusesANameItCannotProveItOwns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	target := "taken"
	srv := fakeGateway(t, "", target, 0, nil)
	t.Cleanup(srv.Close)

	cs := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: gatewayCredentialsSecret, Namespace: gatewayCredentialsNamespace},
		Data:       map[string][]byte{gatewayCredentialsKey: []byte("tok-old-label")},
	})
	current := &clusteridentity.ClusterIdentity{}

	err := registerGatewayLabel(cs, srv.URL, "203.0.113.10", "c1", target+".kipper.run", current)
	if err == nil {
		t.Fatal("a move onto an unprovable name must be refused, not silently carried out with the old token")
	}
	if !strings.Contains(err.Error(), "already registered") {
		t.Errorf("the error must name the cause, got %v", err)
	}

	// And the cluster's credential must be untouched: the old label still works.
	secret, getErr := cs.CoreV1().Secrets(gatewayCredentialsNamespace).Get(context.Background(), gatewayCredentialsSecret, metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("reading credentials: %v", getErr)
	}
	if string(secret.Data[gatewayCredentialsKey]) != "tok-old-label" {
		t.Error("a refused move must leave the working label's token in place")
	}
}

// A retry of an interrupted move already holds the target's token, recorded by
// the attempt that died. Presenting it renews the registration rather than
// arriving anonymously against a name this cluster does own.
func TestDomainMoveRetryPresentsTheTargetTokenItAlreadyHolds(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	target := "moving"
	var sawToken string
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		sawToken = body["token"]
		reg := domain.Registration{Subdomain: body["subdomain"], Domain: body["subdomain"] + ".kipper.run"}
		if body["token"] == "tok-target" {
			// Recognised: the gateway renews and issues a challenge, which is
			// the caller's only evidence the token was accepted.
			reg.Challenge = "nonce-target"
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(reg)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	cs := fake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: gatewayCredentialsSecret, Namespace: gatewayCredentialsNamespace},
		Data: map[string][]byte{
			gatewayCredentialsKey:         []byte("tok-target"),
			gatewayCredentialsNewLabelKey: []byte(target),
		},
	})

	if err := registerGatewayLabel(cs, srv.URL, "203.0.113.10", "c1", target+".kipper.run", &clusteridentity.ClusterIdentity{}); err != nil {
		t.Fatalf("a retry holding the target's token must succeed: %v", err)
	}
	if sawToken != "tok-target" {
		t.Errorf("the retry must present the token it holds, sent %q", sawToken)
	}
}

// Holding a token is not the same as the gateway accepting it. A stale or
// mismatched credential — an adopted identity whose secret names a different
// label, or a token the gateway swept — draws the same tokenless 201 as a
// genuine renewal. Only the challenge distinguishes them, so sending something
// must not be mistaken for having proved something.
func TestDomainMoveRefusesWhenTheGatewayIgnoresAStaleToken(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	target := "adopted"
	// The fake issues a challenge only for the token it holds; this cluster
	// presents a different one, so it answers tokenless and challengeless —
	// exactly what the real gateway does for an unauthenticated same-IP request.
	srv := fakeGateway(t, "", target, 0, nil)
	t.Cleanup(srv.Close)

	cs := fake.NewSimpleClientset(gatewaySecret(map[string][]byte{ //nolint:staticcheck
		gatewayCredentialsKey: []byte("tok-belonging-to-another-label"),
	}))
	current := &clusteridentity.ClusterIdentity{
		Spec: clusteridentity.Spec{Gateway: &clusteridentity.Gateway{KipperRunDomain: target + ".kipper.run"}},
	}

	err := registerGatewayLabel(cs, srv.URL, "203.0.113.10", "c1", target+".kipper.run", current)
	if err == nil {
		t.Fatal("a token the gateway did not accept must not count as ownership")
	}

	data := gatewayData(t, cs)
	if string(data[gatewayCredentialsKey]) != "tok-belonging-to-another-label" {
		t.Error("a refused move must leave the stored credential untouched")
	}
}
