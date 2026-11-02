package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/service"
)

func storagePlatformConfig(mode string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "PlatformConfig",
		"metadata": map[string]interface{}{"name": "platform"},
		"spec":     map[string]interface{}{"storage": mode},
	}}
}

func storageDyn(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		platformConfigGVR: "PlatformConfigList",
		nodeGVR:           "NodeList",
		{Group: "longhorn.io", Version: "v1beta2", Resource: "nodes"}: "NodeList",
		admissionPolicyGVRs["ValidatingAdmissionPolicy"]:              "ValidatingAdmissionPolicyList",
		admissionPolicyGVRs["ValidatingAdmissionPolicyBinding"]:       "ValidatingAdmissionPolicyBindingList",
	}, objects...)
}

func offeredInPolicy(t *testing.T, dyn *dynamicfake.FakeDynamicClient) string {
	t.Helper()
	policy, err := dyn.Resource(admissionPolicyGVRs["ValidatingAdmissionPolicy"]).Get(context.Background(), "kipper-storage-copies-offer", metav1.GetOptions{})
	require.NoError(t, err)
	variables, _, _ := unstructured.NestedSlice(policy.Object, "spec", "variables")
	for _, v := range variables {
		if m := v.(map[string]interface{}); m["name"] == "offered" {
			return m["expression"].(string)
		}
	}
	t.Fatal("the offer policy has no offered variable")
	return ""
}

func TestSettingTheCopiesOfferRecordsItAndRendersThePolicy(t *testing.T) {
	objects := []runtime.Object{storagePlatformConfig(platform.StorageLonghorn)}
	objects = append(objects, replicaNode("server-1")...)
	objects = append(objects, replicaNode("worker-1")...)
	dyn := storageDyn(objects...)
	var out bytes.Buffer
	require.NoError(t, setCopiesOffer(context.Background(), &out, dyn, 2))

	pc, err := dyn.Resource(platformConfigGVR).Get(context.Background(), "platform", metav1.GetOptions{})
	require.NoError(t, err)
	maxCopies, _, _ := unstructured.NestedInt64(pc.Object, "spec", "maxCopies")
	assert.EqualValues(t, 2, maxCopies)
	assert.Equal(t, "2", offeredInPolicy(t, dyn))
	assert.Contains(t, out.String(), "offers two copies")
	assert.NotContains(t, out.String(), "New volumes with two copies need", "two nodes can hold copies")

	require.NoError(t, setCopiesOffer(context.Background(), &out, dyn, 1))
	assert.Equal(t, "1", offeredInPolicy(t, dyn))
}

func storageNode(name string, server bool) *unstructured.Unstructured {
	n := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "v1", "kind": "Node", "metadata": map[string]interface{}{"name": name},
	}}
	if server {
		n.SetLabels(map[string]string{installer.ServerNodeLabel: "true"})
	}
	return n
}

func TestPolicyRenderingListsOnlyTheServerNodes(t *testing.T) {
	dyn := storageDyn(storagePlatformConfig(platform.StorageLonghorn), storageNode("server-1", true), storageNode("worker-1", false))
	require.NoError(t, applyAdmissionPolicies(context.Background(), dyn))
	policy, err := dyn.Resource(admissionPolicyGVRs["ValidatingAdmissionPolicy"]).Get(context.Background(), "kipper-longhorn-server-replicas", metav1.GetOptions{})
	require.NoError(t, err)
	variables, _, _ := unstructured.NestedSlice(policy.Object, "spec", "variables")
	require.NotEmpty(t, variables)
	assert.Equal(t, "['server-1']", variables[0].(map[string]interface{})["expression"])
}

func TestTheCopiesOfferOnOneNodeSaysNewVolumesNeedASecondNode(t *testing.T) {
	dyn := storageDyn(append([]runtime.Object{storagePlatformConfig(platform.StorageLonghorn)}, replicaNode("server-1")...)...)
	var out bytes.Buffer
	require.NoError(t, setCopiesOffer(context.Background(), &out, dyn, 2))
	assert.Contains(t, out.String(), "offers two copies")
	assert.Contains(t, out.String(), "New volumes with two copies need two ready nodes that can hold copies, and the cluster has 1. An admin adds a worker with 'kip node add', or lets an existing worker hold copies with 'kip node storage <node> --copies'.")
}

// replicaNode returns matching Kubernetes and Longhorn nodes eligible to hold copies.
func replicaNode(name string) []runtime.Object {
	ready := []interface{}{map[string]interface{}{"type": "Ready", "status": "True"}}
	return []runtime.Object{
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "v1", "kind": "Node", "metadata": map[string]interface{}{"name": name},
			"status": map[string]interface{}{"conditions": ready},
		}},
		&unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "longhorn.io/v1beta2", "kind": "Node",
			"metadata": map[string]interface{}{"name": name, "namespace": "longhorn-system"},
			"spec":     map[string]interface{}{"allowScheduling": true, "tags": []interface{}{platform.ReplicaStorageTag}},
			"status": map[string]interface{}{"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True"},
				map[string]interface{}{"type": "Schedulable", "status": "True"},
			}},
		}},
	}
}

func TestTheCopiesOfferIsRefusedOnLocalStorage(t *testing.T) {
	dyn := storageDyn(storagePlatformConfig(platform.StorageLocal))
	err := setCopiesOffer(context.Background(), &bytes.Buffer{}, dyn, 2)
	assert.ErrorContains(t, err, "local storage")
}

func TestTheCopiesOfferIsOneOrTwo(t *testing.T) {
	err := setCopiesOffer(context.Background(), &bytes.Buffer{}, storageDyn(storagePlatformConfig(platform.StorageLonghorn)), 3)
	assert.ErrorContains(t, err, "1 or 2")
}

func TestRecordedCopiesOfferWithoutAPlatformConfigIsOne(t *testing.T) {
	assert.EqualValues(t, 1, recordedCopiesOffer(context.Background(), storageDyn()))
}

// longhornOverProvisioning is Longhorn's default over-provisioning setting, 100%.
func longhornOverProvisioning() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "longhorn.io/v1beta2", "kind": "Setting",
		"metadata": map[string]interface{}{"name": "storage-over-provisioning-percentage", "namespace": "longhorn-system"},
		"value":    "100",
	}}
}

func TestSettingOverProvisioningWritesLonghornsSetting(t *testing.T) {
	dyn := storageDyn(storagePlatformConfig(platform.StorageLonghorn), longhornOverProvisioning())
	var out bytes.Buffer

	require.NoError(t, setOverProvisioning(context.Background(), &out, dyn, 200))

	setting, err := dyn.Resource(longhornSettingGVR).Namespace("longhorn-system").Get(context.Background(), "storage-over-provisioning-percentage", metav1.GetOptions{})
	require.NoError(t, err)
	value, _, _ := unstructured.NestedString(setting.Object, "value")
	assert.Equal(t, "200", value)
	assert.Contains(t, out.String(), "200%")
}

func TestOverProvisioningBelowTheDiskOrOnLocalStorageIsRefused(t *testing.T) {
	dyn := storageDyn(storagePlatformConfig(platform.StorageLonghorn), longhornOverProvisioning())
	err := setOverProvisioning(context.Background(), &bytes.Buffer{}, dyn, 50)
	assert.ErrorContains(t, err, "at least 100")

	local := storageDyn(storagePlatformConfig(platform.StorageLocal))
	err = setOverProvisioning(context.Background(), &bytes.Buffer{}, local, 200)
	assert.ErrorContains(t, err, "local storage")
}

func TestClusterStorageShowsTheServersRoomForNewVolumes(t *testing.T) {
	dyn, clientset := longhornServer(t, 20)
	var out bytes.Buffer

	reportLonghornRoom(context.Background(), &out, &service.Manager{Client: clientset, Dynamic: dyn})

	assert.Contains(t, out.String(), "server-1's roomiest disk has about 8Gi for new single-copy volumes")
	assert.Contains(t, out.String(), "over-provisioning 100%")
}

func TestAnInvalidCopiesOfferLeavesOverProvisioningAlone(t *testing.T) {
	dyn := storageDyn(storagePlatformConfig(platform.StorageLonghorn), longhornOverProvisioning())
	percent, copies := int32(200), int32(3)

	err := setStorageFlags(context.Background(), &bytes.Buffer{}, dyn, &percent, &copies)

	require.Error(t, err)
	setting, getErr := dyn.Resource(longhornSettingGVR).Namespace("longhorn-system").Get(context.Background(), "storage-over-provisioning-percentage", metav1.GetOptions{})
	require.NoError(t, getErr)
	value, _, _ := unstructured.NestedString(setting.Object, "value")
	assert.Equal(t, "100", value, "nothing changes while any flag is invalid")
}

func TestOverProvisioningAndTheCopiesOfferCanBeSetTogether(t *testing.T) {
	objects := []runtime.Object{storagePlatformConfig(platform.StorageLonghorn), longhornOverProvisioning()}
	objects = append(objects, replicaNode("server-1")...)
	dyn := storageDyn(objects...)
	percent, copies := int32(100), int32(1)

	require.NoError(t, setStorageFlags(context.Background(), &bytes.Buffer{}, dyn, &percent, &copies))

	pc, err := dyn.Resource(platformConfigGVR).Get(context.Background(), "platform", metav1.GetOptions{})
	require.NoError(t, err)
	offered, _, _ := unstructured.NestedInt64(pc.Object, "spec", "maxCopies")
	assert.Equal(t, int64(1), offered)
}
