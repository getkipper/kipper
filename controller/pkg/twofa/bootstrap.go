// Package twofa holds the enrollment-bootstrap contract shared between the
// kip CLI, which issues bootstrap codes at host level, and the console-api,
// which consumes them. First 2FA enrollment is authorised by possession of
// such a code rather than by the login session, so a stolen console JWT
// cannot enroll an attacker's factor.
package twofa

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// Namespace is where bootstrap Secrets live.
	Namespace = "kipper-system"

	secretPrefix = "kipper-2fa-bootstrap-" //nolint:gosec // k8s Secret name prefix, not a credential

	// BootstrapTTL bounds how long an issued code stays usable.
	BootstrapTTL = 15 * time.Minute
)

// SecretName returns the Secret name holding the bootstrap code for an email
// address. Hashing keeps the address out of object names, which land in logs
// and audit trails.
func SecretName(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return secretPrefix + hex.EncodeToString(sum[:8])
}

// NewCode generates a human-typeable one-time code: 80 random bits as
// grouped base32.
func NewCode() (string, error) {
	raw := make([]byte, 10)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generating bootstrap code: %w", err)
	}
	code := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(raw)
	return code[:4] + "-" + code[4:8] + "-" + code[8:12] + "-" + code[12:16], nil
}

// BuildSecret renders the Secret the kip CLI writes for one issued code. The
// code is stored as issued: whoever can read Secrets in kipper-system
// already outranks everything this ceremony protects.
func BuildSecret(email, code string, now time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SecretName(email),
			Namespace: Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				"kipper.run/twofa-bootstrap":   "true",
			},
		},
		Data: map[string][]byte{
			"code":    []byte(code),
			"email":   []byte(email),
			"expires": []byte(now.Add(BootstrapTTL).UTC().Format(time.RFC3339)),
		},
	}
}
