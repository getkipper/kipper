package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/sharedcred"
	"github.com/getkipper/kipper/kip/internal/deployer"
)

// The app is always called web, because one of these fixtures turns on the
// credential name an app of that name would generate for itself.
func gitApp(namespace, credential string) *unstructured.Unstructured {
	const name = "web"
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

func namespaceOfProject(name, project string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name,
		// Matching the claim owningProject writes. Which project a namespace
		// belongs to is read from that project's records, so a claim names the
		// object and a fixture with no UID matches nothing.
		UID: k8stypes.UID(name + "-uid"),
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "kipper",
			"kipper.run/project":           project,
		},
	}}
}

// owningProject is the project these fixtures build for, whose records say it
// holds these namespaces.
//
// A credential grant reconstructed here is written into the allow-list and
// stays there, so which project a namespace belongs to is decided from the
// project's own records. A fixture without them grants nobody anything.
func owningProject(namespaces ...string) *unstructured.Unstructured {
	claims := make([]any, 0, len(namespaces))
	for _, ns := range namespaces {
		claims = append(claims, map[string]any{"name": ns, "uid": ns + "-uid"})
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Project",
		"metadata":   map[string]any{"name": "shop"},
		"status":     map[string]any{"namespaceClaims": claims},
	}}
}

// The upgrade waits for the console-api it just rolled to record its build.
// A test that exercises the pass without one should not wait it out.
func noStampWait(t *testing.T) {
	t.Helper()
	wait, poll := stampWait, stampPoll
	stampWait, stampPoll = 0, time.Millisecond
	t.Cleanup(func() { stampWait, stampPoll = wait, poll })
}

func kipperSystem() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "kipper-system"}}
}

// The namespace of a cluster whose migration finished long ago.
func kipperSystemMigrated() *corev1.Namespace {
	ns := kipperSystemUpgraded()
	ns.Annotations[labels.AnnoGitCredentialGrantsSeeded] = "2026-08-19T00:00:00Z"
	return ns
}

// The namespace once the console-api that keeps allow-lists has started.
func kipperSystemUpgraded() *corev1.Namespace {
	ns := kipperSystem()
	ns.Annotations = map[string]string{labels.AnnoConsoleAPIBuild: "v0.14.0"}
	return ns
}

func seeded(t *testing.T, clientset *k8sfake.Clientset) bool {
	t.Helper()
	ns, err := clientset.CoreV1().Namespaces().Get(context.Background(), "kipper-system", metav1.GetOptions{})
	require.NoError(t, err)
	return ns.Annotations[labels.AnnoGitCredentialGrantsSeeded] != ""
}

// The defect this exists for: a cluster installed before allow-lists denies
// every project, so the first rebuild after an upgrade fails on an app that was
// building perfectly well the day before. The seeding writes what the operator
// consented to at the preview.
func TestSeedFillsFromTheApprovedSnapshot(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	approved := map[string][]string{"forge": {"shop"}}
	var out bytes.Buffer

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, &out, approving(t, clientset, approved, false)))

	entries := storedEntries(t, clientset)
	require.Len(t, entries, 1)
	assert.True(t, entries[0].AllowsProject("shop"),
		"the upgrade left an app whose grant was approved denied: %v", entries[0].AllowedProjects)
	assert.Contains(t, out.String(), "forge")
}

// A credential the operator has curated is their decision. Even if the approved
// map somehow named it, Seed skips a non-nil AllowedProjects, so the fill
// cannot widen it.
func TestSeedLeavesACuratedAllowListAlone(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		namespaceOfProject("blog-prod", "blog"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token", AllowedProjects: []string{"blog"}}),
	)
	approved := map[string][]string{"forge": {"shop"}}

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, &bytes.Buffer{}, approving(t, clientset, approved, false)))

	entries := storedEntries(t, clientset)
	assert.False(t, entries[0].AllowsProject("shop"),
		"an upgrade widened an allow-list an admin had set")
}

// Nothing to seed is the normal case on a cluster whose undecided entries the
// operator did not approve grants for, and it must not print a step that did
// nothing.
func TestSeedSaysNothingWhenNothingToSeed(t *testing.T) {
	clientset := k8sfake.NewClientset(kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false), sharedCredentialSecret(t,
		sharedcred.Entry{Name: "forge", Token: "a-token", AllowedProjects: []string{"shop"}}))
	var out bytes.Buffer

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, &out, credentialGrants{}))

	assert.Empty(t, out.String())
}

// A cluster with no shared credentials at all must not have the list Secret
// created for it by an upgrade.
func TestSeedDoesNotCreateAListForAClusterWithNoSharedCredentials(t *testing.T) {
	clientset := k8sfake.NewClientset(kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false), namespaceOfProject("shop-prod", "shop"))
	approved := map[string][]string{"forge": {"shop"}}

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, &bytes.Buffer{}, approving(t, clientset, approved, false)))

	_, err := clientset.CoreV1().Secrets(sharedcred.Namespace).Get(
		context.Background(), sharedcred.ConfigSecretName, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "the upgrade created a shared-credential list that did not exist")
}

// The migration ran once per credential, which is not the same as once per
// cluster. Between the seeding and the console-api restart the old writer is
// still serving, and a credential it creates in that window carries no
// allow-list at all, so the next ordinary upgrade would read it as predating
// the guard and grant it whatever names it. A reference is not a grant.
func TestUpgradeSeedsOncePerCluster(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token", AllowedProjects: []string{"blog"}}),
	)

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, &bytes.Buffer{}, credentialGrants{mayClose: true}))
	require.True(t, seeded(t, clientset), "the cluster was not recorded as migrated")

	// What the old console-api writes in the window: an entry with no list.
	require.NoError(t, sharedcred.Update(context.Background(), clientset,
		func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
			return append(entries, sharedcred.Entry{Name: "later", Token: "another-token"}), nil
		}))

	// A subsequent seed on the same cluster must not re-fill the freshly-added
	// entry from any snapshot: the marker is what stops it.
	approved := map[string][]string{"later": {"shop"}}
	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, &bytes.Buffer{}, approving(t, clientset, approved, false)))

	entries := storedEntries(t, clientset)
	for _, entry := range entries {
		if entry.Name == "later" {
			assert.Empty(t, entry.AllowedProjects,
				"an entry created after the migration was filled by a later seed")
		}
	}
}

// A cluster with no shared credentials is migrated too, and recording that is
// the whole point: otherwise its first credential, whenever it arrives, is
// still a candidate for the inference.
func TestUpgradeRecordsTheMigrationOnAClusterWithNoSharedCredentials(t *testing.T) {
	clientset := k8sfake.NewClientset(kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false), namespaceOfProject("shop-prod", "shop"))

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, &bytes.Buffer{}, credentialGrants{mayClose: true}))

	assert.True(t, seeded(t, clientset), "a cluster with nothing to seed was left seedable")
}

// The old console-api keeps serving until its pod is replaced, and its writer
// replaces an entry with the shape that carries no allow-list. A migration
// recorded before that is gone turns a rollout race into a build refused for
// good, because every later upgrade reads the marker and leaves the credential
// alone. Seeding twice is what repairs it, and only the second pass closes the
// migration. Both fills draw from the same approved snapshot the consent
// wrapper captured before the rollout.
func TestUpgradeRepairsAGrantTheOldWriterErasedDuringTheRollout(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	approved := map[string][]string{"forge": {"shop"}}

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, &bytes.Buffer{}, approving(t, clientset, approved, false)))
	require.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"))
	require.False(t, seeded(t, clientset), "the migration was closed while the old writer was still serving")

	// What the old writer does with a token rotation: the entry it sends back
	// carries no allow-list, and its own shape stores that as no list at all.
	require.NoError(t, sharedcred.Update(context.Background(), clientset,
		func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
			entries[0].AllowedProjects = nil
			return entries, nil
		}))

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, &bytes.Buffer{}, approving(t, clientset, approved, true)))

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"a grant erased during the rollout stayed erased")
	assert.True(t, seeded(t, clientset))
}

// A rolled pod is not new code. The image is a moving tag, so an upgrade run
// before the release it belongs to is published pulls the old image, rolls it
// happily, and would record a migration that had not happened — after which the
// old writer erases a grant and nothing ever repairs it.
func TestUpgradeDoesNotCloseTheMigrationWhileTheOldWriterServes(t *testing.T) {
	noStampWait(t)
	clientset := k8sfake.NewClientset(
		kipperSystem(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	approved := map[string][]string{"forge": {"shop"}}

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, &bytes.Buffer{}, approving(t, clientset, approved, true)))

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"the grant that keeps builds working was not written")
	assert.False(t, seeded(t, clientset),
		"the migration was closed against a console-api that still erases allow-lists")
}

// The snapshot is frozen at consent time. An app that appears during the
// rollout window points at what was undecided, and the closing pass must not
// grant it silently: consent covered a specific credential/project set, and
// the plan this exists for turns exactly on nothing outside that set becoming
// a grant.
func TestUpgradeFreezesTheApprovedSnapshotAgainstAppsThatArriveDuringRollout(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	// The cluster the operator was shown: the credential is undecided and no
	// app references it, so there is nothing to consent to.
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)
	require.True(t, grants.mayClose)
	require.Empty(t, grants.approved, "consent captured a grant against a credential nothing referenced")

	require.NoError(t, seedSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, grants))
	require.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"a credential nothing referenced yet was decided before the rollout")

	// The app arrives during the rollout window, on the cluster, pointing at
	// the undecided credential. Under the old rules that was a build waiting to
	// be granted; under consent it is a reference nobody agreed to, and the
	// closing pass has to be blind to it.
	_, err = dyn.Resource(deployer.AppGVR).Namespace("shop-prod").
		Create(ctx, gitApp("shop-prod", "forge"), metav1.CreateOptions{})
	require.NoError(t, err)

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, grants))

	entry := storedEntries(t, clientset)[0]
	assert.NotNil(t, entry.AllowedProjects,
		"the closing pass left the credential undecided instead of deciding it as nobody")
	assert.Empty(t, entry.AllowedProjects,
		"the closing pass granted an app that arrived after consent")
	assert.True(t, seeded(t, clientset))
}

// Deciding is what Seed will not revisit, so deciding against a console-api
// that still allows every build freezes a snapshot of a cluster still changing
// under the old rules. The pass that cannot see the new writer fills and stops
// there.
func TestUpgradeDecidesNothingWhileTheOldWriterServes(t *testing.T) {
	noStampWait(t)
	clientset := k8sfake.NewClientset(
		kipperSystem(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "archive", Token: "a-token"}),
	)
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, &out, credentialGrants{mayClose: true}))

	require.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"a credential nothing references yet was decided against a writer that still allows every build")
	assert.False(t, seeded(t, clientset))
	assert.Contains(t, out.String(), "archive",
		"the upgrade left the migration open without saying which credentials are waiting")
}

// The app that arrives while the old writer is still serving, on a cluster
// whose upgrade could not finish. Consent at the next upgrade captures it and
// the closing pass fills what was approved.
func TestUpgradeSeedsAnAppThatArrivedWhileTheMigrationCouldNotClose(t *testing.T) {
	noStampWait(t)
	clientset := k8sfake.NewClientset(
		kipperSystem(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "archive", Token: "a-token"}),
	)
	// First upgrade: nothing referenced, migration cannot close (no stamp).
	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset,
		&bytes.Buffer{}, credentialGrants{mayClose: true}))

	// A week later: the app is pointed at the credential, and the next upgrade
	// delivers the console-api that keeps allow-lists.
	ns, err := clientset.CoreV1().Namespaces().Get(context.Background(), "kipper-system", metav1.GetOptions{})
	require.NoError(t, err)
	ns.Annotations = map[string]string{labels.AnnoConsoleAPIBuild: "v0.14.0"}
	_, err = clientset.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
	require.NoError(t, err)

	// The second upgrade's consent captures the app that has now been
	// referencing the credential for a week and the operator approves it.
	approved := map[string][]string{"archive": {"shop"}}
	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, &bytes.Buffer{}, approving(t, clientset, approved, true)))

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"the second upgrade refused a reference the operator had approved")
	assert.True(t, seeded(t, clientset))
}

// The stamp has to be evidence about the console-api serving now. Left from a
// build that served weeks ago, it would vouch for a writer an operator has
// since pinned back during an incident, and the upgrade would close the
// migration against the one that clears allow-lists.
func TestUpgradeClearsTheStampBeforeTheRollout(t *testing.T) {
	clientset := k8sfake.NewClientset(kipperSystemUpgraded())

	require.NoError(t, clearConsoleAPIStamp(context.Background(), clientset))

	ns, err := clientset.CoreV1().Namespaces().Get(context.Background(), "kipper-system", metav1.GetOptions{})
	require.NoError(t, err)
	_, stamped := ns.Annotations[labels.AnnoConsoleAPIBuild]
	assert.False(t, stamped, "a stamp from the build that served before the rollout was left in place")
}

// The pod is running before it has recorded anything, so the upgrade waits
// rather than reading once and declaring the writer unknown.
func TestUpgradeWaitsForTheConsoleAPIToRecordItsBuild(t *testing.T) {
	clientset := k8sfake.NewClientset(kipperSystem())
	stampWaitWas, stampPollWas := stampWait, stampPoll
	stampWait, stampPoll = time.Second, time.Millisecond
	t.Cleanup(func() { stampWait, stampPoll = stampWaitWas, stampPollWas })

	go func() {
		time.Sleep(20 * time.Millisecond)
		ns, err := clientset.CoreV1().Namespaces().Get(context.Background(), "kipper-system", metav1.GetOptions{})
		if err != nil {
			return
		}
		ns.Annotations = map[string]string{labels.AnnoConsoleAPIBuild: "v0.14.0"}
		_, _ = clientset.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
	}()

	keeps, err := consoleAPIKeepsGrants(context.Background(), clientset)

	require.NoError(t, err)
	assert.True(t, keeps, "the upgrade gave up before the pod it had just started recorded its build")
}

// A cluster with no shared credentials has nothing waiting on a decision, and
// a warning about allow-lists it does not have is noise on every upgrade until
// the images publish.
func TestUpgradeSaysNothingAboutAMigrationWithNothingInIt(t *testing.T) {
	noStampWait(t)
	clientset := k8sfake.NewClientset(kipperSystem(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false), namespaceOfProject("shop-prod", "shop"))
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, &out, credentialGrants{mayClose: true}))

	assert.Empty(t, out.String())
}

// A pass that grants something and then says nothing was touched contradicts
// itself. What it reports is what is still waiting.
func TestUpgradeNamesOnlyTheCredentialsStillWaiting(t *testing.T) {
	noStampWait(t)
	clientset := k8sfake.NewClientset(
		kipperSystem(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t,
			sharedcred.Entry{Name: "forge", Token: "a-token"},
			sharedcred.Entry{Name: "archive", Token: "another-token"}),
	)
	// Consent granted only forge; archive stays undecided.
	approved := map[string][]string{"forge": {"shop"}}
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, &out, approving(t, clientset, approved, true)))

	printed := out.String()
	assert.Contains(t, printed, "archive", "the credential still waiting was not named")
	assert.NotContains(t, printed, "Still to decide who may build with forge",
		"a credential this pass granted was reported as still waiting")
}

// The declined path reports its own reason. Blaming the console-api stamp
// after the operator has said no would send them into a repair for something
// that is not broken.
func TestUpgradeReportsConsentDeclinedOnPassTwo(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, &out, credentialGrants{}))

	printed := out.String()
	assert.Contains(t, printed, "forge", "the credential still waiting was not named")
	assert.Contains(t, printed, "consent was declined",
		"the operator was told the wrong reason for a migration they declined")
	assert.NotContains(t, printed, "has not recorded its build",
		"the declined path blamed the console-api stamp")
	assert.False(t, seeded(t, clientset), "a declined pass recorded the migration")
	assert.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"a declined pass wrote a grant anyway")
}

// On a migrated cluster every writer records a decision, so a credential with
// no list at all has been edited by an older console-api or restored from an
// older copy. The upgrade says so and grants nothing: filling it from the apps
// that reference it would grant a project that had been revoked while its app
// still named the credential, which is the same shape.
func TestUpgradeNamesACredentialWhoseAllowListWasCleared(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemMigrated(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, &out, credentialGrants{mayClose: true}))

	assert.Contains(t, out.String(), "forge", "a cleared allow-list passed in silence")
	assert.Contains(t, out.String(), "kip credentials allow")
	assert.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"the upgrade granted a project on a cluster whose migration was long finished")
}

// The reports are advisory and run before the trust material and the system
// components, so a blip reading the list must not take the rest of the upgrade
// with it. Saying the check was skipped keeps it from passing as a clean run.
func TestUpgradeSurvivesAFailureToReadTheListForAReport(t *testing.T) {
	clientset := k8sfake.NewClientset(kipperSystemMigrated())
	clientset.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("the apiserver is having a moment")
	})
	var out bytes.Buffer

	err := closeSharedCredentialGrants(context.Background(), clientset, &out, credentialGrants{mayClose: true})

	require.NoError(t, err, "an advisory read failure stopped the upgrade")
	assert.Contains(t, out.String(), "Could not check", "a skipped check passed as a clean run")
}

// The same softening on the other path, which the twin test cannot reach:
// there the report's read is the third, after the fill's own read and its
// write, so a reactor that fails every read kills the fill instead.
func TestUpgradeSurvivesAFailureToReadTheListForTheOpenMigrationReport(t *testing.T) {
	noStampWait(t)
	clientset := k8sfake.NewClientset(
		kipperSystem(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	reads := 0
	clientset.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads < 3 {
			return false, nil, nil
		}
		return true, nil, apierrors.NewServiceUnavailable("the apiserver is having a moment")
	})
	var out bytes.Buffer

	err := closeSharedCredentialGrants(context.Background(), clientset, &out,
		credentialGrants{approved: map[string][]string{"forge": {"shop"}}, mayClose: true})

	require.NoError(t, err, "an advisory read failure stopped the upgrade before its trust material")
	assert.Contains(t, out.String(), "Could not check", "a skipped check passed as a clean run")
}

// erasedByTheOldWriter is what the console-api an upgrade replaces does to the
// whole list when anything is edited through it: its own shape has no allowed
// projects, so every entry comes back with none.
func erasedByTheOldWriter(t *testing.T, clientset *k8sfake.Clientset, names ...string) {
	t.Helper()
	require.NoError(t, sharedcred.Update(context.Background(), clientset,
		func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
			for i := range entries {
				for _, name := range names {
					if entries[i].Name == name {
						entries[i].AllowedProjects = nil
					}
				}
			}
			return entries, nil
		}))
}

// approving is what consent returns once the operator has agreed to a preview:
// the pairs, and the identity each credential carried when they were shown it.
// The two always travel together, because a grant is permission for a
// particular credential rather than for a name.
func approving(t *testing.T, clientset *k8sfake.Clientset, approved map[string][]string, mayClose bool) credentialGrants {
	t.Helper()
	return credentialGrants{
		approved: approved,
		shownAs:  sharedcred.Identities(storedEntries(t, clientset)),
		mayClose: mayClose,
	}
}

// The curated credential these fixtures turn on: decided before the rollout,
// which is what keeps it out of the consent preview and the approved snapshot.
func curatedForge(projects ...string) sharedcred.Entry {
	return sharedcred.Entry{
		Name: "forge", Server: "git.example.com", Token: "a-token",
		AllowedProjects: projects,
	}
}

// The defect this whole change turns on. A curated allow-list is not undecided,
// so consent never previews it and the approved snapshot never names it. The
// old writer erases it during the rollout anyway, and before this the closing
// pass decided it as nobody and sealed the migration over it.
func TestUpgradeRestoresACuratedAllowListTheOldWriterErased(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop")),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)
	require.NoError(t, seedSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, grants))

	erasedByTheOldWriter(t, clientset, "forge")

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"a curated allow-list erased during the rollout was decided as nobody instead of written back")
	assert.True(t, seeded(t, clientset))
	assert.Contains(t, out.String(), "forge", "the repair was not reported")
}

// Declining consent refuses an inference. It is not a decision to throw away a
// list somebody had already curated, and the decline path is where curated
// lists come from: the hint printed there tells the operator to run
// kip credentials allow.
func TestUpgradeRestoresAnErasedAllowListWhenConsentWasDeclined(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop"), sharedcred.Entry{Name: "archive", Token: "another"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "archive"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)
	require.False(t, grants.mayClose, "a scripted upgrade granted on inference")

	erasedByTheOldWriter(t, clientset, "forge")

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	stored := storedEntries(t, clientset)
	assert.True(t, sharedcred.Find(stored, "forge").AllowsProject("shop"),
		"declining an inference threw away a grant somebody had curated")
	assert.Nil(t, sharedcred.Find(stored, "archive").AllowedProjects,
		"a declined run decided a credential nobody consented to")
	assert.False(t, seeded(t, clientset), "a declined run closed the migration")
}

// The commonest cluster there is: v0.14.0 auto-seeded most legacy clusters, so
// the migration is long finished. Its lists are erased by an old console-api
// exactly like anybody else's, and reportClearedAllowLists only ever named them.
func TestUpgradeRestoresOnAClusterThatMigratedLongAgo(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemMigrated(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop")),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)

	erasedByTheOldWriter(t, clientset, "forge")

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"a migrated cluster's erased allow-list was reported instead of repaired")
	assert.NotContains(t, out.String(), "Nobody may build",
		"a list that was written back was still named as cleared")
}

// The freeze, against the repair. An app that arrives during the rollout window
// is a reference nobody agreed to, and a repair that re-derived from live apps
// instead of from the record would grant it.
func TestUpgradeNeverRestoresAProjectThatWasNotAlreadyAllowed(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		namespaceOfProject("evil-prod", "evil"),
		sharedCredentialSecret(t, curatedForge("shop")),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		projectNamed("shop", "shop-prod"), projectNamed("evil", "evil-prod"))

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, true, false, refuseToConfirm(t))
	require.NoError(t, err)

	_, err = dyn.Resource(deployer.AppGVR).Namespace("evil-prod").
		Create(ctx, gitAppNamed("evil-prod", "evil-web", "forge"), metav1.CreateOptions{})
	require.NoError(t, err)
	erasedByTheOldWriter(t, clientset, "forge")

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, grants))

	assert.Equal(t, []string{"shop"}, storedEntries(t, clientset)[0].AllowedProjects,
		"the repair granted a project that only ever referenced the credential")
}

// A revocation is a decision, and it is newer than the record. Revoking writes
// an empty list rather than no list, which is exactly what tells the repair to
// leave it alone.
func TestUpgradeDoesNotUndoARevocationMadeDuringTheRollout(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop", "blog")),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)

	require.NoError(t, sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		return sharedcred.Revoke(entries, "forge", []string{"blog"})
	}))

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, grants))

	assert.Equal(t, []string{"shop"}, storedEntries(t, clientset)[0].AllowedProjects,
		"the record undid a revocation made after it was taken")
}

// The build hands a project the credential's token against the credential's
// host, so a credential now pointing somewhere else is a different credential
// and nobody granted anything about it. It is named rather than written back.
func TestUpgradeNamesACredentialBoundElsewhereInsteadOfRestoringIt(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop")),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)

	require.NoError(t, sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		entries[0].AllowedProjects = nil
		entries[0].Server = "git.other.example"
		return entries, nil
	}))

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	entry := storedEntries(t, clientset)[0]
	assert.NotNil(t, entry.AllowedProjects)
	assert.Empty(t, entry.AllowedProjects,
		"a project was allowed against a credential bound to a server nobody granted")
	printed := out.String()
	assert.Contains(t, printed, "git.example.com", "the server it used to be bound to was not named")
	assert.Contains(t, printed, "kip credentials allow", "the remedy was not named")
}

// Deciding is permanent, so it waits for the writer it replaces to be gone. A
// rollout that reports ready still leaves a pod serving whatever request it had
// for the rest of its termination grace, and that pod's write cannot conflict
// with anything.
func TestUpgradeHoldsTheMigrationOpenWhileTheOldConsoleAPIIsStillRunning(t *testing.T) {
	shortQuiescence(t)
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop")),
		consoleAPIDeployment(),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIReplicaSet("1", "old456"),
		consoleAPIPod("console-api-new", false),
		// The pod being replaced: still Running, not yet marked for deletion,
		// and carrying the previous revision's hash. A count cannot see it.
		consoleAPIPodOfRevision("console-api-old", "old456", false),
	)
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out,
		credentialGrants{mayClose: true, decided: sharedcred.Decisions(storedEntries(t, clientset))}))

	assert.False(t, seeded(t, clientset),
		"the migration was closed while a pod that can still erase a list was running")
	assert.Contains(t, out.String(), "still running", "the operator was not told why it stayed open")
}

// The same cluster once the old pod has gone.
func TestUpgradeClosesOnceTheOldConsoleAPIHasGone(t *testing.T) {
	shortQuiescence(t)
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop")),
		consoleAPIDeployment(),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api-new", false),
	)

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &bytes.Buffer{},
		credentialGrants{mayClose: true}))

	assert.True(t, seeded(t, clientset), "the migration stayed open with nothing left to wait for")
}

// The write that lands after the closing pass. It cannot conflict, so the only
// evidence is the list being absent again, and recording the migration over it
// would seal the loss for good.
func TestUpgradeHoldsTheMigrationOpenWhenTheClosingWriteDidNotSurvive(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		// Undecided, so the closing pass has something to write and the old
		// pod's save has something to land on top of.
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	var out bytes.Buffer

	// The old pod's blind save, landing between the closing write and the read
	// that checks it: straight onto the tracker, so it does not re-enter here.
	erased := false
	clientset.PrependReactor("update", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if erased {
			return false, nil, nil
		}
		erased = true
		obj := action.(k8stesting.UpdateAction).GetObject()
		require.NoError(t, clientset.Tracker().Update(
			corev1.SchemeGroupVersion.WithResource("secrets"), obj, sharedcred.Namespace))
		erasedByTheOldWriterOnTracker(t, clientset)
		return true, obj, nil
	})

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, credentialGrants{mayClose: true}))

	assert.False(t, seeded(t, clientset),
		"the migration was recorded over a write that had already been replaced")
	assert.Contains(t, out.String(), "forge", "the credential that was cleared again was not named")
}

// Pass one runs while the old writer still allows every build, so it may fill
// but must never decide. Writing back a list that was decided as nobody is
// deciding, so pass one is handed no record at all.
func TestPassOneRepairsNothing(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)

	require.NoError(t, seedSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, credentialGrants{
		approved: map[string][]string{"other": {"shop"}},
		decided:  map[string]sharedcred.Decision{"forge": {Identity: sharedcred.Identity{Server: "git.example.com"}}},
	}))

	assert.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"pass one decided a credential while the old writer was still serving")
}

// shortQuiescence keeps a test that never satisfies the wait from waiting it out.
func shortQuiescence(t *testing.T) {
	t.Helper()
	wait, poll := quiescenceWait, quiescencePoll
	quiescenceWait, quiescencePoll = 0, time.Millisecond
	t.Cleanup(func() { quiescenceWait, quiescencePoll = wait, poll })
}

// The console-api as it stands once the upgrade has rolled it: the Deployment,
// and the ReplicaSet of the revision it is now serving. Which revision a pod
// belongs to is what tells the pod being replaced from its replacement, so
// every fixture that closes a migration needs both.
const currentRevision = "abc123"

func consoleAPIDeployment() *appsv1.Deployment { return consoleAPIDeploymentOf(1) }

func consoleAPIDeploymentOf(replicas int32) *appsv1.Deployment {
	updated := replicas
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: consoleAPIName, Namespace: "kipper-system", UID: "console-api-uid"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &updated,
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": consoleAPIName}},
		},
		Status: appsv1.DeploymentStatus{UpdatedReplicas: updated},
	}
}

func consoleAPIReplicaSet(revision, hash string) *appsv1.ReplicaSet {
	controller := true
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:        consoleAPIName + "-" + hash,
			Namespace:   "kipper-system",
			Labels:      map[string]string{"app": consoleAPIName, podTemplateHashLabel: hash},
			Annotations: map[string]string{deploymentRevisionAnnotation: revision},
			OwnerReferences: []metav1.OwnerReference{{
				Name: consoleAPIName, UID: "console-api-uid", Controller: &controller,
			}},
		},
	}
}

// A pod under the console-api Deployment, of the revision it is now serving.
// going is one on its way out, which is the pod that can still write for the
// rest of its termination grace.
func consoleAPIPod(name string, going bool) *corev1.Pod {
	return consoleAPIPodOfRevision(name, currentRevision, going)
}

// The image every fixture pod reports. One image across the live pods is what
// makes the namespace-wide build stamp vouch for all of them rather than for
// whichever pod started first.
const currentImage = "ghcr.io/example/console-api@sha256:aaaa"

func consoleAPIPodOfRevision(name, hash string, going bool) *corev1.Pod {
	return consoleAPIPodRunning(name, hash, "containerd://"+currentImage, going)
}

func consoleAPIPodRunning(name, hash, image string, going bool) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "kipper-system",
			Labels: map[string]string{"app": consoleAPIName, podTemplateHashLabel: hash},
		},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: consoleAPIName, ImageID: image}},
		},
	}
	if going {
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"kipper.run/test"}
	}
	return pod
}

// erasedByTheOldWriterOnTracker is the same erasure written straight to the
// tracker, for a test standing in for a write that does not go through the
// reactor chain.
func erasedByTheOldWriterOnTracker(t *testing.T, clientset *k8sfake.Clientset) {
	t.Helper()
	obj, err := clientset.Tracker().Get(
		corev1.SchemeGroupVersion.WithResource("secrets"), sharedcred.Namespace, sharedcred.ConfigSecretName)
	require.NoError(t, err)
	secret := obj.(*corev1.Secret)
	var entries []sharedcred.Entry
	require.NoError(t, json.Unmarshal(secret.Data["credentials"], &entries))
	for i := range entries {
		entries[i].AllowedProjects = nil
	}
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	secret.Data["credentials"] = data
	require.NoError(t, clientset.Tracker().Update(
		corev1.SchemeGroupVersion.WithResource("secrets"), secret, sharedcred.Namespace))
}

// A credential replaced under its old name during the rollout carries a token
// nobody was granted, and it is the same shape as a rotation. The ambiguous
// case fails closed and is named, rather than being written and warned about
// once the access is already live.
func TestUpgradeRefusesACredentialWhoseTokenChangedAndNamesIt(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop")),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)

	require.NoError(t, sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		entries[0].AllowedProjects = nil
		entries[0].Token = "a-different-token"
		return entries, nil
	}))

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	entry := storedEntries(t, clientset)[0]
	assert.NotNil(t, entry.AllowedProjects)
	assert.Empty(t, entry.AllowedProjects,
		"a project was allowed against a token it was never granted")
	printed := out.String()
	assert.Contains(t, printed, "different token", "the refusal was not explained")
	assert.Contains(t, printed, "kip credentials allow", "the remedy was not named")
}

// The ordering nothing can tell apart: a revocation, then the old writer
// erasing it. Both reach pass two as an absent list, so the record wins and the
// revoked project comes back. It is pinned here because it is real, and the
// line the upgrade prints about it is the only thing that catches it.
func TestUpgradeRestoresARevocationTheOldWriterThenErased(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop")),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)

	require.NoError(t, sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		return sharedcred.Revoke(entries, "forge", []string{"shop"})
	}))
	erasedByTheOldWriter(t, clientset, "forge")

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"the documented behaviour changed: a revocation erased by the old writer now stays revoked")
	assert.Contains(t, out.String(), "revoked one of them during the upgrade",
		"the one notice that catches a revocation undone by the repair was not printed")
}

// A credential the old writer creates during the rollout was never shown to
// anybody, so deciding it as nobody is right. Doing it in silence is not: the
// marker goes on straight after and no later upgrade revisits a migrated
// cluster.
func TestUpgradeNamesACredentialItClosedThatNobodyWasShown(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop")),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)

	require.NoError(t, sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		return append(entries, sharedcred.Entry{Name: "later", Server: "git.example.com", Token: "t"}), nil
	}))

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	later := sharedcred.Find(storedEntries(t, clientset), "later")
	require.NotNil(t, later.AllowedProjects, "a credential nobody was shown was left undecided")
	assert.Empty(t, later.AllowedProjects, "a credential nobody was shown was granted something")
	assert.Contains(t, out.String(), "later", "the credential it decided was never named")
	assert.True(t, seeded(t, clientset))
}

// Without the Deployment there is nothing that says which pods are the
// console-api's, and its absence is not evidence that none are left: a
// Deployment can go before the pods it owns do.
func TestUpgradeHoldsTheMigrationOpenWithoutTheConsoleAPIDeployment(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, credentialGrants{mayClose: true}))

	assert.False(t, seeded(t, clientset),
		"the migration was closed without anything proving the old console-api had stopped")
	assert.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"a credential was decided by a pass that could not prove the old writer was gone")
}

// What runUpgrade defers on every exit. Pass two can fail on its first read,
// before it has written anything back, so the repair cannot be conditional on
// pass two having run.
func TestRepairErasedAllowListsWritesBackWithoutPassTwo(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		sharedCredentialSecret(t, curatedForge("shop")),
	)
	decided := sharedcred.Decisions(storedEntries(t, clientset))
	erasedByTheOldWriter(t, clientset, "forge")

	repairErasedAllowLists(ctx, clientset, &bytes.Buffer{}, decided)

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"a run that gave up before pass two wrote left the erased grant erased")
}

// Consent covers a credential at a host. An undecided credential re-pointed at
// a different server after the operator approved it is not the one they were
// shown, so the approval does not carry over to it — the same rule the repair
// follows, for the same reason.
func TestUpgradeDoesNotGrantACredentialRePointedAfterConsent(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision), consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "forge"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, true, false, refuseToConfirm(t))
	require.NoError(t, err)
	require.Equal(t, []string{"shop"}, grants.approved["forge"])

	require.NoError(t, sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		entries[0].Server = "git.other.example"
		return entries, nil
	}))

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	entry := storedEntries(t, clientset)[0]
	assert.NotNil(t, entry.AllowedProjects)
	assert.Empty(t, entry.AllowedProjects,
		"a grant approved for one host was written onto a credential bound to another")
	assert.Contains(t, out.String(), "re-pointed", "the dropped grant was not named")
}

// An approval covers a credential at a host holding a token, the same identity
// a repair checks. A credential deleted and recreated under its old name during
// the rollout is a different one, and the project being granted had no recorded
// grant at all before this run, so inference is the more sensitive of the two
// paths rather than the less.
func TestUpgradeDoesNotGrantACredentialGivenANewTokenAfterConsent(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "forge"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, true, false, refuseToConfirm(t))
	require.NoError(t, err)
	require.Equal(t, []string{"shop"}, grants.approved["forge"])

	require.NoError(t, sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		entries[0].Token = "a-different-token"
		return entries, nil
	}))

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	entry := storedEntries(t, clientset)[0]
	assert.NotNil(t, entry.AllowedProjects)
	assert.Empty(t, entry.AllowedProjects,
		"a grant approved for one credential was written onto the one that replaced it")
	assert.Contains(t, out.String(), "another token", "the dropped grant was not named")
}

// Undecided before the rollout is not the same as added during it. A credential
// that was simply never referenced is closed as nobody like any other, and
// telling the operator it arrived mid-upgrade would point them at the wrong
// cause for a refused build.
func TestUpgradeDoesNotCallAPreExistingCredentialNewlyAdded(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "archive", Server: "git.example.com", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)
	require.True(t, grants.mayClose)

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	entry := storedEntries(t, clientset)[0]
	assert.NotNil(t, entry.AllowedProjects)
	assert.Empty(t, entry.AllowedProjects, "an unreferenced credential was not decided as nobody")
	assert.NotContains(t, out.String(), "added while this upgrade was running",
		"a credential that existed before the upgrade was reported as having arrived during it")
	assert.True(t, seeded(t, clientset))
}

// A pod of the current revision that is on its way out is still a pod that can
// finish the request it holds, and the write it makes replaces the whole list.
// Deciding is permanent, so it waits for that pod too.
func TestUpgradeHoldsTheMigrationOpenWhileAConsoleAPIPodIsTerminating(t *testing.T) {
	shortQuiescence(t)
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeployment(),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api-new", false),
		consoleAPIPod("console-api-going", true),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, credentialGrants{mayClose: true}))

	assert.False(t, seeded(t, clientset),
		"the migration was closed while a pod inside its termination grace could still write")
	assert.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"a credential was decided by a pass that could not prove the old writer was gone")
}

// An approval for a credential that has since been removed lands nowhere. Said
// nowhere, it reads as a grant that went through, and the operator finds out
// when a build is refused.
func TestUpgradeNamesAnApprovedCredentialThatIsGone(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t,
			sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"},
			sharedcred.Entry{Name: "archive", Server: "git.example.com", Token: "another"},
		),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "forge"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, true, false, refuseToConfirm(t))
	require.NoError(t, err)
	require.Equal(t, []string{"shop"}, grants.approved["forge"])

	require.NoError(t, sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		kept := make([]sharedcred.Entry, 0, len(entries))
		for _, e := range entries {
			if e.Name != "forge" {
				kept = append(kept, e)
			}
		}
		return kept, nil
	}))

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	assert.Nil(t, sharedcred.Find(storedEntries(t, clientset), "forge"),
		"a credential removed during the rollout was put back")
	assert.Contains(t, out.String(), "no longer there",
		"an approval that landed nowhere was not reported")
}

// Unknown is the control plane saying it has lost touch with the node, not that
// nothing is running on it. Deciding is permanent, and what this pass has to
// establish is that nothing can write, so a pod it cannot account for counts.
func TestUpgradeHoldsTheMigrationOpenForAPodInAnUnknownState(t *testing.T) {
	shortQuiescence(t)
	ctx := context.Background()
	stranded := consoleAPIPodOfRevision("console-api-old", "old456", false)
	stranded.Status.Phase = corev1.PodUnknown
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeployment(),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIReplicaSet("1", "old456"),
		consoleAPIPod("console-api-new", false),
		stranded,
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, credentialGrants{mayClose: true}))

	assert.False(t, seeded(t, clientset),
		"the migration was closed while a pod nobody could account for might still be writing")
}

// A credential removed along with every other one leaves nothing to write into,
// and the pass returns early. The approval still landed nowhere, and saying so
// is the difference between an operator knowing and finding out at a build.
func TestUpgradeNamesAnApprovedCredentialWhenTheListIsEmptied(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "forge"), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, true, false, refuseToConfirm(t))
	require.NoError(t, err)
	require.Equal(t, []string{"shop"}, grants.approved["forge"])

	require.NoError(t, sharedcred.Update(ctx, clientset, func([]sharedcred.Entry) ([]sharedcred.Entry, error) {
		return nil, nil
	}))

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	assert.Contains(t, out.String(), "no longer there",
		"an approval with nothing left to write into was not reported")
	assert.Contains(t, out.String(), "forge", "the credential the approval named was not reported")
}

// A rollout that reports ready can still be a replica short: the pod that
// stamped can go, and its replacement may not have appeared yet. Reading that
// as a finished rollout would decide the lists against a fleet nobody has
// counted.
func TestUpgradeHoldsTheMigrationOpenWhileAReplicaIsMissing(t *testing.T) {
	shortQuiescence(t)
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeploymentOf(2),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api-one", false),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, credentialGrants{mayClose: true}))

	assert.False(t, seeded(t, clientset),
		"the migration was closed with one of the console-api's two pods missing")
	assert.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"a credential was decided by a pass that had not counted the fleet")
}

// Filling on the closing pass is filling something twice: the pass before the
// rollout already wrote it, so the list went back to nobody having decided it
// in between. A revocation made in that window looks identical from here, and
// this line is the only place it surfaces.
func TestUpgradeSaysAGrantItRefilledMayHaveUndoneARevocation(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	var out bytes.Buffer
	approved := map[string][]string{"forge": {"shop"}}

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, approving(t, clientset, approved, true)))

	require.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"))
	assert.Contains(t, out.String(), "revoke it again",
		"a grant written after the rollout was reported as an ordinary first grant")
}

// The repair has already said precisely why it left a list alone, with the
// projects and the commands. Following that with the generic notice tells the
// operator the wrong cause for the same credential in the same breath.
func TestUpgradeDoesNotRepeatWhatTheRepairAlreadyExplained(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemMigrated(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop")),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)

	require.NoError(t, sharedcred.Update(ctx, clientset, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		entries[0].AllowedProjects = nil
		entries[0].Server = "git.other.example"
		return entries, nil
	}))

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	printed := out.String()
	assert.Contains(t, printed, "bound to a different server", "the repair did not explain the refusal")
	assert.NotContains(t, printed, "The allowed projects are gone",
		"the same credential was given a second, contradicting explanation")
}

// The revision says which template a pod was made from, not which build the tag
// resolved to when it started. Two pods of one ReplicaSet can be running
// different ones, and the stamp that one of them wrote says nothing about the
// other, which may still be the writer that clears every allow-list.
func TestUpgradeHoldsTheMigrationOpenWhilePodsRunDifferentBuilds(t *testing.T) {
	shortQuiescence(t)
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeploymentOf(2),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api-new", false),
		consoleAPIPodRunning("console-api-stale", currentRevision,
			"containerd://ghcr.io/example/console-api@sha256:bbbb", false),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, credentialGrants{mayClose: true}))

	assert.False(t, seeded(t, clientset),
		"the migration was closed while a pod of the same revision was running another build")
}

// Two runtimes name one image differently either side of "://", and that
// difference says nothing about what is running.
func TestUpgradeIgnoresTheRuntimePrefixOnAnImage(t *testing.T) {
	shortQuiescence(t)
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeploymentOf(2),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPodRunning("console-api-one", currentRevision, "containerd://"+currentImage, false),
		consoleAPIPodRunning("console-api-two", currentRevision, "docker-pullable://"+currentImage, false),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, credentialGrants{mayClose: true}))

	assert.True(t, seeded(t, clientset),
		"one image named by two runtimes was read as two builds")
}

// A pod whose node has not reported what it is running is a pod the upgrade
// cannot vouch for, and deciding is permanent.
func TestUpgradeHoldsTheMigrationOpenWhileAPodReportsNoImage(t *testing.T) {
	shortQuiescence(t)
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeployment(),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPodRunning("console-api-quiet", currentRevision, "", false),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, credentialGrants{mayClose: true}))

	assert.False(t, seeded(t, clientset),
		"the migration was closed while a pod had not said what it was running")
}

// An autoscaler moving the replica count during the wait must not leave a
// converged Deployment failing a comparison it can never satisfy.
func TestUpgradeClosesWhenTheFleetHasGrownSinceTheWaitBegan(t *testing.T) {
	shortQuiescence(t)
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeploymentOf(1),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api-one", false),
		consoleAPIPod("console-api-two", false),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, credentialGrants{mayClose: true}))

	assert.True(t, seeded(t, clientset),
		"a surge of current-revision pods with nothing lingering was read as a rollout still in progress")
}

// An autoscaler moving the replica count while the wait is polling. Reading the
// target once would leave a Deployment that has since converged failing a
// comparison it can never satisfy, and the upgrade would wait that out on a
// healthy cluster.
func TestUpgradeRereadsTheReplicaTargetWhileItWaits(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeploymentOf(2),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api-one", false),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	// The first read sees the fleet it was scaled to; by the second, the
	// autoscaler has taken it down to the one pod that is actually there.
	reads := 0
	clientset.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads == 1 {
			return true, consoleAPIDeploymentOf(2), nil
		}
		return true, consoleAPIDeploymentOf(1), nil
	})

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, credentialGrants{mayClose: true}))

	assert.Greater(t, reads, 1, "the replica target was read once and never again")
	assert.True(t, seeded(t, clientset),
		"a Deployment that had converged to one replica was still measured against two")
}

// The build stamp is an annotation, and it outlives the pod that wrote it. A
// rollback to the console-api being replaced, landing while this waits, would
// otherwise converge on the old build with the stamp still vouching for it, and
// the migration would be recorded against the very writer it waits out.
//
// The second poll is quiescent on its own terms — one pod, no lingering, one
// image — so the only thing that can hold it open is noticing that the rollout
// underneath moved.
func TestUpgradeHoldsTheMigrationOpenWhenTheConsoleAPIRollsAgain(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeployment(),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	polls := 0
	clientset.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
		polls++
		items := []appsv1.ReplicaSet{*consoleAPIReplicaSet("2", currentRevision)}
		if polls > 1 {
			// The rollback, now the Deployment's current revision.
			items = append(items, *consoleAPIReplicaSet("3", "rolledback"))
		}
		return true, &appsv1.ReplicaSetList{Items: items}, nil
	})
	clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		if polls <= 1 {
			// Nothing up yet, so the first poll settles on the stamped revision
			// and comes round again.
			return true, &corev1.PodList{}, nil
		}
		return true, &corev1.PodList{Items: []corev1.Pod{
			*consoleAPIPodOfRevision("console-api-old", "rolledback", false),
		}}, nil
	})
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, credentialGrants{mayClose: true}))

	assert.False(t, seeded(t, clientset),
		"the migration was recorded against a console-api that rolled again while this waited")
	assert.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"a credential was decided by a pass that had followed a different rollout")
}

// The branch that runs when the console-api has not recorded its build: it may
// not decide anything, but a list the old writer erased is still this upgrade's
// to put back.
func TestUpgradeRepairsWhileTheOldWriterIsStillServing(t *testing.T) {
	noStampWait(t)
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystem(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, curatedForge("shop")),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), owningProject("shop-prod"))
	var out bytes.Buffer

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, false, false, refuseToConfirm(t))
	require.NoError(t, err)

	erasedByTheOldWriter(t, clientset, "forge")

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, grants))

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"a curated list erased during the rollout stayed erased because the console-api had not stamped yet")
	assert.False(t, seeded(t, clientset), "the migration closed against a writer that still erases lists")
}

// A rollback that lands before the wait has even looked. The pin is taken when
// the build stamp is read, so a pin that established itself on the first poll
// would adopt the rollback as the rollout to follow and close the migration
// against it.
func TestUpgradeHoldsTheMigrationOpenWhenTheConsoleAPIRolledBeforeTheWait(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeployment(),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	reads := 0
	clientset.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		items := []appsv1.ReplicaSet{*consoleAPIReplicaSet("2", currentRevision)}
		if reads > 1 {
			// By the time the wait polls, the rollback is current.
			items = append(items, *consoleAPIReplicaSet("3", "rolledback"))
		}
		return true, &appsv1.ReplicaSetList{Items: items}, nil
	})
	clientset.PrependReactor("list", "pods", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.PodList{Items: []corev1.Pod{
			*consoleAPIPodOfRevision("console-api-old", "rolledback", false),
		}}, nil
	})
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out, credentialGrants{mayClose: true}))

	assert.False(t, seeded(t, clientset),
		"the migration was recorded against a rollback that landed before the wait began")
}

// The late write the defer exists for, landing after the closing pass checked.
// An approved grant is a decision this run made, and losing it is the same
// accident as losing a curated one, so the repair has to cover both.
func TestUpgradeRepairsAnApprovedGrantClearedAfterTheFinalCheck(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(), consoleAPIDeployment(), consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		gitApp("shop-prod", "forge"), owningProject("shop-prod"))

	grants, err := credentialSeedConsent(ctx, clientset, dyn, &bytes.Buffer{}, true, false, refuseToConfirm(t))
	require.NoError(t, err)
	require.Equal(t, []string{"shop"}, grants.approved["forge"])

	require.NoError(t, seedSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, grants))
	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &bytes.Buffer{}, grants))
	require.True(t, seeded(t, clientset))

	// The old pod's write, arriving after the closing pass had confirmed its own.
	erasedByTheOldWriter(t, clientset, "forge")

	// What runUpgrade defers on every exit.
	repairErasedAllowLists(ctx, clientset, &bytes.Buffer{}, grants.repairRecord())

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"a grant this upgrade made was lost to a late write the repair could not reach")
}

// A Deployment deleted and recreated under the same name during the wait. The
// revision hash can coincide, because an identical template hashes identically,
// so the object is what tells the two apart.
func TestUpgradeHoldsTheMigrationOpenWhenTheConsoleAPIWasReplaced(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeployment(),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	var out bytes.Buffer

	// The upgrade rolled one Deployment; the object answering now is another.
	rolled := consoleAPIRollout{uid: "a-deployment-that-has-gone", hash: currentRevision}

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out,
		credentialGrants{mayClose: true, rolled: rolled}))

	assert.False(t, seeded(t, clientset),
		"the migration was recorded against a console-api Deployment that had been replaced")
	assert.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"a credential was decided by a pass following a Deployment it never rolled")
}

// The ordinary path: the rollout the upgrade recorded is the one still serving.
func TestUpgradeClosesAgainstTheRolloutItRecorded(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeployment(),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &bytes.Buffer{},
		credentialGrants{mayClose: true, rolled: consoleAPIRollout{uid: "console-api-uid", hash: currentRevision}}))

	assert.True(t, seeded(t, clientset),
		"the migration stayed open against the very rollout the upgrade put there")
}

// An upgrade that rolled console-api and could not say which rollout resulted.
// Everything after that moment is time a rollback could have landed in, so
// looking the rollout up at the end would find the rollback and wait for it.
// The two failures are opposite answers and must not share a zero value.
func TestUpgradeHoldsTheMigrationOpenWhenItCouldNotRecordWhatItRolled(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeployment(),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)
	var out bytes.Buffer

	// It rolled console-api; the pin that follows failed, so nothing was
	// recorded. The cluster is otherwise perfectly quiescent, so the only thing
	// that can hold the migration open is refusing to look the rollout up now.
	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &out,
		credentialGrants{mayClose: true, rolledConsoleAPI: true}))

	assert.False(t, seeded(t, clientset),
		"the migration was closed against a rollout this upgrade could not identify")
	assert.Contains(t, out.String(), "could not record which console-api it rolled",
		"the operator was not told why the migration stayed open")
	assert.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"a credential was decided by a pass that could not tell what was serving")
}

// A run that never rolled console-api has replaced nothing, so it may look up
// what is serving and wait for that.
func TestUpgradeClosesWhenItRolledNoConsoleAPIAtAll(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		consoleAPIDeployment(),
		consoleAPIReplicaSet("2", currentRevision),
		consoleAPIPod("console-api", false),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Server: "git.example.com", Token: "a-token"}),
	)

	require.NoError(t, closeSharedCredentialGrants(ctx, clientset, &bytes.Buffer{},
		credentialGrants{mayClose: true}))

	assert.True(t, seeded(t, clientset),
		"a run that replaced nothing was still refused the rollout in front of it")
}
