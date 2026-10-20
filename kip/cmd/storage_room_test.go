package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

// longhornServer is a cluster whose server disk has scheduledGi of 40Gi
// promised to volumes with 12Gi reserved, so 28-scheduledGi Gi is left.
func longhornServer(t *testing.T, scheduledGi int64) (*dynamicfake.FakeDynamicClient, *k8sfake.Clientset) {
	t.Helper()
	dyn := storageDynamic()
	gib := int64(1 << 30)
	node := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "longhorn.io/v1beta2", "kind": "Node",
		"metadata": map[string]interface{}{"name": "server-1", "namespace": "longhorn-system"},
		"spec": map[string]interface{}{"allowScheduling": true, "tags": []interface{}{"kipper-server"},
			"disks": map[string]interface{}{"default-disk": map[string]interface{}{"allowScheduling": true, "storageReserved": 12 * gib, "diskType": "filesystem"}}},
		"status": map[string]interface{}{
			"conditions": []interface{}{
				map[string]interface{}{"type": "Ready", "status": "True"},
				map[string]interface{}{"type": "Schedulable", "status": "True"},
			},
			"diskStatus": map[string]interface{}{"default-disk": map[string]interface{}{
				"storageMaximum": 40 * gib, "storageScheduled": scheduledGi * gib, "storageAvailable": 30 * gib,
				"conditions": []interface{}{map[string]interface{}{"type": "Schedulable", "status": "True"}},
			}},
		},
	}}
	nodes := schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "nodes"}
	settings := schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "settings"}
	ctx := context.Background()
	_, err := dyn.Resource(nodes).Namespace("longhorn-system").Create(ctx, node, metav1.CreateOptions{})
	require.NoError(t, err)
	for name, value := range map[string]string{"storage-over-provisioning-percentage": "100", "storage-minimal-available-percentage": "25"} {
		_, err := dyn.Resource(settings).Namespace("longhorn-system").Create(ctx, &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "longhorn.io/v1beta2", "kind": "Setting",
			"metadata": map[string]interface{}{"name": name, "namespace": "longhorn-system"},
			"value":    value,
		}}, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	clientset := k8sfake.NewSimpleClientset( //nolint:staticcheck
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "longhorn-single"}, Provisioner: "driver.longhorn.io",
			Parameters: map[string]string{"numberOfReplicas": "1", "nodeSelector": "kipper-server"}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "longhorn"}, Provisioner: "driver.longhorn.io"},
	)
	return dyn, clientset
}

// storageDynamic holds Kipper's kinds, the CRDs apply reads, and Longhorn's
// nodes and settings.
func storageDynamic() *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	for _, gk := range []schema.GroupVersionKind{
		{Group: "kipper.run", Version: "v1alpha1", Kind: "App"},
		{Group: "kipper.run", Version: "v1alpha1", Kind: "Function"},
		{Group: "kipper.run", Version: "v1alpha1", Kind: "Service"},
		{Group: "kipper.run", Version: "v1alpha1", Kind: "Volume"},
		{Group: "kipper.run", Version: "v1alpha1", Kind: "Project"},
		{Group: "apiextensions.k8s.io", Version: "v1", Kind: "CustomResourceDefinition"},
		{Group: "longhorn.io", Version: "v1beta2", Kind: "Node"},
		{Group: "longhorn.io", Version: "v1beta2", Kind: "Setting"},
	} {
		scheme.AddKnownTypeWithName(gk, &unstructured.Unstructured{})
		gk.Kind += "List"
		scheme.AddKnownTypeWithName(gk, &unstructured.UnstructuredList{})
	}
	return dynamicfake.NewSimpleDynamicClient(scheme)
}

func postgresResource(name string, spec map[string]interface{}) manifest.Resource {
	return manifest.Resource{GVR: manifest.ServiceGVR, Object: &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "Service",
		"metadata": map[string]interface{}{"name": name},
		"spec":     spec,
	}}}
}

func TestApplyRefusesServicesTheServerDiskCannotHoldBeforeWritingAnything(t *testing.T) {
	dyn, clientset := longhornServer(t, 27)
	plans := []applyPlan{{
		m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme",
		resources: []manifest.Resource{
			postgresResource("db", map[string]interface{}{"type": "postgres", "storage": "1Gi", "consistentBackups": true}),
		},
	}}

	created, _, err := applyPlans(context.Background(), dyn, clientset, plans, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "server-1")
	assert.Contains(t, err.Error(), "db data 1Gi, db dump 1Gi")
	assert.Equal(t, 0, created)
	_, getErr := dyn.Resource(manifest.ServiceGVR).Namespace("acme").Get(context.Background(), "db", metav1.GetOptions{})
	assert.Error(t, getErr, "nothing may be written")
	projects, err := dyn.Resource(manifest.ProjectGVR).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, projects.Items, "nothing may be written")
}

func TestApplyCountsOnlyServicesItCreates(t *testing.T) {
	dyn := storageDynamic()
	_, err := dyn.Resource(manifest.ServiceGVR).Namespace("acme").Create(context.Background(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "Service",
		"metadata": map[string]interface{}{"name": "db", "namespace": "acme"},
		"spec":     map[string]interface{}{"type": "postgres", "storage": "5Gi"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	volumes, err := newVolumesInPlans(context.Background(), &service.Manager{Dynamic: dyn}, []applyPlan{{namespace: "acme", resources: []manifest.Resource{
		postgresResource("db", map[string]interface{}{"type": "postgres", "storage": "5Gi"}),
		postgresResource("cache", map[string]interface{}{"type": "redis"}),
		{GVR: manifest.VolumeGVR, Object: &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1", "kind": "Volume",
			"metadata": map[string]interface{}{"name": "uploads"},
			"spec":     map[string]interface{}{"size": "2Gi"},
		}}},
	}}})

	require.NoError(t, err)
	assert.Equal(t, []service.NewVolume{
		{Name: "cache data", Bytes: 1 << 30, Namespace: "acme", Claim: "data-cache-0"},
		{Name: "uploads", Shared: true, Bytes: 2 << 30, Namespace: "acme", Claim: "shared-uploads"},
	}, volumes)
}

func TestStorageRoomWarningIsPrintedAndTheCommandGoesOn(t *testing.T) {
	var out bytes.Buffer
	mgr := &service.Manager{Client: k8sfake.NewSimpleClientset(), Dynamic: fakeWorkloadDynamic()} //nolint:staticcheck

	err := refuseWithoutStorageRoom(context.Background(), &out, mgr, []service.NewVolume{{Name: "data", Bytes: 1 << 30}}, "--storage")

	require.NoError(t, err)
	assert.Contains(t, out.String(), "⚠")
	assert.Contains(t, out.String(), "not checked")
}

func TestStatusWarnsWhenTheServerDiskIsNearlyFull(t *testing.T) {
	dyn, clientset := longhornServer(t, 26)
	var out bytes.Buffer

	reportServerStorageRoom(context.Background(), &out, &service.Manager{Client: clientset, Dynamic: dyn})

	assert.Contains(t, out.String(), "server-1 has 2Gi left")
	assert.Contains(t, out.String(), "kip service delete")
}

func TestStatusSaysNothingWhenTheServerDiskHasRoom(t *testing.T) {
	dyn, clientset := longhornServer(t, 10)
	var out bytes.Buffer

	reportServerStorageRoom(context.Background(), &out, &service.Manager{Client: clientset, Dynamic: dyn})

	assert.Empty(t, out.String())
}

// Turning consistent backups on for a service that exists creates its dump
// volume, which the manifest has to have room for.
func TestApplyCountsTheDumpVolumeOfAServiceItUpdates(t *testing.T) {
	dyn, clientset := longhornServer(t, 27)
	_, err := dyn.Resource(manifest.ServiceGVR).Namespace("acme").Create(context.Background(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "Service",
		"metadata": map[string]interface{}{"name": "db", "namespace": "acme"},
		"spec":     map[string]interface{}{"type": "postgres", "storage": "5Gi"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)
	plans := []applyPlan{{
		m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme",
		resources: []manifest.Resource{
			postgresResource("db", map[string]interface{}{"type": "postgres", "storage": "5Gi", "consistentBackups": true}),
		},
	}}

	created, updated, err := applyPlans(context.Background(), dyn, clientset, plans, false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "db dump 5Gi")
	assert.Equal(t, 0, created+updated)
	live, err := dyn.Resource(manifest.ServiceGVR).Namespace("acme").Get(context.Background(), "db", metav1.GetOptions{})
	require.NoError(t, err)
	on, _, _ := unstructured.NestedBool(live.Object, "spec", "consistentBackups")
	assert.False(t, on, "nothing may be written")
}

// Apply replaces the whole spec, so a manifest without storage resets the
// service to the CRD default and the new dump volume gets that size.
func TestApplySizesANewDumpVolumeFromTheManifest(t *testing.T) {
	dyn := storageDynamic()
	_, err := dyn.Resource(manifest.ServiceGVR).Namespace("acme").Create(context.Background(), &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "Service",
		"metadata": map[string]interface{}{"name": "db", "namespace": "acme"},
		"spec":     map[string]interface{}{"type": "postgres", "storage": "5Gi"},
	}}, metav1.CreateOptions{})
	require.NoError(t, err)

	volumes, err := newVolumesInPlans(context.Background(), &service.Manager{Dynamic: dyn}, []applyPlan{{namespace: "acme", resources: []manifest.Resource{
		postgresResource("db", map[string]interface{}{"type": "postgres", "consistentBackups": true}),
	}}})

	require.NoError(t, err)
	assert.Equal(t, []service.NewVolume{{Name: "db dump", Bytes: 1 << 30, Namespace: "acme", Claim: "db-dump"}}, volumes)
}

// A service recreated over the claim its predecessor left behind takes that
// claim over, so it needs no room.
func TestApplyRecreatesAServiceOverItsRetainedClaim(t *testing.T) {
	dyn, clientset := longhornServer(t, 28)
	_, err := clientset.CoreV1().PersistentVolumeClaims("acme").Create(context.Background(),
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-cache-0", Namespace: "acme"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	plans := []applyPlan{{
		m: &manifest.Manifest{Project: "acme"}, project: "acme", namespace: "acme",
		resources: []manifest.Resource{postgresResource("cache", map[string]interface{}{"type": "redis", "storage": "1Gi"})},
	}}

	created, _, err := applyPlans(context.Background(), dyn, clientset, plans, false)

	require.NoError(t, err)
	assert.Equal(t, 1, created)
}
