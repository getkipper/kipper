package cmd

import (
	"bytes"
	"context"
	goerrors "errors"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// A namespace resolves to a project through the label for one more release, and
// through the project's claim on the object after that. Between the two, a
// namespace labelled for a project that does not claim it is harmless and
// invisible; once the claim is required it is a namespace whose members are
// locked out of it.
//
// So the operator gets to see the list while it is still harmless. This is the
// readiness evidence for the release that requires the claim, and the drift
// report for the one that does not.
// Every fixture here is one project, named once rather than passed in.
const reportProject = "shop"

func projectClaiming(claims ...map[string]any) *unstructured.Unstructured {
	status := map[string]any{}
	if len(claims) > 0 {
		list := make([]any, 0, len(claims))
		for _, claim := range claims {
			list = append(list, claim)
		}
		status["namespaceClaims"] = list
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Project",
		"metadata":   map[string]any{"name": reportProject},
		"status":     status,
	}}
}

func claimOf(namespace, uid string) map[string]any {
	return map[string]any{"name": namespace, "uid": uid}
}

func namespaceWithUID(name, project, uid string) runtime.Object {
	ns := namespaceOfProject(name, project)
	ns.UID = k8stypes.UID(uid)
	return ns
}

func TestTheUpgradeNamesANamespaceItsProjectDoesNotClaim(t *testing.T) {
	clientset := k8sfake.NewClientset(
		namespaceWithUID("shop-prod", "shop", "the-object"),
		namespaceWithUID("shop-test", "shop", "the-other-object"),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(),
		projectClaiming(claimOf("shop-test", "the-other-object")))
	var out bytes.Buffer

	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, 0)

	assert.Contains(t, out.String(), "shop-prod", "the namespace its project does not claim was not named")
	assert.NotContains(t, out.String(), "shop-test", "a claimed namespace was reported as drifted")
}

// A cluster whose claims are all in place prints nothing. The report is read as
// "you are ready", so anything it says on a healthy cluster is noise that trains
// the operator to skip it.
func TestTheUpgradeSaysNothingWhenEveryNamespaceIsClaimed(t *testing.T) {
	clientset := k8sfake.NewClientset(namespaceWithUID("shop-prod", "shop", "the-object"))
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(),
		projectClaiming(claimOf("shop-prod", "the-object")))
	var out bytes.Buffer

	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, 0)

	assert.Empty(t, out.String(), "a fully claimed cluster was told it had work to do: %s", out.String())
}

// A claim names an object, not a name. One naming a namespace that has since
// been deleted and recreated covers nothing, and the report has to say so or it
// reports a cluster ready that the strict release will refuse.
func TestTheUpgradeNamesANamespaceWhoseClaimIsForAnObjectThatIsGone(t *testing.T) {
	clientset := k8sfake.NewClientset(namespaceWithUID("shop-prod", "shop", "the-new-object"))
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(),
		projectClaiming(claimOf("shop-prod", "the-old-object")))
	var out bytes.Buffer

	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, 0)

	assert.Contains(t, out.String(), "shop-prod", "a claim naming an object that is gone was read as coverage")
}

// A namespace labelled for a project that does not exist is drift of a
// different kind, and naming the project it points at is the only way an
// operator can act on it.
//
// It is also the one case waiting cannot resolve, so it is said differently.
// The other list is namespaces the controller has not recorded yet; this one is
// namespaces no controller will ever record, because the project that would
// have run the pass is gone. A project deleted while another replica was
// mid-pass creating a namespace for it leaves exactly this, and nothing on the
// cluster collects it afterwards.
func TestTheUpgradeNamesANamespaceWhoseProjectIsGone(t *testing.T) {
	clientset := k8sfake.NewClientset(namespaceWithUID("orphan-prod", "orphan", "the-object"))
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme())
	var out bytes.Buffer

	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, 0)

	assert.Contains(t, out.String(), "orphan-prod")
	assert.Contains(t, out.String(), "orphan", "the project the namespace points at was not named")
	assert.Contains(t, out.String(), "Nothing collects these",
		"an orphaned namespace was reported as one the console has not recorded yet, so the operator waits for a record no controller will write")
	assert.NotContains(t, out.String(), "Not yet recorded",
		"the orphan was also counted as pending, which tells the operator two different things about one namespace")
}

// Reading the projects is what the answer rests on, so a failure to read them
// is reported rather than printed as an empty, reassuring list.
func TestTheUpgradeSaysWhenItCouldNotCheck(t *testing.T) {
	clientset := k8sfake.NewClientset(namespaceWithUID("shop-prod", "shop", "the-object"))
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme())
	dyn.PrependReactor("list", "projects", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, goerrors.New("the API server said no")
	})
	var out bytes.Buffer

	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, 0)

	assert.Contains(t, out.String(), "Could not check")
}

func TestANamespaceWithNoProjectLabelIsNotTheReportsBusiness(t *testing.T) {
	plain := namespaceOfProject("kube-system", "")
	delete(plain.Labels, "kipper.run/project")
	clientset := k8sfake.NewClientset(plain)
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme())
	var out bytes.Buffer

	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, 0)

	assert.Empty(t, out.String())
}

// A cluster that never ran this upgrade has no claims and every project still
// carries the namespaces it took before claims existed. That cluster is ready,
// because the resolver reads that record too, and reporting its whole namespace
// list as about to be lost is both wrong and the fastest way to teach an
// operator to skip the report.
func TestTheUpgradeSaysNothingAboutAClusterThatOnlyHasTheOlderRecord(t *testing.T) {
	clientset := k8sfake.NewClientset(namespaceWithUID("shop-prod", "shop", "the-object"))
	shop := projectClaiming()
	shop.Object["status"].(map[string]any)["namespaces"] = []any{"shop-prod"}
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), shop)
	var out bytes.Buffer

	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, 0)

	assert.Empty(t, out.String(),
		"a cluster carrying only the pre-claims record was told its namespaces were about to stop answering to anybody: %s", out.String())
}

// The older record is not a blank cheque. A claim naming the namespace at a
// different object says the object was replaced, and the record must not answer
// over it.
func TestTheUpgradeStillNamesANamespaceWhoseClaimIsStaleEvenWithTheOlderRecord(t *testing.T) {
	clientset := k8sfake.NewClientset(namespaceWithUID("shop-prod", "shop", "the-new-object"))
	shop := projectClaiming(claimOf("shop-prod", "the-old-object"))
	shop.Object["status"].(map[string]any)["namespaces"] = []any{"shop-prod"}
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), shop)
	var out bytes.Buffer

	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, 0)

	assert.Contains(t, out.String(), "shop-prod",
		"the older record answered over a claim that names a different object")
}

// A namespace that is already terminating is being collected right now, so
// saying nothing collects it sends an operator to delete something that is
// going anyway. The reconciler deletes a project's namespaces and takes its
// finalizer off without waiting for them to finish, so every ordinary project
// delete passes through exactly this state.
func TestTheUpgradeLeavesATerminatingNamespaceAlone(t *testing.T) {
	going := namespaceWithUID("shop-prod", "shop", "the-object").(*corev1.Namespace)
	now := metav1.Now()
	going.DeletionTimestamp = &now
	going.Finalizers = []string{"kubernetes"}

	clientset := k8sfake.NewClientset(going)
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme())
	var out bytes.Buffer

	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, 0)

	assert.NotContains(t, out.String(), "shop-prod",
		"a namespace Kubernetes is already collecting was reported as one nothing collects, so the operator is sent to delete it by hand")
}

// A namespace can become orphaned during the settle, which is the interleaving
// this list exists for: the pass that would have recorded it is the one whose
// project went. Reporting the first orphan and then falling silent loses
// exactly the case the report was added for.
func TestTheUpgradeNamesAnOrphanThatAppearsDuringTheSettle(t *testing.T) {
	clientset := k8sfake.NewClientset(
		namespaceWithUID("old-orphan", "gone", "the-old-object"),
		namespaceWithUID("shop-prod", "shop", "the-object"),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), projectClaiming())

	// The second look finds shop's project deleted too.
	reads := 0
	dyn.PrependReactor("list", "projects", func(k8stesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads > 1 {
			return true, &unstructured.UnstructuredList{}, nil
		}
		return false, nil, nil
	})

	var out bytes.Buffer
	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, claimsSettlePoll)

	assert.Contains(t, out.String(), "old-orphan", "the orphan present on the first look was not named")
	assert.Contains(t, out.String(), "shop-prod",
		"a namespace orphaned during the settle was never named, because reporting one orphan silenced the rest")
}

// The same for a terminating namespace whose project is still there. Nothing
// will ever record a namespace that is being deleted, so calling it unrecorded
// costs the operator the full settle and then points them at the console-api
// logs for something Kubernetes is already collecting.
func TestTheUpgradeLeavesATerminatingNamespaceAloneWhileItsProjectStands(t *testing.T) {
	going := namespaceWithUID("shop-test", "shop", "the-object").(*corev1.Namespace)
	now := metav1.Now()
	going.DeletionTimestamp = &now
	going.Finalizers = []string{"kubernetes"}

	clientset := k8sfake.NewClientset(going)
	// The project stands and its records do not name the namespace, because the
	// environment was removed before it finished terminating.
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme(), projectClaiming())
	var out bytes.Buffer

	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, 0)

	assert.NotContains(t, out.String(), "shop-test",
		"a namespace being deleted was reported as one its project has not recorded, which no pass will ever do")
}

// An orphan is confirmed against the API server before it is named. The two
// lists this works from are consecutive snapshots, and the reconciler deletes a
// project's namespaces before it takes the finalizer off, so a project deleted
// between them shows namespaces not yet terminating beside a project already
// gone.
func TestTheUpgradeChecksAnOrphanIsStillThereBeforeNamingIt(t *testing.T) {
	clientset := k8sfake.NewClientset(namespaceWithUID("shop-prod", "shop", "the-object"))
	// The namespace starts terminating between the list and the confirmation.
	clientset.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		going := namespaceWithUID("shop-prod", "shop", "the-object").(*corev1.Namespace)
		now := metav1.Now()
		going.DeletionTimestamp = &now
		return true, going, nil
	})
	dyn := dynamicfake.NewSimpleDynamicClient(projectScheme())
	var out bytes.Buffer

	reportNamespacesWithoutAClaim(context.Background(), clientset, dyn, &out, 0)

	assert.NotContains(t, out.String(), "Nothing collects these",
		"a namespace that had started terminating by the time it was named was reported as one nothing collects")
}
