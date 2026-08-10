package main

// gateway_heartbeat keeps the cluster's kipper.run subdomain alive and pins
// the gateway→cluster hop.
//
// The kipper.run gateway expires registrations after 30 days of inactivity.
// Without a heartbeat, every Kipper cluster using a kipper.run subdomain
// would lose its routing once a month — Dex auth would 404, every
// console-api--<x>.kipper.run URL would 404, and the only fix is to manually
// re-register.
//
// The heartbeat also carries the hop-certificate SPKI fingerprint (see
// internal/hopcert), authenticated by the gateway management token from the
// gateway-credentials Secret. The gateway pins that fingerprint and verifies
// every proxied handshake against it, closing the MITM window on the public
// gateway→cluster hop. The token rides WebPKI-verified TLS to kipper.run, so
// an on-path attacker between gateway and cluster can delay the pin but
// never poison it.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/hopcert"
	"github.com/getkipper/kipper/controller/pkg/hopproof"
)

const (
	gatewayURL              = "https://kipper.run"
	heartbeatInterval       = 24 * time.Hour
	heartbeatRequestTimeout = 10 * time.Second

	// convergeRetry re-runs a beat that is one step from steady state: a
	// pending pin awaiting observation, a freshly persisted token, or a
	// rotation mid-swap. Short, so pinning completes in minutes, not a day.
	convergeRetry = time.Minute
	// Failed beats back off exponentially between these bounds.
	errorRetryMin = time.Minute
	errorRetryMax = 30 * time.Minute

	gatewayCredentialsSecret    = "gateway-credentials"
	gatewayCredentialsNamespace = "kipper-system"
	gatewayCredentialsTokenKey  = "token"
)

// gatewayRegistrationExpected reports whether the ClusterIdentity says this
// cluster holds a *.kipper.run name. Read only to decide whether a missing
// heartbeat configuration is worth warning about, so any error means "cannot
// tell" and stays silent.
func gatewayRegistrationExpected(ctx context.Context, crClient crclient.Client) bool {
	var ci kipperv1.ClusterIdentity
	if err := crClient.Get(ctx, crclient.ObjectKey{Name: clusterIdentityName}, &ci); err != nil {
		return false
	}
	if ci.Spec.Gateway == nil {
		return strings.HasSuffix(ci.Spec.Domain, ".kipper.run")
	}
	return !registrationRefused(&ci)
}

// registrationRefused reports the CR's explicit opt-out (gateway.register:
// false). It is enforced where the traffic starts rather than only where the env
// is rendered, so an operator who turns registration off is not left depending on
// a reconcile having reached the Deployment first.
func registrationRefused(ci *kipperv1.ClusterIdentity) bool {
	g := ci.Spec.Gateway
	return g != nil && g.Register != nil && !*g.Register
}

// registrationOptedOut reads the CR purely to answer "has this cluster refused
// to register". An unreadable CR is not an opt-out: a cluster with working
// heartbeat config keeps beating rather than going quiet on an API blip.
func registrationOptedOut(ctx context.Context, crClient crclient.Client) bool {
	var ci kipperv1.ClusterIdentity
	if err := crClient.Get(ctx, crclient.ObjectKey{Name: clusterIdentityName}, &ci); err != nil {
		return false
	}
	return registrationRefused(&ci)
}

// clusterIdentityName is the singleton ClusterIdentity every cluster carries.
const clusterIdentityName = "cluster"

// gatewayHeartbeat carries the dependencies of the heartbeat loop, injectable
// for tests.
type gatewayHeartbeat struct {
	url        string
	subdomain  string
	host       string
	client     kubernetes.Interface
	crClient   crclient.Client
	httpClient *http.Client

	errorRetry       time.Duration
	warnedOldGateway bool
}

// gatewayRegisterResponse mirrors the gateway's /register response. An older
// gateway omits Pin entirely; its presence is the capability signal that pin
// assertions are understood.
type gatewayRegisterResponse struct {
	Token               string `json:"token"`
	Pin                 string `json:"pin"`
	AssertedFingerprint string `json:"assertedFingerprint"`
	ObservedFingerprint string `json:"observedFingerprint"`
	// Challenge is a proof-of-possession nonce (B16) the cluster signs with
	// the hop-cert private key and submits to /proof. Empty from an older
	// gateway, which wants no proof.
	Challenge string `json:"challenge"`
	Error     string `json:"error"`
}

// proofResponse mirrors the gateway's /proof reply.
type proofResponse struct {
	Proven bool   `json:"proven"`
	Error  string `json:"error"`
}

// proofOrigin is the gateway base domain bound into the signed proof message.
// Both sides must agree on it; the gateway uses its BASE_DOMAIN (kipper.run).
const proofOrigin = "kipper.run"

// startGatewayHeartbeat fires once at startup and then adaptively: daily in
// steady state, faster while converging on a pin. Reads the subdomain from
// KIPPER_RUN_DOMAIN and the host from CLUSTER_HOST, and does nothing without
// both (dev and local clusters, and clusters that use no kipper.run name at
// all) — but says so when anything indicates this cluster should be
// registering.
func startGatewayHeartbeat(ctx context.Context, client kubernetes.Interface, crClient crclient.Client) {
	rawDomain := os.Getenv("KIPPER_RUN_DOMAIN")
	host := os.Getenv("CLUSTER_HOST")
	if rawDomain == "" || host == "" {
		// Staying quiet here is what let a wiped CLUSTER_HOST go unnoticed: the
		// cluster kept serving while its gateway registration aged out, its hop
		// pin was never asserted, and it could never prove control. Say so
		// whenever anything suggests this cluster is supposed to register.
		if rawDomain != "" || host != "" || gatewayRegistrationExpected(ctx, crClient) {
			log.Printf("gateway heartbeat NOT running: KIPPER_RUN_DOMAIN=%q CLUSTER_HOST=%q. Both are required. "+
				"Set spec.gateway.kipperRunDomain and spec.gateway.clusterHost on the ClusterIdentity (or run kip upgrade) "+
				"or this cluster will never renew its kipper.run registration, assert its hop pin, or prove control of its IP",
				rawDomain, host)
		}
		return
	}
	if registrationOptedOut(ctx, crClient) {
		log.Printf("gateway heartbeat disabled: the ClusterIdentity sets gateway.register=false, so %s is not renewed from here", rawDomain)
		return
	}
	subdomain := strings.TrimSuffix(rawDomain, ".kipper.run")
	if subdomain == rawDomain || subdomain == "" {
		// Domain wasn't a kipper.run subdomain — nothing to heartbeat.
		return
	}

	h := &gatewayHeartbeat{
		url:        gatewayURL,
		subdomain:  subdomain,
		host:       host,
		client:     client,
		crClient:   crClient,
		httpClient: http.DefaultClient,
	}
	go h.run(ctx)
}

func (h *gatewayHeartbeat) run(ctx context.Context) {
	// Fire immediately so a freshly-rolled pod refreshes the lease and
	// asserts its pin without waiting up to 24h.
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			timer.Reset(h.beat(ctx))
		}
	}
}

// beat performs one heartbeat cycle and returns when the next should run.
func (h *gatewayHeartbeat) beat(ctx context.Context) time.Duration {
	assertFP, candidate, ensureOK := h.resolveFingerprint(ctx)

	// Read the token unconditionally: both the pin assertion and the proof of
	// possession need it. Its absence right after registration is transient
	// (the installer stores it), so retry soon.
	token, err := h.readToken(ctx)
	if err != nil {
		log.Printf("gateway heartbeat: reading gateway credentials: %v", err)
	}
	if token == "" {
		assertFP = ""
	}

	payload := map[string]string{"subdomain": h.subdomain, "ip": h.host}
	if token != "" {
		payload["token"] = token
	}
	if assertFP != "" {
		payload["certFingerprint"] = assertFP
	}

	var resp gatewayRegisterResponse
	status, rawBody, err := h.post(ctx, "/register", payload, &resp)
	if err != nil {
		log.Printf("gateway heartbeat: %v", err)
		return h.backoff()
	}
	if status != http.StatusCreated && status != http.StatusAccepted {
		// Most likely a 409 (the subdomain is registered to a different IP)
		// or a 403 (the stored token no longer matches the registration).
		// Surface the body so it's debuggable from console-api logs alone,
		// but don't crash; the cluster can keep serving on its custom domain.
		log.Printf("gateway heartbeat: HTTP %d %s", status, errText(resp.Error, rawBody))
		return h.backoff()
	}
	h.errorRetry = 0

	// A token in a renewal response means the gateway re-created the
	// registration (registry loss). That request was authenticated by
	// nothing, so the gateway ignored any fingerprint in it; persist the
	// fresh token and re-assert with it promptly.
	if resp.Token != "" {
		if err := h.persistToken(ctx, resp.Token); err != nil {
			log.Printf("gateway heartbeat: persisting re-issued gateway token: %v", err)
			return h.backoff()
		}
		log.Printf("gateway heartbeat: gateway re-created the registration for %s.kipper.run; stored the new token", h.subdomain)
		return convergeRetry
	}

	// Prove control of the IP (B16): sign the gateway's challenge with the hop
	// key. Until proof completes the gateway won't route this cluster once
	// proof-before-route is on, so retry soon if it hasn't converged yet.
	proofPending := h.proveControl(ctx, token, resp.Challenge)

	next := h.pinInterval(ctx, resp, assertFP, candidate, ensureOK)
	if proofPending && next > convergeRetry {
		next = convergeRetry
	}
	return next
}

// pinInterval runs the pin state machine and returns the pin-driven next
// interval (unchanged B5 behaviour).
func (h *gatewayHeartbeat) pinInterval(ctx context.Context, resp gatewayRegisterResponse, assertFP string, candidate, ensureOK bool) time.Duration {
	if assertFP == "" {
		log.Printf("gateway heartbeat: renewed %s.kipper.run", h.subdomain)
		if !ensureOK {
			return h.backoff()
		}
		// Renewed without a pin assertion (missing token or foreign store):
		// retry soon so the hop doesn't stay unpinned for a day.
		return convergeRetry
	}
	return h.handlePinResponse(ctx, resp, assertFP, candidate)
}

// proveControl signs the gateway's proof-of-possession challenge with the
// hop-cert private key and submits it to /proof. Reports whether proof is
// still pending (so the caller retries soon). No challenge or no token means
// nothing to prove this beat — not pending.
func (h *gatewayHeartbeat) proveControl(ctx context.Context, token, challenge string) bool {
	// No token means the cluster has nothing to prove possession with, and under
	// proof-before-route it will never be routed. Reporting "not pending" hid
	// that completely: the beat kept renewing on the slow interval and logged
	// nothing, so a cluster that could never serve looked healthy for as long as
	// anyone cared to watch. Say it, and keep the fast retry, because the token
	// can still arrive — the installer writes it moments after registration.
	if token == "" {
		log.Printf("gateway heartbeat: no gateway token on this cluster, so control of %s.kipper.run cannot be proven; it will not be routed until one is present", h.subdomain)
		return true
	}
	// A gateway that issues no challenge to a token holder wants no proof. That
	// is an older gateway, not a broken cluster, so nothing is pending.
	if challenge == "" {
		return false
	}
	key, err := hopcert.SigningKey(ctx, h.client)
	if err != nil {
		log.Printf("gateway heartbeat: loading hop signing key for proof: %v", err)
		return true
	}
	sig, err := hopproof.Sign(key, challenge, h.subdomain, h.host, proofOrigin, token)
	if err != nil {
		log.Printf("gateway heartbeat: signing proof challenge: %v", err)
		return true
	}
	var pr proofResponse
	status, rawBody, err := h.post(ctx, "/proof", map[string]string{
		"subdomain": h.subdomain, "token": token, "nonce": challenge, "signature": sig,
	}, &pr)
	if err != nil {
		log.Printf("gateway heartbeat: proof: %v", err)
		return true
	}
	if status == http.StatusOK {
		log.Printf("gateway heartbeat: proved control of %s.kipper.run", h.subdomain)
		return false
	}
	// 202 (cluster not yet reachable/converged) or 403/409 (stale challenge):
	// retry on the next beat.
	log.Printf("gateway heartbeat: proof for %s.kipper.run not yet accepted: HTTP %d %s", h.subdomain, status, errText(pr.Error, rawBody))
	return true
}

// errText prefers a decoded error message, falling back to the raw body.
func errText(decoded, raw string) string {
	if decoded != "" {
		return decoded
	}
	return raw
}

// resolveFingerprint runs the hop-cert reconciler and picks the fingerprint
// to assert: the staged rotation candidate when one exists, the live keypair
// otherwise. Returns "" when nothing may be asserted, and whether the
// reconciler itself succeeded.
func (h *gatewayHeartbeat) resolveFingerprint(ctx context.Context) (fp string, candidate, ensureOK bool) {
	st, err := hopcert.Ensure(ctx, h.client, h.crClient)
	if err != nil {
		log.Printf("gateway heartbeat: hop certificate: %v", err)
		return "", false, false
	}
	if len(st.ForeignStores) > 0 {
		// A competing default TLSStore means Traefik may serve a different
		// certificate than the one whose fingerprint would be asserted —
		// activating that pin could 502 the cluster, so refuse until the
		// conflict is gone.
		log.Printf("gateway heartbeat: refusing to assert the hop-cert fingerprint: foreign default TLSStore(s) %v would displace the pinned certificate; remove them to enable pinning", st.ForeignStores)
		return "", false, true
	}
	if st.CandidateFingerprint != "" {
		return st.CandidateFingerprint, true, true
	}
	return st.Fingerprint, false, true
}

// handlePinResponse drives the pin state machine from the gateway's answer.
func (h *gatewayHeartbeat) handlePinResponse(ctx context.Context, resp gatewayRegisterResponse, assertFP string, candidate bool) time.Duration {
	if resp.Pin == "" {
		if !h.warnedOldGateway {
			h.warnedOldGateway = true
			log.Printf("gateway heartbeat: the gateway does not support certificate pinning yet; the %s.kipper.run hop stays unpinned until it is upgraded", h.subdomain)
		}
		return heartbeatInterval
	}
	h.warnedOldGateway = false

	// For a staged rotation, any acknowledgement — pending or active — means
	// the candidate fingerprint is in the gateway's accepted set, so swapping
	// it live cannot 502. Only then does the candidate reach Traefik.
	if candidate {
		if err := hopcert.PromoteCandidate(ctx, h.client); err != nil {
			log.Printf("gateway heartbeat: promoting rotated hop certificate: %v", err)
			return h.backoff()
		}
		log.Printf("gateway heartbeat: gateway acknowledged the rotated hop key (%s); serving it now", resp.Pin)
		return convergeRetry
	}

	switch resp.Pin {
	case "active":
		log.Printf("gateway heartbeat: renewed %s.kipper.run (hop pin active)", h.subdomain)
		return heartbeatInterval
	case "pending":
		// Observation hasn't confirmed the asserted key yet: Traefik may
		// still be loading the TLSStore, or something on-path answered the
		// gateway's verify dial with a different certificate. Loud, and
		// retried soon — a real mismatch stays visible beat after beat.
		log.Printf("gateway heartbeat: hop pin for %s.kipper.run is pending: asserted SPKI %s, gateway observed %q",
			h.subdomain, assertFP, resp.ObservedFingerprint)
		return convergeRetry
	default:
		log.Printf("gateway heartbeat: unexpected pin state %q", resp.Pin)
		return h.backoff()
	}
}

// post sends a JSON payload to a gateway path and decodes the response into
// out (best effort). Returns the HTTP status and the trimmed raw body, so a
// caller can surface a non-JSON error body.
func (h *gatewayHeartbeat) post(ctx context.Context, path string, payload map[string]string, out any) (int, string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, "", fmt.Errorf("encoding request: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, heartbeatRequestTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, h.url+path, bytes.NewReader(body))
	if err != nil {
		return 0, "", fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	httpResp, err := h.httpClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = httpResp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(httpResp.Body, 4096))
	_ = json.Unmarshal(respBody, out)
	return httpResp.StatusCode, strings.TrimSpace(string(respBody)), nil
}

// backoff returns the next retry interval, doubling per consecutive failure.
func (h *gatewayHeartbeat) backoff() time.Duration {
	if h.errorRetry < errorRetryMin {
		h.errorRetry = errorRetryMin
	} else if h.errorRetry < errorRetryMax {
		h.errorRetry = min(h.errorRetry*2, errorRetryMax)
	}
	return h.errorRetry
}

func (h *gatewayHeartbeat) readToken(ctx context.Context) (string, error) {
	secret, err := h.client.CoreV1().Secrets(gatewayCredentialsNamespace).Get(ctx, gatewayCredentialsSecret, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(secret.Data[gatewayCredentialsTokenKey]), nil
}

func (h *gatewayHeartbeat) persistToken(ctx context.Context, token string) error {
	secrets := h.client.CoreV1().Secrets(gatewayCredentialsNamespace)
	secret, err := secrets.Get(ctx, gatewayCredentialsSecret, metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		_, createErr := secrets.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: gatewayCredentialsSecret, Namespace: gatewayCredentialsNamespace},
			Data:       map[string][]byte{gatewayCredentialsTokenKey: []byte(token)},
		}, metav1.CreateOptions{})
		return createErr
	}
	if err != nil {
		return err
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[gatewayCredentialsTokenKey] = []byte(token)
	_, err = secrets.Update(ctx, secret, metav1.UpdateOptions{})
	return err
}
