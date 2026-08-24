package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/sharedcred"
	"github.com/getkipper/kipper/kip/internal/deployer"
)

// The pure decision function is a five-line table, and everything else in this
// change composes with it, so it earns its own test independent of the
// wrapper. The defect this exists for is silent over-granting; the direction
// that matters is that "no answer" never becomes "yes".
func TestDecideCredentialSeedConsent(t *testing.T) {
	noopConfirm := func() (bool, error) {
		t.Fatalf("confirm was called when the decision should not have needed one")
		return false, nil
	}
	referenced := map[string][]string{"forge": {"shop"}}

	cases := []struct {
		name    string
		usage   map[string][]string
		flag    bool
		isTTY   bool
		confirm func() (bool, error)
		want    consentDecision
		wantErr bool
	}{
		{
			name:    "nothing referenced closes automatically",
			usage:   map[string][]string{},
			flag:    false,
			isTTY:   false,
			confirm: noopConfirm,
			want:    consentAuto,
		},
		{
			name:    "nothing referenced with the flag still closes automatically",
			usage:   nil,
			flag:    true,
			isTTY:   true,
			confirm: noopConfirm,
			want:    consentAuto,
		},
		{
			name:    "the flag grants without a prompt",
			usage:   referenced,
			flag:    true,
			isTTY:   false,
			confirm: noopConfirm,
			want:    consentGrant,
		},
		{
			name:    "no flag and no TTY declines",
			usage:   referenced,
			flag:    false,
			isTTY:   false,
			confirm: noopConfirm,
			want:    consentDecline,
		},
		{
			name:  "TTY, yes at the prompt grants",
			usage: referenced,
			isTTY: true,
			confirm: func() (bool, error) {
				return true, nil
			},
			want: consentGrant,
		},
		{
			name:  "TTY, no at the prompt declines",
			usage: referenced,
			isTTY: true,
			confirm: func() (bool, error) {
				return false, nil
			},
			want: consentDecline,
		},
		{
			name:  "a confirm error declines and is returned",
			usage: referenced,
			isTTY: true,
			confirm: func() (bool, error) {
				return false, errors.New("stdin closed")
			},
			want:    consentDecline,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := decideCredentialSeedConsent(tc.usage, tc.flag, tc.isTTY, tc.confirm)
			if tc.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, tc.want, got)
		})
	}
}

// The nothing-referenced path is the reason the migration can still close
// automatically: a cluster whose undecided credentials are referenced by no
// app has no permission to invent, and asking every upgrade forever is what
// this exists not to do. The wrapper signals it with a non-nil empty map.
func TestSeedConsentClosesAutomaticallyWhenNothingIsReferenced(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())
	var out bytes.Buffer

	approved, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err)
	require.NotNil(t, approved, "an unreferenced credential's fail-closed decision was gated on the operator")
	assert.Empty(t, approved, "the auto path recorded a grant against a credential nothing references")
	assert.Empty(t, out.String(), "the operator was asked about nothing")
}

// The change this test pins: a scripted upgrade of a legacy cluster used to
// grant every credential the apps happened to reference. It has to decline
// silently now and print the flag, so the operator has one path to grant the
// ones that should stay.
func TestSeedConsentDeclinesInAScriptWithoutTheFlag(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))
	var out bytes.Buffer

	approved, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err)
	assert.Nil(t, approved, "a scripted upgrade granted on inference")
	printed := out.String()
	assert.Contains(t, printed, "forge", "the referenced credential was not named")
	assert.Contains(t, printed, "shop", "the project referencing it was not named")
	assert.Contains(t, printed, "--seed-credential-grants", "the flag that would grant them was not named")
	assert.Contains(t, printed, "kip credentials allow", "the per-project remedy was not named")
}

// The flag path mirrors --yes for the system-component prompt: automation opts
// in explicitly, and no prompt is issued. The returned snapshot names exactly
// the previewed pairs.
func TestSeedConsentGrantsUnderTheFlag(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))
	var out bytes.Buffer

	approved, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, true, false, refuseToConfirm(t))

	require.NoError(t, err)
	require.NotNil(t, approved, "the flag was passed and consent was still withheld")
	assert.Equal(t, []string{"shop"}, approved["forge"], "the flag path did not return the previewed pair")
	assert.Contains(t, out.String(), "forge", "the flag path did not show what it was about to grant")
}

// A yes at the prompt is the interactive equivalent of the flag.
func TestSeedConsentGrantsOnYes(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))
	var out bytes.Buffer

	approved, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, true,
		func() (bool, error) { return true, nil })

	require.NoError(t, err)
	require.NotNil(t, approved)
	assert.Equal(t, []string{"shop"}, approved["forge"])
}

// A no at the prompt has to look identical to the no-flag no-TTY case, or the
// interactive operator gets a permission the scripted upgrade would refuse.
func TestSeedConsentDeclinesOnNo(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))
	var out bytes.Buffer

	approved, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, true,
		func() (bool, error) { return false, nil })

	require.NoError(t, err)
	assert.Nil(t, approved)
	assert.Contains(t, out.String(), "--seed-credential-grants",
		"a declined prompt did not print how to answer yes next time")
}

// The preview drives an operator's decision, so it must name the credentials
// they would be granting and no others: an unreferenced credential on the same
// cluster is unrelated, and naming it would frame the ask wrong.
func TestSeedConsentPreviewNamesEveryReferencedCredentialAndNoOther(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		namespaceOfProject("blog-prod", "blog"),
		sharedCredentialSecret(t,
			sharedcred.Entry{Name: "forge", Token: "a-token"},
			sharedcred.Entry{Name: "archive", Token: "another-token"},
			sharedcred.Entry{Name: "unused", Token: "third-token"},
		),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitAppNamed("shop-prod", "shop-web", "forge"),
		gitAppNamed("blog-prod", "blog-web", "archive"),
	)
	var out bytes.Buffer

	_, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))
	require.NoError(t, err)

	printed := out.String()
	assert.Contains(t, printed, "forge", "the first referenced credential was not named")
	assert.Contains(t, printed, "archive", "the second referenced credential was not named")
	assert.NotContains(t, printed, "unused", "an unreferenced credential was named as needing consent")
	assert.Contains(t, printed, "shop")
	assert.Contains(t, printed, "blog")
}

// Not calling the confirm callback when consent is not needed is the guard the
// noopConfirm in the table test also checks, one composition level up: the
// wrapper must not surprise the operator with a prompt in a script.
func refuseToConfirm(t *testing.T) func() (bool, error) {
	t.Helper()
	return func() (bool, error) {
		t.Errorf("confirm was called when the wrapper should not have needed one")
		return false, nil
	}
}

// gitAppNamed is the multi-app variant of gitApp, so a test can place more
// than one app on the same cluster without colliding on the "web" name gitApp
// uses.
func gitAppNamed(namespace, name, credential string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": deployer.AppGVR.GroupVersion().String(),
		"kind":       "App",
		"metadata":   map[string]any{"name": name, "namespace": namespace},
		"spec": map[string]any{
			"image": "nginx", "port": int64(80),
			"git": map[string]any{
				"url": "https://git.example.com/acme/" + name + ".git", "branch": "main",
				"credentialsSecret": credential,
			},
		},
	}}
}

// A cluster already migrated is not asked. The wrapper returns the auto empty
// map and prints nothing, whichever way the flag and the TTY go.
func TestSeedConsentSaysNothingOnAMigratedCluster(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemMigrated(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))
	var out bytes.Buffer

	approved, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err)
	require.NotNil(t, approved, "a migrated cluster was told it had no consent")
	assert.Empty(t, approved)
	assert.Empty(t, out.String(), "the wrapper spoke on a cluster whose migration was long finished")
}

// A curated allow-list is not undecided, so it never appears in the preview
// and consent about it is not asked. The flag also must not touch it (the
// fill itself skips it, but the preview must not name it either or the
// operator would think they were granting something they were not).
func TestSeedConsentIgnoresCuratedAllowLists(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t,
			sharedcred.Entry{Name: "forge", Token: "a-token", AllowedProjects: []string{"blog"}},
			sharedcred.Entry{Name: "archive", Token: "another-token"},
		),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitAppNamed("shop-prod", "storefront", "forge"),
		gitAppNamed("shop-prod", "archiver", "archive"),
	)
	var out bytes.Buffer

	_, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))
	require.NoError(t, err)

	printed := out.String()
	assert.NotContains(t, printed, "forge",
		"the preview offered to widen an allow-list an admin had set")
	assert.Contains(t, printed, "archive",
		"the preview omitted the undecided credential nobody had touched")
}

// An app's own credential is not on the shared list, so nothing an app names
// for itself can invent a grant. Under the new consent model the shared/own
// distinction is enforced by sharedCredentialUsage, which the wrapper calls.
func TestSeedConsentIgnoresACredentialThatIsNotShared(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "web-git-credentials"))
	var out bytes.Buffer

	approved, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err)
	require.NotNil(t, approved, "an app-own credential was treated as a shared one")
	assert.Empty(t, approved)
	assert.Empty(t, out.String(),
		"the operator was asked about a credential the app names for itself")
}

// Membership of the list decides, in the same order the builder decides it.
// A shared credential that happens to be named the way an app of that name
// would name its own is still shared, and skipping it would leave exactly
// that app refused after the upgrade that was meant to keep it building.
func TestSeedConsentTreatsASharedCredentialNamedLikeAnAppsOwnAsShared(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "web-git-credentials", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "web-git-credentials"))
	var out bytes.Buffer

	approved, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, true, false, refuseToConfirm(t))

	require.NoError(t, err)
	require.NotNil(t, approved)
	assert.Equal(t, []string{"shop"}, approved["web-git-credentials"],
		"the builder resolves this name from the shared list, so the consent wrapper has to as well")
}

// A namespace outside any project cannot name one, and an upgrade that
// stopped there would refuse to run on a cluster with a stray namespace.
func TestSeedConsentSkipsAnAppInANamespaceWithNoProject(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "stray"}},
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("stray", "forge"))
	var out bytes.Buffer

	approved, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err)
	require.NotNil(t, approved)
	assert.Empty(t, approved,
		"a stray namespace's app inferred a grant nobody could have approved")
}
