package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// The console API looks invites up by this key, so the format must match its
// inviteKey.
func TestInviteKeyIsTheSHA256OfTheToken(t *testing.T) {
	assert.Equal(t, "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824", inviteKey("hello"))
}

// Reading the ConfigMap must not let anyone redeem the invite, so only a hash
// of the token is stored.
func TestKipInviteStoresOnlyAHashOfTheToken(t *testing.T) {
	cs := fake.NewClientset()
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" //nolint:gosec // G101: a made-up invite token

	for range 2 {
		require.NoError(t, storeInviteEntry(context.Background(), cs, token, inviteEntry{Role: "viewer", Expires: "2099-01-01T00:00:00Z", Email: "new@example.com"}))
	}

	cm, err := cs.CoreV1().ConfigMaps("kipper-system").Get(context.Background(), "kipper-invites", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, cm.Data["invites"], token)
	assert.Contains(t, cm.Data["invites"], inviteKey(token))
	assert.Contains(t, cm.Data["invites"], `"prefix":"01234567"`)
}

// An invite stored before tokens were hashed is found by its token field, so
// adding a new invite must leave that field in place.
func TestKipInviteKeepsEarlierInvitesAsTheyAre(t *testing.T) {
	legacy := `{"legacy-token-value":{"token":"legacy-token-value","role":"viewer","expires":"2099-01-01T00:00:00Z","email":"old@example.com"}}`
	cs := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-invites", Namespace: "kipper-system"},
		Data:       map[string]string{"invites": legacy},
	})

	require.NoError(t, storeInviteEntry(context.Background(), cs, "new-token-value", inviteEntry{Role: "viewer", Expires: "2099-01-01T00:00:00Z", Email: "new@example.com"}))

	cm, err := cs.CoreV1().ConfigMaps("kipper-system").Get(context.Background(), "kipper-invites", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, cm.Data["invites"], `"token":"legacy-token-value"`)
}
