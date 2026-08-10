package share

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

// The signing Secret holds two kid-named slots. current signs new tokens;
// previous keeps links signed before the last rotation valid until they
// expire. Each slot value is "<kid>:<base64 key>", so validation can select
// a key strictly by the token's kid header instead of trying keys blindly.
const (
	slotCurrent  = "current"
	slotPrevious = "previous"
)

// Keyring is the parsed signing Secret.
type Keyring struct {
	CurrentKID  string
	CurrentKey  []byte
	PreviousKID string
	PreviousKey []byte
}

// KeyFor returns the key matching kid, or nil when no slot carries it.
func (k *Keyring) KeyFor(kid string) []byte {
	if kid == "" {
		return nil
	}
	switch kid {
	case k.CurrentKID:
		return k.CurrentKey
	case k.PreviousKID:
		return k.PreviousKey
	}
	return nil
}

func newKID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("share: generating key id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func newKey() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("share: generating signing key: %w", err)
	}
	return b, nil
}

func encodeSlot(kid string, key []byte) []byte {
	return []byte(kid + ":" + base64.StdEncoding.EncodeToString(key))
}

func decodeSlot(raw []byte) (kid string, key []byte, err error) {
	kid, encoded, found := strings.Cut(string(raw), ":")
	if !found || kid == "" {
		return "", nil, fmt.Errorf("share: malformed key slot")
	}
	key, err = base64.StdEncoding.DecodeString(encoded)
	// Exactly 256 bits: a truncated Secret must fail closed, never
	// silently downgrade the key strength.
	if err != nil || len(key) != 32 {
		return "", nil, fmt.Errorf("share: malformed key slot")
	}
	return kid, key, nil
}

// LoadOrCreateKeyring returns the signing keyring, creating a fresh one on
// first use. A legacy v0 Secret (single "key" field) is migrated in place:
// its key becomes current under a generated kid, so tokens signed by the
// migrated key stay valid while v0's kid-less tokens age out.
func LoadOrCreateKeyring(ctx context.Context, client kubernetes.Interface) (*Keyring, error) {
	var kr *Keyring
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secrets := client.CoreV1().Secrets(SigningSecretNamespace)
		secret, err := secrets.Get(ctx, SigningSecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			kid, kerr := newKID()
			if kerr != nil {
				return kerr
			}
			key, kerr := newKey()
			if kerr != nil {
				return kerr
			}
			fresh := signingSecret(map[string][]byte{slotCurrent: encodeSlot(kid, key)})
			if _, cerr := secrets.Create(ctx, fresh, metav1.CreateOptions{}); cerr != nil {
				if apierrors.IsAlreadyExists(cerr) {
					// Lost the create race; re-read the winner's keyring.
					return apierrors.NewConflict(metav1.SchemeGroupVersion.WithResource("secrets").GroupResource(), SigningSecretName, cerr)
				}
				return fmt.Errorf("share: creating signing secret: %w", cerr)
			}
			kr = &Keyring{CurrentKID: kid, CurrentKey: key}
			return nil
		}
		if err != nil {
			return err
		}

		if len(secret.Data[slotCurrent]) == 0 {
			return fmt.Errorf("share: signing secret %s/%s has no %q slot; delete it and mint a fresh link", SigningSecretNamespace, SigningSecretName, slotCurrent)
		}

		parsed, perr := parseKeyring(secret.Data)
		if perr != nil {
			return perr
		}
		kr = parsed
		return nil
	})
	return kr, err
}

// RotateKeyring demotes current to previous and installs a fresh current
// key. Tokens signed with the demoted key stay valid until they expire or
// the next rotation; two rotations retire a key entirely.
func RotateKeyring(ctx context.Context, client kubernetes.Interface) (*Keyring, error) {
	if _, err := LoadOrCreateKeyring(ctx, client); err != nil {
		return nil, err
	}
	var kr *Keyring
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secrets := client.CoreV1().Secrets(SigningSecretNamespace)
		secret, err := secrets.Get(ctx, SigningSecretName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		current := secret.Data[slotCurrent]
		if len(current) == 0 {
			return fmt.Errorf("share: signing secret has no current key to rotate")
		}
		kid, kerr := newKID()
		if kerr != nil {
			return kerr
		}
		key, kerr := newKey()
		if kerr != nil {
			return kerr
		}
		secret.Data[slotPrevious] = current
		secret.Data[slotCurrent] = encodeSlot(kid, key)
		if _, uerr := secrets.Update(ctx, secret, metav1.UpdateOptions{}); uerr != nil {
			return uerr
		}
		parsed, perr := parseKeyring(secret.Data)
		if perr != nil {
			return perr
		}
		kr = parsed
		return nil
	})
	return kr, err
}

func parseKeyring(data map[string][]byte) (*Keyring, error) {
	kid, key, err := decodeSlot(data[slotCurrent])
	if err != nil {
		return nil, fmt.Errorf("share: signing secret current slot: %w", err)
	}
	kr := &Keyring{CurrentKID: kid, CurrentKey: key}
	if prev := data[slotPrevious]; len(prev) > 0 {
		pkid, pkey, perr := decodeSlot(prev)
		if perr != nil {
			return nil, fmt.Errorf("share: signing secret previous slot: %w", perr)
		}
		// A duplicate kid would let the current slot shadow the previous
		// key in KeyFor — fail closed rather than guess.
		if pkid == kid {
			return nil, fmt.Errorf("share: signing secret slots share a key id")
		}
		kr.PreviousKID = pkid
		kr.PreviousKey = pkey
	}
	return kr, nil
}
