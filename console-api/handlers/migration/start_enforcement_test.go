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
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/getkipper/kipper/console-api/middleware"
)

func postStart(t *testing.T, h *Handler, body map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migration/start", bytes.NewReader(payload))
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, planClaims("admin@example.com")))
	rec := httptest.NewRecorder()
	h.StartHandler(rec, req)
	return rec
}

// A direct API start without a plan receipt is refused: the mandatory plan
// screen is server-enforced, not UI decoration.
func TestStartRefusedWithoutPlanReceipt(t *testing.T) {
	h := &Handler{Sessions: NewSessionStore(), StepUp: acceptAnyStepUp}
	_, tokenStr := testToken(t, "https://api.target.example.com")

	rec := postStart(t, h, map[string]interface{}{
		"token":     tokenStr,
		"projects":  []string{"shop"},
		"totp_code": "123456",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	// Case-insensitive: the sentence the message opens with is not the point,
	if !strings.Contains(strings.ToLower(rec.Body.String()), "review the plan") {
		t.Fatalf("refusal must point at the plan, got: %s", rec.Body.String())
	}
}

// The kill switch refuses before anything else runs.
func TestStartRefusedByKillSwitch(t *testing.T) {
	t.Setenv("KIPPER_DISABLE_OUTBOUND_MIGRATION", "1")
	h := &Handler{Sessions: NewSessionStore(), StepUp: acceptAnyStepUp}
	rec := postStart(t, h, map[string]interface{}{
		"token":    "irrelevant",
		"projects": []string{"shop"},
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "KIPPER_DISABLE_OUTBOUND_MIGRATION") {
		t.Fatalf("refusal must name the control, got: %s", rec.Body.String())
	}
}

// startTestTarget serves the non-consuming target endpoints a start's plan
// recomputation needs: roomy capacity and an empty project list.
func startTestTarget(onAccept func()) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/accept"):
			if onAccept != nil {
				onAccept()
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"session_id": "t1", "target_version": "dev"})
		case strings.HasSuffix(r.URL.Path, "/projects"):
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{})
		default:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"allocatable_cpu_millis": 8000.0, "allocatable_memory_bytes": 1 << 34,
				"allocatable_storage_bytes": 1 << 40, "requested_cpu_millis": 0.0,
				"requested_memory_bytes": 0.0, "requested_storage_bytes": 0.0,
				"target_version": "dev",
			})
		}
	}))
}

// issueMatchingReceipt issues a receipt whose digest matches the live plan,
// the way the plan endpoint would.
func issueMatchingReceipt(t *testing.T, h *Handler, claims *middleware.Claims, token *Token, projects, overwrites []string) string {
	t.Helper()
	current := h.buildPlan(context.Background(), claims, token, projects, overwrites, nil, false)
	receipt, err := h.issueReceipt(planReceipt{
		User:       receiptUser(claims),
		TokenFP:    tokenFingerprint(token),
		Projects:   canonicalProjects(projects),
		Overwrites: canonicalProjects(overwrites),
		Digest:     planDigest(current),
		ExpiresAt:  time.Now().Add(planReceiptTTL),
	})
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

// Without a wired verifier the start refuses — fail closed. The failure
// happens after the receipt but before any consuming call.
func TestStartFailsClosedWithoutStepUp(t *testing.T) {
	target := startTestTarget(nil)
	defer target.Close()

	h := &Handler{
		Client:   fakeClientWithProject(t),
		CRClient: crFakeWithWorkloads(t),
		Sessions: NewSessionStore(),
	}
	token, tokenStr := testToken(t, target.URL)
	claims := planClaims("admin@example.com")
	receipt := issueMatchingReceipt(t, h, claims, token, []string{"shop"}, nil)

	rec := postStart(t, h, map[string]interface{}{
		"token":        tokenStr,
		"projects":     []string{"shop"},
		"plan_receipt": receipt,
		"totp_code":    "123456",
	})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", rec.Code, rec.Body.String())
	}
}

// A refused TOTP code stops the start after the prechecks, without touching
// the target's token.
func TestStartRefusesBadCode(t *testing.T) {
	accepted := false
	target := startTestTarget(func() { accepted = true })
	defer target.Close()

	h := &Handler{
		Client:   fakeClientWithProject(t),
		CRClient: crFakeWithWorkloads(t),
		Sessions: NewSessionStore(),
		StepUp: func(ctx context.Context, claims *middleware.Claims, code, operation string) error {
			return fmt.Errorf("invalid 2FA code")
		},
	}
	token, tokenStr := testToken(t, target.URL)
	claims := planClaims("admin@example.com")
	receipt := issueMatchingReceipt(t, h, claims, token, []string{"shop"}, nil)

	rec := postStart(t, h, map[string]interface{}{
		"token":        tokenStr,
		"projects":     []string{"shop"},
		"plan_receipt": receipt,
		"totp_code":    "000000",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403: %s", rec.Code, rec.Body.String())
	}
	if accepted {
		t.Fatal("a refused code must never reach the target accept call")
	}
}

// A receipt whose digest no longer matches the live report is refused: the
// operator approved a plan that no longer describes what would happen.
func TestStartRefusedWhenPlanChanged(t *testing.T) {
	target := startTestTarget(nil)
	defer target.Close()

	h := &Handler{
		Client:   fakeClientWithProject(t),
		CRClient: crFakeWithWorkloads(t),
		Sessions: NewSessionStore(),
		StepUp:   acceptAnyStepUp,
	}
	token, tokenStr := testToken(t, target.URL)
	claims := planClaims("admin@example.com")
	receipt, _ := h.issueReceipt(planReceipt{
		User:      receiptUser(claims),
		TokenFP:   tokenFingerprint(token),
		Projects:  canonicalProjects([]string{"shop"}),
		Digest:    "stale-digest",
		ExpiresAt: time.Now().Add(planReceiptTTL),
	})

	rec := postStart(t, h, map[string]interface{}{
		"token":        tokenStr,
		"projects":     []string{"shop"},
		"plan_receipt": receipt,
		"totp_code":    "123456",
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "plan changed") {
		t.Fatalf("refusal must say the plan changed, got: %s", rec.Body.String())
	}
	// The unspent receipt still validates: the operator reviews a fresh plan
	// but a transient mismatch never burns anything.
	if err := h.validateReceipt(receipt, claims, token, []string{"shop"}, nil); err != nil {
		t.Fatalf("receipt must survive a digest mismatch: %v", err)
	}
}

// A precheck failure (target unreachable) leaves the receipt intact.
func TestStartPrecheckFailureKeepsReceipt(t *testing.T) {
	target := startTestTarget(nil)
	h := &Handler{
		Client:   fakeClientWithProject(t),
		CRClient: crFakeWithWorkloads(t),
		Sessions: NewSessionStore(),
		StepUp:   acceptAnyStepUp,
	}
	token, tokenStr := testToken(t, target.URL)
	claims := planClaims("admin@example.com")
	receipt := issueMatchingReceipt(t, h, claims, token, []string{"shop"}, nil)

	// The target goes away between plan and start: recomputation reports a
	// blocker and refuses.
	target.Close()
	rec := postStart(t, h, map[string]interface{}{
		"token":        tokenStr,
		"projects":     []string{"shop"},
		"plan_receipt": receipt,
		"totp_code":    "123456",
	})
	if rec.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412: %s", rec.Code, rec.Body.String())
	}
	if err := h.validateReceipt(receipt, claims, token, []string{"shop"}, nil); err != nil {
		t.Fatalf("receipt must survive a precheck failure: %v", err)
	}
}

// Plaintext endpoints are refused at decode, except loopback for local
// development.
func TestDecodeTokenRequiresHTTPS(t *testing.T) {
	mint := func(endpoint string) string {
		payload, _ := json.Marshal(Token{ //nolint:gosec // minting a test migration token is the point
			Endpoint: endpoint, Secret: "s", Cluster: "c", BaseDomain: "target.example.com", Expires: time.Now().Add(time.Hour),
		})
		return base64.StdEncoding.EncodeToString(payload)
	}

	if _, err := DecodeToken(mint("http://api.target.example.com")); err == nil {
		t.Fatal("plaintext public endpoint must be refused")
	}
	if _, err := DecodeToken(mint("https://api.target.example.com")); err != nil {
		t.Fatalf("https endpoint refused: %v", err)
	}
	if _, err := DecodeToken(mint("http://127.0.0.1:8080")); err != nil {
		t.Fatalf("loopback endpoint refused: %v", err)
	}
}

// TestAcceptIdempotentRetry covers the lost-response case: a second accept
// carrying the same idempotency key and secret returns the already-created
// session instead of failing on the consumed token.
func TestAcceptIdempotentRetry(t *testing.T) {
	h := &Handler{
		Client:   fake.NewSimpleClientset(migrationToken(t)),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).Build(),
		Sessions: NewSessionStore(),
	}

	body := map[string]interface{}{
		"secret":          "valid-secret",
		"source_cluster":  "source.example.com",
		"projects":        []string{"shop"},
		"idempotency_key": "receipt-abc",
	}
	first := postAccept(t, h, body)
	if first.Code != http.StatusOK {
		t.Fatalf("first accept returned %d: %s", first.Code, first.Body.String())
	}
	var firstResp map[string]interface{}
	_ = json.Unmarshal(first.Body.Bytes(), &firstResp)

	// The token is consumed now; the retry must still succeed via the key.
	retry := postAccept(t, h, body)
	if retry.Code != http.StatusOK {
		t.Fatalf("idempotent retry returned %d: %s", retry.Code, retry.Body.String())
	}
	var retryResp map[string]interface{}
	_ = json.Unmarshal(retry.Body.Bytes(), &retryResp)
	if firstResp["session_id"] != retryResp["session_id"] {
		t.Fatalf("retry returned session %v, want %v", retryResp["session_id"], firstResp["session_id"])
	}

	// The key alone must not authenticate: a wrong secret is refused.
	body["secret"] = "wrong-secret"
	if rec := postAccept(t, h, body); rec.Code == http.StatusOK {
		t.Fatal("an idempotent retry with the wrong secret must be refused")
	}
}

// The build gate refuses a cutover before the step-up verifier runs, so a
// pending build never burns a code.
func TestCutoverBuildGateRunsBeforeStepUp(t *testing.T) {
	stepUpCalled := false
	target := targetWithApps(
		`{"apps":[{"name":"web","namespace":"shop-prod","has_git":true,"build_phase":"Building"}]}`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"created"}`))
		})
	defer target.Close()

	h := &Handler{Sessions: NewSessionStore(), StepUp: func(ctx context.Context, claims *middleware.Claims, code, operation string) error {
		stepUpCalled = true
		return nil
	}}
	h.Sessions.Put(cutoverSession(target.URL))
	rec := serveCutover(t, h, `{"totp_code":"123456"}`)
	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", rec.Code, rec.Body.String())
	}
	if stepUpCalled {
		t.Fatal("a build-gate refusal must not spend a TOTP code")
	}
}
