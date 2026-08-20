package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/registrycred"
)

func registrySecret(t *testing.T, entries ...registrycred.Entry) *corev1.Secret {
	t.Helper()
	data, err := json.Marshal(entries) //nolint:gosec // test fixture: an invented password, stored in a K8s Secret
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registrycred.ConfigSecretName, Namespace: registrycred.Namespace},
		Data:       map[string][]byte{"registries": data},
	}
}

func storedRegistryEntries(t *testing.T, clientset *k8sfake.Clientset) []registrycred.Entry {
	t.Helper()
	entries, err := registrycred.Load(context.Background(), clientset)
	require.NoError(t, err)
	return entries
}

// The gap this exists for: the only way to grant a project was a flag that
// replaces the whole allow-list, so adding one project took every other away.
func TestRegistryAllowIsAdditive(t *testing.T) {
	clientset := k8sfake.NewClientset(registrySecret(t,
		registrycred.Entry{Name: "ghcr", Server: "ghcr.io", Password: "p", AllowedProjects: []string{"blog"}}))
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), projectCR("shop"), projectCR("blog"))

	require.NoError(t, grantRegistryCredential(context.Background(), clientset, dyn, "ghcr", []string{"shop"}, &bytes.Buffer{}))

	entries := storedRegistryEntries(t, clientset)
	assert.True(t, entries[0].AllowsProject("shop"))
	assert.True(t, entries[0].AllowsProject("blog"), "granting one project took another away")
}

// Changing who may pull must never require re-entering the password.
func TestRegistryAllowKeepsTheStoredPassword(t *testing.T) {
	clientset := k8sfake.NewClientset(registrySecret(t,
		registrycred.Entry{Name: "ghcr", Server: "ghcr.io", Username: "deploy", Password: "the-password"}))
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), projectCR("shop"))

	require.NoError(t, grantRegistryCredential(context.Background(), clientset, dyn, "ghcr", []string{"shop"}, &bytes.Buffer{}))

	entries := storedRegistryEntries(t, clientset)
	assert.Equal(t, "the-password", entries[0].Password, "the grant lost the password it was not given")
	assert.Equal(t, "deploy", entries[0].Username)
}

// A project name is compared exactly at pull time, so the wrong case is a grant
// that can never match. Refusing beats storing a success nobody can use.
func TestRegistryAllowRefusesAProjectThatDoesNotExist(t *testing.T) {
	clientset := k8sfake.NewClientset(registrySecret(t, registrycred.Entry{Name: "ghcr"}))
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), projectCR("shop"))

	err := grantRegistryCredential(context.Background(), clientset, dyn, "ghcr", []string{"Shop"}, &bytes.Buffer{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `no project "Shop"`)
	assert.Empty(t, storedRegistryEntries(t, clientset)[0].AllowedProjects, "a refused grant was stored anyway")
}

func TestRegistryAllowRefusesACredentialThatIsNotThere(t *testing.T) {
	clientset := k8sfake.NewClientset(registrySecret(t, registrycred.Entry{Name: "ghcr"}))
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), projectCR("shop"))

	err := grantRegistryCredential(context.Background(), clientset, dyn, "quay", []string{"shop"}, &bytes.Buffer{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "quay")
}

// Revoking is the way out of a grant that should not be there, including one an
// older writer left in a form this version would not write, so it validates
// nothing and removes every spelling.
func TestRegistryRevokeRemovesWithoutRequiringTheProjectToExist(t *testing.T) {
	clientset := k8sfake.NewClientset(registrySecret(t,
		registrycred.Entry{Name: "ghcr", AllowedProjects: []string{"gone", "blog"}}))

	require.NoError(t, revokeRegistryCredential(context.Background(), clientset,
		"ghcr", []string{"gone"}, []string{"gone"}, &bytes.Buffer{}))

	entries := storedRegistryEntries(t, clientset)
	assert.False(t, entries[0].AllowsProject("gone"))
	assert.True(t, entries[0].AllowsProject("blog"), "revoking one project removed another")
}

// `--allow-project` replaces the allow-list, which is what it has always done
// and what the documentation says. What it never did was say which grants that
// took away, so an operator adding one project silently lost the others.
func TestRegistryAddReportsTheGrantsItTakesAway(t *testing.T) {
	entries := []registrycred.Entry{{
		Name: "ghcr", Server: "ghcr.io", Username: "deploy", Password: "p",
		AllowedProjects: []string{"shop", "blog"},
	}}

	_, allowed, removed, err := applyRegistryAdd(entries, registryAdd{
		Name: "ghcr", Server: "ghcr.io", AllowedProjects: []string{"shop"}, ReplaceAllowed: true,
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"shop"}, allowed)
	assert.Equal(t, []string{"blog"}, removed, "the projects the replacement dropped were not reported")
}

// An omitted flag keeps what is stored, so granting never requires re-entering
// the password and never touches the allow-list.
func TestRegistryAddWithoutTheFlagLeavesTheAllowListAlone(t *testing.T) {
	entries := []registrycred.Entry{{
		Name: "ghcr", Server: "ghcr.io", Username: "deploy", Password: "old",
		AllowedProjects: []string{"shop"},
	}}

	updated, allowed, removed, err := applyRegistryAdd(entries, registryAdd{
		Name: "ghcr", Server: "ghcr.io", Password: "new",
	})

	require.NoError(t, err)
	assert.Equal(t, []string{"shop"}, allowed)
	assert.Empty(t, removed)
	assert.Equal(t, "new", updated[0].Password)
	assert.Equal(t, "deploy", updated[0].Username, "an omitted username replaced the stored one")
}

// A credential is addressed by name, so pointing an existing one at another host
// hands that host the password stored for the first. That takes a fresh one.
func TestRegistryAddRefusesToRepointWithoutAPassword(t *testing.T) {
	entries := []registrycred.Entry{{Name: "ghcr", Server: "ghcr.io", Username: "deploy", Password: "p"}}

	_, _, _, err := applyRegistryAdd(entries, registryAdd{Name: "ghcr", Server: "quay.io"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--password")
	assert.Contains(t, err.Error(), "quay.io")
}

func TestRegistryAddAllowsARepointWithAPassword(t *testing.T) {
	entries := []registrycred.Entry{{Name: "ghcr", Server: "ghcr.io", Username: "deploy", Password: "p"}}

	updated, _, _, err := applyRegistryAdd(entries, registryAdd{Name: "ghcr", Server: "quay.io", Password: "fresh"})

	require.NoError(t, err)
	assert.Equal(t, "quay.io", updated[0].Server)
	assert.Equal(t, "fresh", updated[0].Password)
}

// The Docker Hub aliases are one registry, so re-running add with a different
// spelling of the same host is not a repoint and must not demand a password.
func TestRegistryAddTreatsTheDockerHubAliasesAsOneRegistry(t *testing.T) {
	entries := []registrycred.Entry{{
		Name: "docker-io", Server: registrycred.NormalizeServer("docker.io"), Username: "u", Password: "p",
	}}

	_, _, _, err := applyRegistryAdd(entries, registryAdd{
		Name: "docker-io", Server: registrycred.NormalizeServer("index.docker.io"),
	})

	require.NoError(t, err)
}
