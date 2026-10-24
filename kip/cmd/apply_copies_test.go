package cmd

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func TestApplyCreatesOneCopyWhenTheManifestOmitsCopies(t *testing.T) {
	spec := map[string]interface{}{"type": "redis"}
	assert.Equal(t, int64(1), withNewCopies("Service", spec)["copies"])
	assert.NotContains(t, spec, "copies", "the manifest's own map is left alone")

	assert.Equal(t, int64(2), withNewCopies("Volume", map[string]interface{}{"size": "1Gi", "copies": int64(2)})["copies"])
	assert.NotContains(t, withNewCopies("App", map[string]interface{}{}), "copies")
}

func TestApplyKeepsTheLiveCopiesWhenTheManifestOmitsThem(t *testing.T) {
	live := map[string]interface{}{"type": "redis", "copies": int64(2)}
	assert.Equal(t, int64(2), withLiveCopies("Service", map[string]interface{}{"type": "redis"}, live)["copies"])

	unmanaged := withLiveCopies("Volume", map[string]interface{}{"size": "1Gi"}, map[string]interface{}{"size": "1Gi"})
	assert.NotContains(t, unmanaged, "copies", "an unmanaged object stays unmanaged")
}

func copiesService(spec, live *int64) *unstructured.Unstructured {
	obj := copiesObject(spec, live)
	obj.SetNamespace("shop")
	return obj
}

func TestApplyChangesCopiesExceptDuringAConversion(t *testing.T) {
	assert.NoError(t, refuseCopiesChange("Service", map[string]interface{}{"copies": int64(2)}, copiesService(ptr.To[int64](1), ptr.To[int64](1))))
	assert.NoError(t, refuseCopiesChange("Service", map[string]interface{}{"copies": int64(2)}, copiesService(nil, ptr.To[int64](1))),
		"an unmanaged object is converted to the copies the manifest sets")
	assert.NoError(t, refuseCopiesChange("Service", map[string]interface{}{"type": "redis"}, copiesService(nil, ptr.To[int64](3))),
		"a manifest without copies leaves an unmanaged object unmanaged")

	converting := withCopiesCondition(copiesService(ptr.To[int64](2), ptr.To[int64](1)), "False", "Converting", "Longhorn is building a copy of the volume (1 of 2 copies healthy).")
	err := refuseCopiesChange("Service", map[string]interface{}{"copies": int64(1)}, converting)
	assert.ErrorContains(t, err, "being converted")
	assert.NoError(t, refuseCopiesChange("Service", map[string]interface{}{"copies": int64(2)}, converting), "the same copies are no change")
}

func copiesWrites(dyn *dynamicfake.FakeDynamicClient) []string {
	var writes []string
	for _, a := range dyn.Actions() {
		if verb := a.GetVerb(); verb == "create" || verb == "update" || verb == "patch" {
			writes = append(writes, verb+" "+a.GetResource().Resource)
		}
	}
	return writes
}

func copiesPlan(svcs map[string]manifest.SvcSpec) applyPlan {
	m := &manifest.Manifest{Project: "shop", Services: svcs}
	return applyPlan{m: m, project: "shop", projectCR: "shop", namespace: "shop", resources: manifest.Convert(m, "shop", nil)}
}

// copiesApplyDyn is a cluster on Longhorn storage that offers one copy and
// has no node that can hold a second.
func copiesApplyDyn() *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		manifest.ServiceGVR: "ServiceList", manifest.VolumeGVR: "VolumeList", manifest.ProjectGVR: "ProjectList",
		platformConfigGVR: "PlatformConfigList",
		{Group: "longhorn.io", Version: "v1beta2", Resource: "nodes"}: "NodeList",
		{Version: "v1", Resource: "nodes"}:                            "NodeList",
	}, storagePlatformConfig(platform.StorageLonghorn))
}

func TestApplyRefusesTwoCopiesTheClusterDoesNotOfferBeforeWritingAnything(t *testing.T) {
	dyn := copiesApplyDyn()
	clientset := k8sfake.NewSimpleClientset()
	plan := copiesPlan(map[string]manifest.SvcSpec{
		"cache": {Type: "redis"},
		"db":    {Type: "postgres", Copies: ptr.To[int32](2)},
	})
	_, _, err := applyPlans(context.Background(), dyn, clientset, []applyPlan{plan}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "offers one copy")
	assert.Empty(t, copiesWrites(dyn), "no project, service or other object is written")
	for _, a := range clientset.Actions() {
		assert.NotEqual(t, "create", a.GetVerb(), "no namespace is created")
	}
}

// offerHiddenDyn simulates a project member who cannot read PlatformConfig or
// nodes on a cluster whose admission policy allows only one copy.
func offerHiddenDyn(existingNamespace bool) *dynamicfake.FakeDynamicClient {
	dyn := copiesApplyDyn()
	dyn.PrependReactor("get", "platformconfigs", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(platformConfigGVR.GroupResource(), "platform", nil)
	})
	dyn.PrependReactor("list", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", nil)
	})
	dyn.PrependReactor("create", "services", func(a k8stesting.Action) (bool, runtime.Object, error) {
		create := a.(k8stesting.CreateAction)
		if !existingNamespace {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, create.GetNamespace())
		}
		obj := create.GetObject().(*unstructured.Unstructured)
		if copies, _, _ := unstructured.NestedInt64(obj.Object, "spec", "copies"); copies >= 2 {
			return true, nil, apierrors.NewInvalid(schema.GroupKind{Group: "kipper.run", Kind: "Service"}, obj.GetName(), field.ErrorList{
				field.Forbidden(field.NewPath("spec", "copies"), "This cluster offers one copy of each volume."),
			})
		}
		return false, nil, nil
	})
	return dyn
}

func TestApplyAsksAdmissionWhenItCannotReadTheOffer(t *testing.T) {
	dyn := offerHiddenDyn(true)
	plan := copiesPlan(map[string]manifest.SvcSpec{"db": {Type: "postgres", Copies: ptr.To[int32](2)}})
	_, _, err := applyPlans(context.Background(), dyn, k8sfake.NewSimpleClientset(), []applyPlan{plan}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "offers one copy")
	for _, a := range dyn.Actions() {
		switch a.GetVerb() {
		case "create":
			assert.Equal(t, []string{metav1.DryRunAll}, a.(k8stesting.CreateActionImpl).CreateOptions.DryRun, "only a dry run is sent")
		case "update", "patch":
			t.Errorf("%s %s was written before admission refused the copies", a.GetVerb(), a.GetResource().Resource)
		}
	}
}

func TestApplyCannotAskAdmissionInANamespaceThatDoesNotExistYet(t *testing.T) {
	dyn := offerHiddenDyn(false)
	plan := copiesPlan(map[string]manifest.SvcSpec{"db": {Type: "postgres", Copies: ptr.To[int32](2)}})
	assert.NoError(t, refuseCopiesInPlans(context.Background(), io.Discard, dyn, []applyPlan{plan}),
		"admission still refuses the real create")
}

func TestApplyRefusesTwoCopiesOnAnExistingServiceTheClusterCannotHoldBeforeWritingAnything(t *testing.T) {
	dyn := copiesApplyDyn()
	seedCopiesService(t, dyn, nil, ptr.To[int64](1))
	dyn.ClearActions()
	plan := copiesPlan(map[string]manifest.SvcSpec{
		"cache": {Type: "redis"},
		"db":    {Type: "postgres", Copies: ptr.To[int32](2)},
	})
	_, _, err := applyPlans(context.Background(), dyn, k8sfake.NewSimpleClientset(), []applyPlan{plan}, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "offers one copy")
	assert.Empty(t, copiesWrites(dyn))
	live := getCopiesService(t, dyn)
	_, set, _ := unstructured.NestedInt64(live.Object, "spec", "copies")
	assert.False(t, set)
}

func seedCopiesService(t *testing.T, dyn dynamic.Interface, spec, live *int64) {
	t.Helper()
	obj := copiesService(spec, live)
	obj.SetName("db")
	_, err := dyn.Resource(manifest.ServiceGVR).Namespace("shop").Create(context.Background(), obj, metav1.CreateOptions{})
	require.NoError(t, err)
}

func getCopiesService(t *testing.T, dyn dynamic.Interface) *unstructured.Unstructured {
	t.Helper()
	obj, err := dyn.Resource(manifest.ServiceGVR).Namespace("shop").Get(context.Background(), "db", metav1.GetOptions{})
	require.NoError(t, err)
	return obj
}
