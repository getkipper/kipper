package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/apiservertest"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// longhornCRD is a schemaless Longhorn CRD of kind, without a status
// subresource, so a test can write the status Longhorn would.
func longhornCRD(kind, plural string) *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: plural + ".longhorn.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "longhorn.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: plural, Kind: kind, ListKind: kind + "List"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1beta2", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: &preserve,
				}},
			}},
		},
	}
}

// A real API server runs a service's reduction from two copies to one end
// to end, with the test playing Longhorn between passes: the stop, the
// detach, the counter switch, the hold, the count, Longhorn's removal of the
// worker copy and the restart. The conversion patches carry resourceVersion
// preconditions, which the API server checks.
func TestAServiceReductionRunsEndToEndOnARealAPIServer(t *testing.T) {
	cfg := apiservertest.Start(t)
	s := testScheme()
	require.NoError(t, apiextensionsv1.AddToScheme(s))
	c, err := crclient.New(cfg, crclient.Options{Scheme: s})
	require.NoError(t, err)
	ctx := context.Background()

	for kind, plural := range map[string]string{"Volume": "volumes", "Engine": "engines", "Replica": "replicas",
		"VolumeAttachment": "volumeattachments", "Node": "nodes", "Setting": "settings"} {
		require.NoError(t, c.Create(ctx, longhornCRD(kind, plural)))
	}
	for _, ns := range []string{"ns", longhornNamespace, conversionLeaseNamespace} {
		require.NoError(t, c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}))
	}
	for _, name := range []string{"server-1", "worker-1"} {
		node := kubeNode(name, corev1.ConditionTrue)
		status := node.Status
		require.NoError(t, c.Create(ctx, node))
		node.Status = status
		require.NoError(t, c.Status().Update(ctx, node))
	}

	volume := counterOff(attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1"))
	engine := counterOff(lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"}))
	_ = unstructured.SetNestedField(engine.Object, "worker-1", "spec", "nodeID")
	_ = unstructured.SetNestedField(engine.Object, "worker-1", "status", "ownerID")
	objs := []crclient.Object{
		lhDiskNode("server-1", 90*gib, platform.ServerStorageTag, platform.ReplicaStorageTag),
		lhDiskNode("worker-1", 90*gib, platform.ReplicaStorageTag),
		lhSetting("current-longhorn-version", "v1.11.3"),
		lhSetting("storage-over-provisioning-percentage", "100"),
		lhSetting("storage-minimal-available-percentage", "25"),
		volume, engine, lhAttachmentObj(""),
		counterOff(longhornReplica("r-s", "pvc-1", "server-1", healthyTime, "")),
		counterOff(longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, "")),
	}
	claim := dbDataClaim()
	claim.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
	claim.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}
	objs = append(objs, claim)
	for _, o := range objs {
		require.NoError(t, c.Create(ctx, o), o.GetName())
	}
	labels := map[string]string{"app": "db"}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "db"},
		Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1)), ServiceName: "db",
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "db", Image: "db:1"}}}}},
	}
	require.NoError(t, c.Create(ctx, sts))

	sc := storageConverter{Reader: c, Writer: c, Now: func() time.Time { return convNow }, StatefulSet: "db",
		Commands: serviceStorageCommands("db")}
	pass := func() *string {
		t.Helper()
		var pvc corev1.PersistentVolumeClaim
		require.NoError(t, c.Get(ctx, types.NamespacedName{Namespace: "ns", Name: "data-db-0"}, &pvc))
		cond, err := sc.convert(ctx, &pvc, ptr.To(int32(1)))
		require.NoError(t, err)
		if cond == nil {
			return nil
		}
		return &cond.Reason
	}
	longhorn := func(kind, name string, value interface{}, path ...string) {
		t.Helper()
		setLonghornField(t, c, kind, name, value, path...)
	}

	pass()
	require.NotNil(t, stopOf(t, c), "the stop is recorded")
	assert.Equal(t, int32(0), replicasOfDB(t, c))

	longhorn("Volume", "pvc-1", "detached", "status", "state")
	longhorn("Volume", "pvc-1", "", "status", "currentNodeID")
	longhorn("Engine", "pvc-1-e-0", "stopped", "status", "currentState")
	pass()
	for _, o := range [][2]string{{"Volume", "pvc-1"}, {"Engine", "pvc-1-e-0"}, {"Replica", "r-s"}, {"Replica", "r-w"}} {
		assert.False(t, counterDisabled(t, c, o[0], o[1]), "the counter is on for %s %s", o[0], o[1])
	}

	pass()
	ticket := ticketOf(t, c)
	require.NotNil(t, ticket)
	assert.Equal(t, "server-1", ticket["nodeID"])
	assert.Equal(t, map[string]interface{}{"disableFrontend": "true", "lastAttachedBy": "kipper"}, ticket["parameters"])

	longhorn("VolumeAttachment", "pvc-1", map[string]interface{}{conversionTicket: map[string]interface{}{
		"id": conversionTicket, "satisfied": true, "generation": int64(0)}}, "status", "attachmentTicketStatuses")
	heldVolume := heldOnServer(getLonghorn(t, c, "Volume", "pvc-1"))
	_ = unstructured.SetNestedField(heldVolume.Object, "attached", "status", "state")
	require.NoError(t, c.Update(ctx, heldVolume))
	setEngine(t, c, "server-1", "server-1", "running")

	assert.Equal(t, "ConversionPending", *pass(), "the count is lowered")
	assert.Equal(t, int64(1), countOf(t, c))
	assert.Equal(t, "best-effort", localityOf(t, c))

	require.Equal(t, "r-w", longhornCleanup(t, c), "Longhorn removes the worker copy")
	assert.True(t, replicaExists(t, c, "r-s"))
	assert.Equal(t, "ConversionPending", *pass(), "data locality goes off again")
	assert.Equal(t, "disabled", localityOf(t, c))
	assert.Equal(t, int32(0), replicasOfDB(t, c), "db stays stopped until the hold ends")

	assert.Nil(t, pass(), "the volume reports its live copies")
	assert.Nil(t, ticketOf(t, c))
	assert.Equal(t, int32(1), replicasOfDB(t, c))
	assert.Nil(t, stopOf(t, c))

	stale := getLonghorn(t, c, "Volume", "pvc-1")
	longhorn("Volume", "pvc-1", "touched", "metadata", "annotations", "example.com/by")
	err = sc.patchLonghorn(ctx, longhornVolumeKind, "pvc-1", stale.GetResourceVersion(), map[string]interface{}{"numberOfReplicas": 3})
	assert.True(t, apierrors.IsConflict(err), "a stale resourceVersion is refused: %v", err)
}
