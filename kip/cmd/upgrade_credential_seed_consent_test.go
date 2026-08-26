package cmd

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

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
// this exists not to do. The wrapper signals it by letting the run close.
func TestSeedConsentClosesAutomaticallyWhenNothingIsReferenced(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err)
	assert.True(t, grants.mayClose, "an unreferenced credential's fail-closed decision was gated on the operator")
	assert.Empty(t, grants.approved, "the auto path recorded a grant against a credential nothing references")
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
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "forge"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err)
	assert.False(t, grants.mayClose, "a scripted upgrade closed the migration without consent")
	assert.Empty(t, grants.approved, "a scripted upgrade granted on inference")
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
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "forge"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, true, false, refuseToConfirm(t))

	require.NoError(t, err)
	assert.True(t, grants.mayClose, "the flag was passed and consent was still withheld")
	assert.Equal(t, []string{"shop"}, grants.approved["forge"], "the flag path did not return the previewed pair")
	assert.Contains(t, out.String(), "forge", "the flag path did not show what it was about to grant")
}

// A yes at the prompt is the interactive equivalent of the flag.
func TestSeedConsentGrantsOnYes(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "forge"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, true,
		func() (bool, error) { return true, nil })

	require.NoError(t, err)
	assert.True(t, grants.mayClose)
	assert.Equal(t, []string{"shop"}, grants.approved["forge"])
}

// A no at the prompt has to look identical to the no-flag no-TTY case, or the
// interactive operator gets a permission the scripted upgrade would refuse.
func TestSeedConsentDeclinesOnNo(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "forge"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, true,
		func() (bool, error) { return false, nil })

	require.NoError(t, err)
	assert.False(t, grants.mayClose)
	assert.Contains(t, out.String(), "--seed-credential-grants",
		"a declined prompt did not print how to answer yes next time")
}

// projectNamed builds a Project CR under a chosen name that claims the given
// namespaces. owningProject hardcodes the name to "shop"; this variant is for
// tests that need more than one project on the same cluster.
func projectNamed(project string, namespaces ...string) *unstructured.Unstructured {
	claims := make([]any, 0, len(namespaces))
	for _, ns := range namespaces {
		claims = append(claims, map[string]any{"name": ns, "uid": ns + "-uid"})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Project",
		"metadata":   map[string]any{"name": project},
		"status":     map[string]any{"namespaceClaims": claims},
	}}
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
		projectNamed("shop", "shop-prod"),
		projectNamed("blog", "blog-prod"),
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

// The preview is what a scripted operator diffs between runs, and map order is
// random, so an unsorted preview reorders itself on an unchanged cluster. Both
// sorts are pinned here: the credential names, and the projects under each.
func TestSeedConsentPreviewIsOrdered(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("zed-prod", "zed"),
		namespaceOfProject("abe-prod", "abe"),
		sharedCredentialSecret(t,
			sharedcred.Entry{Name: "zeta", Token: "a-token"},
			sharedcred.Entry{Name: "alpha", Token: "another-token"},
		),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitAppNamed("zed-prod", "zed-web", "zeta"),
		gitAppNamed("abe-prod", "abe-web", "zeta"),
		gitAppNamed("zed-prod", "zed-api", "alpha"),
		projectNamed("zed", "zed-prod"),
		projectNamed("abe", "abe-prod"),
	)
	var out bytes.Buffer

	_, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))
	require.NoError(t, err)

	printed := out.String()
	assert.Less(t, strings.Index(printed, "alpha"), strings.Index(printed, "zeta"),
		"the credentials were previewed in map order")
	assert.Contains(t, printed, "abe, zed",
		"the projects under a credential were previewed in map order")
}

// Absent is not empty, and this is the line that tells them apart. A list
// somebody decided as nobody is a decision, so the upgrade neither offers to
// fill it nor asks about it. Reading it as undecided would re-prompt on every
// upgrade for ever, which is what the automatic close exists to prevent.
func TestSeedConsentLeavesADecidedEmptyListAlone(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{
			Name: "forge", Token: "a-token", AllowedProjects: []string{},
		}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "forge"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err)
	assert.True(t, grants.mayClose, "a credential already decided as nobody was treated as undecided")
	assert.Empty(t, grants.approved, "a decided-empty list was offered for a grant")
	assert.Empty(t, out.String(), "the operator was asked about a decision already made")
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

// A cluster already migrated is not asked, and nothing is offered up to be
// closed again: it prints nothing and approves nothing, whichever way the flag
// and the TTY go.
//
// It does still record what the allow-lists held. That cluster is the commonest
// one there is — v0.14.0 auto-seeded most legacy clusters — and its lists can be
// erased by an old console-api during this rollout like anybody else's.
func TestSeedConsentSaysNothingOnAMigratedCluster(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemMigrated(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{
			Name: "forge", Server: "git.example.com", Token: "a-token",
			AllowedProjects: []string{"shop"},
		}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "forge"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err)
	assert.False(t, grants.mayClose, "a finished migration was offered up to be closed again")
	assert.Empty(t, grants.approved)
	assert.Equal(t, []string{"shop"}, grants.decided["forge"].AllowedProjects,
		"a migrated cluster kept no record, so nothing could be written back")
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
		owningProject("shop-prod"),
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
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "web-git-credentials"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err)
	assert.True(t, grants.mayClose, "an app-own credential was treated as a shared one")
	assert.Empty(t, grants.approved)
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
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "web-git-credentials"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, true, false, refuseToConfirm(t))

	require.NoError(t, err)
	assert.True(t, grants.mayClose)
	assert.Equal(t, []string{"shop"}, grants.approved["web-git-credentials"],
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

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err)
	assert.True(t, grants.mayClose)
	assert.Empty(t, grants.approved,
		"a stray namespace's app inferred a grant nobody could have approved")
}

// The population this consent covers on the pre-claims side: a cluster
// upgrading from a build that wrote no claims at all, where the only record a
// project has of holding a namespace is the older status.namespaces list. The
// wrapper must accept that record, or every build on that cluster stops.
func TestSeedConsentAcceptsAPreClaimsProjectRecord(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	preClaims := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Project",
		"metadata":   map[string]any{"name": "shop"},
		"status":     map[string]any{"namespaces": []any{"shop-prod"}},
	}}
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"), preClaims)
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, true, false, refuseToConfirm(t))

	require.NoError(t, err)
	assert.True(t, grants.mayClose)
	assert.Equal(t, []string{"shop"}, grants.approved["forge"],
		"a cluster with no claims yet was left with a credential nobody may build with")
}

// And the label on its own still grants nothing, which is the half the record
// is there to make decidable. The consent wrapper leaves the pair out of the
// approved map and calls it a missed grant instead.
func TestSeedConsentReportsAMissedGrantForANamespaceNoRecordCovers(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("victim-prod", "attacker"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	attacker := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Project",
		"metadata":   map[string]any{"name": "attacker"},
		"status":     map[string]any{},
	}}
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("victim-prod", "forge"), attacker)
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, true, false, refuseToConfirm(t))

	require.NoError(t, err)
	assert.True(t, grants.mayClose)
	assert.Empty(t, grants.approved["forge"],
		"a namespace pointed at a project by its label alone was previewed for a standing grant to a shared credential")
	printed := out.String()
	assert.Contains(t, printed, "victim-prod",
		"a namespace whose project could not be proven was not named as a missed grant")
	assert.Contains(t, printed, "kip credentials allow forge --project attacker",
		"the notice does not say how to put it right")
}

// A migrated cluster did not read this list at all before the repair existed,
// so a blip reading it must not become a new way for an upgrade to abort on a
// working cluster. Saying it was skipped keeps that from passing as a clean
// bill of health.
func TestSeedConsentSurvivesAFailureToReadTheListOnAMigratedCluster(t *testing.T) {
	clientset := k8sfake.NewClientset(kipperSystemMigrated())
	clientset.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the apiserver is having a moment")
	})
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())
	var out bytes.Buffer

	grants, err := credentialSeedConsent(context.Background(), clientset, dyn, &out, false, false, refuseToConfirm(t))

	require.NoError(t, err, "an advisory read failure aborted the upgrade on a migrated cluster")
	assert.Empty(t, grants.decided, "a failed read was recorded as a cluster with nothing decided")
	assert.Contains(t, out.String(), "Could not record", "a skipped check passed as a clean run")
}

// Before the migration is finished the same read is load-bearing: an unreadable
// list must not read as nothing to decide.
func TestSeedConsentStillFailsOnAnUnreadableListBeforeTheMigration(t *testing.T) {
	clientset := k8sfake.NewClientset(kipperSystemUpgraded())
	clientset.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the apiserver is having a moment")
	})
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())

	_, err := credentialSeedConsent(context.Background(), clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))

	require.Error(t, err, "an unreadable list read as a cluster with nothing to decide")
}
