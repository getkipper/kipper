package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stypes "k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/memberbinding"
)

func binding(namespace, name string) *rbacv1.RoleBinding {
	return &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
}

// A live namespace labelled for a project, which is where the resolver starts.
// Its UID is what a claim has to match: a name outlives the object that carried
// it, and the label is writable by anyone who can write a namespace, so neither
// answers on its own.
func liveNamespace(name, project string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: name, UID: k8stypes.UID(name + "-uid"),
		Labels: map[string]string{labels.Project: project},
	}}
}

// A project whose claims name these objects by name and UID, which is the
// record the resolver actually reads.
func shopClaiming(namespaces ...string) *unstructured.Unstructured {
	p := projectCR("shop")
	claims := make([]any, 0, len(namespaces))
	for _, n := range namespaces {
		claims = append(claims, map[string]any{"name": n, "uid": n + "-uid"})
	}
	p.Object["status"] = map[string]any{"namespaceClaims": claims}
	return p
}

// The project these fixtures turn on, holding the namespaces named on its older
// name-only record.
func shopHolding(namespaces ...string) *unstructured.Unstructured {
	p := projectCR("shop")
	ns := make([]any, 0, len(namespaces))
	for _, n := range namespaces {
		ns = append(ns, n)
	}
	p.Object["status"] = map[string]any{"namespaces": ns}
	return p
}

// The listing exists because a binding outlives the thing that explains it: a
// project deleted while the console was down leaves grants in namespaces with
// nothing left pointing at them, and nothing else on the cluster says so.
func TestUnclaimedBindingsNamesOnlyTheOnesNoProjectExplains(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		liveNamespace("shop-prod", "shop"), liveNamespace("gone-prod", "gone"),
		binding("shop-prod", memberbinding.Name("shop", "owner")),
		binding("gone-prod", memberbinding.Name("gone", "deployer")),
		// Somebody else's object, under a name this does not generate.
		binding("shop-prod", "team-readonly"),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), shopHolding("shop-prod"))
	var out bytes.Buffer

	require.NoError(t, reportUnclaimedBindings(ctx, clientset, dyn, &out))

	printed := out.String()
	assert.Contains(t, printed, memberbinding.Name("gone", "deployer"),
		"a binding whose project is gone was not named")
	assert.Contains(t, printed, "gone-prod", "the namespace holding it was not named")
	assert.NotContains(t, printed, memberbinding.Name("shop", "owner"),
		"a binding a live project explains was named as unclaimed")
	assert.NotContains(t, printed, "team-readonly",
		"an object this does not manage was named as one of ours")
}

// A legacy name carries no project digest, so the name alone cannot say whose
// it is. The namespace's own owner is what explains it, and one in a namespace
// no project holds is as unclaimed as any other.
func TestUnclaimedBindingsHandlesTheLegacyNames(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		liveNamespace("shop-prod", "shop"), liveNamespace("orphan-prod", "orphan"),
		binding("shop-prod", "kipper-project-owner"),
		binding("orphan-prod", "kipper-project-viewer"),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(),
		shopHolding("shop-prod"))
	var out bytes.Buffer

	require.NoError(t, reportUnclaimedBindings(ctx, clientset, dyn, &out))

	printed := out.String()
	assert.Contains(t, printed, "orphan-prod",
		"a legacy binding in a namespace no project holds was not named")
	assert.NotContains(t, strings.SplitN(printed, "orphan-prod", 2)[0], "shop-prod",
		"a legacy binding in a namespace its project holds was named as unclaimed")
}

// Saying nothing is the answer on a healthy cluster, and it has to be a quiet
// nothing: an operator who runs this on every cluster should be able to tell
// the clean ones apart at a glance.
func TestUnclaimedBindingsSaysSoWhenThereAreNone(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(liveNamespace("shop-prod", "shop"), binding("shop-prod", memberbinding.Name("shop", "owner")))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), shopHolding("shop-prod"))
	var out bytes.Buffer

	require.NoError(t, reportUnclaimedBindings(ctx, clientset, dyn, &out))

	assert.Contains(t, out.String(), "Every Kipper role binding belongs to a project")
}

// A namespace that changed hands, or an environment removed while cleanup was
// interrupted, leaves the project that wrote the binding alive and the grant
// behind in a namespace it no longer holds. That is the case this command
// exists for, and checking only that the project still exists would report it
// as healthy.
func TestUnclaimedBindingsNamesAGrantLeftInANamespaceItsProjectLost(t *testing.T) {
	ctx := context.Background()
	// The namespace shop no longer has: cleanup took its label off and stopped
	// there. The live gate reads no owner for it, so shop cannot reach it
	// through the console while its binding still grants Kubernetes access.
	orphaned := liveNamespace("was-shops", "shop")
	orphaned.Labels = nil
	clientset := k8sfake.NewClientset(
		liveNamespace("shop-prod", "shop"), orphaned,
		binding("shop-prod", memberbinding.Name("shop", "owner")),
		binding("was-shops", memberbinding.Name("shop", "deployer")),
	)
	// shop is alive and still holds shop-prod. It no longer holds was-shops.
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), shopHolding("shop-prod"))
	var out bytes.Buffer

	require.NoError(t, reportUnclaimedBindings(ctx, clientset, dyn, &out))

	printed := out.String()
	assert.Contains(t, printed, "was-shops",
		"a grant left in a namespace its project no longer holds was reported as healthy")
	assert.NotContains(t, printed, "shop-prod",
		"a grant in a namespace its project still holds was named as unclaimed")
}

// A claim is the record the resolver reads, and it names the object rather than
// the name. A binding in a namespace its project claims is explained.
func TestUnclaimedBindingsAcceptsAClaimOnTheLiveObject(t *testing.T) {
	ctx := context.Background()
	clientset := k8sfake.NewClientset(
		liveNamespace("shop-prod", "shop"),
		binding("shop-prod", memberbinding.Name("shop", "owner")),
	)
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), shopClaiming("shop-prod"))
	var out bytes.Buffer

	require.NoError(t, reportUnclaimedBindings(ctx, clientset, dyn, &out))

	assert.Contains(t, out.String(), "Every Kipper role binding belongs to a project",
		"a binding in a namespace its project claims was named as unclaimed")
}

// A claim naming the namespace at a different object is a claim about something
// that has gone. The name matching is not enough.
// A namespace recreated outside Kipper keeps its label and leaves the project's
// claim pointing at the object that is gone. The gate in this release reads the
// label, so the project still reaches the namespace and its bindings still do
// what they say. Listing them as belonging to nobody would send an operator to
// delete grants that are working.
func TestUnclaimedBindingsLeavesAGrantTheGateStillHonoursAlone(t *testing.T) {
	ctx := context.Background()
	replaced := liveNamespace("shop-prod", "shop")
	replaced.UID = "a-different-object"
	clientset := k8sfake.NewClientset(replaced, binding("shop-prod", memberbinding.Name("shop", "owner")))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), shopClaiming("shop-prod"))
	var out bytes.Buffer

	require.NoError(t, reportUnclaimedBindings(ctx, clientset, dyn, &out))

	assert.NotContains(t, out.String(), "delete rolebinding",
		"a binding the gate still honours was offered up for deletion")
	assert.NotContains(t, out.String(), memberbinding.Name("shop", "owner"),
		"a binding the gate still honours was listed as belonging to nobody")
}

// The same drift is worth seeing even though its bindings are not orphans: the
// claim heals on the next reconciler pass, and the release that stops reading
// the label takes the namespace's owner away if it has not.
func TestUnclaimedBindingsReportsANamespaceWhoseClaimNamesAnotherObject(t *testing.T) {
	ctx := context.Background()
	replaced := liveNamespace("shop-prod", "shop")
	replaced.UID = "a-different-object"
	clientset := k8sfake.NewClientset(replaced, binding("shop-prod", memberbinding.Name("shop", "owner")))
	both := shopClaiming("shop-prod")
	both.Object["status"].(map[string]any)["namespaces"] = []any{"shop-prod"}
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme(), both)
	var out bytes.Buffer

	require.NoError(t, reportUnclaimedBindings(ctx, clientset, dyn, &out))

	assert.Contains(t, out.String(), "shop-prod (labelled shop)",
		"a namespace whose claim names a replaced object was not reported at all")
}

// An ordinary project delete removes the Project CR and leaves its namespaces
// finalizing. Every binding in them would read as belonging to nobody, and the
// operator would be sent to delete what is already going.
func TestUnclaimedBindingsSaysNothingAboutANamespaceBeingDeleted(t *testing.T) {
	ctx := context.Background()
	going := liveNamespace("shop-prod", "shop")
	going.DeletionTimestamp = &metav1.Time{Time: time.Unix(1, 0)}
	going.Finalizers = []string{"kubernetes"}
	clientset := k8sfake.NewClientset(going, binding("shop-prod", memberbinding.Name("shop", "owner")))
	dyn := dynamicfake.NewSimpleDynamicClient(appScheme())
	var out bytes.Buffer

	require.NoError(t, reportUnclaimedBindings(ctx, clientset, dyn, &out))

	assert.NotContains(t, out.String(), memberbinding.Name("shop", "owner"),
		"a binding in a namespace that is being deleted was reported as unclaimed")
}
