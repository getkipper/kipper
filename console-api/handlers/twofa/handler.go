package twofa

import (
	"context"
	"encoding/base32"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"

	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/security"
)

// Handler exposes the 2FA lifecycle endpoints.
type Handler struct {
	Store *Store
	// Issuer is the display name authenticator apps show, usually the
	// cluster's console hostname.
	Issuer string
	// Security records factor lifecycle events. Enrolling or resetting a
	// factor is a precursor to migration abuse, so both alert as loudly as a
	// migration itself.
	Security *security.Notifier

	limiter *rateLimiter
}

// NewHandler wires the 2FA endpoints around a store.
func NewHandler(store *Store, issuer string, notifier *security.Notifier) *Handler {
	return &Handler{Store: store, Issuer: issuer, Security: notifier, limiter: newRateLimiter()}
}

func (h *Handler) emit(ctx context.Context, e security.Event) {
	if h.Security != nil {
		h.Security.Emit(ctx, e)
	}
}

// identity resolves the caller's storage key and claims, rejecting tokens
// without the issuer/subject pair the key derives from.
func identity(r *http.Request) (*middleware.Claims, string, error) {
	claims := middleware.UserFromContext(r.Context())
	key, err := userKey(claims)
	if err != nil {
		return nil, "", err
	}
	return claims, key, nil
}

// EnrollHandler starts an enrollment. The bootstrap code — issued at host
// level via `kip 2fa bootstrap` — is what authorises it, never the JWT alone.
// POST /api/v1/auth/2fa/enroll
func (h *Handler) EnrollHandler(w http.ResponseWriter, r *http.Request) {
	claims, key, err := identity(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var req struct {
		BootstrapCode string `json:"bootstrap_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.BootstrapCode == "" {
		respondError(w, http.StatusBadRequest, "bootstrap_code is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if !h.limiter.allowed("bootstrap/" + key) {
		respondError(w, http.StatusTooManyRequests, "too many failed attempts — wait a few minutes")
		return
	}
	if err := h.Store.consumeBootstrapCode(ctx, claims.Email, req.BootstrapCode); err != nil {
		if h.limiter.recordFailure("bootstrap/" + key) {
			h.emit(ctx, security.Event{
				Kind:    "twofa_lockout",
				User:    claims.Email,
				Summary: fmt.Sprintf("repeated failed 2FA bootstrap attempts for %s", claims.Email),
			})
		}
		respondError(w, http.StatusForbidden, err.Error())
		return
	}
	h.limiter.reset("bootstrap/" + key)

	secret, err := newTOTPSecret()
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	f := &factor{
		State:     factorStatePending,
		Secret:    secret,
		Issuer:    claims.Issuer,
		Subject:   claims.Subject,
		Email:     claims.Email,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.Store.createPending(ctx, key, f); err != nil {
		respondError(w, http.StatusConflict, err.Error())
		return
	}

	h.emit(ctx, security.Event{
		Kind:    "twofa_enrollment_started",
		User:    claims.Email,
		Summary: fmt.Sprintf("2FA enrollment started for %s", claims.Email),
	})

	respondJSON(w, http.StatusOK, map[string]string{
		"otpauth_uri": otpauthURI(h.Issuer, claims.Email, secret),
		"secret":      base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(secret),
	})
}

// ConfirmHandler proves possession of the enrolled device and activates the
// factor. The age-gate clock starts here — at possession — never at enroll.
// POST /api/v1/auth/2fa/confirm
func (h *Handler) ConfirmHandler(w http.ResponseWriter, r *http.Request) {
	claims, key, err := identity(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Code == "" {
		respondError(w, http.StatusBadRequest, "code is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	defer h.Store.lock(key)()

	f, secret, err := h.Store.get(ctx, key)
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "no enrollment in progress")
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if f.State != factorStatePending {
		respondError(w, http.StatusConflict, "the factor is already active — use reset to rotate it")
		return
	}
	if time.Since(f.CreatedAt) > pendingTTL {
		respondError(w, http.StatusGone, "the enrollment has expired — start again")
		return
	}

	counter, ok := matchCode(f.Secret, req.Code, time.Now(), f.LastCounter)
	if !ok {
		// Single-attempt: a wrong code voids the whole enrollment, so
		// nobody gets to guess at a pending factor — the recovery codes go
		// to whoever confirms it. Restarting costs a fresh bootstrap code.
		if delErr := h.Store.deletePending(ctx, key, secret); delErr != nil {
			respondError(w, http.StatusConflict, delErr.Error())
			return
		}
		h.emit(ctx, security.Event{
			Kind:    "twofa_confirm_failed",
			User:    claims.Email,
			Summary: fmt.Sprintf("2FA confirmation failed for %s — the pending enrollment was voided", claims.Email),
		})
		respondError(w, http.StatusForbidden, "wrong code — the enrollment is void, start again with a new code from the host")
		return
	}
	h.limiter.reset(key)

	plainCodes, hashes, err := newRecoveryCodes()
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	now := time.Now().UTC()
	f.State = factorStateActive
	f.EnrolledAt = now
	f.LastCounter = counter
	f.RecoveryCodes = hashes
	if err := h.Store.update(ctx, secret, f); err != nil {
		respondError(w, http.StatusConflict, err.Error())
		return
	}

	eligibleAt := now.Add(MinFactorAge())
	h.emit(ctx, security.Event{
		Kind:    "twofa_enrolled",
		User:    claims.Email,
		Summary: fmt.Sprintf("2FA factor enrolled for %s — if this was not them, their account is compromised", claims.Email),
		Fields: []security.Field{
			{Key: "migration_eligible_at", Value: eligibleAt.Format(time.RFC3339)},
			{Key: "action_if_unexpected", Value: "this factor can start a cluster migration once eligible — reset it and the account credentials now"},
		},
	})

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "active",
		"recovery_codes": plainCodes,
		"enrolled_at":    now.Format(time.RFC3339),
		"eligible_at":    eligibleAt.Format(time.RFC3339),
	})
}

// StatusHandler reports the caller's factor state and when it becomes
// eligible to authorise a migration.
// GET /api/v1/auth/2fa/status
func (h *Handler) StatusHandler(w http.ResponseWriter, r *http.Request) {
	_, key, err := identity(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	minAge := MinFactorAge()
	resp := map[string]interface{}{
		"state":        "absent",
		"min_age_days": int(minAge.Hours() / 24),
	}

	f, _, err := h.Store.get(ctx, key)
	if err == nil {
		switch f.State {
		case factorStatePending:
			if time.Since(f.CreatedAt) > pendingTTL {
				resp["state"] = "absent"
			} else {
				resp["state"] = "pending"
			}
		case factorStateActive:
			eligibleAt := f.EnrolledAt.Add(minAge)
			resp["state"] = "active"
			resp["enrolled_at"] = f.EnrolledAt.Format(time.RFC3339)
			resp["eligible_at"] = eligibleAt.Format(time.RFC3339)
			resp["eligible"] = !time.Now().Before(eligibleAt)
		}
	} else if !errors.IsNotFound(err) {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, resp)
}

// ResetHandler rotates the factor: a valid current code or a recovery code
// authorises replacing it with a fresh pending enrollment. The age clock
// restarts — a rotated factor is a new factor, or rotation becomes the
// age-gate bypass. Without either code, recovery is host-level: delete the
// factor Secret, then re-enroll with a new bootstrap code.
// POST /api/v1/auth/2fa/reset
func (h *Handler) ResetHandler(w http.ResponseWriter, r *http.Request) {
	claims, key, err := identity(r)
	if err != nil {
		respondError(w, http.StatusUnauthorized, err.Error())
		return
	}

	var req struct {
		Code         string `json:"code,omitempty"`
		RecoveryCode string `json:"recovery_code,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || (req.Code == "" && req.RecoveryCode == "") {
		respondError(w, http.StatusBadRequest, "code or recovery_code is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	defer h.Store.lock(key)()

	f, secret, err := h.Store.get(ctx, key)
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "no 2FA factor enrolled")
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if f.State != factorStateActive {
		respondError(w, http.StatusConflict, "no active factor to reset — confirm or restart the pending enrollment")
		return
	}

	if !h.limiter.allowed(key) {
		respondError(w, http.StatusTooManyRequests, "too many failed attempts — wait a few minutes")
		return
	}

	authorised := false
	usedRecovery := false
	if req.Code != "" {
		if counter, ok := matchCode(f.Secret, req.Code, time.Now(), f.LastCounter); ok {
			f.LastCounter = counter
			authorised = true
		}
	} else {
		if remaining, ok := consumeRecoveryCode(f.RecoveryCodes, req.RecoveryCode); ok {
			f.RecoveryCodes = remaining
			authorised = true
			usedRecovery = true
		}
	}
	if !authorised {
		if h.limiter.recordFailure(key) {
			h.emit(ctx, security.Event{
				Kind:    "twofa_lockout",
				User:    claims.Email,
				Summary: fmt.Sprintf("repeated failed 2FA reset attempts for %s", claims.Email),
			})
		}
		respondError(w, http.StatusForbidden, "invalid code")
		return
	}
	h.limiter.reset(key)

	newSecret, err := newTOTPSecret()
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	replacement := &factor{
		State:     factorStatePending,
		Secret:    newSecret,
		Issuer:    claims.Issuer,
		Subject:   claims.Subject,
		Email:     claims.Email,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.Store.update(ctx, secret, replacement); err != nil {
		respondError(w, http.StatusConflict, err.Error())
		return
	}

	h.emit(ctx, security.Event{
		Kind:    "twofa_reset",
		User:    claims.Email,
		Summary: fmt.Sprintf("2FA factor reset for %s — the old factor and recovery codes are void", claims.Email),
		Fields: []security.Field{
			{Key: "via_recovery_code", Value: fmt.Sprintf("%t", usedRecovery)},
		},
	})

	respondJSON(w, http.StatusOK, map[string]string{
		"otpauth_uri": otpauthURI(h.Issuer, claims.Email, newSecret),
		"secret":      base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(newSecret),
	})
}

// StatusFor reports a user's factor state for callers outside the HTTP
// surface — the migration plan renders it as a blocker row with the
// eligibility date.
func (h *Handler) StatusFor(ctx context.Context, claims *middleware.Claims) (state string, eligibleAt time.Time, eligible bool, err error) {
	key, keyErr := userKey(claims)
	if keyErr != nil {
		return "", time.Time{}, false, keyErr
	}
	f, _, getErr := h.Store.get(ctx, key)
	if getErr != nil {
		if errors.IsNotFound(getErr) {
			return "absent", time.Time{}, false, nil
		}
		return "", time.Time{}, false, getErr
	}
	switch f.State {
	case factorStatePending:
		if time.Since(f.CreatedAt) > pendingTTL {
			return "absent", time.Time{}, false, nil
		}
		return "pending", time.Time{}, false, nil
	case factorStateActive:
		at := f.EnrolledAt.Add(MinFactorAge())
		return "active", at, !time.Now().Before(at), nil
	}
	return "absent", time.Time{}, false, nil
}

// ErrFactorNotEligible marks step-up failures whose message is safe to show
// the operator (age gate, missing factor). Typed so callers can distinguish
// policy refusals from transport errors.
type ErrFactorNotEligible struct{ msg string }

func (e ErrFactorNotEligible) Error() string { return e.msg }

// VerifyStepUp checks a TOTP code for a destructive operation and atomically
// claims its counter. Nothing is consumed on policy failures — only a
// matched, claimed counter mutates state, and the compare-and-swap retry
// ensures two concurrent requests can never both spend the same code.
func (h *Handler) VerifyStepUp(ctx context.Context, claims *middleware.Claims, code, operation string) error {
	key, err := userKey(claims)
	if err != nil {
		return ErrFactorNotEligible{msg: err.Error()}
	}
	if code == "" {
		return ErrFactorNotEligible{msg: "a 2FA code is required for this operation"}
	}

	defer h.Store.lock(key)()

	for attempt := 0; attempt < 3; attempt++ {
		f, secret, err := h.Store.get(ctx, key)
		if err != nil {
			if errors.IsNotFound(err) {
				return ErrFactorNotEligible{msg: "no 2FA factor enrolled — enroll one in Settings first"}
			}
			return fmt.Errorf("reading 2FA factor: %w", err)
		}
		if f.State != factorStateActive {
			return ErrFactorNotEligible{msg: "the 2FA enrollment is unconfirmed — finish it in Settings first"}
		}
		minAge := MinFactorAge()
		if eligibleAt := f.EnrolledAt.Add(minAge); time.Now().Before(eligibleAt) {
			h.emit(ctx, security.Event{
				Kind:    "twofa_young_factor_refused",
				User:    claims.Email,
				Summary: fmt.Sprintf("%s attempt by %s refused: 2FA factor is younger than the required %d days", operation, claims.Email, int(minAge.Hours()/24)),
				Fields: []security.Field{
					{Key: "eligible_at", Value: eligibleAt.Format(time.RFC3339)},
				},
			})
			return ErrFactorNotEligible{msg: fmt.Sprintf(
				"the 2FA factor is too new for this operation — it becomes eligible on %s (host operators can adjust %s)",
				eligibleAt.Format("2 January 2006 15:04 MST"), envMinAgeDays)}
		}

		if !h.limiter.allowed(key) {
			return ErrFactorNotEligible{msg: "too many failed attempts — wait a few minutes"}
		}
		counter, ok := matchCode(f.Secret, code, time.Now(), f.LastCounter)
		if !ok {
			if h.limiter.recordFailure(key) {
				h.emit(ctx, security.Event{
					Kind:    "twofa_lockout",
					User:    claims.Email,
					Summary: fmt.Sprintf("repeated failed 2FA attempts by %s during %s", claims.Email, operation),
				})
			}
			return ErrFactorNotEligible{msg: "invalid 2FA code"}
		}

		f.LastCounter = counter
		if err := h.Store.update(ctx, secret, f); err != nil {
			// A concurrent claim won the compare-and-swap. Re-read: if it
			// spent this same counter, the re-verify rejects the code.
			continue
		}
		h.limiter.reset(key)
		return nil
	}
	return fmt.Errorf("2FA state changed concurrently — try again")
}

func respondJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func respondError(w http.ResponseWriter, status int, message string) {
	respondJSON(w, status, map[string]string{"error": message})
}
