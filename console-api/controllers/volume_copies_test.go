package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func copiesVolume(copies *int32, labels map[string]string) *kipperv1.Volume {
	return &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: "shop", Labels: labels},
		Spec:       kipperv1.VolumeSpec{Size: "1Gi", Copies: copies},
	}
}

func reconcileVolume(t *testing.T, vol *kipperv1.Volume, extra ...crclient.Object) (crclient.Client, *kipperv1.Volume) {
	t.Helper()
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).
		WithObjects(append(append(twoReplicaNodes(), vol), extra...)...).WithStatusSubresource(&kipperv1.Volume{}).Build()
	r := &VolumeReconciler{Client: c, APIReader: c, Scheme: testScheme()}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "shop", Name: "uploads"}})
	require.NoError(t, err)
	var got kipperv1.Volume
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Namespace: "shop", Name: "uploads"}, &got))
	return c, &got
}

func sharedClaimClass(t *testing.T, c crclient.Client) (string, bool) {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	err := c.Get(context.Background(), types.NamespacedName{Namespace: "shop", Name: "shared-uploads"}, &pvc)
	if apierrors.IsNotFound(err) {
		return "", false
	}
	require.NoError(t, err)
	return *pvc.Spec.StorageClassName, true
}

func TestANewSharedVolumeWithoutCopiesGetsOneCopy(t *testing.T) {
	c, _ := reconcileVolume(t, copiesVolume(nil, nil))
	class, ok := sharedClaimClass(t, c)
	require.True(t, ok)
	assert.Equal(t, platform.LonghornSingleClass, class)
}

func TestASharedVolumeCannotBeCreatedWithTwoCopies(t *testing.T) {
	c, got := reconcileVolume(t, copiesVolume(ptr.To(int32(2)), nil))
	_, ok := sharedClaimClass(t, c)
	assert.False(t, ok, "no claim while shared copies are not qualified")
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesRWXNotQualified, cond.Reason)
	assert.Equal(t, "Pending", got.Status.Phase)
}

func TestARestoredTwoCopySharedVolumeGetsOneCopyAndKeepsItsIntent(t *testing.T) {
	c, got := reconcileVolume(t, copiesVolume(ptr.To(int32(2)), map[string]string{veleroRestoreNameLabel: "restore-1"}))
	class, ok := sharedClaimClass(t, c)
	require.True(t, ok, "a restore must bring the data back")
	assert.Equal(t, platform.LonghornSingleClass, class)
	assert.Equal(t, int32(2), *got.Spec.Copies)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesRWXNotQualified, cond.Reason)
}

func TestAnExistingSharedClaimIsLeftOnItsClass(t *testing.T) {
	existing := sharedClaim(nil)
	existing.Namespace = "shop"
	c, got := reconcileVolume(t, copiesVolume(ptr.To(int32(2)), nil), existing)
	class, ok := sharedClaimClass(t, c)
	require.True(t, ok)
	assert.Equal(t, platform.LonghornSharedClass, class)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesRWXNotQualified, cond.Reason)
	assert.Contains(t, cond.Message, "keeps its current copies", "an unbound claim has no live copies yet")
}

func TestSharedVolumeStatusReportsTheLiveCopies(t *testing.T) {
	existing := sharedClaim(nil)
	existing.Namespace = "shop"
	existing.Spec.VolumeName = "pvc-9"
	_, got := reconcileVolume(t, copiesVolume(nil, nil), existing,
		longhornVolume("pvc-9", "shop", "shared-uploads", 3),
		longhornReplica("r1", "pvc-9", "server-1", "2026-10-24T01:00:00Z", ""),
		longhornReplica("r2", "pvc-9", "worker-1", "2026-10-24T01:00:00Z", ""),
	)
	require.NotNil(t, got.Status.Storage)
	assert.Equal(t, int32(3), got.Status.Storage.Copies)
	assert.Equal(t, []string{"server-1", "worker-1"}, got.Status.Storage.Nodes)
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesNotSet, cond.Reason)
	assert.Contains(t, cond.Message, "Longhorn keeps 3 copies (2 healthy), more than Kipper sets",
		"a shared volume on the stock class has three copies, which no copies value records")
	assert.Contains(t, cond.Message, "'kip volume update uploads --copies 1'", "a shared volume keeps one copy in this release")
}

func TestTheHolderAcceptsEveryLonghornClass(t *testing.T) {
	for _, class := range []string{platform.LonghornSharedClass, platform.LonghornSingleClass, platform.LonghornReplicatedClass} {
		pvc := sharedClaim(nil)
		pvc.Spec.StorageClassName = ptr.To(class)
		assert.Empty(t, claimUnsupported(pvc), class)
	}
	pvc := sharedClaim(nil)
	pvc.Spec.StorageClassName = ptr.To("local-path")
	assert.NotEmpty(t, claimUnsupported(pvc))
}

func TestProjectSharedStorageWithTwoCopiesIsHeldBack(t *testing.T) {
	scheme := testScheme()
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: kipperv1.ProjectSpec{
			Environments:  []kipperv1.ProjectEnvironment{{Name: "prod"}},
			SharedStorage: &kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi", Copies: ptr.To(int32(2))},
		},
	}
	c := projectFakeBuilder().WithScheme(scheme).WithObjects(project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c}
	for range 2 {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
		require.NoError(t, err)
	}
	err := c.Get(context.Background(), crclient.ObjectKey{Namespace: "demo-prod", Name: "shared-uploads"}, &corev1.PersistentVolumeClaim{})
	assert.True(t, apierrors.IsNotFound(err))
	var got kipperv1.Project
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Name: "demo"}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesRWXNotQualified, cond.Reason)
}

// Two copies on a shared volume that has at least two keep the copies it
// has, which is how a reduction to one is stopped. A volume with fewer is
// still told that shared volumes take no second copy.
func TestTwoCopiesKeepTheCopiesASharedVolumeHas(t *testing.T) {
	if platform.RWXCopiesQualified {
		t.Skip("shared volumes may have two copies")
	}
	cond := sharedCopiesCondition(ptr.To(int32(2)), &liveCopies{Copies: 3, Healthy: 3}, "kip volume update files --copies 1")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, kipperv1.StorageCopiesHealthy, cond.Reason)
	assert.Equal(t, "Shared volumes take no new copies in this Kipper release, so Kipper keeps this volume's copies. 3 copies are healthy, more than the 2 set. "+
		"Longhorn still asks for 3 copies, and Kipper lowers that only while it holds the volume for a change, such as the next one to one copy.", cond.Message)

	cond = sharedCopiesCondition(ptr.To(int32(2)), &liveCopies{Copies: 2, Healthy: 1}, "kip volume update files --copies 1")
	assert.Equal(t, kipperv1.StorageCopiesDegraded, cond.Reason)
	assert.Equal(t, "Shared volumes take no new copies in this Kipper release, so Kipper keeps this volume's copies. 1 of 2 copies are healthy. "+
		"Longhorn rebuilds a missing copy when a node that can hold it is available.", cond.Message)

	cond = sharedCopiesCondition(ptr.To(int32(2)), &liveCopies{Copies: 1, Healthy: 1}, "kip volume update files --copies 1")
	assert.Equal(t, kipperv1.StorageCopiesRWXNotQualified, cond.Reason)
}

// A shared volume whose engine Longhorn runs on a worker is not held on the
// server, so it loses no copy. Setting the copies back to two releases it
// and keeps every copy.
func TestASharedVolumeOnAWorkerLosesNoCopyAndIsReleasedOnSetBack(t *testing.T) {
	c := lowerCountGuard(t, removalWorld()...)
	setEngine(t, c, "worker-1", "worker-1", "running")
	sc := removalConverter(c, convNow)
	sc.Commands = volumeStorageCommands("files")

	cond, err := convertOnce(t, sc, ptr.To(int32(1)))

	require.NoError(t, err)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
	assert.Contains(t, cond.Message, attachingMessage("server-1"))
	assert.True(t, replicaExists(t, c, "r-w"))

	pvc := storedDataClaim(t, c)
	cond, err = convertShared(context.Background(), sc, pvc, ptr.To(int32(2)))
	require.NoError(t, err)
	assert.Nil(t, cond, "two copies start no conversion of a shared volume")
	assert.True(t, replicaExists(t, c, "r-w"))
	assert.Nil(t, stopOf(t, c))
	assert.Nil(t, ticketOf(t, c))
}

// lowerCountGuard wraps objs in a client that checks the constraints for
// releasing a volume: its requested count must cover the replicas Longhorn
// counts, and data locality and auto-balance must be off. It fails t if a patch
// violates these constraints outside a hold, if Kipper drops its ticket or
// clears its stop record while they are violated, or if Kipper deletes a replica.
// At test completion it also checks stopped volumes outside a hold.
// assertCountKept performs that last check after each pass.
func lowerCountGuard(t *testing.T, objs ...crclient.Object) crclient.Client {
	return failingGuard(t, nil, objs...)
}

// failingGuard is lowerCountGuard whose patches first ask fail, which may
// refuse one with an error.
func failingGuard(t *testing.T, fail func(crclient.Object) error, objs ...crclient.Object) crclient.Client {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).WithObjects(withDataClaim(objs)...).
		WithInterceptorFuncs(interceptor.Funcs{Patch: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch, opts ...crclient.PatchOption) error {
			if fail != nil {
				if err := fail(obj); err != nil {
					return err
				}
			}
			raw, err := patch.Data(obj)
			require.NoError(t, err)
			u, _ := obj.(*unstructured.Unstructured)
			switch {
			case u != nil && u.GetKind() == "Volume":
				var body struct {
					Spec struct {
						NumberOfReplicas   *int64  `json:"numberOfReplicas"`
						DataLocality       *string `json:"dataLocality"`
						ReplicaAutoBalance *string `json:"replicaAutoBalance"`
					} `json:"spec"`
				}
				require.NoError(t, json.Unmarshal(raw, &body))
				g := guardedCounts(ctx, t, cl, u.GetName())
				spec := body.Spec
				switch {
				case g.held:
				case spec.NumberOfReplicas != nil && *spec.NumberOfReplicas < g.counted:
					t.Errorf("the copy count was lowered to %d below the %d copies Longhorn counts outside the hold", *spec.NumberOfReplicas, g.counted)
				case spec.DataLocality != nil && *spec.DataLocality != "disabled", spec.ReplicaAutoBalance != nil && *spec.ReplicaAutoBalance != "disabled":
					t.Errorf("data locality or auto-balance was turned on outside the hold: %s", raw)
				}
			case u != nil && u.GetKind() == "VolumeAttachment" && strings.Contains(string(raw), `"`+conversionTicket+`":null`):
				if g := guardedCounts(ctx, t, cl, u.GetName()); g.broken() {
					t.Errorf("the ticket was dropped while the volume breaks the rule: %+v", g)
				}
			case strings.Contains(string(raw), `"`+platform.CopyChangeStopAnnotation+`":null`):
				var pvc corev1.PersistentVolumeClaim
				require.NoError(t, cl.Get(ctx, crclient.ObjectKeyFromObject(obj), &pvc))
				if g := guardedCounts(ctx, t, cl, pvc.Spec.VolumeName); g.broken() {
					t.Errorf("the stop was cleared while the volume breaks the rule: %+v", g)
				}
			}
			return cl.Patch(ctx, obj, patch, opts...)
		}, Delete: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "Replica" && ctx.Value(longhornActor{}) == nil {
				t.Errorf("Kipper deleted the replica %s", u.GetName())
			}
			return cl.Delete(ctx, obj, opts...)
		}}).Build()
	t.Cleanup(func() { assertCountKept(t, c) })
	return c
}

// guarded is what the guard reads of a Longhorn volume: its count, the copies
// Longhorn counts, which are scheduled, not failed, not being deleted and not
// evicted, whether data locality or auto-balance is on, and whether Kipper
// holds it, which is its ticket on the attachment with the frontend disabled.
type guarded struct {
	count, counted int64
	removes, held  bool
}

// broken reports a volume that breaks the rule outside the hold.
func (g guarded) broken() bool { return g.count < g.counted || g.removes }

// guardedCounts reads guarded for the Longhorn volume name. A missing volume
// reports zeros.
func guardedCounts(ctx context.Context, t *testing.T, cl crclient.Client, name string) guarded {
	volume := longhornObject("Volume", name, map[string]interface{}{})
	if err := cl.Get(ctx, crclient.ObjectKey{Namespace: longhornNamespace, Name: name}, volume); err != nil {
		return guarded{}
	}
	var g guarded
	g.count, _, _ = unstructured.NestedInt64(volume.Object, "spec", "numberOfReplicas")
	locality, _, _ := unstructured.NestedString(volume.Object, "spec", "dataLocality")
	balance, _, _ := unstructured.NestedString(volume.Object, "spec", "replicaAutoBalance")
	g.removes = (locality != "" && locality != "disabled") || balance != "disabled"
	replicas := &unstructured.UnstructuredList{}
	replicas.SetGroupVersionKind(longhornReplicaList)
	require.NoError(t, cl.List(ctx, replicas, crclient.InNamespace(longhornNamespace), crclient.MatchingLabels{"longhornvolume": name}))
	for _, r := range replicas.Items {
		node, _, _ := unstructured.NestedString(r.Object, "spec", "nodeID")
		failed, _, _ := unstructured.NestedString(r.Object, "spec", "failedAt")
		evicting, _, _ := unstructured.NestedBool(r.Object, "spec", "evictionRequested")
		if node != "" && failed == "" && !evicting && r.GetDeletionTimestamp() == nil {
			g.counted++
		}
	}
	disabled, _, _ := unstructured.NestedBool(volume.Object, "status", "frontendDisabled")
	attachment := longhornObject("VolumeAttachment", name, map[string]interface{}{})
	ticket := false
	if cl.Get(ctx, crclient.ObjectKey{Namespace: longhornNamespace, Name: name}, attachment) == nil {
		_, ticket, _ = unstructured.NestedMap(attachment.Object, "spec", "attachmentTickets", conversionTicket)
	}
	g.held = disabled && ticket
	return g
}

// assertCountKept fails t when a claim on c records a copy-change stop while
// its Longhorn volume is not held and breaks the rule outside the hold.
func assertCountKept(t *testing.T, c crclient.Client) {
	t.Helper()
	ctx := context.Background()
	var claims corev1.PersistentVolumeClaimList
	require.NoError(t, c.List(ctx, &claims))
	for _, pvc := range claims.Items {
		if pvc.Annotations[platform.CopyChangeStopAnnotation] == "" || pvc.Spec.VolumeName == "" {
			continue
		}
		if g := guardedCounts(ctx, t, c, pvc.Spec.VolumeName); !g.held && g.broken() {
			t.Errorf("%s is stopped and not held while its volume breaks the rule: %+v", pvc.Name, g)
		}
	}
}

// A shared volume from an earlier release that asks Longhorn for three copies
// and has two keeps that count on a set-back to two, since Kipper lowers a
// count only under the hold. Its status is judged against the two copies set,
// so it is healthy and says what Longhorn still asks for.
func TestASharedSetBackNeverLowersTheCount(t *testing.T) {
	volume := attachedOn(lhVolumeObj("pvc-1", 3, platform.ReplicaStorageTag), "worker-1")
	sc, c := converterFor(t, convWorld(volume, longhornReplica("r-s", "pvc-1", "server-1", healthyTime, ""),
		longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""), lhEngine(map[string]interface{}{"r-s": "RW", "r-w": "RW"}),
		lhAttachmentObj(""), claimPod("vapp", "worker-1"))...)
	pvc := boundClaim("ns", platform.LonghornReplicatedClass, "pvc-1")

	for range 2 {
		cond, err := convertShared(context.Background(), sc, pvc, ptr.To(int32(2)))
		require.NoError(t, err)
		assert.Nil(t, cond)
	}

	assert.Equal(t, int64(3), countOf(t, c))
	live, err := readLiveCopies(context.Background(), c, pvc)
	require.NoError(t, err)
	cond := sharedCopiesCondition(ptr.To(int32(2)), live, "kip volume update vol --copies 1")
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Shared volumes take no new copies in this Kipper release, so Kipper keeps this volume's copies. 2 of 2 copies are healthy. "+
		"Longhorn still asks for 3 copies, and Kipper lowers that only while it holds the volume for a change, such as the next one to one copy.", cond.Message)
}

// A set-back after the held reduction lowered the count to one raises it to
// the two copies present once the hold ends, so the volume ends at two with
// both copies and nothing lowers a count outside the hold.
func TestASharedSetBackAfterTheCountWasLoweredEndsAtTwo(t *testing.T) {
	sc, c := converterFor(t, removalWorld()...)
	pvc := storedDataClaim(t, c)

	cond, err := convertShared(context.Background(), sc, pvc, ptr.To(int32(2)))

	require.NoError(t, err)
	assert.Nil(t, cond)
	assert.Equal(t, int64(2), countOf(t, c))
	selector, _, _ := unstructured.NestedStringSlice(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "nodeSelector")
	assert.Equal(t, []string{platform.ReplicaStorageTag}, selector)
	assert.Nil(t, stopOf(t, c))
	assert.Nil(t, ticketOf(t, c))
	assert.True(t, replicaExists(t, c, "r-s"))
	assert.True(t, replicaExists(t, c, "r-w"))
}

// A set-back after the held reduction lowered the count of a volume with three
// copies raises the count to the three present, so Longhorn sees no extra copy
// that an eviction could remove without a replacement.
func TestASharedSetBackRaisesTheCountToTheCopiesPresent(t *testing.T) {
	objs := append(removalWorld(), longhornReplica("r-w2", "pvc-1", "worker-2", healthyTime, ""))
	sc, c := converterFor(t, objs...)

	_, err := convertShared(context.Background(), sc, storedDataClaim(t, c), ptr.To(int32(2)))

	require.NoError(t, err)
	assert.Equal(t, int64(3), countOf(t, c))
}

// A set-back raises a count the hold lowered before it lets the volume go, so
// a failed count write keeps the ticket and the stop, and the next pass
// finishes the set-back.
func TestASharedSetBackKeepsTheHoldUntilTheCountIsRaised(t *testing.T) {
	failing := true
	c := failingGuard(t, func(obj crclient.Object) error {
		if u, ok := obj.(*unstructured.Unstructured); ok && failing && u.GetKind() == "Volume" {
			return apierrors.NewInternalError(errors.New("the API server is restarting"))
		}
		return nil
	}, removalWorld()...)
	sc := storageConverter{Reader: c, Writer: c, Now: func() time.Time { return convNow }}

	cond, err := convertShared(context.Background(), sc, storedDataClaim(t, c), ptr.To(int32(2)))

	require.NoError(t, err)
	require.NotNil(t, cond, "the failed pass still reports a condition")
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.Equal(t, int64(1), countOf(t, c))
	assert.NotNil(t, ticketOf(t, c))
	assert.NotNil(t, stopOf(t, c))

	failing = false
	_, err = convertShared(context.Background(), sc, storedDataClaim(t, c), ptr.To(int32(2)))
	require.NoError(t, err)
	assert.Equal(t, int64(2), countOf(t, c))
	assert.Nil(t, ticketOf(t, c))
	assert.Nil(t, stopOf(t, c))
}

// A restart after the count is raised and before the ticket is dropped
// leaves the stop recorded, and the next pass releases the volume.
func TestASharedSetBackInterruptedAfterTheCountWriteIsFinishedLater(t *testing.T) {
	failing := true
	c := failingGuard(t, func(obj crclient.Object) error {
		if u, ok := obj.(*unstructured.Unstructured); ok && failing && u.GetKind() == "VolumeAttachment" {
			return apierrors.NewInternalError(errors.New("the API server is restarting"))
		}
		return nil
	}, removalWorld()...)
	sc := storageConverter{Reader: c, Writer: c, Now: func() time.Time { return convNow }}

	_, err := convertShared(context.Background(), sc, storedDataClaim(t, c), ptr.To(int32(2)))

	require.NoError(t, err)
	assert.Equal(t, int64(2), countOf(t, c))
	assert.NotNil(t, ticketOf(t, c))
	assert.NotNil(t, stopOf(t, c))

	failing = false
	_, err = convertShared(context.Background(), sc, storedDataClaim(t, c), ptr.To(int32(2)))
	require.NoError(t, err)
	assert.Equal(t, int64(2), countOf(t, c))
	assert.Nil(t, ticketOf(t, c))
	assert.Nil(t, stopOf(t, c))
}

// Longhorn drops Kipper's ticket when the server is cordoned with its
// detach setting on, so a set-back without the ticket still raises the count
// before it clears the stop.
func TestASharedSetBackWithoutTheTicketRaisesTheCountBeforeTheStopEnds(t *testing.T) {
	objs := slices.DeleteFunc(removalWorld(), func(o crclient.Object) bool {
		u, ok := o.(*unstructured.Unstructured)
		return ok && u.GetKind() == "VolumeAttachment"
	})
	sc, c := converterFor(t, append(objs, lhAttachmentObj(""))...)

	_, err := convertShared(context.Background(), sc, storedDataClaim(t, c), ptr.To(int32(2)))

	require.NoError(t, err)
	assert.Equal(t, int64(2), countOf(t, c))
	assert.Nil(t, stopOf(t, c))
}

// A shared set-back while the server copy builds under the hold ends the
// build Kipper asked for: the lease, the rebuild marker and the server-only
// placement go with the stop, and the count stays at the two Longhorn is
// building, so the volume ends with both copies.
func TestASharedSetBackDuringTheServerBuildReleasesTheLeaseAndMarker(t *testing.T) {
	volume := lhVolumeObj("pvc-1", 2, platform.ServerStorageTag)
	volume.SetAnnotations(map[string]string{copiesRebuildAnnotation: convNow.Add(-time.Minute).Format(time.RFC3339)})
	sc, c := converterFor(t, heldWorld(volume, longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
		longhornReplica("r-s", "pvc-1", "server-1", "", ""), lhEngine(map[string]interface{}{"r-w": "RW", "r-s": "WO"}),
		heldLease("pvc-1", convNow))...)

	_, err := convertShared(context.Background(), sc, storedDataClaim(t, c), ptr.To(int32(2)))

	require.NoError(t, err)
	assert.Equal(t, int64(2), countOf(t, c))
	assert.NotContains(t, getLonghorn(t, c, "Volume", "pvc-1").GetAnnotations(), copiesRebuildAnnotation)
	selector, _, _ := unstructured.NestedStringSlice(getLonghorn(t, c, "Volume", "pvc-1").Object, "spec", "nodeSelector")
	assert.Equal(t, targetSelector(2), selector, "the server-only placement of the build goes with it")
	assert.Nil(t, conversionLease(t, c).Spec.HolderIdentity)
	assert.Nil(t, ticketOf(t, c))
	assert.Nil(t, stopOf(t, c))
}

// A server copy Longhorn has not placed when a shared reduction is set back is
// not asked for any more, whether Longhorn has created its replica without a
// node yet or not created it at all, so it never lands on a worker under the
// placement for two copies.
func TestASharedSetBackDropsAServerCopyLonghornHasNotPlaced(t *testing.T) {
	for name, server := range map[string][]crclient.Object{
		"replica without a node": {longhornReplica("r-s", "pvc-1", "", "", "")},
		"no replica yet":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			volume := lhVolumeObj("pvc-1", 2, platform.ServerStorageTag)
			volume.SetAnnotations(map[string]string{copiesRebuildAnnotation: convNow.Add(-time.Minute).Format(time.RFC3339)})
			objs := append([]crclient.Object{longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, ""),
				lhEngine(map[string]interface{}{"r-w": "RW"}), heldLease("pvc-1", convNow)}, server...)
			sc, c := converterFor(t, heldWorld(volume, objs...)...)

			_, err := convertShared(context.Background(), sc, storedDataClaim(t, c), ptr.To(int32(2)))

			require.NoError(t, err)
			assert.Equal(t, int64(1), countOf(t, c))
			assert.NotContains(t, getLonghorn(t, c, "Volume", "pvc-1").GetAnnotations(), copiesRebuildAnnotation)
			assert.Nil(t, stopOf(t, c))
		})
	}
}

// Raising a count removes no copy, so a set-back raises it even when data
// locality was turned on during the hold.
func TestASharedSetBackRaisesTheCountWithDataLocalityOn(t *testing.T) {
	objs := removalWorld()
	for _, o := range objs {
		if u, ok := o.(*unstructured.Unstructured); ok && u.GetKind() == "Volume" {
			_ = unstructured.SetNestedField(u.Object, "best-effort", "spec", "dataLocality")
		}
	}
	sc, c := converterFor(t, objs...)

	_, err := convertShared(context.Background(), sc, storedDataClaim(t, c), ptr.To(int32(2)))

	require.NoError(t, err)
	assert.Equal(t, int64(2), countOf(t, c))
	assert.Nil(t, stopOf(t, c))
}

// A set-back with one copy present, as after Longhorn removed the extra copy
// and before data locality went off again, leaves the count, since raising it
// would build a copy that shared volumes do not take in this release, and
// turns data locality off before the volume is let go.
func TestASharedSetBackWithOneCopyLeavesTheCount(t *testing.T) {
	objs := removalWorld()
	objs = slices.DeleteFunc(objs, func(o crclient.Object) bool { return o.GetName() == "r-w" })
	sc, c := converterFor(t, objs...)

	_, err := convertShared(context.Background(), sc, storedDataClaim(t, c), ptr.To(int32(2)))

	require.NoError(t, err)
	assert.Equal(t, int64(1), countOf(t, c))
	assert.Equal(t, "disabled", localityOf(t, c))
	assert.Nil(t, ticketOf(t, c))
}
