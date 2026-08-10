package twofa

import (
	"bytes"
	"context"
	"encoding/json"
	stderrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	ctrltwofa "github.com/getkipper/kipper/controller/pkg/twofa"

	"github.com/getkipper/kipper/console-api/middleware"
)

//nolint:unparam // the email parameter keeps each test self-describing
func testClaims(email string) *middleware.Claims {
	return &middleware.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:  "https://dex.example.com/dex",
			Subject: "CgVhZG1pbhIFbG9jYWw",
		},
		Email: email,
	}
}

func newTestHandler(t *testing.T) *Handler {
	t.Helper()
	return NewHandler(&Store{Client: fake.NewSimpleClientset()}, "console.example.com", nil)
}

// seedBootstrap writes the host-issued bootstrap Secret the way kip does.
func seedBootstrap(t *testing.T, h *Handler, email, code string) {
	t.Helper()
	_, err := h.Store.Client.CoreV1().Secrets(factorNamespace).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      ctrltwofa.SecretName(email),
			Namespace: factorNamespace,
		},
		Data: map[string][]byte{
			"code":    []byte(code),
			"expires": []byte(time.Now().Add(ctrltwofa.BootstrapTTL).Format(time.RFC3339)),
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seeding bootstrap secret: %v", err)
	}
}

func doRequest(h http.HandlerFunc, claims *middleware.Claims, body interface{}) *httptest.ResponseRecorder {
	payload, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(payload))
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, claims))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

// enrollAndConfirm drives the full happy path and returns the current TOTP
// secret so tests can mint valid codes.
func enrollAndConfirm(t *testing.T, h *Handler, claims *middleware.Claims) []byte {
	t.Helper()
	seedBootstrap(t, h, claims.Email, "HOST-CODE-1234")

	rec := doRequest(h.EnrollHandler, claims, map[string]string{"bootstrap_code": "HOST-CODE-1234"})
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll returned %d: %s", rec.Code, rec.Body.String())
	}

	key, _ := userKey(claims)
	f, _, err := h.Store.get(context.Background(), key)
	if err != nil {
		t.Fatalf("reading pending factor: %v", err)
	}

	code := totpCode(f.Secret, nowCounter())
	rec = doRequest(h.ConfirmHandler, claims, map[string]string{"code": code})
	if rec.Code != http.StatusOK {
		t.Fatalf("confirm returned %d: %s", rec.Code, rec.Body.String())
	}
	return f.Secret
}

func TestEnrollRequiresBootstrapCode(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")

	rec := doRequest(h.EnrollHandler, claims, map[string]string{"bootstrap_code": "WRONG"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("enroll without a seeded code returned %d, want 403", rec.Code)
	}

	seedBootstrap(t, h, claims.Email, "HOST-CODE-1234")
	rec = doRequest(h.EnrollHandler, claims, map[string]string{"bootstrap_code": "WRONG"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("enroll with a wrong code returned %d, want 403", rec.Code)
	}
	rec = doRequest(h.EnrollHandler, claims, map[string]string{"bootstrap_code": "HOST-CODE-1234"})
	if rec.Code != http.StatusOK {
		t.Fatalf("enroll with the right code returned %d: %s", rec.Code, rec.Body.String())
	}
}

func TestBootstrapCodeIsSingleUse(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	seedBootstrap(t, h, claims.Email, "HOST-CODE-1234")

	if rec := doRequest(h.EnrollHandler, claims, map[string]string{"bootstrap_code": "HOST-CODE-1234"}); rec.Code != http.StatusOK {
		t.Fatalf("first enroll returned %d", rec.Code)
	}
	// A second enrollment cannot reuse the consumed code, and the pending
	// factor blocks a new enrollment anyway.
	if rec := doRequest(h.EnrollHandler, claims, map[string]string{"bootstrap_code": "HOST-CODE-1234"}); rec.Code == http.StatusOK {
		t.Fatal("second enroll with a consumed code must fail")
	}
}

func TestExpiredBootstrapCodeRejected(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	_, err := h.Store.Client.CoreV1().Secrets(factorNamespace).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ctrltwofa.SecretName(claims.Email), Namespace: factorNamespace},
		Data: map[string][]byte{
			"code":    []byte("HOST-CODE-1234"),
			"expires": []byte(time.Now().Add(-time.Minute).Format(time.RFC3339)),
		},
	}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}

	rec := doRequest(h.EnrollHandler, claims, map[string]string{"bootstrap_code": "HOST-CODE-1234"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expired code returned %d, want 403", rec.Code)
	}
}

func TestEnrollCannotReplaceActiveFactor(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	enrollAndConfirm(t, h, claims)

	seedBootstrap(t, h, claims.Email, "SECOND-CODE")
	rec := doRequest(h.EnrollHandler, claims, map[string]string{"bootstrap_code": "SECOND-CODE"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("enroll over an active factor returned %d, want 409", rec.Code)
	}
}

func TestConfirmStartsAgeClock(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	enrollAndConfirm(t, h, claims)

	key, _ := userKey(claims)
	f, _, err := h.Store.get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if f.State != factorStateActive {
		t.Fatalf("factor state = %s, want active", f.State)
	}
	if f.EnrolledAt.IsZero() || time.Since(f.EnrolledAt) > time.Minute {
		t.Fatalf("EnrolledAt must be stamped at confirmation, got %v", f.EnrolledAt)
	}
	if len(f.RecoveryCodes) != recoveryCodeCount {
		t.Fatalf("stored %d recovery hashes, want %d", len(f.RecoveryCodes), recoveryCodeCount)
	}
}

func TestVerifyStepUpEnforcesAgeGate(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	secret := enrollAndConfirm(t, h, claims)

	code := totpCode(secret, nowCounter()+1)
	err := h.VerifyStepUp(context.Background(), claims, code, "migration start")
	if err == nil {
		t.Fatal("a factor younger than the age gate must be refused")
	}
	var notEligible ErrFactorNotEligible
	if !stderrors.As(err, &notEligible) {
		t.Fatalf("want ErrFactorNotEligible, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "eligible on") {
		t.Fatalf("refusal must name the eligibility date, got: %v", err)
	}
}

func TestVerifyStepUpAcceptsMatureFactor(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	secret := enrollAndConfirm(t, h, claims)
	backdateFactor(t, h, claims, 8*24*time.Hour)

	code := totpCode(secret, nowCounter()+1)
	if err := h.VerifyStepUp(context.Background(), claims, code, "migration start"); err != nil {
		t.Fatalf("mature factor with valid code refused: %v", err)
	}
}

func TestVerifyStepUpRefusesReplay(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	secret := enrollAndConfirm(t, h, claims)
	backdateFactor(t, h, claims, 8*24*time.Hour)

	code := totpCode(secret, nowCounter()+1)
	if err := h.VerifyStepUp(context.Background(), claims, code, "migration start"); err != nil {
		t.Fatalf("first use refused: %v", err)
	}
	if err := h.VerifyStepUp(context.Background(), claims, code, "migration start"); err == nil {
		t.Fatal("the same code must never authorise two operations")
	}
}

// TestVerifyStepUpConcurrentSameCode races many verifications of one code;
// the compare-and-swap must let exactly one through.
func TestVerifyStepUpConcurrentSameCode(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	secret := enrollAndConfirm(t, h, claims)
	backdateFactor(t, h, claims, 8*24*time.Hour)

	code := totpCode(secret, nowCounter()+1)
	const racers = 8
	var wg sync.WaitGroup
	successes := make(chan struct{}, racers)
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.VerifyStepUp(context.Background(), claims, code, "migration start"); err == nil {
				successes <- struct{}{}
			}
		}()
	}
	wg.Wait()
	close(successes)
	won := 0
	for range successes {
		won++
	}
	if won != 1 {
		t.Fatalf("%d concurrent verifications of one code succeeded, want exactly 1", won)
	}
}

func TestVerifyStepUpRateLimits(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	enrollAndConfirm(t, h, claims)
	backdateFactor(t, h, claims, 8*24*time.Hour)

	for i := 0; i < rateLimitMax; i++ {
		if err := h.VerifyStepUp(context.Background(), claims, "000000", "migration start"); err == nil {
			t.Fatal("wrong code must be refused")
		}
	}
	err := h.VerifyStepUp(context.Background(), claims, "000000", "migration start")
	if err == nil || !strings.Contains(err.Error(), "too many failed attempts") {
		t.Fatalf("want lockout after %d failures, got: %v", rateLimitMax, err)
	}
}

func TestVerifyStepUpRequiresEnrollment(t *testing.T) {
	h := newTestHandler(t)
	err := h.VerifyStepUp(context.Background(), testClaims("admin@example.com"), "123456", "migration start")
	if err == nil || !strings.Contains(err.Error(), "no 2FA factor enrolled") {
		t.Fatalf("unenrolled user must be refused with guidance, got: %v", err)
	}
}

func TestResetRestartsAgeClock(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	secret := enrollAndConfirm(t, h, claims)
	backdateFactor(t, h, claims, 8*24*time.Hour)

	code := totpCode(secret, nowCounter()+1)
	rec := doRequest(h.ResetHandler, claims, map[string]string{"code": code})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset returned %d: %s", rec.Code, rec.Body.String())
	}

	key, _ := userKey(claims)
	f, _, err := h.Store.get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	if f.State != factorStatePending {
		t.Fatalf("reset factor state = %s, want pending", f.State)
	}
	if !f.EnrolledAt.IsZero() {
		t.Fatal("reset must clear EnrolledAt — the age clock restarts")
	}
	if bytes.Equal(f.Secret, secret) {
		t.Fatal("reset must rotate the TOTP secret")
	}
}

func TestResetWithRecoveryCode(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	seedBootstrap(t, h, claims.Email, "HOST-CODE-1234")

	rec := doRequest(h.EnrollHandler, claims, map[string]string{"bootstrap_code": "HOST-CODE-1234"})
	if rec.Code != http.StatusOK {
		t.Fatal("enroll failed")
	}
	key, _ := userKey(claims)
	f, _, _ := h.Store.get(context.Background(), key)
	code := totpCode(f.Secret, nowCounter())
	rec = doRequest(h.ConfirmHandler, claims, map[string]string{"code": code})
	if rec.Code != http.StatusOK {
		t.Fatal("confirm failed")
	}
	var confirmResp struct {
		RecoveryCodes []string `json:"recovery_codes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &confirmResp); err != nil || len(confirmResp.RecoveryCodes) == 0 {
		t.Fatalf("confirm returned no recovery codes: %v", err)
	}

	rec = doRequest(h.ResetHandler, claims, map[string]string{"recovery_code": confirmResp.RecoveryCodes[0]})
	if rec.Code != http.StatusOK {
		t.Fatalf("reset with recovery code returned %d: %s", rec.Code, rec.Body.String())
	}

	// The consumed recovery code and its siblings die with the old factor.
	rec = doRequest(h.ResetHandler, claims, map[string]string{"recovery_code": confirmResp.RecoveryCodes[1]})
	if rec.Code == http.StatusOK {
		t.Fatal("recovery codes must not survive a reset")
	}
}

func TestStatusReportsEligibility(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, claims))
	rec := httptest.NewRecorder()
	h.StatusHandler(rec, req)
	var resp map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["state"] != "absent" {
		t.Fatalf("unenrolled state = %v, want absent", resp["state"])
	}

	enrollAndConfirm(t, h, claims)
	rec = httptest.NewRecorder()
	h.StatusHandler(rec, req)
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["state"] != "active" || resp["eligible"] != false {
		t.Fatalf("fresh factor status = %v, want active and ineligible", resp)
	}
}

func TestUserKeyRejectsMissingIdentity(t *testing.T) {
	if _, err := userKey(nil); err == nil {
		t.Fatal("nil claims must be rejected")
	}
	if _, err := userKey(&middleware.Claims{Email: "admin@example.com"}); err == nil {
		t.Fatal("claims without issuer/subject must be rejected")
	}
}

// nowCounter is the current TOTP counter for minting valid test codes.
func nowCounter() uint64 {
	return uint64(time.Now().Unix()) / 30 //nolint:gosec // G115: Unix time is positive for all relevant dates
}

// backdateFactor ages an active factor past the gate.
//
//nolint:unparam // the duration parameter keeps the gate distance visible at the call site
func backdateFactor(t *testing.T, h *Handler, claims *middleware.Claims, age time.Duration) {
	t.Helper()
	key, _ := userKey(claims)
	f, secret, err := h.Store.get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	f.EnrolledAt = time.Now().UTC().Add(-age)
	if err := h.Store.update(context.Background(), secret, f); err != nil {
		t.Fatal(err)
	}
}

// A wrong confirmation code voids the whole pending enrollment: the
// recovery codes go to whoever confirms, so nobody gets to guess.
func TestConfirmSingleAttempt(t *testing.T) {
	h := newTestHandler(t)
	claims := testClaims("admin@example.com")
	seedBootstrap(t, h, claims.Email, "HOST-CODE-1234")
	if rec := doRequest(h.EnrollHandler, claims, map[string]string{"bootstrap_code": "HOST-CODE-1234"}); rec.Code != http.StatusOK {
		t.Fatal("enroll failed")
	}

	rec := doRequest(h.ConfirmHandler, claims, map[string]string{"code": "000000"})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("wrong code returned %d, want 403", rec.Code)
	}

	// The pending enrollment is gone; even the right code cannot confirm it.
	key, _ := userKey(claims)
	if _, _, err := h.Store.get(context.Background(), key); err == nil {
		t.Fatal("a failed confirmation must void the pending enrollment")
	}
	rec = doRequest(h.ConfirmHandler, claims, map[string]string{"code": "123456"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("confirm after voiding returned %d, want 404", rec.Code)
	}
}

// The age-gate override parses fail-closed: garbage and negatives mean the
// default, never zero.
func TestMinFactorAgeFailsClosed(t *testing.T) {
	cases := map[string]time.Duration{
		"":       7 * 24 * time.Hour,
		"banana": 7 * 24 * time.Hour,
		"-3":     7 * 24 * time.Hour,
		"3":      3 * 24 * time.Hour,
		"0":      0,
	}
	for value, want := range cases {
		t.Setenv(envMinAgeDays, value)
		if got := MinFactorAge(); got != want {
			t.Errorf("MinFactorAge with %q = %v, want %v", value, got, want)
		}
	}
}

// Recovery codes carry 128 bits each.
func TestRecoveryCodeStrength(t *testing.T) {
	plain, hashes, err := newRecoveryCodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != recoveryCodeCount || len(hashes) != recoveryCodeCount {
		t.Fatalf("got %d/%d codes, want %d", len(plain), len(hashes), recoveryCodeCount)
	}
	for _, code := range plain {
		stripped := strings.ReplaceAll(code, "-", "")
		if len(stripped) != 26 {
			t.Fatalf("code %q carries %d base32 chars, want 26 (128 bits)", code, len(stripped))
		}
	}
}
