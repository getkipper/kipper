package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func TestParseYesNo(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		// affirmative — these are the only two we accept
		{"y", true},
		{"Y", true},
		{"yes", true},
		{"YES", true},
		{"Yes", true},
		{"  y  ", true},
		{"y\n", true},
		{"yes\r\n", true},

		// negative — explicit
		{"n", false},
		{"no", false},
		{"N", false},
		{"NO", false},

		// negative by default — anything ambiguous is treated as no so
		// a user pressing Enter without thinking does not approve a
		// system upgrade
		{"", false},
		{"\n", false},
		{"yep", false},
		{"yeah", false},
		{"ok", false},
		{"sure", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, parseYesNo(tt.input))
		})
	}
}

// crdWithVersions builds a CRD carrying the given served versions.
func crdWithVersions(served ...string) *unstructured.Unstructured {
	const name = "apps.kipper.run" // the CRD every case here uses
	versions := make([]interface{}, 0, len(served))
	for _, v := range served {
		versions = append(versions, map[string]interface{}{
			"name": v, "served": true, "storage": v == served[0],
		})
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "apiextensions.k8s.io/v1",
		"kind":       "CustomResourceDefinition",
		"metadata":   map[string]interface{}{"name": name},
		"spec":       map[string]interface{}{"group": "kipper.run", "versions": versions},
	}}
}

// rbacDynamic is a fake dynamic client that knows the RBAC and CRD kinds the
// upgrade path writes.
func rbacDynamic(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	sch := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRole"},
		{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "ClusterRoleBinding"},
		{Group: "", Version: "v1", Kind: "ServiceAccount"},
		{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"},
	} {
		sch.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		sch.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return dynamicfake.NewSimpleDynamicClient(sch, objects...)
}

// storedVersions is what decides whether removing a version strands data.
// Comparing served versions instead blocked the deprecation path the versioning
// plan prescribes: a release legitimately retiring an old version looked exactly
// like an older CLI carrying older CRDs.
func TestStoredVersionsDroppedBy(t *testing.T) {
	// The data-loss case: objects are stored under a version the incoming CRD
	// does not declare.
	live := crdWithVersions("v1beta1", "v1alpha1")
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1alpha1"}, "status", "storedVersions"))
	older := crdWithVersions("v1beta1")
	assert.Equal(t, []string{"v1alpha1"}, storedVersionsDroppedBy(live, older))

	// The legitimate retirement: nothing is stored under the version being
	// dropped, so it must be allowed.
	migrated := crdWithVersions("v1beta1", "v1alpha1")
	require.NoError(t, unstructured.SetNestedStringSlice(migrated.Object, []string{"v1beta1"}, "status", "storedVersions"))
	assert.Empty(t, storedVersionsDroppedBy(migrated, older),
		"a version with nothing stored under it can be retired")

	// Adding is always safe.
	assert.Empty(t, storedVersionsDroppedBy(older, live))
}

// A CRD that reports no storedVersions tells us nothing, so the declared set is
// the fallback and dropping an unproven version is refused.
func TestStoredVersionsDroppedBy_FallsBackWhenStatusIsAbsent(t *testing.T) {
	live := crdWithVersions("v1beta1", "v1alpha1")
	older := crdWithVersions("v1beta1")
	assert.Equal(t, []string{"v1alpha1"}, storedVersionsDroppedBy(live, older))
}

// A version present but unserved still has to be declared, because Kubernetes
// needs its schema to decode what is stored.
func TestCrdVersionNames_SeparatesServedFromDeclared(t *testing.T) {
	crd := &unstructured.Unstructured{Object: map[string]interface{}{
		"spec": map[string]interface{}{"versions": []interface{}{
			map[string]interface{}{"name": "v1beta1", "served": true, "storage": true},
			map[string]interface{}{"name": "v1alpha1", "served": false},
		}},
	}}
	assert.Equal(t, []string{"v1beta1"}, crdServedVersions(crd))
	assert.Equal(t, []string{"v1beta1", "v1alpha1"}, crdDeclaredVersions(crd))
}

// The wiring, not the helper: applyCRDs must actually refuse. Asserting only on
// storedVersionsDroppedBy leaves the refusal untested, and the whole point is that an
// older CLI is stopped before it rewrites a newer cluster's schema.
func TestApplyCRDs_RefusesToRemoveAVersionTheClusterServes(t *testing.T) {
	// The embedded CRDs are v1alpha1 only, so a cluster that has gained
	// v1beta1 is ahead of this binary.
	live := crdWithVersions("v1beta1", "v1alpha1")
	live.SetResourceVersion("100")
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1beta1"}, "status", "storedVersions"))
	dyn := rbacDynamic(live)

	err := applyCRDs(context.Background(), dyn, "v0.9.0", io.Discard)
	require.Error(t, err, "an older kip must not rewrite a newer cluster's schema")
	assert.Contains(t, err.Error(), "v1beta1")
	assert.Contains(t, err.Error(), "upgrade kip first")

	after, getErr := dyn.Resource(crdGVR).Get(context.Background(), "apps.kipper.run", metav1.GetOptions{})
	require.NoError(t, getErr)
	assert.Equal(t, []string{"v1beta1", "v1alpha1"}, crdDeclaredVersions(after),
		"the live CRD must be left exactly as it was")
}

// The seam runUpgrade calls. Earlier versions of this test asserted on the
// appliers and then on the step table, and both stayed green when the
// production call was removed, because the test was a second consumer of the
// same helper rather than a check on what upgrade runs.
func TestReconcileClusterAPIState_AppliesSchemasAndRBACAndNeverTheAdminBinding(t *testing.T) {
	ctx := context.Background()
	dyn := rbacDynamic()
	var out bytes.Buffer

	require.NoError(t, reconcileClusterAPIState(ctx, dyn, "v0.9.0", &out))

	crds, err := dyn.Resource(crdGVR).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	assert.NotEmpty(t, crds.Items, "an upgrade has to register the CRD schemas")

	roleGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	for _, name := range []string{"kipper:project-viewer", "kipper:project-deployer", "kipper:project-owner", "console-api"} {
		_, getErr := dyn.Resource(roleGVR).Get(ctx, name, metav1.GetOptions{})
		require.NoError(t, getErr, "upgrade must deliver the %s ClusterRole to existing clusters", name)
	}

	bindingGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
	bindings, err := dyn.Resource(bindingGVR).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	for _, b := range bindings.Items {
		assert.NotEqual(t, "kipper-initial-admin", b.GetName(),
			"re-applying the initial admin binding would reset its subjects and revoke every admin added since install")
	}

	assert.Contains(t, out.String(), "Operator roles", "the operator step has to be reported, not silently skipped")
}

// A CRD failure has to stop the upgrade there, because the RBAC that follows
// addresses kinds those CRDs register.
func TestReconcileClusterAPIState_StopsAtTheCRDFailure(t *testing.T) {
	ctx := context.Background()
	ahead := crdWithVersions("v1beta1", "v1alpha1")
	require.NoError(t, unstructured.SetNestedStringSlice(ahead.Object, []string{"v1beta1"}, "status", "storedVersions"))
	ahead.SetResourceVersion("100")
	dyn := rbacDynamic(ahead)

	err := reconcileClusterAPIState(ctx, dyn, "v0.9.0", io.Discard)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "updating CRD schemas")

	roleGVR := schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}
	roles, listErr := dyn.Resource(roleGVR).List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	assert.Empty(t, roles.Items, "RBAC must not be applied after the schema step refused")
}

// Refusing has to mean nothing happened. The guard used to sit inside the same
// loop that writes, so a drop detected on a later CRD left earlier ones already
// replaced and their stored objects already pruned.
func TestApplyCRDs_WritesNothingWhenAnyCRDWouldBeDowngraded(t *testing.T) {
	ctx := context.Background()
	// A cluster ahead of this binary on one CRD only.
	ahead := crdWithVersions("v1beta1", "v1alpha1")
	require.NoError(t, unstructured.SetNestedStringSlice(ahead.Object, []string{"v1beta1"}, "status", "storedVersions"))
	ahead.SetResourceVersion("100")
	dyn := rbacDynamic(ahead)

	require.Error(t, applyCRDs(ctx, dyn, "v0.9.0", io.Discard))

	all, err := dyn.Resource(crdGVR).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, all.Items, 1, "no other embedded CRD may have been created before the refusal")
	assert.Equal(t, "apps.kipper.run", all.Items[0].GetName())
	assert.Equal(t, []string{"v1beta1", "v1alpha1"}, crdDeclaredVersions(&all.Items[0]),
		"and the one that was there must be untouched")
}

// The case that separates the two guards, at the call site rather than in the
// helper. A version the cluster declares but has nothing stored under is being
// retired, which the versioning plan requires to be possible. Keying the guard
// on served versions refused this; keying it on storedVersions allows it.
func TestApplyCRDs_AllowsRetiringAVersionNothingIsStoredUnder(t *testing.T) {
	ctx := context.Background()
	// The embedded CRDs declare v1alpha1, so a live CRD carrying an extra
	// v1beta1 with nothing stored under it is the retirement case.
	live := crdWithVersions("v1alpha1", "v1beta1")
	live.SetResourceVersion("100")
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1alpha1"}, "status", "storedVersions"))
	dyn := rbacDynamic(live)

	require.NoError(t, applyCRDs(ctx, dyn, "v0.9.0", io.Discard),
		"retiring a version with nothing stored under it must not be refused")

	after, err := dyn.Resource(crdGVR).Get(ctx, "apps.kipper.run", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, crdDeclaredVersions(after), "v1beta1", "and the retirement actually applied")
}

// The other tests build status with unstructured.SetNestedStringSlice, using
// the same field name the implementation reads, so a misspelling on both sides
// would agree with itself and pass. This one parses the JSON an apiserver
// actually returns, so the field name comes from the API rather than from us.
func TestStoredVersionsDroppedBy_AgainstTheAPIsOwnSerialisation(t *testing.T) {
	const raw = `{
  "apiVersion": "apiextensions.k8s.io/v1",
  "kind": "CustomResourceDefinition",
  "metadata": {"name": "apps.kipper.run"},
  "spec": {"group": "kipper.run", "versions": [
    {"name": "v1alpha1", "served": true, "storage": true},
    {"name": "v1beta1", "served": true, "storage": false}
  ]},
  "status": {"acceptedNames": {"kind": "App"}, "conditions": [], "storedVersions": ["v1alpha1"]}
}`
	var live unstructured.Unstructured
	require.NoError(t, json.Unmarshal([]byte(raw), &live.Object))

	// Nothing is stored under v1beta1, so retiring it is allowed.
	assert.Empty(t, storedVersionsDroppedBy(&live, crdWithVersions("v1alpha1")),
		"a version with nothing stored under it must be retirable")

	// And dropping the one that does hold objects is refused.
	assert.Equal(t, []string{"v1alpha1"}, storedVersionsDroppedBy(&live, crdWithVersions("v1beta1")),
		"the version objects are stored under must not be dropped")
}

// The gap the stored-version guard cannot see. A newer release adds a field or
// a validation rule to an existing version, so no version name disappears and
// storedVersions is unchanged; an older kip carrying the older shape replaces
// it and Kubernetes prunes whatever the newer one added. The only thing that
// distinguishes this from a legitimate forward apply is which binary wrote the
// live schema.
func TestApplyCRDs_RefusesWhenTheClusterWasWrittenByANewerKip(t *testing.T) {
	ctx := context.Background()
	live := crdWithVersions("v1alpha1")
	live.SetResourceVersion("100")
	live.SetAnnotations(map[string]string{crdWrittenByAnnotation: "v0.11.0"})
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1alpha1"}, "status", "storedVersions"))
	dyn := rbacDynamic(live)

	err := applyCRDs(ctx, dyn, "v0.9.0", io.Discard)
	require.Error(t, err, "an older kip must not overwrite a newer cluster's schema")
	assert.Contains(t, err.Error(), "v0.11.0")
	assert.Contains(t, err.Error(), "Upgrade kip first")

	all, listErr := dyn.Resource(crdGVR).List(ctx, metav1.ListOptions{})
	require.NoError(t, listErr)
	require.Len(t, all.Items, 1, "refusing has to mean nothing was written")
	assert.Equal(t, "v0.11.0", all.Items[0].GetAnnotations()[crdWrittenByAnnotation],
		"and the live stamp must be left alone")
}

// The second gap, which is the same refusal reached by a different route: a
// version that is served but not yet the storage version never appears in
// storedVersions, so dropping it looks like nothing was dropped.
func TestApplyCRDs_RefusesDroppingAServedVersionThatIsNotYetStorage(t *testing.T) {
	ctx := context.Background()
	// Release N added v1beta1 as served while storage stayed v1alpha1. The
	// embedded CRDs declare only v1alpha1, so this apply would drop v1beta1.
	live := crdWithVersions("v1alpha1", "v1beta1")
	live.SetResourceVersion("100")
	live.SetAnnotations(map[string]string{crdWrittenByAnnotation: "v0.10.0"})
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1alpha1"}, "status", "storedVersions"))
	dyn := rbacDynamic(live)

	require.Error(t, applyCRDs(ctx, dyn, "v0.9.0", io.Discard),
		"an older kip must not drop a served version the newer cluster still answers on")

	after, err := dyn.Resource(crdGVR).Get(ctx, "apps.kipper.run", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, crdDeclaredVersions(after), "v1beta1", "and v1beta1 must survive")
}

// A newer kip against an older cluster is the ordinary upgrade, and it must
// carry the stamp forward so the next run can make the same judgement.
func TestApplyCRDs_NewerKipAppliesAndRecordsItself(t *testing.T) {
	ctx := context.Background()
	live := crdWithVersions("v1alpha1")
	live.SetResourceVersion("100")
	live.SetAnnotations(map[string]string{crdWrittenByAnnotation: "v0.9.0"})
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1alpha1"}, "status", "storedVersions"))
	dyn := rbacDynamic(live)

	require.NoError(t, applyCRDs(ctx, dyn, "v0.10.0", io.Discard))

	after, err := dyn.Resource(crdGVR).Get(ctx, "apps.kipper.run", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "v0.10.0", after.GetAnnotations()[crdWrittenByAnnotation],
		"the applying version has to be recorded, or the next run has nothing to compare against")
}

// Every cluster in existence predates the stamp, so an unstamped CRD must not
// be refused — and must come out stamped.
func TestApplyCRDs_UnstampedClusterIsAllowedAndGetsStamped(t *testing.T) {
	ctx := context.Background()
	live := crdWithVersions("v1alpha1")
	live.SetResourceVersion("100")
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1alpha1"}, "status", "storedVersions"))
	dyn := rbacDynamic(live)

	require.NoError(t, applyCRDs(ctx, dyn, "v0.9.0", io.Discard),
		"a cluster written before stamping existed must still be upgradable")

	after, err := dyn.Resource(crdGVR).Get(ctx, "apps.kipper.run", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "v0.9.0", after.GetAnnotations()[crdWrittenByAnnotation])
}

// A source build cannot be ordered against a release, so the check cannot run.
// It has to say so: silence would read as a guard that passed.
func TestApplyCRDs_SaysWhenItCannotOrderThisBuildAgainstTheCluster(t *testing.T) {
	ctx := context.Background()
	live := crdWithVersions("v1alpha1")
	live.SetResourceVersion("100")
	live.SetAnnotations(map[string]string{crdWrittenByAnnotation: "v0.11.0"})
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1alpha1"}, "status", "storedVersions"))
	dyn := rbacDynamic(live)

	var out bytes.Buffer
	require.NoError(t, applyCRDs(ctx, dyn, "dev", &out),
		"a source build must not be blocked by a check it cannot perform")
	assert.Contains(t, out.String(), "v0.11.0")
	assert.Contains(t, out.String(), "skipped")
}

// The CRD Update is a full replace, so anything the cluster put on the object's
// metadata is lost unless the apply carries it across. A GitOps tool's sync
// option is the case that bites: the upgrade succeeds and the tool's next
// reconcile behaves differently.
func TestApplyCRDs_KeepsClusterOwnedMetadataAcrossTheReplace(t *testing.T) {
	ctx := context.Background()
	live := crdWithVersions("v1alpha1")
	live.SetResourceVersion("100")
	live.SetAnnotations(map[string]string{
		"argocd.argoproj.io/sync-options": "Prune=false",
		crdWrittenByAnnotation:            "v0.9.0",
	})
	live.SetLabels(map[string]string{"app.kubernetes.io/managed-by": "Helm"})
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1alpha1"}, "status", "storedVersions"))
	dyn := rbacDynamic(live)

	require.NoError(t, applyCRDs(ctx, dyn, "v0.10.0", io.Discard))

	after, err := dyn.Resource(crdGVR).Get(ctx, "apps.kipper.run", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "Prune=false", after.GetAnnotations()["argocd.argoproj.io/sync-options"],
		"a cluster-owned annotation must survive kip's replace")
	assert.Equal(t, "Helm", after.GetLabels()["app.kubernetes.io/managed-by"],
		"and so must a cluster-owned label")
	assert.Equal(t, "v0.10.0", after.GetAnnotations()[crdWrittenByAnnotation],
		"while the stamp is still moved forward, not carried over stale")
}

// A source build is allowed to apply, but it must not destroy the evidence the
// guard runs on. Overwriting an orderable stamp with "dev" would let every
// later release — including an older one — skip the check silently.
func TestApplyCRDs_SourceBuildDoesNotDestroyAnOrderableStamp(t *testing.T) {
	ctx := context.Background()
	live := crdWithVersions("v1alpha1")
	live.SetResourceVersion("100")
	live.SetAnnotations(map[string]string{crdWrittenByAnnotation: "v0.11.0"})
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1alpha1"}, "status", "storedVersions"))
	dyn := rbacDynamic(live)

	require.NoError(t, applyCRDs(ctx, dyn, "dev", io.Discard))

	after, err := dyn.Resource(crdGVR).Get(ctx, "apps.kipper.run", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "v0.11.0", after.GetAnnotations()[crdWrittenByAnnotation],
		"the orderable stamp must survive a build that cannot be ordered against it")

	// And the guard still works afterwards: an older release is still refused.
	require.Error(t, applyCRDs(ctx, dyn, "v0.9.0", io.Discard),
		"the protection must outlive the source-build apply")
}

// kubectl diffs its next apply against the snapshot in this annotation. Carrying
// one forward across an Update that changed the schema leaves kubectl merging
// against an object that never existed, and a three-way merge then deletes
// fields the stale snapshot does not mention. The replace dropped it before
// anything was carried over; it must keep dropping it.
func TestApplyCRDs_DoesNotCarryOverKubectlsAppliedSnapshot(t *testing.T) {
	ctx := context.Background()
	live := crdWithVersions("v1alpha1")
	live.SetResourceVersion("100")
	live.SetAnnotations(map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": `{"spec":{"versions":[{"name":"v1alpha1"}]}}`,
		"argocd.argoproj.io/sync-options":                  "Prune=false",
	})
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1alpha1"}, "status", "storedVersions"))
	dyn := rbacDynamic(live)

	require.NoError(t, applyCRDs(ctx, dyn, "v0.10.0", io.Discard))

	after, err := dyn.Resource(crdGVR).Get(ctx, "apps.kipper.run", metav1.GetOptions{})
	require.NoError(t, err)
	assert.NotContains(t, after.GetAnnotations(), "kubectl.kubernetes.io/last-applied-configuration",
		"a stale applied-snapshot must not survive a schema replace")
	assert.Equal(t, "Prune=false", after.GetAnnotations()["argocd.argoproj.io/sync-options"],
		"while genuinely cluster-owned metadata still does")
}

// A finalizer is a controller's claim on cleanup before deletion. The CRD
// Update is a full replace, so omitting it removes it: the claim is dropped
// silently, and on a terminating object deletion proceeds ahead of the cleanup
// it was holding open.
func TestApplyCRDs_KeepsLifecycleMetadataAcrossTheReplace(t *testing.T) {
	ctx := context.Background()
	live := crdWithVersions("v1alpha1")
	live.SetResourceVersion("100")
	live.SetFinalizers([]string{"customresourcecleanup.apiextensions.k8s.io"})
	require.NoError(t, unstructured.SetNestedStringSlice(live.Object, []string{"v1alpha1"}, "status", "storedVersions"))
	dyn := rbacDynamic(live)

	require.NoError(t, applyCRDs(ctx, dyn, "v0.10.0", io.Discard))

	after, err := dyn.Resource(crdGVR).Get(ctx, "apps.kipper.run", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, after.GetFinalizers(), "customresourcecleanup.apiextensions.k8s.io",
		"a finalizer must survive kip's replace")
}
