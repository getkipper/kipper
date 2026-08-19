package cmd

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
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
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "kipper",
			"kipper.run/project":           project,
		},
	}}
}

// The defect this exists for: a cluster installed before allow-lists denies every
// project, so the first rebuild after an upgrade fails on an app that was
// building perfectly well the day before.
func TestUpgradeSeedsAnEmptyAllowListFromWhatIsAlreadyBuilding(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))
	var out bytes.Buffer

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, dyn, &out))

	entries := storedEntries(t, clientset)
	require.Len(t, entries, 1)
	assert.True(t, entries[0].AllowsProject("shop"),
		"the upgrade left an app that was building denied: %v", entries[0].AllowedProjects)
	assert.Contains(t, out.String(), "forge")
}

// An app's own credential is not on the shared list, so nothing an app names for
// itself can invent a grant.
func TestUpgradeIgnoresACredentialThatIsNotShared(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "web-git-credentials"))

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))

	entries := storedEntries(t, clientset)
	assert.Empty(t, entries[0].AllowedProjects,
		"a credential the app names for itself was treated as a shared one")
}

// Membership of the list decides, in the same order the builder decides it. A
// shared credential that happens to be named the way an app of that name would
// name its own is still shared, and skipping it would leave exactly that app
// refused after the upgrade that was meant to keep it building.
func TestUpgradeSeedsASharedCredentialNamedLikeAnAppsOwn(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "web-git-credentials", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "web-git-credentials"))

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))

	entries := storedEntries(t, clientset)
	assert.True(t, entries[0].AllowsProject("shop"),
		"the builder resolves this name from the shared list, so the seeding has to as well")
}

func TestUpgradeLeavesACuratedAllowListAlone(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		namespaceOfProject("blog-prod", "blog"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token", AllowedProjects: []string{"blog"}}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))

	entries := storedEntries(t, clientset)
	assert.False(t, entries[0].AllowsProject("shop"),
		"an upgrade widened an allow-list an admin had set")
}

// A namespace outside any project cannot name one, and an upgrade that stopped
// there would refuse to run on a cluster with a stray namespace.
func TestUpgradeSkipsAnAppInANamespaceWithNoProject(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "stray"}},
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("stray", "forge"))

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))

	entries := storedEntries(t, clientset)
	assert.Empty(t, entries[0].AllowedProjects)
}

// Nothing to seed is the normal case on a cluster installed since allow-lists
// existed, and it must not print a step that did nothing.
func TestUpgradeSaysNothingWhenThereIsNothingToSeed(t *testing.T) {
	clientset := k8sfake.NewClientset(kipperSystemUpgraded(), sharedCredentialSecret(t,
		sharedcred.Entry{Name: "forge", Token: "a-token", AllowedProjects: []string{"shop"}}))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())
	var out bytes.Buffer

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, dyn, &out))

	assert.Empty(t, out.String())
}

// A cluster with no shared credentials at all must not have the list Secret
// created for it by an upgrade.
func TestUpgradeDoesNotCreateAListForAClusterWithNoSharedCredentials(t *testing.T) {
	clientset := k8sfake.NewClientset(kipperSystemUpgraded(), namespaceOfProject("shop-prod", "shop"))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))

	_, err := clientset.CoreV1().Secrets(sharedcred.Namespace).Get(
		context.Background(), sharedcred.ConfigSecretName, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "the upgrade created a shared-credential list that did not exist")
}

// The namespace as a cluster whose console-api still replaces an allow-list
// leaves it: no build stamped on it.
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

// The migration ran once per credential, which is not the same as once per
// cluster. Between the seeding and the console-api restart the old writer is
// still serving, and a credential it creates in that window carries no
// allow-list at all, so the next ordinary upgrade would read it as predating
// the guard and grant it whatever names it. A reference is not a grant.
func TestUpgradeSeedsOncePerCluster(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token", AllowedProjects: []string{"blog"}}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "later"))

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))
	require.True(t, seeded(t, clientset), "the cluster was not recorded as migrated")

	// What the old console-api writes in the window: an entry with no list.
	require.NoError(t, sharedcred.Update(context.Background(), clientset,
		func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
			return append(entries, sharedcred.Entry{Name: "later", Token: "another-token"}), nil
		}))

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))

	entries := storedEntries(t, clientset)
	for _, entry := range entries {
		if entry.Name == "later" {
			assert.Empty(t, entry.AllowedProjects,
				"an app reference became a grant on a credential created after the migration")
		}
	}
}

// A cluster with no shared credentials is migrated too, and recording that is
// the whole point: otherwise its first credential, whenever it arrives, is
// still a candidate for the inference.
func TestUpgradeRecordsTheMigrationOnAClusterWithNoSharedCredentials(t *testing.T) {
	clientset := k8sfake.NewClientset(kipperSystemUpgraded(), namespaceOfProject("shop-prod", "shop"))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))

	assert.True(t, seeded(t, clientset), "a cluster with nothing to seed was left seedable")
}

// The old console-api keeps serving until its pod is replaced, and its writer
// replaces an entry with the shape that carries no allow-list. A migration
// recorded before that is gone turns a rollout race into a build refused for
// good, because every later upgrade reads the marker and leaves the credential
// alone. Seeding twice is what repairs it, and only the second pass closes the
// migration.
func TestUpgradeRepairsAGrantTheOldWriterErasedDuringTheRollout(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))
	require.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"))
	require.False(t, seeded(t, clientset), "the migration was closed while the old writer was still serving")

	// What the old writer does with a token rotation: the entry it sends back
	// carries no allow-list, and its own shape stores that as no list at all.
	require.NoError(t, sharedcred.Update(context.Background(), clientset,
		func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
			entries[0].AllowedProjects = nil
			return entries, nil
		}))

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))

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
		kipperSystem(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"the grant that keeps builds working was not written")
	assert.False(t, seeded(t, clientset),
		"the migration was closed against a console-api that still erases allow-lists")
}

// The pass that runs while the old writer is still serving fills grants and
// decides nothing. A credential nothing references yet is one an app may be
// pointed at a second later, under rules that still allow it, and a decision
// recorded now is one the later pass will not revisit.
func TestUpgradeLeavesAnUnusedCredentialUndecidedBeforeTheRollout(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemUpgraded(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())

	require.NoError(t, seedSharedCredentialGrants(context.Background(), clientset, dyn, &bytes.Buffer{}))
	require.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"a credential nothing referenced yet was decided before the rollout")

	// The app arrives during the rollout window, still building under the old
	// rules, and the pass that runs afterwards has to see it.
	withApp := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))
	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, withApp, &bytes.Buffer{}))

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"an app that was building through the rollout was left refused")
	assert.True(t, seeded(t, clientset))
}

// Deciding is what Seed will not revisit, so deciding against a console-api that
// still allows every build freezes a snapshot of a cluster still changing under
// the old rules. The pass that cannot see the new writer fills and stops there.
func TestUpgradeDecidesNothingWhileTheOldWriterServes(t *testing.T) {
	noStampWait(t)
	clientset := k8sfake.NewClientset(
		kipperSystem(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "archive", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, dyn, &out))

	require.Nil(t, storedEntries(t, clientset)[0].AllowedProjects,
		"a credential nothing references yet was decided against a writer that still allows every build")
	assert.False(t, seeded(t, clientset))
	assert.Contains(t, out.String(), "archive",
		"the upgrade left the migration open without saying which credentials are waiting")
}

// The app that arrives while the old writer is still serving, on a cluster whose
// upgrade could not finish. It builds fine under the old rules, and the upgrade
// that does replace the writer has to seed it rather than refuse it.
func TestUpgradeSeedsAnAppThatArrivedWhileTheMigrationCouldNotClose(t *testing.T) {
	noStampWait(t)
	clientset := k8sfake.NewClientset(
		kipperSystem(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "archive", Token: "a-token"}),
	)
	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset,
		dynamicfake.NewSimpleDynamicClient(appScheme()), &bytes.Buffer{}))

	// A week later: the app is pointed at the credential, and the next upgrade
	// delivers the console-api that keeps allow-lists.
	ns, err := clientset.CoreV1().Namespaces().Get(context.Background(), "kipper-system", metav1.GetOptions{})
	require.NoError(t, err)
	ns.Annotations = map[string]string{labels.AnnoConsoleAPIBuild: "v0.14.0"}
	_, err = clientset.CoreV1().Namespaces().Update(context.Background(), ns, metav1.UpdateOptions{})
	require.NoError(t, err)

	withApp := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "archive"))
	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, withApp, &bytes.Buffer{}))

	assert.True(t, storedEntries(t, clientset)[0].AllowsProject("shop"),
		"the upgrade refused an app that had been building since the one before it")
	assert.True(t, seeded(t, clientset))
}

// The stamp has to be evidence about the console-api serving now. Left from a
// build that served weeks ago, it would vouch for a writer an operator has since
// pinned back during an incident, and the upgrade would close the migration
// against the one that clears allow-lists.
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

// A cluster with no shared credentials has nothing waiting on a decision, and a
// warning about allow-lists it does not have is noise on every upgrade until the
// images publish.
func TestUpgradeSaysNothingAboutAMigrationWithNothingInIt(t *testing.T) {
	noStampWait(t)
	clientset := k8sfake.NewClientset(kipperSystem(), namespaceOfProject("shop-prod", "shop"))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, dyn, &out))

	assert.Empty(t, out.String())
}

// A pass that grants something and then says nothing was touched contradicts
// itself. What it reports is what is still waiting.
func TestUpgradeNamesOnlyTheCredentialsStillWaiting(t *testing.T) {
	noStampWait(t)
	clientset := k8sfake.NewClientset(
		kipperSystem(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t,
			sharedcred.Entry{Name: "forge", Token: "a-token"},
			sharedcred.Entry{Name: "archive", Token: "another-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, dyn, &out))

	printed := out.String()
	assert.Contains(t, printed, "archive", "the credential still waiting was not named")
	assert.NotContains(t, printed, "Still to decide who may build with forge",
		"a credential this pass granted was reported as still waiting")
}

// On a migrated cluster every writer records a decision, so a credential with no
// list at all has been edited by an older console-api or restored from an older
// copy. The upgrade says so and grants nothing: filling it from the apps that
// reference it would grant a project that had been revoked while its app still
// named the credential, which is the same shape.
func TestUpgradeNamesACredentialWhoseAllowListWasCleared(t *testing.T) {
	clientset := k8sfake.NewClientset(
		kipperSystemMigrated(),
		namespaceOfProject("shop-prod", "shop"),
		sharedCredentialSecret(t, sharedcred.Entry{Name: "forge", Token: "a-token"}),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge"))
	var out bytes.Buffer

	require.NoError(t, closeSharedCredentialGrants(context.Background(), clientset, dyn, &out))

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

	err := closeSharedCredentialGrants(context.Background(), clientset,
		dynamicfake.NewSimpleDynamicClient(appScheme()), &out)

	require.NoError(t, err, "an advisory read failure stopped the upgrade")
	assert.Contains(t, out.String(), "Could not check", "a skipped check passed as a clean run")
}

// The same softening on the other path, which the twin test cannot reach: there
// the report's read is the third, after the fill's own read and its write, so a
// reactor that fails every read kills the fill instead.
func TestUpgradeSurvivesAFailureToReadTheListForTheOpenMigrationReport(t *testing.T) {
	noStampWait(t)
	clientset := k8sfake.NewClientset(
		kipperSystem(),
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

	err := closeSharedCredentialGrants(context.Background(), clientset,
		dynamicfake.NewSimpleDynamicClient(appScheme(), gitApp("shop-prod", "forge")), &out)

	require.NoError(t, err, "an advisory read failure stopped the upgrade before its trust material")
	assert.Contains(t, out.String(), "Could not check", "a skipped check passed as a clean run")
}
