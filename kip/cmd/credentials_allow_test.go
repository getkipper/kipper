package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/sharedcred"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func projectsExist(names ...string) *dynamicfake.FakeDynamicClient {
	crs := make([]runtime.Object, 0, len(names))
	for _, name := range names {
		crs = append(crs, projectCR(name))
	}
	return dynamicfake.NewSimpleDynamicClient(projectScheme(), crs...)
}

func projectCR(name string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": manifest.ProjectGVR.GroupVersion().String(),
		"kind":       "Project",
		"metadata":   map[string]any{"name": name},
	}}
}

func sharedCredentialSecret(t *testing.T, entries ...sharedcred.Entry) *corev1.Secret {
	t.Helper()
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sharedcred.ConfigSecretName, Namespace: sharedcred.Namespace},
		Data:       map[string][]byte{"credentials": data},
	}
}

func storedEntries(t *testing.T, clientset *k8sfake.Clientset) []sharedcred.Entry {
	t.Helper()
	entries, err := sharedcred.Load(context.Background(), clientset)
	require.NoError(t, err)
	return entries
}

// The gap this exists for: a shared credential could only be granted through the
// console UI or a raw API call that made the operator re-send the token.
func TestCredentialsAllowGrantsAProject(t *testing.T) {
	clientset := k8sfake.NewClientset(sharedCredentialSecret(t,
		sharedcred.Entry{Name: "forge", Server: "git.example.com", Username: "deploy", Token: "a-token"}))
	var out bytes.Buffer

	require.NoError(t, grantSharedCredential(context.Background(), clientset, projectsExist("shop"), "forge", []string{"shop"}, &out))

	entries := storedEntries(t, clientset)
	require.Len(t, entries, 1)
	assert.True(t, entries[0].AllowsProject("shop"), "allowed projects = %v", entries[0].AllowedProjects)
	assert.Contains(t, out.String(), "shop")
}

// Changing who may build with a token must never require re-entering the token,
// which is the whole reason the console path was unusable from a terminal.
func TestCredentialsAllowKeepsTheStoredToken(t *testing.T) {
	clientset := k8sfake.NewClientset(sharedCredentialSecret(t,
		sharedcred.Entry{Name: "forge", Server: "git.example.com", Username: "deploy", Token: "a-token"}))

	require.NoError(t, grantSharedCredential(context.Background(), clientset, projectsExist("shop"), "forge", []string{"shop"}, &bytes.Buffer{}))

	entries := storedEntries(t, clientset)
	assert.Equal(t, "a-token", entries[0].Token, "the grant lost the token it was not given")
	assert.Equal(t, "git.example.com", entries[0].Server)
	assert.Equal(t, "deploy", entries[0].Username)
}

func TestCredentialsRevokeRemovesAProject(t *testing.T) {
	clientset := k8sfake.NewClientset(sharedCredentialSecret(t,
		sharedcred.Entry{Name: "forge", Token: "a-token", AllowedProjects: []string{"shop", "blog"}}))

	require.NoError(t, revokeSharedCredential(context.Background(), clientset, projectsExist("shop"), "forge", []string{"shop"}, []string{"shop"}, &bytes.Buffer{}))

	entries := storedEntries(t, clientset)
	assert.False(t, entries[0].AllowsProject("shop"))
	assert.True(t, entries[0].AllowsProject("blog"), "revoking one project removed another")
}

func TestCredentialsAllowRefusesAnUnknownCredential(t *testing.T) {
	clientset := k8sfake.NewClientset(sharedCredentialSecret(t, sharedcred.Entry{Name: "forge"}))

	err := grantSharedCredential(context.Background(), clientset, projectsExist("shop"), "missing", []string{"shop"}, &bytes.Buffer{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing")
}

// A registry credential is granted with a different command, and an operator who
// reaches for the wrong one should be told which, rather than that the name does
// not exist.
func TestCredentialsAllowPointsAtTheRegistryCommandForARegistryName(t *testing.T) {
	clientset := k8sfake.NewClientset(
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge"}),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "kipper-registries", Namespace: "kipper-system"},
			Data: map[string][]byte{"registries": []byte(
				`[{"name":"ghcr","server":"ghcr.io","username":"deploy","password":"p"}]`)},
		})

	err := grantSharedCredential(context.Background(), clientset, projectsExist("shop"), "ghcr", []string{"shop"}, &bytes.Buffer{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip registry add",
		"the operator was told the name does not exist when it exists in the other store")
}

func TestCredentialsAllowPrintsTheResultingList(t *testing.T) {
	clientset := k8sfake.NewClientset(sharedCredentialSecret(t,
		sharedcred.Entry{Name: "forge", Token: "a-token", AllowedProjects: []string{"blog"}}))
	var out bytes.Buffer

	require.NoError(t, grantSharedCredential(context.Background(), clientset, projectsExist("shop"), "forge", []string{"shop"}, &out))

	printed := out.String()
	assert.Contains(t, printed, "blog")
	assert.Contains(t, printed, "shop")
	assert.False(t, strings.Contains(printed, "a-token"), "the grant printed the token")
}

// A grant is write-only from the CLI: nothing at build time reports which
// project the token was meant for, so a typo, a wrong case, or a comma-separated
// list stored as one name all read as a success and refuse every build.
func TestCredentialsAllowRefusesAProjectThatDoesNotExist(t *testing.T) {
	clientset := k8sfake.NewClientset(sharedCredentialSecret(t, sharedcred.Entry{Name: "forge"}))
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), projectCR("shop"), projectCR("blog"))

	// The wrong case is the case that matters: the name is compared exactly at
	// build time, so "Shop" is a grant that can never match the "shop" the
	// namespace label carries.
	err := grantSharedCredential(context.Background(), clientset, dyn, "forge", []string{"Shop"}, &bytes.Buffer{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), `no project "Shop"`)
	assert.Contains(t, err.Error(), "It has:", "the refusal does not say which projects there are")
	assert.Contains(t, err.Error(), "blog", "the refusal does not name the projects the cluster has")

	entries := storedEntries(t, clientset)
	assert.Empty(t, entries[0].AllowedProjects, "a refused grant was stored anyway")
}

func TestCredentialsAllowStoresAProjectThatExists(t *testing.T) {
	clientset := k8sfake.NewClientset(sharedCredentialSecret(t, sharedcred.Entry{Name: "forge"}))
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), projectCR("shop"))

	require.NoError(t, grantSharedCredential(context.Background(), clientset, dyn, "forge", []string{"shop"}, &bytes.Buffer{}))

	entries := storedEntries(t, clientset)
	assert.True(t, entries[0].AllowsProject("shop"))
}

// Revoking is the way out of a mistake, including one made against a project
// that has since been deleted, so it cannot require the project to exist.
func TestCredentialsRevokeDoesNotRequireTheProjectToExist(t *testing.T) {
	clientset := k8sfake.NewClientset(sharedCredentialSecret(t,
		sharedcred.Entry{Name: "forge", AllowedProjects: []string{"gone"}}))
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme())

	require.NoError(t, revokeSharedCredential(context.Background(), clientset, dyn, "forge", []string{"gone"}, []string{"gone"}, &bytes.Buffer{}))

	entries := storedEntries(t, clientset)
	assert.False(t, entries[0].AllowsProject("gone"))
}

// The registry name is what identifies the entry. Sending the operator to
// 'kip registry add --server ghcr.io' without it derives a different name,
// finds no entry, and asks for a username and password it should not need.
func TestCredentialsAllowNamesTheRegistryCredentialInTheRemedy(t *testing.T) {
	clientset := k8sfake.NewClientset(
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge"}),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "kipper-registries", Namespace: "kipper-system"},
			Data: map[string][]byte{"registries": []byte(
				`[{"name":"ghcr","server":"ghcr.io","username":"deploy","password":"p"}]`)},
		})
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), projectCR("shop"))

	err := grantSharedCredential(context.Background(), clientset, dyn, "ghcr", []string{"shop"}, &bytes.Buffer{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--name ghcr")
	assert.Contains(t, err.Error(), "replaces", "the remedy presents a replacement as if it added one project")
}

// The registry hint stands in for "there is no such git credential" only. Any
// other failure has to reach the operator as itself, or a name that exists in
// both stores sends them to the wrong one whenever the write fails.
func TestCredentialsAllowKeepsARealFailureVisible(t *testing.T) {
	clientset := k8sfake.NewClientset(
		sharedCredentialSecret(t, sharedcred.Entry{Name: "ghcr"}),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "kipper-registries", Namespace: "kipper-system"},
			Data: map[string][]byte{"registries": []byte(
				`[{"name":"ghcr","server":"ghcr.io","username":"deploy","password":"p"}]`)},
		})
	clientset.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Resource: "secrets"}, "kipper-git-credentials", errors.New("no permission"))
	})
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), projectCR("shop"))

	err := grantSharedCredential(context.Background(), clientset, dyn, "ghcr", []string{"shop"}, &bytes.Buffer{})

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "registry credential",
		"an RBAC failure was reported as the name belonging to the other store")
}

// Revoking is the way out of a grant that should not be there, including one an
// older kip, a hand edit or a restore left in a form this version would not
// write. Resolving the argument the way a grant does would refuse to remove
// those while reporting success.
func TestRevokeRemovesTheProjectAsTypedAndAsTheClusterKnowsIt(t *testing.T) {
	org := &config.Cluster{Org: "acme"}

	forms := revocableForms(org, []string{"shop"})

	assert.Contains(t, forms, "shop", "a bare name left by an older writer cannot be revoked")
	assert.Contains(t, forms, "acme-shop", "the name a grant is stored under cannot be revoked")
}

func TestRevocableFormsDoesNotRepeatANameThatNeedsNoPrefix(t *testing.T) {
	assert.Equal(t, []string{"shop"}, revocableForms(&config.Cluster{}, []string{"shop"}))
}
