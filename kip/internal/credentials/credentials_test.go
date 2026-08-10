package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func seedGit(t *testing.T, client *fake.Clientset, entries []gitEntry) {
	t.Helper()
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	_, err = client.CoreV1().Secrets(systemNamespace).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: gitCredentialsConfigName, Namespace: systemNamespace},
		Data:       map[string][]byte{"credentials": data},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func seedRegistries(t *testing.T, client *fake.Clientset, entries []registryEntry) {
	t.Helper()
	data, err := json.Marshal(entries) //nolint:gosec // test fixture: marshals a struct with a Password field whose value is a fake test credential
	require.NoError(t, err)
	_, err = client.CoreV1().Secrets(systemNamespace).Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryConfigName, Namespace: systemNamespace},
		Data:       map[string][]byte{"registries": data},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func TestGetForApp(t *testing.T) {
	const ns = "acme-test"

	t.Run("returns the token from the app's namespace secret", func(t *testing.T) {
		client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
		_, err := client.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "acme-website-git-credentials", Namespace: ns},
			Data:       map[string][]byte{"token": []byte("github_pat_example")},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		cred, err := GetForApp(context.Background(), client, ns, "acme-website-git-credentials", "acme-website")
		require.NoError(t, err)
		assert.Equal(t, TypeGit, cred.Type)
		assert.Equal(t, "acme-website-git-credentials", cred.Name)
		assert.Equal(t, "github_pat_example", cred.Value)
	})

	t.Run("errors when the secret is missing", func(t *testing.T) {
		client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
		_, err := GetForApp(context.Background(), client, ns, "missing-git-credentials", "acme-website")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("errors when the secret has no token key", func(t *testing.T) {
		client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
		_, err := client.CoreV1().Secrets(ns).Create(context.Background(), &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "broken-git-credentials", Namespace: ns},
			Data:       map[string][]byte{"ssh-key": []byte("irrelevant")},
		}, metav1.CreateOptions{})
		require.NoError(t, err)

		_, err = GetForApp(context.Background(), client, ns, "broken-git-credentials", "acme-website")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no token key")
	})
}

func TestList_EmptyCluster(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	out, err := List(context.Background(), client, "")
	require.NoError(t, err)
	assert.Empty(t, out)
}

func TestList_BothTypes(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedGit(t, client, []gitEntry{
		{Name: "git-acme-tools", Server: "git.example.com", Username: "kipper-deploy", Token: "glpa-abc123"},
	})
	seedRegistries(t, client, []registryEntry{
		{Name: "ghcr-io", Server: "ghcr.io", Username: "acme", Password: "ghp_xyz789"},
	})

	out, err := List(context.Background(), client, "")
	require.NoError(t, err)
	require.Len(t, out, 2)

	byType := map[Type]Credential{}
	for _, c := range out {
		byType[c.Type] = c
	}
	assert.Equal(t, "glpa-abc123", byType[TypeGit].Value)
	assert.Equal(t, "kipper-deploy", byType[TypeGit].Username)
	assert.Equal(t, "ghp_xyz789", byType[TypeRegistry].Value)
	assert.Equal(t, "acme", byType[TypeRegistry].Username)
}

func TestList_TypeFilter(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedGit(t, client, []gitEntry{
		{Name: "git-acme-tools", Server: "git.example.com", Username: "u", Token: "t"},
	})
	seedRegistries(t, client, []registryEntry{
		{Name: "ghcr-io", Server: "ghcr.io", Username: "u", Password: "p"},
	})

	gits, err := List(context.Background(), client, TypeGit)
	require.NoError(t, err)
	require.Len(t, gits, 1)
	assert.Equal(t, TypeGit, gits[0].Type)

	regs, err := List(context.Background(), client, TypeRegistry)
	require.NoError(t, err)
	require.Len(t, regs, 1)
	assert.Equal(t, TypeRegistry, regs[0].Type)
}

func TestGet_GitOnly(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedGit(t, client, []gitEntry{
		{Name: "git-acme-tools", Server: "git.example.com", Username: "kipper-deploy", Token: "glpa-abc123"},
	})

	cred, err := Get(context.Background(), client, "git-acme-tools", "")
	require.NoError(t, err)
	assert.Equal(t, TypeGit, cred.Type)
	assert.Equal(t, "glpa-abc123", cred.Value)
}

func TestGet_RegistryOnly(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedRegistries(t, client, []registryEntry{
		{Name: "ghcr-io", Server: "ghcr.io", Username: "acme", Password: "ghp_xyz789"},
	})

	cred, err := Get(context.Background(), client, "ghcr-io", "")
	require.NoError(t, err)
	assert.Equal(t, TypeRegistry, cred.Type)
	assert.Equal(t, "ghp_xyz789", cred.Value)
}

func TestGet_AmbiguousName(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedGit(t, client, []gitEntry{
		{Name: "shared", Server: "a", Username: "u", Token: "token-value"},
	})
	seedRegistries(t, client, []registryEntry{
		{Name: "shared", Server: "b", Username: "u", Password: "password-value"},
	})

	_, err := Get(context.Background(), client, "shared", "")
	require.Error(t, err)
	var ambig *AmbiguousError
	require.True(t, errors.As(err, &ambig))
	assert.Equal(t, "shared", ambig.Name)
	assert.ElementsMatch(t, []Type{TypeGit, TypeRegistry}, ambig.Types)
}

func TestGet_AmbiguousResolvedByType(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedGit(t, client, []gitEntry{
		{Name: "shared", Server: "a", Username: "u", Token: "token-value"},
	})
	seedRegistries(t, client, []registryEntry{
		{Name: "shared", Server: "b", Username: "u", Password: "password-value"},
	})

	cred, err := Get(context.Background(), client, "shared", TypeGit)
	require.NoError(t, err)
	assert.Equal(t, "token-value", cred.Value)

	cred, err = Get(context.Background(), client, "shared", TypeRegistry)
	require.NoError(t, err)
	assert.Equal(t, "password-value", cred.Value)
}

func TestGet_NotFound(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	_, err := Get(context.Background(), client, "missing", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestGet_NotFoundWithType(t *testing.T) {
	client := fake.NewSimpleClientset() //nolint:staticcheck // matches project test pattern
	seedRegistries(t, client, []registryEntry{
		{Name: "ghcr-io", Server: "ghcr.io", Username: "u", Password: "p"},
	})

	_, err := Get(context.Background(), client, "ghcr-io", TypeGit)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "git credential")
	assert.Contains(t, err.Error(), "not found")
}

func TestMask(t *testing.T) {
	assert.Equal(t, "••••••••", Mask(""))
	assert.Equal(t, "••••••••", Mask("short"))
	assert.Equal(t, "••••••••", Mask("exactly8"))
	assert.Equal(t, "glpa••••••••", Mask("glpa-abc123"))
}
