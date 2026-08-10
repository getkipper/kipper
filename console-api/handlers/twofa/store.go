package twofa

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/middleware"
)

const (
	factorNamespace    = "kipper-system"
	factorSecretPrefix = "kipper-2fa-" //nolint:gosec // k8s Secret name prefix, not a credential
	factorLabel        = "kipper.run/twofa"

	// factorStatePending exists between enroll and confirm; only confirm
	// promotes it. A pending factor authorises nothing.
	factorStatePending = "pending"
	// factorStateActive is a confirmed factor. Its enrolled_at timestamp is
	// set at confirmation time — the age gate must count from the moment the
	// human proved possession, or enrolling early and confirming late would
	// skip the gate entirely.
	factorStateActive = "active"

	// pendingTTL bounds how long an unconfirmed enrollment stays claimable.
	pendingTTL = 15 * time.Minute

	// recoveryCodeCount is how many one-time recovery codes a confirmation
	// issues.
	recoveryCodeCount = 8

	// envMinAgeDays overrides the factor age a migration requires. Host-level
	// only: it lives in the deployment environment, out of reach of a console
	// session.
	envMinAgeDays     = "KIPPER_MIGRATION_2FA_MIN_AGE_DAYS"
	defaultMinAgeDays = 7
)

// factor is the stored state for one user's TOTP factor.
type factor struct {
	State         string    `json:"state"`
	Secret        []byte    `json:"secret"`
	Issuer        string    `json:"issuer"`
	Subject       string    `json:"subject"`
	Email         string    `json:"email"`
	CreatedAt     time.Time `json:"created_at"`
	EnrolledAt    time.Time `json:"enrolled_at,omitempty"`
	LastCounter   uint64    `json:"last_counter,omitempty"`
	RecoveryCodes []string  `json:"recovery_codes,omitempty"` // bcrypt hashes
}

// Store persists TOTP factors, one Kubernetes Secret per user. Separate
// Secrets keep concurrent enrollments by different admins from clobbering
// each other. Per-user transitions serialise on an in-process lock — the
// console-api runs as a single replica, so this is the real mutual
// exclusion — and are additionally compare-and-swapped on the Secret's
// resourceVersion, which covers the brief two-pod overlap of a rolling
// restart.
type Store struct {
	Client kubernetes.Interface

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

// lock serialises all factor transitions for one user and returns the
// unlock. Every read-verify-write sequence must hold it, or two concurrent
// requests could both spend the same code.
func (s *Store) lock(key string) func() {
	s.mu.Lock()
	if s.locks == nil {
		s.locks = make(map[string]*sync.Mutex)
	}
	l, ok := s.locks[key]
	if !ok {
		l = &sync.Mutex{}
		s.locks[key] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

// userKey derives the stable storage identity from the JWT issuer and
// subject. Email is display metadata only: it can change or collide across
// connectors, while (iss, sub) is the identity Dex actually guarantees.
func userKey(claims *middleware.Claims) (string, error) {
	if claims == nil || claims.Issuer == "" || claims.Subject == "" {
		return "", fmt.Errorf("token carries no issuer or subject")
	}
	sum := sha256.Sum256([]byte(claims.Issuer + "\x00" + claims.Subject))
	return hex.EncodeToString(sum[:8]), nil
}

func factorSecretName(key string) string {
	return factorSecretPrefix + key
}

// get loads a user's factor. Returns the Secret alongside so callers can
// compare-and-swap updates against its resourceVersion.
func (s *Store) get(ctx context.Context, key string) (*factor, *corev1.Secret, error) {
	secret, err := s.Client.CoreV1().Secrets(factorNamespace).Get(ctx, factorSecretName(key), metav1.GetOptions{})
	if err != nil {
		return nil, nil, err
	}
	var f factor
	if err := json.Unmarshal(secret.Data["factor"], &f); err != nil {
		return nil, nil, fmt.Errorf("corrupt 2FA factor record: %w", err)
	}
	return &f, secret, nil
}

// createPending writes a fresh pending factor. Fails if any factor already
// exists for the user: an active factor must never be replaced by enroll
// (rotation goes through reset), and a live pending enrollment is only
// replaced once it has expired.
func (s *Store) createPending(ctx context.Context, key string, f *factor) error {
	existing, secret, err := s.get(ctx, key)
	if err == nil {
		if existing.State == factorStateActive {
			return fmt.Errorf("a 2FA factor is already enrolled. Use reset to rotate it")
		}
		if time.Since(existing.CreatedAt) < pendingTTL {
			return fmt.Errorf("an enrollment is already in progress. Confirm it or retry after it expires")
		}
		// Expired pending enrollment: replace it via CAS so two concurrent
		// enrolls cannot both claim the slot.
		return s.update(ctx, secret, f)
	}
	if !errors.IsNotFound(err) {
		return err
	}

	payload, err := json.Marshal(f) //nolint:gosec // persisting the TOTP secret into a Kubernetes Secret is the point of this store
	if err != nil {
		return err
	}
	_, err = s.Client.CoreV1().Secrets(factorNamespace).Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      factorSecretName(key),
			Namespace: factorNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				factorLabel:                    "true",
			},
		},
		Data: map[string][]byte{"factor": payload},
	}, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		return fmt.Errorf("an enrollment is already in progress. Confirm it or retry after it expires")
	}
	return err
}

// update compare-and-swaps the factor payload onto the exact Secret revision
// the caller read. A conflict means a concurrent transition won; callers
// treat that as failure and re-read.
func (s *Store) update(ctx context.Context, secret *corev1.Secret, f *factor) error {
	payload, err := json.Marshal(f) //nolint:gosec // persisting the TOTP secret into a Kubernetes Secret is the point of this store
	if err != nil {
		return err
	}
	secret.Data["factor"] = payload
	_, err = s.Client.CoreV1().Secrets(factorNamespace).Update(ctx, secret, metav1.UpdateOptions{})
	if errors.IsConflict(err) {
		return fmt.Errorf("2FA state changed concurrently. Try again")
	}
	return err
}

// deletePending removes a pending enrollment under its revision
// precondition, so a concurrent confirmation cannot be voided retroactively.
func (s *Store) deletePending(ctx context.Context, key string, secret *corev1.Secret) error {
	err := s.Client.CoreV1().Secrets(factorNamespace).Delete(ctx, factorSecretName(key), metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion},
	})
	if errors.IsConflict(err) || errors.IsNotFound(err) {
		return fmt.Errorf("2FA state changed concurrently. Try again")
	}
	return err
}

// newTOTPSecret returns 20 random bytes, the RFC 4226 recommended length.
func newTOTPSecret() ([]byte, error) {
	secret := make([]byte, 20)
	if _, err := rand.Read(secret); err != nil {
		return nil, fmt.Errorf("generating TOTP secret: %w", err)
	}
	return secret, nil
}

// newRecoveryCodes generates the one-time codes handed to the user and the
// bcrypt hashes that get stored. 16 random bytes each — 128 bits, credential
// strength independent of the rate limit.
func newRecoveryCodes() (plain []string, hashes []string, err error) {
	enc := base32.StdEncoding.WithPadding(base32.NoPadding)
	for i := 0; i < recoveryCodeCount; i++ {
		raw := make([]byte, 16)
		if _, err := rand.Read(raw); err != nil {
			return nil, nil, fmt.Errorf("generating recovery code: %w", err)
		}
		code := enc.EncodeToString(raw) // 26 chars
		code = code[:5] + "-" + code[5:10] + "-" + code[10:15] + "-" + code[15:20] + "-" + code[20:]
		hash, err := bcrypt.GenerateFromPassword([]byte(code), bcrypt.DefaultCost)
		if err != nil {
			return nil, nil, fmt.Errorf("hashing recovery code: %w", err)
		}
		plain = append(plain, code)
		hashes = append(hashes, string(hash))
	}
	return plain, hashes, nil
}

// consumeRecoveryCode verifies a presented recovery code against the stored
// hashes and returns the remaining set with the matched hash removed. Every
// stored hash is always evaluated, so a miss costs the same as a hit.
func consumeRecoveryCode(hashes []string, presented string) ([]string, bool) {
	matchedIdx := -1
	for i, h := range hashes {
		if bcrypt.CompareHashAndPassword([]byte(h), []byte(presented)) == nil && matchedIdx == -1 {
			matchedIdx = i
		}
	}
	if matchedIdx == -1 {
		return hashes, false
	}
	remaining := append([]string(nil), hashes[:matchedIdx]...)
	return append(remaining, hashes[matchedIdx+1:]...), true
}

// MinFactorAge returns how old a confirmed factor must be before it can
// authorise a migration. Parsing fails closed: anything unparseable or
// negative means the 7-day default, never zero.
func MinFactorAge() time.Duration {
	raw := strings.TrimSpace(os.Getenv(envMinAgeDays))
	if raw == "" {
		return defaultMinAgeDays * 24 * time.Hour
	}
	days, err := strconv.Atoi(raw)
	if err != nil || days < 0 {
		log.Printf("SECURITY twofa: invalid %s value %q, using the %d-day default", envMinAgeDays, raw, defaultMinAgeDays)
		return defaultMinAgeDays * 24 * time.Hour
	}
	return time.Duration(days) * 24 * time.Hour
}

// WarnIfWeakened logs a persistent startup warning when the host has lowered
// the factor-age gate below the default. The override is legitimate host
// policy, but it must never be silent.
func WarnIfWeakened() {
	if MinFactorAge() < defaultMinAgeDays*24*time.Hour {
		log.Printf("SECURITY twofa: %s lowers the migration factor-age gate below %d days, freshly enrolled factors can start migrations sooner", envMinAgeDays, defaultMinAgeDays)
	}
}
