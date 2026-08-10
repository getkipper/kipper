package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

func TestEnableCreatesWebhookSecret(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	ctx := context.Background()

	token, err := Enable(ctx, client, "default", "api")
	require.NoError(t, err)
	assert.NotEmpty(t, token)
	assert.Len(t, token, 64)

	secret, err := client.CoreV1().Secrets("default").Get(ctx, "api-webhook", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, token, string(secret.Data["token"]))
	assert.Equal(t, "api", secret.Labels["app"])
	assert.Equal(t, labels.Kipper, secret.Labels[labels.ManagedBy])
}

func TestEnableRegeneratesToken(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	ctx := context.Background()

	token1, _ := Enable(ctx, client, "default", "api")
	token2, _ := Enable(ctx, client, "default", "api")

	assert.NotEqual(t, token1, token2)
}

func TestDisableRemovesSecret(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	ctx := context.Background()

	_, _ = Enable(ctx, client, "default", "api")
	err := Disable(ctx, client, "default", "api")
	assert.NoError(t, err)

	_, err = client.CoreV1().Secrets("default").Get(ctx, "api-webhook", metav1.GetOptions{})
	assert.Error(t, err)
}

func TestDisableNoOpWhenNotConfigured(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	ctx := context.Background()

	err := Disable(ctx, client, "default", "nonexistent")
	assert.NoError(t, err)
}

func TestGetTokenReturnsToken(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	ctx := context.Background()

	token, _ := Enable(ctx, client, "default", "api")

	got, err := GetToken(ctx, client, "default", "api")
	require.NoError(t, err)
	assert.Equal(t, token, got)
}

func TestGetTokenErrorsWhenNotConfigured(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck
	ctx := context.Background()

	_, err := GetToken(ctx, client, "default", "api")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no webhook configured")
}

func TestVerifySignatureValid(t *testing.T) {
	payload := []byte(`{"image":"test:v1"}`)
	token := "mysecrettoken"

	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	assert.True(t, VerifySignature(payload, sig, token))
}

func TestVerifySignatureInvalid(t *testing.T) {
	payload := []byte(`{"image":"test:v1"}`)
	assert.False(t, VerifySignature(payload, "sha256=invalid", "mysecret"))
}

func TestVerifySignatureTamperedPayload(t *testing.T) {
	payload := []byte(`{"image":"test:v1"}`)
	token := "mysecrettoken"

	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(payload)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	tampered := []byte(`{"image":"evil:v1"}`)
	assert.False(t, VerifySignature(tampered, sig, token))
}
