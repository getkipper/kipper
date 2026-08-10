package main

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/hopcert"
	"github.com/getkipper/kipper/controller/pkg/hopproof"
)

// capturedRegister is one request body the fake gateway received.
type capturedRegister struct {
	Subdomain       string `json:"subdomain"`
	IP              string `json:"ip"`
	CertFingerprint string `json:"certFingerprint"`
	Token           string `json:"token"`
}

// fakeGateway serves scripted /register responses and records the requests.
type fakeGateway struct {
	t        *testing.T
	status   int
	body     map[string]string
	requests []capturedRegister
}

func (g *fakeGateway) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req capturedRegister
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			g.t.Errorf("gateway received an undecodable body: %v", err)
		}
		g.requests = append(g.requests, req)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.status)
		_ = json.NewEncoder(w).Encode(g.body)
	}
}

func heartbeatUnderTest(t *testing.T, gw *fakeGateway, objs ...crclient.Object) (*gatewayHeartbeat, *fake.Clientset) {
	t.Helper()
	ts := httptest.NewServer(gw.handler())
	t.Cleanup(ts.Close)

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayCredentialsSecret, Namespace: gatewayCredentialsNamespace},
		Data:       map[string][]byte{gatewayCredentialsTokenKey: []byte("cluster-token")},
	})
	return &gatewayHeartbeat{
		url:        ts.URL,
		subdomain:  "myapp",
		host:       "203.0.113.1",
		client:     client,
		crClient:   crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		httpClient: ts.Client(),
	}, client
}

func TestBeatAssertsFingerprintWithToken(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"pin": "active"}}
	h, client := heartbeatUnderTest(t, gw)

	next := h.beat(context.Background())
	if next != heartbeatInterval {
		t.Errorf("an active pin is steady state: expected %v, got %v", heartbeatInterval, next)
	}
	if len(gw.requests) != 1 {
		t.Fatalf("expected 1 request, got %d", len(gw.requests))
	}
	req := gw.requests[0]
	if req.Subdomain != "myapp" || req.IP != "203.0.113.1" {
		t.Errorf("unexpected renewal payload: %+v", req)
	}
	if req.Token != "cluster-token" {
		t.Errorf("the assertion must carry the management token, got %q", req.Token)
	}

	st, err := hopcert.Ensure(context.Background(), client, h.crClient)
	if err != nil {
		t.Fatal(err)
	}
	if req.CertFingerprint != st.Fingerprint {
		t.Errorf("asserted %q, hop cert fingerprint is %q", req.CertFingerprint, st.Fingerprint)
	}
}

func TestBeatWithoutTokenRenewsPlainAndRetriesSoon(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"pin": "none"}}
	h, client := heartbeatUnderTest(t, gw)
	if err := client.CoreV1().Secrets(gatewayCredentialsNamespace).Delete(context.Background(), gatewayCredentialsSecret, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	next := h.beat(context.Background())
	if next != convergeRetry {
		t.Errorf("a tokenless renewal should retry soon, got %v", next)
	}
	if req := gw.requests[0]; req.CertFingerprint != "" || req.Token != "" {
		t.Errorf("no token means no assertion, got %+v", req)
	}
}

func TestBeatPersistsReissuedToken(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"token": "fresh-token", "pin": "none"}}
	h, client := heartbeatUnderTest(t, gw)

	next := h.beat(context.Background())
	if next != convergeRetry {
		t.Errorf("a re-created registration should re-assert soon, got %v", next)
	}
	secret, err := client.CoreV1().Secrets(gatewayCredentialsNamespace).Get(context.Background(), gatewayCredentialsSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(secret.Data[gatewayCredentialsTokenKey]); got != "fresh-token" {
		t.Errorf("the re-issued token must be persisted, got %q", got)
	}
}

func TestBeatToleratesOldGateway(t *testing.T) {
	// An old gateway echoes no pin field at all.
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{}}
	h, _ := heartbeatUnderTest(t, gw)

	next := h.beat(context.Background())
	if next != heartbeatInterval {
		t.Errorf("an old gateway is not an error: expected %v, got %v", heartbeatInterval, next)
	}
	if !h.warnedOldGateway {
		t.Error("expected the old-gateway warning to be recorded")
	}
}

func TestBeatPendingRetriesSoon(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusAccepted, body: map[string]string{"pin": "pending", "observedFingerprint": ""}}
	h, _ := heartbeatUnderTest(t, gw)

	if next := h.beat(context.Background()); next != convergeRetry {
		t.Errorf("a pending pin should retry soon, got %v", next)
	}
}

func TestBeatRefusesAssertionWithForeignStore(t *testing.T) {
	foreign := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "traefik.io/v1alpha1",
		"kind":       "TLSStore",
		"metadata":   map[string]any{"name": "default", "namespace": "tenant-a"},
	}}
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"pin": "none"}}
	h, _ := heartbeatUnderTest(t, gw, foreign)

	h.beat(context.Background())
	if req := gw.requests[0]; req.CertFingerprint != "" {
		t.Errorf("a foreign default TLSStore must block pin assertion, got %+v", req)
	}
}

func TestBeatRotationPromotesOnAcknowledgement(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusAccepted, body: map[string]string{"pin": "pending"}}
	h, client := heartbeatUnderTest(t, gw)

	// Provision, then request a key rotation.
	before, err := hopcert.Ensure(context.Background(), client, h.crClient)
	if err != nil {
		t.Fatal(err)
	}
	secret, _ := client.CoreV1().Secrets(hopcert.Namespace).Get(context.Background(), hopcert.SecretName, metav1.GetOptions{})
	secret.Annotations = map[string]string{hopcert.RotateAnnotation: "requested"}
	if _, err := client.CoreV1().Secrets(hopcert.Namespace).Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if next := h.beat(context.Background()); next != convergeRetry {
		t.Errorf("a mid-rotation beat should retry soon, got %v", next)
	}

	// The beat asserted the candidate, and the gateway's acknowledgement
	// (pending) allowed the swap: the candidate is now live.
	after, err := hopcert.Ensure(context.Background(), client, h.crClient)
	if err != nil {
		t.Fatal(err)
	}
	asserted := gw.requests[0].CertFingerprint
	if asserted == before.Fingerprint {
		t.Error("mid-rotation the candidate fingerprint must be asserted, not the live one")
	}
	if after.Fingerprint != asserted {
		t.Errorf("after acknowledgement the candidate must be live: got %s, asserted %s", after.Fingerprint, asserted)
	}
	if after.CandidateFingerprint != "" {
		t.Error("the staged candidate must be cleared by the swap")
	}
}

func TestBeatBacksOffOnServerError(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusInternalServerError, body: map[string]string{"error": "boom"}}
	h, _ := heartbeatUnderTest(t, gw)

	first := h.beat(context.Background())
	second := h.beat(context.Background())
	third := h.beat(context.Background())
	if first != errorRetryMin {
		t.Errorf("first failure should retry at %v, got %v", errorRetryMin, first)
	}
	if second <= first || third <= second {
		t.Errorf("retries should back off: %v, %v, %v", first, second, third)
	}
	if third > errorRetryMax {
		t.Errorf("backoff must cap at %v, got %v", errorRetryMax, third)
	}

	// Recovery resets the backoff.
	gw.status = http.StatusCreated
	gw.body = map[string]string{"pin": "active"}
	if next := h.beat(context.Background()); next != heartbeatInterval {
		t.Errorf("recovery should return to steady state, got %v", next)
	}
	if h.errorRetry != 0 {
		t.Error("recovery must reset the backoff")
	}
}

// --- proof of possession (B16) ---

func TestBeatProvesControl(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayCredentialsSecret, Namespace: gatewayCredentialsNamespace},
		Data:       map[string][]byte{gatewayCredentialsTokenKey: []byte("cluster-token")},
	})
	crClient := crfake.NewClientBuilder().WithScheme(scheme).Build()

	// Provision the hop cert so the heartbeat can sign, and grab its public key
	// so the fake gateway can verify like the real one (dialing IP:443).
	if _, err := hopcert.Ensure(context.Background(), client, crClient); err != nil {
		t.Fatal(err)
	}
	signKey, err := hopcert.SigningKey(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}

	const nonce = "0011223344556677"
	var proofVerified bool
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"pin": "active", "challenge": nonce})
	})
	mux.HandleFunc("/proof", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Subdomain, Token, Nonce, Signature string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		// The gateway verifies the signature against the key it observes at
		// the cluster IP — here, the provisioned hop cert's public key.
		ok := hopproof.Verify(&signKey.PublicKey, req.Nonce, req.Subdomain, "203.0.113.1", "kipper.run", req.Token, req.Signature)
		if req.Nonce != nonce {
			ok = false
		}
		if ok {
			proofVerified = true
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(map[string]bool{"proven": true})
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "signature does not verify"})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	h := &gatewayHeartbeat{
		url: ts.URL, subdomain: "myapp", host: "203.0.113.1",
		client: client, crClient: crClient, httpClient: ts.Client(),
	}

	next := h.beat(context.Background())
	if !proofVerified {
		t.Fatal("the heartbeat must sign the challenge and complete /proof")
	}
	if next != heartbeatInterval {
		t.Errorf("a completed proof is steady state: expected %v, got %v", heartbeatInterval, next)
	}
}

func TestBeatRetriesWhenProofRejected(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayCredentialsSecret, Namespace: gatewayCredentialsNamespace},
		Data:       map[string][]byte{gatewayCredentialsTokenKey: []byte("cluster-token")},
	})
	crClient := crfake.NewClientBuilder().WithScheme(scheme).Build()
	if _, err := hopcert.Ensure(context.Background(), client, crClient); err != nil {
		t.Fatal(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]string{"pin": "active", "challenge": "abcd"})
	})
	mux.HandleFunc("/proof", func(w http.ResponseWriter, r *http.Request) {
		// Cluster not yet observable → 202, retry.
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "retry"})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	h := &gatewayHeartbeat{
		url: ts.URL, subdomain: "myapp", host: "203.0.113.1",
		client: client, crClient: crClient, httpClient: ts.Client(),
	}
	if next := h.beat(context.Background()); next != convergeRetry {
		t.Errorf("a not-yet-accepted proof must retry soon, got %v", next)
	}
}

// A key rotation converges over two beats, and each beat proves the key that is
// live at the time it signs. The gateway binds routing to the proven key, so this
// ordering is what decides how long a rotating cluster stays unroutable: the first
// beat still proves the old key (Traefik is still serving it), and only the second
// beat asserts and proves the new one.
func TestBeatRotationProvesTheLiveKeyEachBeat(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayCredentialsSecret, Namespace: gatewayCredentialsNamespace},
		Data:       map[string][]byte{gatewayCredentialsTokenKey: []byte("cluster-token")},
	})
	crClient := crfake.NewClientBuilder().WithScheme(scheme).Build()
	if _, err := hopcert.Ensure(context.Background(), client, crClient); err != nil {
		t.Fatal(err)
	}
	oldKey, err := hopcert.SigningKey(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}

	// Request the rotation.
	secret, _ := client.CoreV1().Secrets(hopcert.Namespace).Get(context.Background(), hopcert.SecretName, metav1.GetOptions{})
	secret.Annotations = map[string]string{hopcert.RotateAnnotation: "requested"}
	if _, err := client.CoreV1().Secrets(hopcert.Namespace).Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	type proofCall struct{ nonce, signature, token string }
	var proofs []proofCall
	var asserted []string
	pin := "pending" // first beat: the gateway has not observed the candidate yet
	mux := http.NewServeMux()
	mux.HandleFunc("/register", func(w http.ResponseWriter, r *http.Request) {
		var req capturedRegister
		_ = json.NewDecoder(r.Body).Decode(&req)
		asserted = append(asserted, req.CertFingerprint)
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]string{"pin": pin, "challenge": "00112233445566" + pin[:2]})
	})
	mux.HandleFunc("/proof", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Subdomain, Token, Nonce, Signature string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		proofs = append(proofs, proofCall{req.Nonce, req.Signature, req.Token})
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]bool{"proven": true})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)

	h := &gatewayHeartbeat{
		url: ts.URL, subdomain: "myapp", host: "203.0.113.1",
		client: client, crClient: crClient, httpClient: ts.Client(),
	}

	// Beat one: assert the candidate, prove the still-live old key, then swap.
	h.beat(context.Background())
	newKey, err := hopcert.SigningKey(context.Background(), client)
	if err != nil {
		t.Fatal(err)
	}
	// Beat two: the candidate is live, so it is both asserted and proven.
	pin = "active"
	h.beat(context.Background())

	if len(proofs) != 2 || len(asserted) != 2 {
		t.Fatalf("expected two beats to register and prove, got %d proofs and %d assertions", len(proofs), len(asserted))
	}
	verifies := func(key *ecdsa.PublicKey, c proofCall) bool {
		return hopproof.Verify(key, c.nonce, "myapp", "203.0.113.1", "kipper.run", c.token, c.signature)
	}
	if !verifies(&oldKey.PublicKey, proofs[0]) {
		t.Error("the mid-rotation beat must prove the key Traefik still serves (the old one)")
	}
	if verifies(&newKey.PublicKey, proofs[0]) {
		t.Error("the mid-rotation beat cannot prove the candidate: it is not served yet")
	}
	if !verifies(&newKey.PublicKey, proofs[1]) {
		t.Error("the beat after the swap must prove the new live key, which is what restores routing")
	}
	if asserted[0] != asserted[1] {
		t.Errorf("both beats assert the rotated key: %q then %q", asserted[0], asserted[1])
	}
}

// The documented opt-out has to hold where the traffic starts, not only where
// the env is rendered: an operator who refuses registration should not depend on
// a reconcile having reached the Deployment first.
func TestHeartbeatRefusesToStartWhenRegistrationIsOptedOut(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	no := false
	ci := &kipperv1.ClusterIdentity{
		ObjectMeta: metav1.ObjectMeta{Name: clusterIdentityName},
		Spec: kipperv1.ClusterIdentitySpec{
			Domain:  "acme.kipper.run",
			Gateway: &kipperv1.GatewaySpec{KipperRunDomain: "acme.kipper.run", Register: &no},
		},
	}
	crClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(ci).Build()

	if !registrationOptedOut(context.Background(), crClient) {
		t.Error("register:false must read as an opt-out")
	}
	if gatewayRegistrationExpected(context.Background(), crClient) {
		t.Error("an opted-out cluster must not be warned about missing heartbeat config")
	}

	// Without the opt-out, the same cluster is expected to register.
	yes := true
	ci2 := ci.DeepCopy()
	ci2.Spec.Gateway.Register = &yes
	crClient2 := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(ci2).Build()
	if registrationOptedOut(context.Background(), crClient2) {
		t.Error("register:true is not an opt-out")
	}
	if !gatewayRegistrationExpected(context.Background(), crClient2) {
		t.Error("a registering cluster with no env must be warned")
	}
}

// A cluster with no gateway token can never prove possession, so under
// proof-before-route it will never be routed. Reporting "proof not pending" hid
// that entirely: the beat settled onto its slow interval and logged nothing, so
// a cluster that could not serve looked healthy indefinitely. That is how a
// broken install stayed invisible for hours.
func TestProveControlReportsThatItCannotTryWithoutAToken(t *testing.T) {
	h := &gatewayHeartbeat{subdomain: "myapp", host: "203.0.113.1"}

	if pending := h.proveControl(context.Background(), "", "some-nonce"); !pending {
		t.Error("with no token the proof is outstanding, not settled — the beat must keep retrying")
	}
}

// A gateway that issues no challenge to a token holder is an older gateway that
// wants no proof. Nothing is outstanding there, and treating it as pending
// would pin every cluster to the fast retry forever.
func TestProveControlSettlesWhenTheGatewayAsksForNoProof(t *testing.T) {
	h := &gatewayHeartbeat{subdomain: "myapp", host: "203.0.113.1"}

	if pending := h.proveControl(context.Background(), "a-token", ""); pending {
		t.Error("no challenge from a token holder means no proof is wanted")
	}
}
