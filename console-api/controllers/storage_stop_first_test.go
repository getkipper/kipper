package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/controller/pkg/stranded"
)

// lhAttachmentObj is pvc-1's VolumeAttachment. It holds Kipper's ticket for
// node, satisfied by Longhorn, unless node is empty.
func lhAttachmentObj(node string) *unstructured.Unstructured {
	tickets, statuses := map[string]interface{}{}, map[string]interface{}{}
	if node != "" {
		tickets[conversionTicket] = map[string]interface{}{"id": conversionTicket, "type": conversionTicketType, "nodeID": node, "generation": int64(1),
			"parameters": map[string]interface{}{"disableFrontend": "true"}}
		statuses[conversionTicket] = map[string]interface{}{"id": conversionTicket, "satisfied": true, "generation": int64(1)}
	}
	return longhornObject("VolumeAttachment", "pvc-1", map[string]interface{}{
		"spec":   map[string]interface{}{"volume": "pvc-1", "attachmentTickets": tickets},
		"status": map[string]interface{}{"attachmentTicketStatuses": statuses},
	})
}

// heldOnServer marks volume attached on server-1 with its frontend disabled.
func heldOnServer(volume *unstructured.Unstructured) *unstructured.Unstructured {
	status := volume.Object["status"].(map[string]interface{})
	status["currentNodeID"] = "server-1"
	status["frontendDisabled"] = true
	return volume
}

// attachedOn marks volume attached on node for a pod, with the frontend on.
func attachedOn(volume *unstructured.Unstructured, node string) *unstructured.Unstructured {
	volume.Object["status"].(map[string]interface{})["currentNodeID"] = node
	return volume
}

// counterOff turns Longhorn's revision counter off on a volume, engine or
// replica object, as on volumes created before Kipper turned it on.
func counterOff(obj *unstructured.Unstructured) *unstructured.Unstructured {
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "revisionCounterDisabled")
	return obj
}

func counterDisabled(t *testing.T, c crclient.Client, kind, name string) bool {
	t.Helper()
	disabled, _, _ := unstructured.NestedBool(getLonghorn(t, c, kind, name).Object, "spec", "revisionCounterDisabled")
	return disabled
}

// stoppedClaim is db's data claim carrying a stop of one replica.
func stoppedClaim() *corev1.PersistentVolumeClaim {
	pvc := dbDataClaim()
	pvc.Annotations = map[string]string{platform.CopyChangeStopAnnotation: `{"replicas":1,"at":"2026-10-24T11:59:00Z"}`}
	return pvc
}

// heldWorld is convWorld with pvc-1 held on server-1 for the service db,
// which is stopped for the change, plus objs.
func heldWorld(volume *unstructured.Unstructured, objs ...crclient.Object) []crclient.Object {
	return heldWorldOf(stoppedClaim(), volume, objs...)
}

// heldWorldOf is heldWorld with claim as db's stopped data claim.
func heldWorldOf(claim *corev1.PersistentVolumeClaim, volume *unstructured.Unstructured, objs ...crclient.Object) []crclient.Object {
	return convWorld(append([]crclient.Object{heldOnServer(volume), lhAttachmentObj("server-1"), claim, dbStatefulSet(0)}, objs...)...)
}

func dbStatefulSet(replicas int32) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: "db"}, Spec: appsv1.StatefulSetSpec{Replicas: ptr.To(replicas)}}
}

// dbPod is db's pod on node, mounting its data claim.
func dbPod(node string) *corev1.Pod {
	return claimPod("db-0", node, metav1.OwnerReference{APIVersion: "apps/v1", Kind: "StatefulSet", Name: "db", UID: "uid-db", Controller: ptr.To(true)})
}

func claimPod(name, node string, owners ...metav1.OwnerReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name, OwnerReferences: owners},
		Spec: corev1.PodSpec{NodeName: node, Volumes: []corev1.Volume{{Name: "data", VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "data-db-0"}}}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

// serviceConverterFor is converterFor for the service db, which a conversion stops.
func serviceConverterFor(t *testing.T, objs ...crclient.Object) (storageConverter, crclient.Client) {
	sc, c := converterFor(t, objs...)
	sc.StatefulSet = "db"
	sc.Commands = serviceStorageCommands("db")
	return sc, c
}

// twoCopies is pvc-1 at two copies, both RW in an engine on node.
func twoCopies(node string, objs ...crclient.Object) []crclient.Object {
	e := lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"})
	_ = unstructured.SetNestedField(e.Object, node, "spec", "nodeID")
	_ = unstructured.SetNestedField(e.Object, node, "status", "ownerID")
	return append([]crclient.Object{
		longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""), longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""), e,
	}, objs...)
}

func stopOf(t *testing.T, c crclient.Client) *platform.CopyChangeStop {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "data-db-0"}, &pvc))
	var stop platform.CopyChangeStop
	found, err := platform.ReadRecord(pvc.Annotations, platform.CopyChangeStopAnnotation, &stop)
	if !found {
		return nil
	}
	require.NoError(t, err)
	return &stop
}

func replicasOfDB(t *testing.T, c crclient.Client) int32 {
	t.Helper()
	var sts appsv1.StatefulSet
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	return *sts.Spec.Replicas
}

func ticketOf(t *testing.T, c crclient.Client) map[string]interface{} {
	t.Helper()
	ticket, _, _ := unstructured.NestedMap(getLonghorn(t, c, "VolumeAttachment", "pvc-1").Object, "spec", "attachmentTickets", conversionTicket)
	return ticket
}

func replicaExists(t *testing.T, c crclient.Client, name string) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: longhornNamespace, Name: name}, longhornObject("Replica", name, map[string]interface{}{}))
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func countOf(t *testing.T, c crclient.Client) int64 {
	t.Helper()
	count, _, _ := unstructured.NestedInt64(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "numberOfReplicas")
	return count
}

func convertDB(t *testing.T, sc storageConverter, copies int32) *metav1.Condition {
	t.Helper()
	cond, err := sc.convert(context.Background(), boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1"), ptr.To(copies))
	require.NoError(t, err)
	if c, ok := sc.Reader.(crclient.Client); ok {
		assertCountKept(t, c)
	}
	return cond
}

// A reduction of a running service records the stop with the replicas the
// StatefulSet had, then scales it to zero, and changes nothing in Longhorn.
func TestAReductionStopsTheServiceBeforeTouchingACopy(t *testing.T) {
	volume := attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1")
	sc, c := serviceConverterFor(t, convWorld(append(twoCopies("worker-1"), volume, lhAttachmentObj(""), dbDataClaim(), dbStatefulSet(1), dbPod("worker-1"))...)...)

	cond := convertDB(t, sc, 1)

	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason, "the copies may still be set back")
	assert.Contains(t, cond.Message, "db is stopped for the copy change")
	assert.Contains(t, cond.Message, "Waiting for the volume to detach.")
	assert.Contains(t, cond.Message, "Setting the copies back with 'kip service update db --copies 2' starts db again at two copies. "+
		"If Longhorn has built the server copy for the change already, Kipper keeps it, and Longhorn removes one worker copy first.")
	stop := stopOf(t, c)
	require.NotNil(t, stop)
	assert.Equal(t, ptr.To(int32(1)), stop.Replicas)
	assert.Equal(t, int32(0), replicasOfDB(t, c))
	assert.Equal(t, int64(2), countOf(t, c))
	assert.True(t, replicaExists(t, c, "r-w"))
	assert.Nil(t, ticketOf(t, c), "no ticket while the volume is attached for the pod")
}

// A volume attached on a node that cannot report cannot detach, and a pod
// Kipper does not own keeps it attached. Both block with a way out. A node
// that holds a copy blocks before anything is stopped, since the held engine
// could not compare that copy.
func TestTheWaitForADetachNamesWhatHoldsTheVolume(t *testing.T) {
	sc, _ := serviceConverterFor(t, convWorld(append(twoCopies("worker-2"), attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-2"),
		kubeNode("worker-2", corev1.ConditionUnknown), lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(0), dbPod("worker-2"))...)...)
	cond := convertDB(t, sc, 1)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.Contains(t, cond.Message, "still attached on worker-2")
	assert.Contains(t, cond.Message, "kip node fence worker-2")

	down := convWorld(append(twoCopies("worker-1"), attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1"),
		lhAttachmentObj(""), dbDataClaim(), dbStatefulSet(1), dbPod("worker-1"))...)
	down[3] = kubeNode("worker-1", corev1.ConditionUnknown)
	sc, c := serviceConverterFor(t, down...)
	cond = convertDB(t, sc, 1)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.True(t, strings.HasPrefix(cond.Message, "db keeps running. A copy of this volume is on worker-1"), cond.Message)
	assert.NotContains(t, cond.Message, "starts again")
	assert.Nil(t, stopOf(t, c), "nothing is stopped for a change that cannot go on")
	assert.Equal(t, int32(1), replicasOfDB(t, c))

	sc, _ = serviceConverterFor(t, convWorld(append(twoCopies("worker-1"), attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1"),
		lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(0), claimPod("debug", "worker-1"))...)...)
	cond = convertDB(t, sc, 1)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.Contains(t, cond.Message, "in use by pod debug")
}

// A StatefulSet scaled up by anything else while the stop lasts goes back to zero.
func TestAStoppedServiceStaysStopped(t *testing.T) {
	sc, c := serviceConverterFor(t, convWorld(append(twoCopies("worker-1"), attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1"),
		lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(1))...)...)
	convertDB(t, sc, 1)
	assert.Equal(t, int32(0), replicasOfDB(t, c))
	assert.Equal(t, ptr.To(int32(1)), stopOf(t, c).Replicas, "the recorded count is kept")
}

// A block that stopping cannot help, such as a copy on a node that is down,
// keeps a service that is already stopped stopped, and the condition says so.
func TestAStoppedServiceStaysStoppedWhileTheChangeIsBlocked(t *testing.T) {
	objs := convWorld(append(twoCopies("worker-1"), attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1"),
		lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(1))...)
	objs[3] = kubeNode("worker-1", corev1.ConditionUnknown)
	sc, c := serviceConverterFor(t, objs...)

	cond := convertDB(t, sc, 1)

	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.Contains(t, cond.Message, "db is stopped for the copy change")
	assert.Contains(t, cond.Message, "A copy of this volume is on worker-1")
	assert.Equal(t, int32(0), replicasOfDB(t, c))
	assert.True(t, replicaExists(t, c, "r-w"))
}

// A cordoned server takes no attachment, because Longhorn can drop Kipper's
// ticket on a cordoned node. The change is Blocked, names the cordon and
// adds no ticket, and it goes on once the server is uncordoned.
func TestACordonedServerBlocksTheHold(t *testing.T) {
	world := convWorld(append(twoCopies("server-1"), detached(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag)),
		lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(0))...)
	world[1].(*corev1.Node).Spec.Unschedulable = true
	sc, c := serviceConverterFor(t, world...)

	cond := convertDB(t, sc, 1)

	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.Contains(t, cond.Message, "server-1 is cordoned, so Kipper does not attach the volume there to compare its copies. The change continues once server-1 is uncordoned.")
	assert.Contains(t, cond.Message, "'kip service update db --copies 2'")
	assert.Nil(t, ticketOf(t, c))

	var node corev1.Node
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "server-1"}, &node))
	node.Spec.Unschedulable = false
	require.NoError(t, c.Update(context.Background(), &node))
	cond = convertDB(t, sc, 1)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	assert.Equal(t, "server-1", ticketOf(t, c)["nodeID"])
}

// Once detached, a volume with the counter on is attached on the server
// through Kipper's ticket with the frontend disabled.
func TestADetachedVolumeIsAttachedOnTheServerWithNothingAbleToWrite(t *testing.T) {
	sc, c := serviceConverterFor(t, convWorld(append(twoCopies("server-1"), detached(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag)),
		lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(0))...)...)

	cond := convertDB(t, sc, 1)

	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	assert.Contains(t, cond.Message, "Longhorn attaches the volume on server-1 with nothing able to write to it")
	ticket := ticketOf(t, c)
	require.NotNil(t, ticket)
	assert.Equal(t, conversionTicket, ticket["id"])
	assert.Equal(t, "longhorn-api", ticket["type"])
	assert.Equal(t, "server-1", ticket["nodeID"])
	assert.Equal(t, map[string]interface{}{"disableFrontend": "true", "lastAttachedBy": "kipper"}, ticket["parameters"])
	assert.True(t, replicaExists(t, c, "r-w"))
}

// The counter goes on everywhere while the volume is detached, when
// Longhorn's records show every copy healthy, and before any ticket.
func TestTheCounterIsSwitchedOnEverywhereWhileDetached(t *testing.T) {
	objs := twoCopies("server-1", counterOff(detached(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag))),
		lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(0))
	for _, o := range objs[:3] {
		counterOff(o.(*unstructured.Unstructured))
	}
	sc, c := serviceConverterFor(t, convWorld(objs...)...)

	cond := convertDB(t, sc, 1)

	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	assert.Contains(t, cond.Message, "switched on Longhorn's revision counter")
	for kind, name := range map[string]string{"Volume": "pvc-1", "Engine": "pvc-1-e-0", "Replica": "r-s"} {
		assert.False(t, counterDisabled(t, c, kind, name), "%s %s", kind, name)
	}
	assert.False(t, counterDisabled(t, c, "Replica", "r-w"))
	assert.Nil(t, ticketOf(t, c), "the ticket waits for the next pass")
}

// Longhorn's records must show no failed copy and no fault before the switch.
// Otherwise every copy is kept and the counter stays off.
func TestTheCounterSwitchWaitsForLonghornsRecordsToShowEveryCopyHealthy(t *testing.T) {
	for name, spoil := range map[string]func(objs []crclient.Object){
		"failed copy": func(objs []crclient.Object) {
			_ = unstructured.SetNestedField(objs[1].(*unstructured.Unstructured).Object, "2026-10-24T11:00:00Z", "spec", "failedAt")
		},
		"copy in error": func(objs []crclient.Object) {
			_ = unstructured.SetNestedField(objs[1].(*unstructured.Unstructured).Object, "error", "status", "currentState")
		},
		"faulted volume": func(objs []crclient.Object) {
			_ = unstructured.SetNestedField(objs[3].(*unstructured.Unstructured).Object, "faulted", "status", "robustness")
		},
	} {
		t.Run(name, func(t *testing.T) {
			objs := twoCopies("server-1", counterOff(detached(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag))),
				lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(0))
			for _, o := range objs[:3] {
				counterOff(o.(*unstructured.Unstructured))
			}
			spoil(objs)
			sc, c := serviceConverterFor(t, convWorld(objs...)...)

			cond := convertDB(t, sc, 1)

			assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
			assert.Contains(t, cond.Message, "created without Longhorn's revision counter")
			assert.Contains(t, cond.Message, "Setting the copies back with 'kip service update db --copies 2' starts db again")
			assert.True(t, counterDisabled(t, c, "Volume", "pvc-1"))
			assert.True(t, counterDisabled(t, c, "Replica", "r-s"))
			assert.Nil(t, ticketOf(t, c))
		})
	}
}

// A switch an earlier pass left half done is completed while detached, since
// the volume was checked before its first write.
func TestAHalfSwitchedCounterIsCompleted(t *testing.T) {
	objs := twoCopies("server-1", counterOff(detached(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag))),
		lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(0))
	counterOff(objs[2].(*unstructured.Unstructured))
	_ = unstructured.SetNestedField(objs[1].(*unstructured.Unstructured).Object, "2026-10-24T11:00:00Z", "spec", "failedAt")
	sc, c := serviceConverterFor(t, convWorld(objs...)...)

	convertDB(t, sc, 1)

	assert.False(t, counterDisabled(t, c, "Volume", "pvc-1"))
	assert.False(t, counterDisabled(t, c, "Engine", "pvc-1-e-0"))
}

// The counter is never switched while the volume is attached.
func TestTheCounterIsNeverSwitchedWhileAttached(t *testing.T) {
	objs := twoCopies("worker-1", counterOff(attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1")),
		lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(0))
	for _, o := range objs[:3] {
		counterOff(o.(*unstructured.Unstructured))
	}
	sc, c := serviceConverterFor(t, convWorld(objs...)...)
	convertDB(t, sc, 1)
	assert.True(t, counterDisabled(t, c, "Volume", "pvc-1"))
	assert.True(t, counterDisabled(t, c, "Replica", "r-w"))
}

// A ticket from before the counter was on is dropped, since the engine it
// started compared nothing.
func TestATicketWithoutTheCounterIsDropped(t *testing.T) {
	objs := heldWorld(lhVolumeObj("pvc-1", 1, platform.ReplicaStorageTag), twoCopies("server-1")...)
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetKind() == "Engine" {
			counterOff(u)
		}
	}
	sc, c := serviceConverterFor(t, objs...)

	cond := convertDB(t, sc, 1)

	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	assert.Nil(t, ticketOf(t, c))
	assert.True(t, replicaExists(t, c, "r-w"))
}

// Held on the server with every copy RW, the count is lowered with data
// locality on, so Longhorn removes the worker copy, and db stays stopped
// until locality is off again.
func TestAHeldVolumeLosesTheWorkerCopy(t *testing.T) {
	sc, c := serviceConverterFor(t, heldWorld(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), twoCopies("server-1")...)...)

	cond := convertDB(t, sc, 1)

	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	assert.Equal(t, int64(1), countOf(t, c))
	assert.Equal(t, "best-effort", localityOf(t, c))
	assert.True(t, replicaExists(t, c, "r-w"), "Kipper deletes no replica")
	assert.Equal(t, "r-w", longhornCleanup(t, c))

	cond = convertDB(t, sc, 1)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	assert.Equal(t, "disabled", localityOf(t, c))
	assert.True(t, replicaExists(t, c, "r-s"))
	assert.Equal(t, int32(0), replicasOfDB(t, c), "db stays stopped until locality is off")
}

// Without the hold, nothing is removed, however healthy the copies look: an
// engine attached for a pod on the server takes writes.
func TestNoCopyIsRemovedWithoutTheHold(t *testing.T) {
	for name, objs := range map[string][]crclient.Object{
		"attached for a pod": convWorld(append(twoCopies("server-1"), attachedOn(lhVolumeObj("pvc-1", 1, platform.ReplicaStorageTag), "server-1"),
			lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(0))...),
		"ticket not satisfied": func() []crclient.Object {
			objs := heldWorld(lhVolumeObj("pvc-1", 1, platform.ReplicaStorageTag), twoCopies("server-1")...)
			for _, o := range objs {
				if u, ok := o.(*unstructured.Unstructured); ok && u.GetKind() == "VolumeAttachment" {
					_ = unstructured.SetNestedField(u.Object, false, "status", "attachmentTicketStatuses", conversionTicket, "satisfied")
				}
			}
			return objs
		}(),
		"frontend on": func() []crclient.Object {
			objs := heldWorld(lhVolumeObj("pvc-1", 1, platform.ReplicaStorageTag), twoCopies("server-1")...)
			for _, o := range objs {
				if u, ok := o.(*unstructured.Unstructured); ok && u.GetKind() == "Volume" {
					_ = unstructured.SetNestedField(u.Object, false, "status", "frontendDisabled")
				}
			}
			return objs
		}(),
		"engine elsewhere": heldWorld(lhVolumeObj("pvc-1", 1, platform.ReplicaStorageTag), twoCopies("worker-1")...),
	} {
		t.Run(name, func(t *testing.T) {
			sc, c := serviceConverterFor(t, objs...)
			cond := convertDB(t, sc, 1)
			require.NotNil(t, cond)
			assert.NotEqual(t, kipperv1.StorageCopiesConverting, cond.Reason)
			assert.True(t, replicaExists(t, c, "r-w"))
		})
	}
}

// Once only the server copy is left, the ticket goes, db gets its replicas
// back and the stop record is removed, in that order of importance.
func TestTheServiceStartsAgainOnceTheCopyIsGone(t *testing.T) {
	volume := lhVolumeObj("pvc-1", 1, platform.ServerStorageTag)
	sc, c := serviceConverterFor(t, heldWorld(volume, longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""),
		lhEngine(map[string]interface{}{"r-s": "RW"}))...)

	cond := convertDB(t, sc, 1)

	assert.Nil(t, cond, "the live copies are reported")
	assert.Nil(t, ticketOf(t, c))
	assert.Equal(t, int32(1), replicasOfDB(t, c))
	assert.Nil(t, stopOf(t, c))
}

// Setting the copies back before the removal starts the service again and
// keeps every copy.
func TestSettingTheCopiesBackStartsTheServiceWithEveryCopy(t *testing.T) {
	sc, c := serviceConverterFor(t, heldWorld(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), twoCopies("server-1")...)...)

	convertDB(t, sc, 2)

	assert.Nil(t, ticketOf(t, c))
	assert.Equal(t, int32(1), replicasOfDB(t, c))
	assert.Nil(t, stopOf(t, c))
	assert.True(t, replicaExists(t, c, "r-w"))
	assert.Equal(t, int64(2), countOf(t, c))
}

// Unsetting the copies or losing the volume also ends the stop.
func TestAStopEndsWhenNothingIsLeftToConvert(t *testing.T) {
	sc, c := serviceConverterFor(t, heldWorld(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), twoCopies("server-1")...)...)
	cond, err := sc.convert(context.Background(), stoppedClaim(), nil)
	require.NoError(t, err)
	assert.Nil(t, cond)
	assert.Equal(t, int32(1), replicasOfDB(t, c))
	assert.Nil(t, stopOf(t, c))

	sc, c = serviceConverterFor(t, convWorld(stoppedClaim(), dbStatefulSet(0))...)
	convertDB(t, sc, 1)
	assert.Equal(t, int32(1), replicasOfDB(t, c))
	assert.Nil(t, stopOf(t, c))
}

// A raise needs no stop while the counter is on, and stops the service once
// to switch the counter on otherwise. The build runs after the restart.
func TestARaiseStopsTheServiceOnlyToSwitchTheCounterOn(t *testing.T) {
	sc, c := serviceConverterFor(t, convWorld(append(oneServerCopy(), dbStatefulSet(1))...)...)
	cond := convertDB(t, sc, 2)
	assert.Equal(t, kipperv1.StorageCopiesConverting, cond.Reason)
	assert.Equal(t, int32(1), replicasOfDB(t, c))
	assert.Equal(t, int64(2), countOf(t, c))

	objs := append(oneServerCopy(), dbStatefulSet(1), lhAttachmentObj(""))
	for _, o := range objs[:3] {
		counterOff(o.(*unstructured.Unstructured))
	}
	sc, c = serviceConverterFor(t, convWorld(objs...)...)
	cond = convertDB(t, sc, 2)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	assert.Equal(t, int32(0), replicasOfDB(t, c))
	assert.Equal(t, int64(1), countOf(t, c), "nothing is built before the counter is on")
}

// After the switch, a raise drops the stop and builds the copy live.
func TestARaiseBuildsOnceTheCounterIsOn(t *testing.T) {
	objs := append([]crclient.Object{detached(lhVolumeObj("pvc-1", 1, platform.ServerStorageTag)),
		longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""), lhEngine(map[string]interface{}{})},
		stoppedClaim(), dbStatefulSet(0), lhAttachmentObj(""))
	sc, c := serviceConverterFor(t, convWorld(objs...)...)

	cond := convertDB(t, sc, 2)

	assert.Equal(t, kipperv1.StorageCopiesConverting, cond.Reason)
	assert.Equal(t, int32(1), replicasOfDB(t, c))
	assert.Nil(t, stopOf(t, c))
	assert.Equal(t, int64(2), countOf(t, c))
}

// Kipper stops nothing it does not own: a volume whose claim is in use waits
// with the users named, and gets its stop only once nothing uses it. The
// backup holder is Kipper's and does not count.
func TestAVolumeIsStoppedOnlyOnceNothingElseUsesIt(t *testing.T) {
	world := func(objs ...crclient.Object) []crclient.Object {
		return convWorld(append(twoCopies("worker-1"), append([]crclient.Object{attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1"),
			lhAttachmentObj(""), dbDataClaim()}, objs...)...)...)
	}
	sc, c := converterFor(t, world(claimPod("app-7d9", "worker-1"))...)
	sc.Holder = "holder"
	sc.Commands = volumeStorageCommands("files")
	cond := convertDB(t, sc, 1)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.True(t, strings.HasPrefix(cond.Message, "The volume is in use by pod app-7d9."), cond.Message)
	assert.Contains(t, cond.Message, "Setting the copies back with 'kip volume update files --copies 2' stops the change and keeps every copy.")
	assert.Nil(t, stopOf(t, c))

	sc, c = converterFor(t, world(claimPod("holder", "worker-1"))...)
	sc.Holder = "holder"
	cond = convertDB(t, sc, 1)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)

	leaving := claimPod("app-7d9", "worker-1")
	leaving.DeletionTimestamp, leaving.Finalizers = ptr.To(metav1.NewTime(convNow)), []string{"example.com/test"}
	sc, _ = converterFor(t, world(leaving)...)
	cond = convertDB(t, sc, 1)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason, "a pod being deleted leaves the wait to the detach")
	assert.True(t, strings.HasPrefix(cond.Message, "Waiting for pod app-7d9 to finish terminating and the volume to detach."), cond.Message)
	require.NotNil(t, stopOf(t, c))
	assert.Nil(t, stopOf(t, c).Replicas)
}

// The stop record is written before the StatefulSet is scaled, so a failed
// scale leaves a record that the next pass finishes.
func TestTheStopIsRecordedBeforeTheServiceStops(t *testing.T) {
	var order []string
	objs := convWorld(append(twoCopies("worker-1"), attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1"),
		lhAttachmentObj(""), dbDataClaim(), dbStatefulSet(1))...)
	c := recordingPatches(objs, &order)
	sc := storageConverter{Reader: c, Writer: c, Now: func() time.Time { return convNow }, StatefulSet: "db"}
	_, err := sc.convert(context.Background(), dbDataClaim(), ptr.To(int32(1)))
	require.NoError(t, err)
	assert.Equal(t, []string{"PersistentVolumeClaim", "StatefulSet"}, order)
}

// recordingPatches is a client for objs that appends the kind of every
// object it patches to order.
func recordingPatches(objs []crclient.Object, order *[]string) crclient.Client {
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch, opts ...crclient.PatchOption) error {
			switch o := obj.(type) {
			case *corev1.PersistentVolumeClaim:
				*order = append(*order, "PersistentVolumeClaim")
			case *appsv1.StatefulSet:
				*order = append(*order, "StatefulSet")
			case *unstructured.Unstructured:
				*order = append(*order, o.GetKind())
			}
			return cl.Patch(ctx, obj, patch, opts...)
		}}).Build()
}

// failingCounterPatches is a client for objs whose Longhorn patches fail
// after the first after of them while *fail is set, as if console-api stopped
// partway through the counter switch.
func failingCounterPatches(objs []crclient.Object, after int, fail *bool) crclient.Client {
	sent := 0
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).WithObjects(withDataClaim(objs)...).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch, opts ...crclient.PatchOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && *fail && slices.Contains([]string{"Replica", "Engine", "Volume"}, u.GetKind()) {
				if sent == after {
					return apierrors.NewServerTimeout(schema.GroupResource{Group: "longhorn.io", Resource: strings.ToLower(u.GetKind()) + "s"}, "patch", 1)
				}
				sent++
			}
			return cl.Patch(ctx, obj, patch, opts...)
		}}).Build()
}

// A counter switch interrupted after any of its patches is finished before db
// starts again, whether the copies are set back or unset meanwhile, since an
// engine started with mixed counters marks copies ERR.
func TestAnInterruptedCounterSwitchIsFinishedBeforeTheServiceStarts(t *testing.T) {
	for _, after := range []int{1, 2, 3} {
		for name, wanted := range map[string]*int32{"set back": ptr.To(int32(2)), "unset": nil} {
			t.Run(fmt.Sprintf("after %d patches, %s", after, name), func(t *testing.T) {
				objs := twoCopies("server-1", counterOff(detached(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag))),
					lhAttachmentObj(""), stoppedClaim(), dbStatefulSet(0))
				for _, o := range objs[:3] {
					counterOff(o.(*unstructured.Unstructured))
				}
				fail := true
				c := failingCounterPatches(convWorld(objs...), after, &fail)
				sc := storageConverter{Reader: c, Writer: c, Now: func() time.Time { return convNow }, StatefulSet: "db"}
				_, err := convertOnce(t, sc, ptr.To(int32(1)))
				require.Error(t, err, "the switch stops partway")
				fail = false

				cond, err := convertOnce(t, sc, wanted)
				require.NoError(t, err)
				require.NotNil(t, cond)
				assert.Contains(t, cond.Message, "switched on Longhorn's revision counter")
				assert.Equal(t, int32(0), replicasOfDB(t, c), "db waits for the switch")
				for kind, name := range map[string]string{"Volume": "pvc-1", "Engine": "pvc-1-e-0", "Replica": "r-s"} {
					assert.False(t, counterDisabled(t, c, kind, name), "%s %s", kind, name)
				}
				assert.False(t, counterDisabled(t, c, "Replica", "r-w"))

				_, err = convertOnce(t, sc, wanted)
				require.NoError(t, err)
				assert.Equal(t, int32(1), replicasOfDB(t, c))
				assert.Nil(t, stopOf(t, c))
			})
		}
	}
}

// On Longhorn before 1.9, which builds copies only while a volume is attached,
// a raise on a volume without the counter stops db once, switches the counter
// on while detached, starts db again and builds the copy live.
func TestARaiseBeforeLonghorn19SwitchesTheCounterOnceAndBuildsLive(t *testing.T) {
	objs := append(oneServerCopy(), dbStatefulSet(1), lhAttachmentObj(""))
	for _, o := range objs[:3] {
		counterOff(o.(*unstructured.Unstructured))
	}
	world := convWorld(objs...)
	world[4] = lhSetting("current-longhorn-version", "v1.8.2")
	sc, c := serviceConverterFor(t, world...)

	convertDB(t, sc, 2)
	require.Equal(t, int32(0), replicasOfDB(t, c))
	setLonghornField(t, c, "Volume", "pvc-1", "detached", "status", "state")

	cond := convertDB(t, sc, 2)
	assert.Contains(t, cond.Message, "switched on Longhorn's revision counter")
	assert.False(t, counterDisabled(t, c, "Volume", "pvc-1"))

	for range 2 {
		cond = convertDB(t, sc, 2)
		assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason, "db attaches the volume once it runs")
		assert.Equal(t, "Waiting for db to attach the volume. This Longhorn version builds copies only while the volume is attached.", cond.Message)
		assert.Equal(t, int32(1), replicasOfDB(t, c), "db starts again for the build")
		assert.Nil(t, stopOf(t, c))
	}

	setLonghornField(t, c, "Volume", "pvc-1", "attached", "status", "state")
	cond = convertDB(t, sc, 2)
	assert.Equal(t, kipperv1.StorageCopiesConverting, cond.Reason)
	assert.Equal(t, int64(2), countOf(t, c))
	assert.Equal(t, int32(1), replicasOfDB(t, c), "the build runs live")
}

// Before Longhorn 1.9, a raise on a detached volume of a service that is not
// set to run waits for nothing that will attach it, so it is Blocked. That
// includes a service kip node fence stopped, which the end of a stop leaves
// at zero.
func TestARaiseBeforeLonghorn19OnAStoppedServiceIsBlocked(t *testing.T) {
	for name, sts := range map[string]*appsv1.StatefulSet{"stopped": dbStatefulSet(0), "fence-stopped": fenceStopped()} {
		t.Run(name, func(t *testing.T) {
			objs := append(oneServerCopy(), sts, lhAttachmentObj(""))
			world := convWorld(objs...)
			world[4] = lhSetting("current-longhorn-version", "v1.8.2")
			sc, c := serviceConverterFor(t, world...)
			setLonghornField(t, c, "Volume", "pvc-1", "detached", "status", "state")

			cond := convertDB(t, sc, 2)

			assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
			assert.Contains(t, cond.Message, "builds copies only while the volume is attached")
		})
	}
}

// A pod Kipper does not own that mounts the data claim blocks a service
// reduction before db is stopped, since stopping db cannot detach the volume.
func TestAServiceIsStoppedOnlyOnceNothingElseUsesItsClaim(t *testing.T) {
	sc, c := serviceConverterFor(t, convWorld(append(twoCopies("worker-1"), attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1"),
		lhAttachmentObj(""), dbDataClaim(), dbStatefulSet(1), dbPod("worker-1"), claimPod("debug", "worker-1"))...)...)

	cond := convertDB(t, sc, 1)

	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.Equal(t, "db keeps running. The volume is in use by pod debug. Kipper stops only what it owns, so the change continues once nothing else uses the volume. "+
		"Setting the copies back with 'kip service update db --copies 2' stops the change and keeps every copy. "+
		"On a cluster that offers one copy, setting the copies back needs an admin.", cond.Message)
	assert.Nil(t, stopOf(t, c))
	assert.Equal(t, int32(1), replicasOfDB(t, c))
}

// fenceStopped is db's StatefulSet as kip node fence leaves it: at zero, with
// the fence's mark.
func fenceStopped() *appsv1.StatefulSet {
	sts := dbStatefulSet(0)
	sts.Annotations = map[string]string{stranded.FenceStopAnnotation: `{"node":"worker-2","replicas":1}`}
	return sts
}

// A stop of a service kip node fence left at zero records one replica, as the
// fence does, so a 'kip service start' during the change is not undone by it.
func TestAStopOfAFenceStoppedServiceRecordsOneReplica(t *testing.T) {
	sc, c := serviceConverterFor(t, convWorld(append(twoCopies("worker-1"), attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1"),
		lhAttachmentObj(""), dbDataClaim(), fenceStopped())...)...)

	convertDB(t, sc, 1)

	require.NotNil(t, stopOf(t, c))
	assert.Equal(t, ptr.To(int32(1)), stopOf(t, c).Replicas)
}

// The end of a stop leaves a StatefulSet that kip node fence has marked at
// zero, since only kip's own checks start a fence-stopped service.
func TestTheEndOfAStopLeavesAFenceStoppedServiceStopped(t *testing.T) {
	sc, c := serviceConverterFor(t, convWorld(append(twoCopies("server-1"), lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag),
		lhAttachmentObj(""), stoppedClaim(), fenceStopped())...)...)

	convertDB(t, sc, 2)

	assert.Nil(t, stopOf(t, c))
	assert.Equal(t, int32(0), replicasOfDB(t, c))
}

// serverBuildWorld is a volume of two worker copies reduced to one, held on
// server-1 for the stopped service db while Longhorn builds the server copy
// at count three. server is the server copy's mode in the engine, or empty
// while Longhorn has not placed it yet.
func serverBuildWorld(server string) []crclient.Object {
	volume := lhVolumeObj("pvc-1", 3, platform.ServerStorageTag)
	volume.SetAnnotations(map[string]string{copiesRebuildAnnotation: convNow.Add(-time.Minute).Format(time.RFC3339)})
	modes := map[string]interface{}{"r-w": "RW", "r-w2": "RW"}
	world := []crclient.Object{longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""), longhornReplica("r-w2", "pvc-1", "worker-2", healthyTime, ""),
		lhDiskNode("worker-2", 90*gib, platform.ReplicaStorageTag), kubeNode("worker-2", corev1.ConditionTrue)}
	if server != "" {
		healthy := healthyTime
		if server == "WO" {
			healthy = ""
		}
		modes["r-s"] = server
		world = append(world, longhornReplica("r-s", "pvc-1", "server-1", healthy, ""))
	}
	return heldWorld(volume, append(world, lhEngine(modes))...)
}

// Setting a reduction back before Longhorn has placed the server copy
// cancels it without waiting for the build: the held volume's count goes
// back to two, then db starts again and both worker copies stay.
func TestASetBackBeforeTheServerCopyIsPlacedCancelsTheReduction(t *testing.T) {
	sc, c := serviceConverterFor(t, serverBuildWorld("")...)

	cond := convertDB(t, sc, 2)
	require.NotNil(t, cond)
	assert.True(t, strings.HasPrefix(cond.Message, "db is stopped for the copy change and starts again once it is done. Lowering the copy count to 2."), cond.Message)
	assert.Equal(t, int64(2), countOf(t, c))

	convertUntilDone(t, sc, c, 2)
	selector, _, _ := unstructured.NestedStringSlice(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "nodeSelector")
	assert.Equal(t, []string{platform.ReplicaStorageTag}, selector)
	assert.Nil(t, stopOf(t, c))
	assert.Nil(t, ticketOf(t, c))
	assert.Equal(t, int32(1), replicasOfDB(t, c))
	assert.True(t, replicaExists(t, c, "r-w"))
	assert.True(t, replicaExists(t, c, "r-w2"))
}

// A server copy that Longhorn is still building is waited for under the hold,
// as in any reduction, so nothing is removed and db stays stopped.
func TestASetBackWhileTheServerCopyBuildsWaitsUnderTheHold(t *testing.T) {
	sc, c := serviceConverterFor(t, serverBuildWorld("WO")...)

	cond := convertDB(t, sc, 2)

	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	assert.Contains(t, cond.Message, "db is stopped for the copy change")
	assert.Equal(t, int32(0), replicasOfDB(t, c))
	for _, name := range []string{"r-s", "r-w", "r-w2"} {
		assert.True(t, replicaExists(t, c, name), name)
	}
}

// After Longhorn has built the server copy, a set-back finishes as a held
// reduction to the copies set: the server copy stays and one worker copy
// goes, so the volume ends at exactly two copies.
func TestASetBackAfterTheServerCopyIsBuiltEndsAtTheSetCount(t *testing.T) {
	sc, c := serviceConverterFor(t, serverBuildWorld("RW")...)

	cond := convertDB(t, sc, 2)
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "db is stopped for the copy change")
	assert.Equal(t, int64(2), countOf(t, c))
	assert.Equal(t, int32(0), replicasOfDB(t, c))

	convertUntilDone(t, sc, c, 2)
	assert.True(t, replicaExists(t, c, "r-s"))
	assert.NotEqual(t, replicaExists(t, c, "r-w"), replicaExists(t, c, "r-w2"), "exactly one worker copy is removed")
	assert.Equal(t, int64(2), countOf(t, c))
	assert.Equal(t, "disabled", localityOf(t, c))
	assert.Equal(t, int32(1), replicasOfDB(t, c))
}

// convertUntilDone runs Longhorn's cleanup and a pass of db's conversion to
// wanted until the pass reports nothing more to do.
func convertUntilDone(t *testing.T, sc storageConverter, c crclient.Client, wanted int32) {
	t.Helper()
	for range 8 {
		longhornCleanup(t, c)
		if convertDB(t, sc, wanted) == nil {
			return
		}
	}
	t.Fatal("the conversion did not finish within 8 passes")
}

// A service set back after the hold lowered its count to one gets the count
// raised before the ticket goes and the service starts, and the step after it
// is planned for the raised count in the same pass.
func TestAServiceSetBackRaisesTheCountBeforeItStarts(t *testing.T) {
	sc, c := serviceConverterFor(t, removalWorld()...)

	cond, err := sc.convert(context.Background(), storedDataClaim(t, c), ptr.To(int32(2)))

	require.NoError(t, err)
	assert.Nil(t, cond)
	assert.Equal(t, int64(2), countOf(t, c))
	assert.Nil(t, ticketOf(t, c))
	assert.Nil(t, stopOf(t, c))
	assert.Equal(t, int32(1), replicasOfDB(t, c))
}

// Without a copy-change stop the count is Longhorn's own: while it replaces a
// copy that is being evicted it has one copy more than it asks for, and Kipper
// leaves that alone, or the evicted copy would never go.
func TestLonghornsOwnReplacementKeepsItsCount(t *testing.T) {
	evicting := longhornReplica("r-x", "pvc-1", "worker-2", healthyTime, "")
	_ = unstructured.SetNestedField(evicting.Object, true, "spec", "evictionRequested")
	sc, c := serviceConverterFor(t, convWorld(append(twoCopies("worker-1"), attachedOn(lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag), "worker-1"),
		evicting, dbStatefulSet(1))...)...)

	convertDB(t, sc, 2)

	assert.Equal(t, int64(2), countOf(t, c))
}
