package share

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// SigningSecretName holds the single HS256 signing key. Deleting
	// this Secret is the v0 revocation mechanism: it invalidates every
	// live share token at once, and the next Mint recreates it.
	SigningSecretName      = "kipper-share-signing"
	SigningSecretNamespace = "kipper-system"
)

// randomID returns a 128-bit token id as lowercase hex. Hex keeps the id
// usable directly in a grant Secret's DNS-1123 name.
func randomID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("share: generating token id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// signingSecret builds the signing Secret object with the given data.
func signingSecret(data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SigningSecretName,
			Namespace: SigningSecretNamespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
		},
		Data: data,
	}
}
