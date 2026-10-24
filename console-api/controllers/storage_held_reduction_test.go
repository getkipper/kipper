package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
)

// removalWorld is a two-copy volume held on the server whose count the
// reduction already lowered to one with data locality on, so Longhorn's next
// cleanup removes the worker copy r-w and keeps the server copy r-s.
func removalWorld() []crclient.Object {
	v := lhVolumeObj("pvc-1", 1, platform.ReplicaStorageTag)
	v.SetUID("uid-pvc-1")
	v.Object["status"].(map[string]interface{})["robustness"] = "healthy"
	v.Object["spec"].(map[string]interface{})["dataLocality"] = "best-effort"
	server := longhornReplica("r-s", "pvc-1", "server-1", healthyTime, "")
	worker := longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, "")
	return heldWorld(v, server, worker, lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"}))
}

// beforeLowering is removalWorld before the count was lowered: the volume asks
// for two copies with data locality off.
func beforeLowering() []crclient.Object {
	objs := removalWorld()
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetKind() == "Volume" {
			spec := u.Object["spec"].(map[string]interface{})
			spec["numberOfReplicas"], spec["dataLocality"] = int64(2), "disabled"
		}
	}
	return objs
}

func removalConverter(c crclient.Client, now time.Time) storageConverter {
	return storageConverter{Reader: c, Writer: c, Now: func() time.Time { return now }}
}

func storedDataClaim(t *testing.T, c crclient.Client) *corev1.PersistentVolumeClaim {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "data-db-0"}, &pvc))
	return &pvc
}

func setEngineModes(t *testing.T, c crclient.Client, modes map[string]interface{}) {
	t.Helper()
	e := getLonghorn(t, c, "Engine", "pvc-1-e-0")
	require.NoError(t, unstructured.SetNestedField(e.Object, modes, "status", "replicaModeMap"))
	require.NoError(t, c.Update(context.Background(), e))
}

func setLonghornField(t *testing.T, c crclient.Client, kind, name string, value interface{}, path ...string) {
	t.Helper()
	obj := getLonghorn(t, c, kind, name)
	require.NoError(t, unstructured.SetNestedField(obj.Object, value, path...))
	require.NoError(t, c.Update(context.Background(), obj))
}

// convertOnce runs one pass on the data claim as the reconciler reads it.
func convertOnce(t *testing.T, sc storageConverter, wanted *int32) (*metav1.Condition, error) {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, sc.Reader.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "data-db-0"}, &pvc))
	cond, err := sc.convert(context.Background(), &pvc, wanted)
	if c, ok := sc.Reader.(crclient.Client); ok {
		assertCountKept(t, c)
	}
	return cond, err
}

// setEngine places pvc-1's engine on node, as Longhorn's manager on owner
// reports it, in state.
func setEngine(t *testing.T, c crclient.Client, node, owner, state string) {
	t.Helper()
	setLonghornField(t, c, "Engine", "pvc-1-e-0", node, "spec", "nodeID")
	setLonghornField(t, c, "Engine", "pvc-1-e-0", owner, "status", "ownerID")
	setLonghornField(t, c, "Engine", "pvc-1-e-0", state, "status", "currentState")
}

func setNodeReady(t *testing.T, c crclient.Client, name string, ready corev1.ConditionStatus) {
	t.Helper()
	var node corev1.Node
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name}, &node))
	node.Status.Conditions = []corev1.NodeCondition{{Type: corev1.NodeReady, Status: ready, LastTransitionTime: metav1.NewTime(convNow.Add(-time.Hour))}}
	require.NoError(t, c.Status().Update(context.Background(), &node))
}

// setServerManagerDown marks the Longhorn node server-1 not Ready because
// its longhorn-manager pod is down, while Kubernetes still reports it Ready.
func setServerManagerDown(t *testing.T, c crclient.Client) {
	t.Helper()
	setLonghornField(t, c, "Node", "server-1", []interface{}{
		map[string]interface{}{"type": "Ready", "status": "False", "reason": "ManagerPodDown"},
		map[string]interface{}{"type": "Schedulable", "status": "True"},
	}, "status", "conditions")
}

// loseTheTicket does what Longhorn's handleNodeCordoned does to Kipper's
// longhorn-api ticket when the server is cordoned with
// detach-manually-attached-volumes-when-cordoned on: the ticket goes and the
// volume detaches.
func loseTheTicket(t *testing.T, c crclient.Client) {
	t.Helper()
	setLonghornField(t, c, "VolumeAttachment", "pvc-1", map[string]interface{}{}, "spec", "attachmentTickets")
	setLonghornField(t, c, "VolumeAttachment", "pvc-1", map[string]interface{}{}, "status", "attachmentTicketStatuses")
	volume := getLonghorn(t, c, "Volume", "pvc-1")
	status := volume.Object["status"].(map[string]interface{})
	status["state"], status["currentNodeID"], status["frontendDisabled"] = "detached", "", false
	require.NoError(t, c.Update(context.Background(), volume))
	setLonghornField(t, c, "Engine", "pvc-1-e-0", "", "spec", "nodeID")
	setLonghornField(t, c, "Engine", "pvc-1-e-0", "stopped", "status", "currentState")
	setLonghornField(t, c, "Engine", "pvc-1-e-0", map[string]interface{}{}, "status", "replicaModeMap")
}

// longhornActor marks a context as Longhorn's, whose replica deletes the
// guard lets through.
type longhornActor struct{}

// longhornCleanup models the eviction and locality branches of Longhorn
// 1.11.3's cleanupExtraHealthyReplicas for pvc-1. It selects replicas by name
// rather than Longhorn's disk and node grouping, so fixtures must not depend
// on that grouping. While healthy copies exceed the requested count, it deletes
// one: a copy it is evicting first, keeping one evicting copy when no other
// healthy copy is left, and otherwise, with data locality on and a copy on the
// engine's node, the first copy by name on another node. It returns the
// deleted replica's name, or "" when it deleted none.
func longhornCleanup(t *testing.T, c crclient.Client) string {
	t.Helper()
	ctx := context.WithValue(context.Background(), longhornActor{}, true)
	vol := getLonghorn(t, c, "Volume", "pvc-1")
	count, _, _ := unstructured.NestedInt64(vol.Object, "spec", "numberOfReplicas")
	locality, _, _ := unstructured.NestedString(vol.Object, "spec", "dataLocality")
	engine := getLonghorn(t, c, "Engine", "pvc-1-e-0")
	engineNode, _, _ := unstructured.NestedString(engine.Object, "spec", "nodeID")
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(longhornReplicaList)
	require.NoError(t, c.List(ctx, list, crclient.InNamespace(longhornNamespace), crclient.MatchingLabels{"longhornvolume": "pvc-1"}))
	replicas := list.Items
	slices.SortFunc(replicas, func(a, b unstructured.Unstructured) int { return strings.Compare(a.GetName(), b.GetName()) })
	field := func(r *unstructured.Unstructured, name string) string {
		s, _, _ := unstructured.NestedString(r.Object, "spec", name)
		return s
	}
	evicting := func(r *unstructured.Unstructured) bool {
		b, _, _ := unstructured.NestedBool(r.Object, "spec", "evictionRequested")
		return b
	}
	var healthy int64
	nonEvicting, kept := false, ""
	for i := range replicas {
		r := &replicas[i]
		if field(r, "failedAt") != "" || field(r, "healthyAt") == "" || r.GetDeletionTimestamp() != nil {
			continue
		}
		healthy++
		if evicting(r) {
			kept = r.GetName()
		} else {
			nonEvicting = true
		}
	}
	if healthy <= count {
		return ""
	}
	remove := func(r *unstructured.Unstructured) string {
		require.NoError(t, c.Delete(ctx, r))
		modes, _, _ := unstructured.NestedMap(engine.Object, "status", "replicaModeMap")
		delete(modes, r.GetName())
		setEngineModes(t, c, modes)
		return r.GetName()
	}
	for i := range replicas {
		if evicting(&replicas[i]) && (nonEvicting || replicas[i].GetName() != kept) {
			return remove(&replicas[i])
		}
	}
	local := slices.ContainsFunc(replicas, func(r unstructured.Unstructured) bool { return field(&r, "nodeID") == engineNode })
	if locality == "" || locality == "disabled" || engineNode == "" || !local {
		return ""
	}
	for i := range replicas {
		if field(&replicas[i], "nodeID") != engineNode {
			return remove(&replicas[i])
		}
	}
	return ""
}

// healthyCopies counts pvc-1's replicas that are healthy and not being deleted.
func healthyCopies(t *testing.T, c crclient.Client) int {
	t.Helper()
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(longhornReplicaList)
	require.NoError(t, c.List(context.Background(), list, crclient.InNamespace(longhornNamespace), crclient.MatchingLabels{"longhornvolume": "pvc-1"}))
	n := 0
	for _, r := range list.Items {
		failed, _, _ := unstructured.NestedString(r.Object, "spec", "failedAt")
		healthy, _, _ := unstructured.NestedString(r.Object, "spec", "healthyAt")
		if failed == "" && healthy != "" && r.GetDeletionTimestamp() == nil {
			n++
		}
	}
	return n
}

func localityOf(t *testing.T, c crclient.Client) string {
	t.Helper()
	locality, _, _ := unstructured.NestedString(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "dataLocality")
	return locality
}

// An engine upgrade or a move that Longhorn starts after the count was lowered
// is waited out under the hold: the change stays pending, the count and data
// locality stay, the ticket stays and db stays stopped.
func TestAnUpgradeOrMoveAfterTheLoweringKeepsTheHold(t *testing.T) {
	for name, tc := range map[string]struct {
		value interface{}
		path  []string
		want  string
	}{
		"engine upgrade": {"engine:v2", []string{"spec", "image"}, "Longhorn is upgrading this volume's engine."},
		"move":           {"worker-1", []string{"spec", "migrationNodeID"}, "Longhorn is moving this volume to another node."},
	} {
		t.Run(name, func(t *testing.T) {
			sc, c := serviceConverterFor(t, beforeLowering()...)
			setLonghornField(t, c, "Volume", "pvc-1", "engine:v1", "spec", "image")
			setLonghornField(t, c, "Volume", "pvc-1", "engine:v1", "status", "currentImage")
			convertDB(t, sc, 1)
			require.Equal(t, int64(1), countOf(t, c))
			setLonghornField(t, c, "Volume", "pvc-1", tc.value, tc.path...)

			cond := convertDB(t, sc, 1)

			assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
			assert.Contains(t, cond.Message, tc.want)
			assert.Contains(t, cond.Message, "'kip service update db --copies 2'")
			assert.Equal(t, int64(1), countOf(t, c))
			assert.Equal(t, "best-effort", localityOf(t, c))
			assert.NotNil(t, ticketOf(t, c))
			assert.Equal(t, int32(0), replicasOfDB(t, c))
		})
	}
}

// Under the hold Kipper lowers the count with data locality on, and Longhorn
// removes the worker copy. Kipper turns data locality off again and starts db
// once the server copy alone is left and RW. Kipper deletes no replica.
func TestLonghornRemovesTheWorkerCopyUnderTheHold(t *testing.T) {
	sc, c := serviceConverterFor(t, beforeLowering()...)

	cond := convertDB(t, sc, 1)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	assert.Equal(t, "db is stopped for the copy change and starts again once it is done. "+
		"Lowering the copy count to 1. Longhorn removes the extra copies one at a time and keeps the copy on server-1. "+
		"Setting the copies back with 'kip service update db --copies 2' starts db again at two copies. "+
		"If Longhorn has built the server copy for the change already, Kipper keeps it, and Longhorn removes one worker copy first. "+
		"On a cluster that offers one copy, setting the copies back needs an admin.", cond.Message)
	assert.Equal(t, int64(1), countOf(t, c))
	assert.Equal(t, "best-effort", localityOf(t, c))

	cond = convertDB(t, sc, 1)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	assert.Contains(t, cond.Message, "Longhorn is removing the extra copies: the volume has 2 and keeps 1.")

	require.Equal(t, "r-w", longhornCleanup(t, c))
	assert.Empty(t, longhornCleanup(t, c), "Longhorn keeps the count it was given")

	cond = convertDB(t, sc, 1)
	assert.Contains(t, cond.Message, "Longhorn removed the extra copies, so Kipper turns data locality off again.")
	assert.Equal(t, "disabled", localityOf(t, c))
	assert.Equal(t, int32(0), replicasOfDB(t, c), "db stays stopped until the hold ends")

	convertDB(t, sc, 1)
	assert.Nil(t, ticketOf(t, c))
	assert.Nil(t, stopOf(t, c))
	assert.Equal(t, int32(1), replicasOfDB(t, c))
	assert.True(t, replicaExists(t, c, "r-s"))
	assert.Equal(t, int64(1), countOf(t, c))
}

// An admin's eviction of the server copy while the count is lowered under the
// hold leaves Longhorn the only one removing copies. Whichever order Kipper's
// passes, Longhorn's cleanup and the eviction come in, a healthy copy is always
// left. When the eviction takes the server copy, the worker copy is the one
// kept.
func TestAnEvictionWhileTheCountIsLoweredAlwaysLeavesACopy(t *testing.T) {
	for _, evicted := range [][]string{{"r-s"}, {"r-w"}, {"r-s", "r-w"}} {
		for at := 0; at < 8; at++ {
			t.Run(fmt.Sprintf("%v at step %d", evicted, at), func(t *testing.T) {
				sc, c := serviceConverterFor(t, beforeLowering()...)
				for step := 0; step < 8; step++ {
					if step == at {
						for _, name := range evicted {
							if replicaExists(t, c, name) {
								setLonghornField(t, c, "Replica", name, true, "spec", "evictionRequested")
							}
						}
					}
					if step%2 == 0 {
						convertDB(t, sc, 1)
					} else {
						longhornCleanup(t, c)
					}
					require.GreaterOrEqual(t, healthyCopies(t, c), 1, "step %d", step)
				}
			})
		}
	}
}

// Longhorn drops the hold while it is removing copies. The next pass puts the
// count back to the copies Longhorn counts and turns data locality off, in one
// patch and before anything else, so Longhorn removes nothing more.
func TestALostHoldMidCleanupRestoresTheCountAndLocalityFirst(t *testing.T) {
	objs := append(removalWorld(), longhornReplica("r-w2", "pvc-1", "worker-2", healthyTime, ""))
	var first map[string]interface{}
	c := failingGuard(t, func(obj crclient.Object) error {
		if first == nil {
			first = map[string]interface{}{"kind": obj.GetObjectKind().GroupVersionKind().Kind}
		}
		return nil
	}, objs...)
	for _, o := range []crclient.Object{lhDiskNode("worker-2", 90*gib, platform.ReplicaStorageTag), kubeNode("worker-2", corev1.ConditionTrue)} {
		require.NoError(t, c.Create(context.Background(), o))
	}
	setEngineModes(t, c, map[string]interface{}{"r-s": "RW", "r-w": "RW", "r-w2": "RW"})
	require.Equal(t, "r-w", longhornCleanup(t, c))
	loseTheTicket(t, c)
	sc := removalConverter(c, convNow)
	sc.StatefulSet = "db"

	_, err := convertOnce(t, sc, ptr.To(int32(1)))

	require.NoError(t, err)
	assert.Equal(t, "Volume", first["kind"], "the first write is the volume's")
	assert.Equal(t, int64(2), countOf(t, c), "r-s and r-w2 are left")
	assert.Equal(t, "disabled", localityOf(t, c))
	assert.Empty(t, longhornCleanup(t, c))
	assert.True(t, replicaExists(t, c, "r-w2"))
	assert.Equal(t, int32(0), replicasOfDB(t, c), "the reduction holds the volume again before it goes on")
}

// A write that fails at any step of the held reduction, as when console-api
// restarts, leaves a state the next pass goes on from. The reduction still
// ends with the server copy alone, data locality off and db running, and no
// pass breaks the guard's rules.
func TestAFailedWriteAtAnyStepOfTheHeldReductionIsResumed(t *testing.T) {
	for failAt := 1; failAt <= 6; failAt++ {
		t.Run(fmt.Sprintf("write %d", failAt), func(t *testing.T) {
			writes := 0
			c := failingGuard(t, func(crclient.Object) error {
				writes++
				if writes == failAt {
					return apierrors.NewInternalError(errors.New("console-api restarts"))
				}
				return nil
			}, beforeLowering()...)
			sc := storageConverter{Reader: c, Writer: c, Now: func() time.Time { return convNow }, StatefulSet: "db", Commands: serviceStorageCommands("db")}
			for range 12 {
				_, _ = sc.convertOrBlock(context.Background(), storedDataClaim(t, c), ptr.To(int32(1)))
				assertCountKept(t, c)
				longhornCleanup(t, c)
				require.GreaterOrEqual(t, healthyCopies(t, c), 1)
			}
			assert.Nil(t, stopOf(t, c))
			assert.Nil(t, ticketOf(t, c))
			assert.Equal(t, int32(1), replicasOfDB(t, c))
			assert.Equal(t, "disabled", localityOf(t, c))
			assert.True(t, replicaExists(t, c, "r-s"))
			assert.False(t, replicaExists(t, c, "r-w"))
		})
	}
}

// Setting the copies back while the count is lowered raises it to the copies
// left and turns data locality off before db starts, so Longhorn removes no
// copy the set-back keeps.
func TestASetBackWhileTheCountIsLoweredKeepsBothCopies(t *testing.T) {
	sc, c := serviceConverterFor(t, removalWorld()...)

	convertDB(t, sc, 2)

	assert.Equal(t, int64(2), countOf(t, c))
	assert.Equal(t, "disabled", localityOf(t, c))
	assert.Nil(t, ticketOf(t, c))
	assert.Nil(t, stopOf(t, c))
	assert.Equal(t, int32(1), replicasOfDB(t, c))
	assert.Empty(t, longhornCleanup(t, c))
	assert.True(t, replicaExists(t, c, "r-w"))
}

// A shared volume's reduction goes the same way, with no service to stop.
func TestLonghornRemovesTheWorkerCopyOfASharedVolume(t *testing.T) {
	sc, c := converterFor(t, beforeLowering()...)
	sc.Commands = volumeStorageCommands("files")
	pvc := storedDataClaim(t, c)

	for range 2 {
		_, err := convertShared(context.Background(), sc, pvc, ptr.To(int32(1)))
		require.NoError(t, err)
	}
	assert.Equal(t, int64(1), countOf(t, c))
	require.Equal(t, "r-w", longhornCleanup(t, c))
	for range 2 {
		_, err := convertShared(context.Background(), sc, pvc, ptr.To(int32(1)))
		require.NoError(t, err)
	}
	assert.Nil(t, ticketOf(t, c))
	assert.Nil(t, stopOf(t, c))
	assert.Equal(t, "disabled", localityOf(t, c))
	assert.True(t, replicaExists(t, c, "r-s"))
}

// A volume from an earlier release with three copies goes to one in the same
// held reduction, and Longhorn removes the two worker copies one at a time.
func TestThreeCopiesGoToOneOnTheServer(t *testing.T) {
	v := lhVolumeObj("pvc-1", 3, platform.ReplicaStorageTag)
	objs := heldWorld(v, longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""),
		longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""), longhornReplica("r-w2", "pvc-1", "worker-2", healthyTime, ""),
		lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW", "r-w2": "RW"}),
		lhDiskNode("worker-2", 90*gib, platform.ReplicaStorageTag), kubeNode("worker-2", corev1.ConditionTrue))
	sc, c := serviceConverterFor(t, objs...)

	convertDB(t, sc, 1)
	assert.Equal(t, int64(1), countOf(t, c))
	assert.Contains(t, convertDB(t, sc, 1).Message, "the volume has 3 and keeps 1")
	require.Equal(t, "r-w", longhornCleanup(t, c))
	assert.Contains(t, convertDB(t, sc, 1).Message, "the volume has 2 and keeps 1")
	require.Equal(t, "r-w2", longhornCleanup(t, c))
	convertDB(t, sc, 1)
	convertDB(t, sc, 1)

	assert.Nil(t, stopOf(t, c))
	assert.Equal(t, int32(1), replicasOfDB(t, c))
	assert.True(t, replicaExists(t, c, "r-s"))
}

// This models a powered-off worker whose engine state is unknown while its
// last replicaModeMap still marks both copies RW. Kipper cannot distinguish
// a dead worker from an unreachable one still writing to its own copy, so it
// lowers no count until the volume is healthy.
func TestNoCountIsLoweredOnTheReportOfAnEngineOnADownNode(t *testing.T) {
	c := lowerCountGuard(t, beforeLowering()...)
	setLonghornField(t, c, "Volume", "pvc-1", "unknown", "status", "robustness")
	setEngine(t, c, "worker-1", "server-1", "unknown")
	setNodeReady(t, c, "worker-1", corev1.ConditionUnknown)

	cond, err := convertOnce(t, removalConverter(c, convNow), ptr.To(int32(1)))

	require.NoError(t, err)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.Contains(t, cond.Message, "A copy of this volume is on worker-1, which is not Ready or is fenced")
	assert.Contains(t, cond.Message, "'kip node fence worker-1' and then 'kip node remove worker-1 --force'",
		"the engine's report is set aside, and the message names it rather than the server copy")
	assert.Equal(t, int64(2), countOf(t, c))
	assert.Equal(t, "disabled", localityOf(t, c))
}

// The engine's status shows a hold only while it runs on the ticket's node
// and the managers there can report. An engine elsewhere, in an unknown state
// or reported by a manager that is down holds nothing, so no count is lowered.
func TestNoCountIsLoweredWhileTheEngineIsNotHeldOnTheServer(t *testing.T) {
	for name, fault := range map[string]func(t *testing.T, c crclient.Client){
		"engine on a down node without a copy": func(t *testing.T, c crclient.Client) {
			require.NoError(t, c.Create(context.Background(), kubeNode("worker-2", corev1.ConditionUnknown)))
			setEngine(t, c, "worker-2", "server-1", "unknown")
		},
		"engine state unknown":                 func(t *testing.T, c crclient.Client) { setEngine(t, c, "server-1", "server-1", "unknown") },
		"manager reporting the engine is down": func(t *testing.T, c crclient.Client) { setServerManagerDown(t, c) },
		"no active engine": func(t *testing.T, c crclient.Client) {
			setLonghornField(t, c, "Engine", "pvc-1-e-0", false, "spec", "active")
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := lowerCountGuard(t, beforeLowering()...)
			fault(t, c)

			cond, err := convertOnce(t, removalConverter(c, convNow), ptr.To(int32(1)))

			require.NoError(t, err)
			require.NotNil(t, cond)
			assert.Equal(t, int64(2), countOf(t, c), cond.Message)
			assert.Equal(t, "disabled", localityOf(t, c))
		})
	}
}

// A held engine whose status shows fewer copies RW than are present lowers
// no count: Longhorn brings the copies up to date first.
func TestNoCountIsLoweredUntilTheHeldEngineShowsEveryCopyRW(t *testing.T) {
	for name, modes := range map[string]map[string]interface{}{
		"empty mode map": {},
		"every copy ERR": {"r-s": "ERR", "r-w": "ERR"},
		"one copy WO":    {"r-s": "RW", "r-w": "WO"},
	} {
		t.Run(name, func(t *testing.T) {
			c := lowerCountGuard(t, heldWorld(lhVolumeObj("pvc-1", 3, platform.ReplicaStorageTag),
				longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""), longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
				lhEngine(modes))...)

			cond, err := convertOnce(t, removalConverter(c, convNow), ptr.To(int32(2)))

			require.NoError(t, err)
			assert.Equal(t, int64(3), countOf(t, c))
			assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason, cond.Message)
			assert.Contains(t, cond.Message, "Longhorn is bringing every copy up to date")
		})
	}
}

// Even when a running engine reports the server copy RW, Kipper keeps the
// count because it cannot establish the state of the other copy's node.
func TestNoCountIsLoweredWhileANodeHoldingACopyIsNotReady(t *testing.T) {
	c := lowerCountGuard(t, beforeLowering()...)
	setNodeReady(t, c, "worker-1", corev1.ConditionFalse)

	cond, err := convertOnce(t, removalConverter(c, convNow), ptr.To(int32(1)))

	require.NoError(t, err)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.Contains(t, cond.Message, "on worker-1, which is not Ready or is fenced")
	assert.Contains(t, cond.Message, "'kip node fence worker-1'")
	assert.Contains(t, cond.Message, "'kip node remove worker-1 --force'")
	assert.Equal(t, int64(2), countOf(t, c))
}

// Longhorn marks a copy for eviction some time after an admin asks to evict
// its node or disk. The count is not lowered from the moment the node or the
// disk asks for it, so an eviction can only start after the lowering.
func TestNoCountIsLoweredWhileTheNodeOrDiskOfACopyIsEvicted(t *testing.T) {
	for name, evict := range map[string]func(t *testing.T, c crclient.Client){
		"node": func(t *testing.T, c crclient.Client) {
			setLonghornField(t, c, "Node", "worker-1", true, "spec", "evictionRequested")
		},
		"disk": func(t *testing.T, c crclient.Client) {
			setLonghornField(t, c, "Node", "worker-1", "uuid-w1", "status", "diskStatus", "disk-1", "diskUUID")
			setLonghornField(t, c, "Node", "worker-1", true, "spec", "disks", "disk-1", "evictionRequested")
			setLonghornField(t, c, "Replica", "r-w", "uuid-w1", "spec", "diskID")
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := lowerCountGuard(t, beforeLowering()...)
			evict(t, c)

			cond, err := convertOnce(t, removalConverter(c, convNow), ptr.To(int32(1)))

			require.NoError(t, err)
			require.NotNil(t, cond)
			assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
			assert.Contains(t, cond.Message, "Longhorn is evicting the copy on worker-1")
			assert.Equal(t, int64(2), countOf(t, c))
			assert.Equal(t, "disabled", localityOf(t, c))
		})
	}
}

// With no copy present, nothing shows that a copy would remain in sync, so
// the copy count is not lowered.
func TestNoCountIsLoweredWhileNoCopyIsPresent(t *testing.T) {
	v := lhVolumeObj("pvc-1", 3, platform.ReplicaStorageTag)
	failed := "2026-10-24T11:59:00Z"
	c := lowerCountGuard(t, withDataClaim(convWorld(v,
		longhornReplica("r-s", "pvc-1", "server-1", healthyTime, failed),
		longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, failed),
		lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"})))...)

	cond, err := convertOnce(t, removalConverter(c, convNow), ptr.To(int32(2)))

	require.NoError(t, err)
	assert.Equal(t, int64(3), countOf(t, c))
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason, cond.Message)
	assert.Contains(t, cond.Message, "No copy of this volume is present")
}

// During a partial partition, the worker's manager marked the
// server copy failed, and the only other copy is on the worker, which is not
// Ready. A server copy cannot be built from it, so the change waits and says
// why instead of asking Longhorn for the build.
func TestNoServerCopyIsBuiltFromACopyOnAnUnavailableNode(t *testing.T) {
	v := lhVolumeObj("pvc-1", 2, platform.ReplicaStorageTag)
	v.Object["status"].(map[string]interface{})["robustness"] = "degraded"
	c := lowerCountGuard(t, withDataClaim(convWorld(v,
		longhornReplica("r-s", "pvc-1", "server-1", healthyTime, "2026-10-24T11:59:00Z"),
		longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
		lhEngine(map[string]interface{}{"r-w": "RW", "r-s": "ERR"})))...)
	setEngine(t, c, "worker-1", "worker-1", "unknown")
	setNodeReady(t, c, "worker-1", corev1.ConditionUnknown)

	cond, err := convertOnce(t, removalConverter(c, convNow), ptr.To(int32(1)))

	require.NoError(t, err)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason, cond.Message)
	assert.Contains(t, cond.Message, "A copy of this volume is on worker-1, which is not Ready or is fenced")
	assert.Equal(t, int64(2), countOf(t, c))
	selector, _, _ := unstructured.NestedStringSlice(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "nodeSelector")
	assert.Equal(t, []string{platform.ReplicaStorageTag}, selector, "no server-only build was asked for")
}

// A conflicting write means the volume changed since it was read, so the
// pass reads it again and plans the step afresh, sending the new
// resourceVersion rather than the stale patch.
func TestAConflictingStepIsPlannedAgainFromAFreshRead(t *testing.T) {
	var sent []string
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).
		WithObjects(withDataClaim(convWorld(oneServerCopy()...))...).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch, opts ...crclient.PatchOption) error {
			if _, claim := obj.(*corev1.PersistentVolumeClaim); claim {
				return cl.Patch(ctx, obj, patch, opts...)
			}
			raw, err := patch.Data(obj)
			require.NoError(t, err)
			var body struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			}
			require.NoError(t, json.Unmarshal(raw, &body))
			sent = append(sent, body.Metadata.ResourceVersion)
			if len(sent) == 1 {
				// Longhorn writes the volume between Kipper's read and its patch.
				vol := getLonghorn(t, cl, "Volume", "pvc-1")
				vol.SetAnnotations(map[string]string{"example.com/touched": "yes"})
				require.NoError(t, cl.Update(ctx, vol))
				return apierrors.NewConflict(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, "pvc-1", fmt.Errorf("the object has been modified"))
			}
			return cl.Patch(ctx, obj, patch, opts...)
		}}).Build()
	sc := storageConverter{Reader: c, Writer: c, Now: func() time.Time { return convNow }}

	cond, err := sc.convertOrBlock(context.Background(), storedDataClaim(t, c), ptr.To(int32(2)))

	require.NoError(t, err)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesConverting, cond.Reason, cond.Message)
	require.Len(t, sent, 2)
	assert.NotEqual(t, sent[0], sent[1])
	assert.Equal(t, int64(2), countOf(t, c), "the second, freshly planned patch went through")
}

// A step that keeps conflicting reports that it waits, instead of leaving the
// last condition, such as Healthy, in place.
func TestAStepThatKeepsConflictingReportsThatItWaits(t *testing.T) {
	conflicts := 0
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).
		WithObjects(withDataClaim(convWorld(oneServerCopy()...))...).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch, opts ...crclient.PatchOption) error {
			if _, claim := obj.(*corev1.PersistentVolumeClaim); claim {
				return cl.Patch(ctx, obj, patch, opts...)
			}
			conflicts++
			return apierrors.NewConflict(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, "pvc-1", fmt.Errorf("the object has been modified"))
		}}).Build()
	sc := storageConverter{Reader: c, Writer: c, Now: func() time.Time { return convNow }}

	cond, err := sc.convertOrBlock(context.Background(), storedDataClaim(t, c), ptr.To(int32(2)))

	require.NoError(t, err)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason, cond.Message)
	assert.Contains(t, cond.Message, "the object has been modified")
	assert.Equal(t, 3, conflicts, "each try was planned from a fresh read")
}

// Longhorn drops Kipper's ticket on its own, as on a cordon with its detach
// setting on, right after the hold lowered the count and before Longhorn
// removed a copy. The next pass puts the count back to the copies Longhorn
// counts and turns data locality off before it reports the block. A copy
// Longhorn is evicting does not count, as in Longhorn's own replacement.
func TestALostHoldAfterTheCountWasLoweredRaisesItBeforeAnyBlock(t *testing.T) {
	for name, tc := range map[string]struct {
		cause func(t *testing.T, c crclient.Client)
		count int64
	}{
		"server cordoned": {func(t *testing.T, c crclient.Client) {
			var node corev1.Node
			require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "server-1"}, &node))
			node.Spec.Unschedulable = true
			require.NoError(t, c.Update(context.Background(), &node))
		}, 2},
		"copy evicted": {func(t *testing.T, c crclient.Client) {
			setLonghornField(t, c, "Replica", "r-w", true, "spec", "evictionRequested")
		}, 1},
	} {
		t.Run(name, func(t *testing.T) {
			c := lowerCountGuard(t, removalWorld()...)
			loseTheTicket(t, c)
			tc.cause(t, c)

			cond, err := convertOnce(t, removalConverter(c, convNow), ptr.To(int32(1)))

			require.NoError(t, err)
			require.NotNil(t, cond)
			assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
			assert.Contains(t, cond.Message, map[string]string{"server cordoned": "server-1 is cordoned", "copy evicted": "Longhorn is evicting the copy on worker-1"}[name])
			assert.Equal(t, 1, strings.Count(cond.Message, "Setting the copies back"), "the block names the way out once: %s", cond.Message)
			assert.Equal(t, tc.count, countOf(t, c))
			assert.Equal(t, "disabled", localityOf(t, c))
			assert.True(t, replicaExists(t, c, "r-s"))
			assert.True(t, replicaExists(t, c, "r-w"))
			assert.NotNil(t, stopOf(t, c), "the change still waits to reduce under the hold")
		})
	}
}
