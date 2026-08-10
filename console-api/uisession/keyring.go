// Package uisession issues and validates the per-host session artefacts that
// authenticate a browser to a Traefik-fronted service UI (MailHog, RabbitMQ,
// and so on). It replaces the domain-wide Dex ID-token cookie with a
// short-lived, single-host session cookie minted here, so a stolen cookie
// opens one UI as one operator for a bounded time and nothing else.
//
// It keeps its own HMAC signing keyring, modelled on the share-link keyring
// (a later refactor may extract the shared signing machinery; kept separate
// here to avoid destabilising the shipped share feature).
package uisession

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// SigningSecretName holds the HMAC keyring for UI-session artefacts.
	SigningSecretName      = "kipper-ui-session-signing" //nolint:gosec // Secret object name, not a credential
	SigningSecretNamespace = "kipper-system"

	slotCurrent  = "current"
	slotPrevious = "previous"
)

// Keyring is the parsed two-slot signing Secret. current signs new artefacts;
// previous keeps artefacts signed before the last rotation valid until they
// expire. Each slot is "<kid>:<base64 key>", so validation selects a key
// strictly by the artefact's kid header.
type Keyring struct {
	CurrentKID  string
	CurrentKey  []byte
	PreviousKID string
	PreviousKey []byte
}

// KeyFor returns the key matching kid, or nil when no slot carries it.
func (k *Keyring) KeyFor(kid string) []byte {
	switch kid {
	case "":
		return nil
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
		return "", fmt.Errorf("uisession: generating key id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func newKey() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, fmt.Errorf("uisession: generating signing key: %w", err)
	}
	return b, nil
}

func encodeSlot(kid string, key []byte) []byte {
	return []byte(kid + ":" + base64.StdEncoding.EncodeToString(key))
}

func decodeSlot(raw []byte) (kid string, key []byte, err error) {
	kid, encoded, found := strings.Cut(string(raw), ":")
	if !found || kid == "" {
		return "", nil, fmt.Errorf("uisession: malformed key slot")
	}
	key, err = base64.StdEncoding.DecodeString(encoded)
	// Exactly 256 bits: a truncated Secret must fail closed, never silently
	// downgrade the key strength.
	if err != nil || len(key) != 32 {
		return "", nil, fmt.Errorf("uisession: malformed key slot")
	}
	return kid, key, nil
}

func parseKeyring(data map[string][]byte) (*Keyring, error) {
	curKID, curKey, err := decodeSlot(data[slotCurrent])
	if err != nil {
		return nil, err
	}
	kr := &Keyring{CurrentKID: curKID, CurrentKey: curKey}
	if len(data[slotPrevious]) > 0 {
		prevKID, prevKey, perr := decodeSlot(data[slotPrevious])
		if perr != nil {
			return nil, perr
		}
		kr.PreviousKID, kr.PreviousKey = prevKID, prevKey
	}
	return kr, nil
}

func signingSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SigningSecretName,
			Namespace: SigningSecretNamespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
		},
		Type: corev1.SecretTypeOpaque,
		Data: data,
	}
}

// LoadOrCreateKeyring returns the signing keyring, creating a fresh one on
// first use. A corrupt Secret is replaced with a fresh keyring — the artefacts
// it signs are ephemeral (≤12h), so replacing it is equivalent to a bulk
// revocation and never loses durable state.
func LoadOrCreateKeyring(ctx context.Context, client kubernetes.Interface) (*Keyring, error) {
	var kr *Keyring
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secrets := client.CoreV1().Secrets(SigningSecretNamespace)
		secret, err := secrets.Get(ctx, SigningSecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			fresh, ferr := freshKeyringSecret()
			if ferr != nil {
				return ferr
			}
			if _, cerr := secrets.Create(ctx, fresh, metav1.CreateOptions{}); cerr != nil {
				if apierrors.IsAlreadyExists(cerr) {
					return apierrors.NewConflict(corev1.Resource("secrets"), SigningSecretName, cerr)
				}
				return fmt.Errorf("uisession: creating signing secret: %w", cerr)
			}
			kr, _ = parseKeyring(fresh.Data)
			return nil
		}
		if err != nil {
			return err
		}
		parsed, perr := parseKeyring(secret.Data)
		if perr != nil {
			// Corrupt: replace wholesale (equivalent to revoke-all; safe
			// because every artefact is ephemeral).
			return replaceKeyring(ctx, secrets, secret, &kr)
		}
		kr = parsed
		return nil
	})
	return kr, err
}

// ResetKeyring rotates to a brand-new current key and drops the previous slot,
// invalidating every outstanding cookie and code at once — the bulk-revocation
// lever.
func ResetKeyring(ctx context.Context, client kubernetes.Interface) error {
	secrets := client.CoreV1().Secrets(SigningSecretNamespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		secret, err := secrets.Get(ctx, SigningSecretName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			fresh, ferr := freshKeyringSecret()
			if ferr != nil {
				return ferr
			}
			_, cerr := secrets.Create(ctx, fresh, metav1.CreateOptions{})
			return cerr
		}
		if err != nil {
			return err
		}
		var kr *Keyring
		return replaceKeyring(ctx, secrets, secret, &kr)
	})
}

func freshKeyringSecret() (*corev1.Secret, error) {
	kid, err := newKID()
	if err != nil {
		return nil, err
	}
	key, err := newKey()
	if err != nil {
		return nil, err
	}
	return signingSecret(map[string][]byte{slotCurrent: encodeSlot(kid, key)}), nil
}

func replaceKeyring(ctx context.Context, secrets secretWriter, existing *corev1.Secret, out **Keyring) error {
	kid, err := newKID()
	if err != nil {
		return err
	}
	key, err := newKey()
	if err != nil {
		return err
	}
	existing.Data = map[string][]byte{slotCurrent: encodeSlot(kid, key)}
	if _, uerr := secrets.Update(ctx, existing, metav1.UpdateOptions{}); uerr != nil {
		return uerr
	}
	*out = &Keyring{CurrentKID: kid, CurrentKey: key}
	return nil
}

type secretWriter interface {
	Update(ctx context.Context, secret *corev1.Secret, opts metav1.UpdateOptions) (*corev1.Secret, error)
}
