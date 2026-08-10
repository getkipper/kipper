package twofa

import (
	"context"
	"crypto/subtle"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ctrltwofa "github.com/getkipper/kipper/controller/pkg/twofa"
)

// First enrollment is authorised by a one-time bootstrap code issued at host
// level (`kip 2fa bootstrap <email>`), never by the JWT alone. A stolen admin
// session could otherwise mint the attacker's own factor and simply wait out
// the age gate. The code lives in a Secret only host-level access can create;
// possession of it proves exactly the trust class that outranks a stolen JWT.
// The Secret contract (name, fields, TTL) is shared with the kip CLI via
// controller/pkg/twofa.

// consumeBootstrapCode validates a presented code for the email and consumes
// it single-use, using the same precondition-delete pattern as the migration
// token: two concurrent enrollments cannot both spend one code.
func (s *Store) consumeBootstrapCode(ctx context.Context, email, presented string) error {
	name := ctrltwofa.SecretName(email)
	secret, err := s.Client.CoreV1().Secrets(factorNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("no enrollment code found — ask a host operator to run: kip 2fa bootstrap %s", email)
		}
		return fmt.Errorf("reading enrollment code: %w", err)
	}

	stored := string(secret.Data["code"])
	expiresRaw := string(secret.Data["expires"])
	expires, parseErr := time.Parse(time.RFC3339, expiresRaw)
	if stored == "" || parseErr != nil || time.Now().After(expires) {
		return fmt.Errorf("the enrollment code has expired — ask a host operator to issue a new one")
	}
	if subtle.ConstantTimeCompare([]byte(stored), []byte(strings.TrimSpace(presented))) != 1 {
		return fmt.Errorf("invalid enrollment code")
	}

	err = s.Client.CoreV1().Secrets(factorNamespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &secret.UID, ResourceVersion: &secret.ResourceVersion},
	})
	if err != nil {
		if errors.IsNotFound(err) || errors.IsConflict(err) {
			return fmt.Errorf("enrollment code already used")
		}
		return fmt.Errorf("consuming enrollment code: %w", err)
	}
	return nil
}
