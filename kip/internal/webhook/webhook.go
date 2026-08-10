package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

const (
	webhookSecretName = "-webhook"
	secretKeyField    = "token"
)

// Enable generates a webhook secret for an app and stores it as a Kubernetes Secret.
// Returns the generated token.
func Enable(ctx context.Context, client kubernetes.Interface, namespace, appName string) (string, error) {
	token, err := generateToken()
	if err != nil {
		return "", fmt.Errorf("generating webhook token: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName + webhookSecretName,
			Namespace: namespace,
			Labels: map[string]string{
				labels.App:       appName,
				labels.ManagedBy: labels.Kipper,
			},
		},
		Data: map[string][]byte{
			secretKeyField: []byte(token),
		},
	}

	_, err = client.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		existing, getErr := client.CoreV1().Secrets(namespace).Get(ctx, appName+webhookSecretName, metav1.GetOptions{})
		if getErr != nil {
			return "", getErr
		}
		existing.Data = secret.Data
		_, err = client.CoreV1().Secrets(namespace).Update(ctx, existing, metav1.UpdateOptions{})
	}

	if err != nil {
		return "", fmt.Errorf("storing webhook secret: %w", err)
	}

	return token, nil
}

// Disable removes the webhook secret for an app.
func Disable(ctx context.Context, client kubernetes.Interface, namespace, appName string) error {
	err := client.CoreV1().Secrets(namespace).Delete(ctx, appName+webhookSecretName, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	return err
}

// GetToken retrieves the webhook token for an app.
func GetToken(ctx context.Context, client kubernetes.Interface, namespace, appName string) (string, error) {
	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, appName+webhookSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return "", fmt.Errorf("no webhook configured for %s. Run 'kip app webhook enable %s'", appName, appName)
		}
		return "", fmt.Errorf("getting webhook secret: %w", err)
	}

	token, ok := secret.Data[secretKeyField]
	if !ok {
		return "", fmt.Errorf("webhook secret is corrupt. Re-run 'kip app webhook enable %s'", appName)
	}

	return string(token), nil
}

// VerifySignature checks a GitLab/GitHub webhook signature against the stored token.
func VerifySignature(payload []byte, signature, token string) bool {
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(payload)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
