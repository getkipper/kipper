package cmd

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	"github.com/getkipper/kipper/kip/internal/service"

	"github.com/getkipper/kipper/kip/internal/manifest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func offeringTwo(nodes int) copiesCluster {
	return copiesCluster{mode: platform.StorageLonghorn, offered: 2, replicaNodes: nodes, backup: platform.BackupExternal}
}

func TestOneCopyIsAlwaysAllowed(t *testing.T) {
	for _, c := range []copiesCluster{
		{mode: platform.StorageLocal, offered: 1, replicaNodes: 0},
		{mode: platform.StorageLonghorn, offered: 1, replicaNodes: unknownReplicaNodes},
	} {
		assert.NoError(t, refuseCopiesOnCreate(1, false, c))
		assert.NoError(t, refuseCopiesOnCreate(1, true, c))
	}
}

func TestCopiesOutsideOneAndTwoAreRefused(t *testing.T) {
	for _, copies := range []int32{0, 3, -1} {
		assert.ErrorContains(t, refuseCopiesOnCreate(copies, false, offeringTwo(2)), "1 or 2")
	}
}

func TestTwoCopiesOnLocalStorageAreRefused(t *testing.T) {
	c := offeringTwo(2)
	c.mode = platform.StorageLocal
	assert.ErrorContains(t, refuseCopiesOnCreate(2, false, c), "local storage")
}

func TestTwoCopiesNeedTheAdminsOffer(t *testing.T) {
	c := offeringTwo(2)
	c.offered = 1
	err := refuseCopiesOnCreate(2, false, c)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip cluster storage --max-copies 2")
}

func TestAnUnreadableOfferLeavesTheDecisionToTheCluster(t *testing.T) {
	c := offeringTwo(2)
	c.offered = 0
	assert.NoError(t, refuseCopiesOnCreate(2, false, c))
}

func TestTwoCopiesOfASharedVolumeAreRefusedWhileRWXIsNotQualified(t *testing.T) {
	err := refuseCopiesOnCreate(2, true, offeringTwo(3))
	if platform.RWXCopiesQualified {
		assert.NoError(t, err)
		return
	}
	assert.ErrorContains(t, err, "shared volumes cannot have two copies")
}

func TestTwoCopiesNeedTwoReplicaNodes(t *testing.T) {
	err := refuseCopiesOnCreate(2, false, offeringTwo(1))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has 1")
	assert.Contains(t, err.Error(), "kip node storage <node> --copies")

	assert.NoError(t, refuseCopiesOnCreate(2, false, offeringTwo(2)))
	assert.NoError(t, refuseCopiesOnCreate(2, false, offeringTwo(unknownReplicaNodes)),
		"an unreadable node list leaves the decision to the controller")
}

func TestTwoCopiesWarnOnceAboutInClusterBackups(t *testing.T) {
	c := offeringTwo(2)
	c.backup = platform.BackupInCluster
	var out bytes.Buffer
	warnAboutCopies(&out, 2, c)
	assert.Contains(t, out.String(), "inside the cluster")
	assert.Equal(t, 1, bytes.Count(out.Bytes(), []byte("⚠")))

	out.Reset()
	warnAboutCopies(&out, 1, c)
	warnAboutCopies(&out, 2, offeringTwo(2))
	assert.Empty(t, out.String())
}

func copiesObject(spec, live *int64) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": "db", "namespace": "shop"},
		"spec":       map[string]interface{}{"type": "postgres"},
	}}
	if spec != nil {
		_ = unstructured.SetNestedField(obj.Object, *spec, "spec", "copies")
	}
	if live != nil {
		_ = unstructured.SetNestedField(obj.Object, *live, "status", "storage", "copies")
	}
	return obj
}

func withCopiesCondition(obj *unstructured.Unstructured, status, reason, message string) *unstructured.Unstructured {
	_ = unstructured.SetNestedSlice(obj.Object, []interface{}{map[string]interface{}{
		"type": "StorageCopies", "status": status, "reason": reason, "message": message, "observedGeneration": int64(0),
	}}, "status", "conditions")
	return obj
}

func quickCopiesPolling(t *testing.T) {
	t.Helper()
	poll, limit, waitPoll, waitLimit := copiesVerdictPoll, copiesVerdictLimit, copiesWaitPoll, copiesWaitLimit
	copiesVerdictPoll, copiesVerdictLimit, copiesWaitPoll, copiesWaitLimit = time.Millisecond, 50*time.Millisecond, time.Millisecond, time.Second
	t.Cleanup(func() {
		copiesVerdictPoll, copiesVerdictLimit, copiesWaitPoll, copiesWaitLimit = poll, limit, waitPoll, waitLimit
	})
}

func setServiceCopies(t *testing.T, dyn *dynamicfake.FakeDynamicClient, want int32, wait bool) (string, *unstructured.Unstructured, error) {
	t.Helper()
	quickCopiesPolling(t)
	var out bytes.Buffer
	err := setCopies(context.Background(), &out, dyn, copiesChange{
		gvr: manifest.ServiceGVR, namespace: "shop", name: "db", want: want, wait: wait, infoCommand: "kip service info db",
	})
	got, getErr := dyn.Resource(manifest.ServiceGVR).Namespace("shop").Get(context.Background(), "db", metav1.GetOptions{})
	require.NoError(t, getErr)
	return out.String(), got, err
}

func copiesDyn(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	dyn := copiesApplyDyn()
	for _, obj := range objs {
		u := obj.(*unstructured.Unstructured)
		_, _ = dyn.Resource(manifest.ServiceGVR).Namespace("shop").Create(context.Background(), u, metav1.CreateOptions{})
	}
	return dyn
}

// convertingAfterPatch serves obj until a patch arrives and then the patched
// object with a Converting condition carrying message.
func convertingAfterPatch(dyn *dynamicfake.FakeDynamicClient, message string) {
	patched := false
	dyn.PrependReactor("patch", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		patched = true
		return false, nil, nil
	})
	dyn.PrependReactor("get", "services", func(a k8stesting.Action) (bool, runtime.Object, error) {
		if !patched {
			return false, nil, nil
		}
		obj, err := dyn.Tracker().Get(manifest.ServiceGVR, a.GetNamespace(), a.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		return true, withCopiesCondition(obj.(*unstructured.Unstructured).DeepCopy(), "False", "Converting", message), nil
	})
}

func TestSettingCopiesWritesThemAndPrintsTheConversion(t *testing.T) {
	obj := withCopiesCondition(copiesObject(ptr.To[int64](2), ptr.To[int64](2)), "True", "Healthy", "2 of 2 copies are healthy.")
	dyn := copiesDyn(obj)
	convertingAfterPatch(dyn, "Lowering the copy count to 1.")
	out, got, err := setServiceCopies(t, dyn, 1, false)
	require.NoError(t, err)
	copies, _, _ := unstructured.NestedInt64(got.Object, "spec", "copies")
	assert.EqualValues(t, 1, copies)
	assert.Contains(t, out, "Copies set to 1")
	assert.Contains(t, out, "Lowering the copy count to 1.")
	assert.Contains(t, out, "'kip service info db' shows the progress")
}

func TestABlockedConversionFailsTheCommand(t *testing.T) {
	obj := withCopiesCondition(copiesObject(ptr.To[int64](2), ptr.To[int64](2)), "False", "Blocked", "The server has no room for a copy of this volume (10.0 GiB).")
	_, _, err := setServiceCopies(t, copiesDyn(obj), 1, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot change now: The server has no room")
}

func TestCopiesDoNotChangeWhileAConversionRuns(t *testing.T) {
	obj := withCopiesCondition(copiesObject(ptr.To[int64](2), ptr.To[int64](1)), "False", "Converting", "Longhorn is building a copy of the volume (1 of 2 copies healthy).")
	dyn := copiesDyn(obj)
	dyn.ClearActions()
	_, got, err := setServiceCopies(t, dyn, 1, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "being converted, and can change again once that ends: Longhorn is building")
	copies, _, _ := unstructured.NestedInt64(got.Object, "spec", "copies")
	assert.EqualValues(t, 2, copies, "nothing is written")
	for _, a := range dyn.Actions() {
		assert.NotEqual(t, "patch", a.GetVerb())
	}
}

func TestTwoCopiesRunTheCreationChecksUnlessTheVolumeHasThem(t *testing.T) {
	obj := copiesObject(ptr.To[int64](1), ptr.To[int64](1))
	_, got, err := setServiceCopies(t, copiesDyn(obj), 2, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "offers one copy")
	copies, _, _ := unstructured.NestedInt64(got.Object, "spec", "copies")
	assert.EqualValues(t, 1, copies)

	adopted := withCopiesCondition(copiesObject(nil, ptr.To[int64](2)), "True", "Healthy", "2 of 2 copies are healthy.")
	out, _, err := setServiceCopies(t, copiesDyn(adopted), 2, false)
	require.NoError(t, err, "a volume that has two copies already needs no new node")
	assert.Contains(t, out, "2 of 2 copies are healthy.")
}

func TestWaitFollowsTheConversionUntilEveryCopyIsHealthy(t *testing.T) {
	obj := copiesObject(ptr.To[int64](2), ptr.To[int64](2))
	dyn := copiesDyn(obj)
	gets := 0
	dyn.PrependReactor("get", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		live := copiesObject(ptr.To[int64](1), ptr.To[int64](2))
		switch {
		case gets < 3:
			return true, withCopiesCondition(live, "False", "Converting", "Lowering the copy count to 1."), nil
		case gets < 5:
			return true, withCopiesCondition(live, "False", "Converting", "Removing an extra copy (1 left, 1 wanted)."), nil
		}
		return true, withCopiesCondition(copiesObject(ptr.To[int64](1), ptr.To[int64](1)), "True", "Healthy", "1 of 1 copies are healthy."), nil
	})
	out, _, err := setServiceCopies(t, dyn, 1, true)
	require.NoError(t, err)
	assert.Contains(t, out, "Lowering the copy count to 1.")
	assert.Contains(t, out, "Removing an extra copy")
	assert.Contains(t, out, "✔  1 of 1 copies are healthy.")
	assert.NotContains(t, out, "--wait waits")
}

func TestSettingTheCopiesTheObjectHasWritesNothing(t *testing.T) {
	obj := withCopiesCondition(copiesObject(ptr.To[int64](2), ptr.To[int64](2)), "True", "Healthy", "2 of 2 copies are healthy.")
	dyn := copiesDyn(obj)
	dyn.ClearActions()
	out, _, err := setServiceCopies(t, dyn, 2, false)
	require.NoError(t, err)
	assert.Contains(t, out, "Copies are already 2")
	for _, a := range dyn.Actions() {
		assert.NotEqual(t, "patch", a.GetVerb())
	}
}

func TestServiceInfoShowsTheCopiesAndWhatHoldsThemBack(t *testing.T) {
	cr := healthyServiceCR("db", "redis")
	_ = unstructured.SetNestedField(cr.Object, int64(2), "spec", "copies")
	_ = unstructured.SetNestedSlice(cr.Object, []interface{}{map[string]interface{}{
		"type": "StorageCopies", "status": "False", "reason": "NotEnoughReplicaNodes",
		"message": "Two copies need two ready nodes that can hold copies, and the cluster has 1.",
	}}, "status", "conditions")
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList", service.LonghornReplicaGVR: "ReplicaList"}, cr)
	client := k8sfake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "default"},
		Data:       map[string][]byte{"HOST": []byte("db.default.svc.cluster.local"), "PORT": []byte("6379")},
	})

	var out bytes.Buffer
	require.NoError(t, writeServiceInfo(context.Background(), &out, &service.Manager{Client: client, Dynamic: dyn}, "default", "db", false))
	assert.Contains(t, out.String(), "Copies:   2, not created yet")
	assert.Contains(t, out.String(), "the cluster has 1.")
}

// A verdict the controller wrote before the change is never printed as the
// change's verdict.
func TestAVerdictOnAnOlderGenerationIsNotTheAnswer(t *testing.T) {
	obj := withCopiesCondition(copiesObject(ptr.To[int64](2), ptr.To[int64](2)), "True", "Healthy", "2 of 2 copies are healthy.")
	obj.SetGeneration(1)
	_ = unstructured.SetNestedField(obj.Object, []interface{}{map[string]interface{}{
		"type": "StorageCopies", "status": "True", "reason": "Healthy", "message": "2 of 2 copies are healthy.", "observedGeneration": int64(1),
	}}, "status", "conditions")
	dyn := copiesDyn(obj)
	dyn.PrependReactor("patch", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		written := withCopiesCondition(copiesObject(ptr.To[int64](1), ptr.To[int64](2)), "True", "Healthy", "2 of 2 copies are healthy.")
		written.SetGeneration(2)
		return true, written, dyn.Tracker().Update(manifest.ServiceGVR, written, "shop")
	})
	out, _, err := setServiceCopies(t, dyn, 1, false)
	require.NoError(t, err)
	assert.NotContains(t, out, "2 of 2 copies are healthy.")
	assert.Contains(t, out, "Kipper converts the copies in the background")
}

// Another client set the copies back before the controller answered, so the
// healthy verdict describes a count this command did not ask for.
func TestWaitFailsWhenTheCopiesWereChangedByAnotherClient(t *testing.T) {
	obj := withCopiesCondition(copiesObject(ptr.To[int64](2), ptr.To[int64](2)), "True", "Healthy", "2 of 2 copies are healthy.")
	dyn := copiesDyn(obj)
	dyn.PrependReactor("get", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, withCopiesCondition(copiesObject(ptr.To[int64](2), ptr.To[int64](2)), "True", "Healthy", "2 of 2 copies are healthy."), nil
	})
	out, _, err := setServiceCopies(t, dyn, 1, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the copies of db were changed to 2 while waiting")
	assert.NotContains(t, out, "✔  2 of 2 copies are healthy.")
}

func TestWaitFailsWhenTheObjectWasCreatedAgain(t *testing.T) {
	obj := withCopiesCondition(copiesObject(ptr.To[int64](2), ptr.To[int64](2)), "True", "Healthy", "2 of 2 copies are healthy.")
	obj.SetUID("uid-1")
	dyn := copiesDyn(obj)
	dyn.PrependReactor("get", "services", func(a k8stesting.Action) (bool, runtime.Object, error) {
		stored, err := dyn.Tracker().Get(manifest.ServiceGVR, a.GetNamespace(), a.(k8stesting.GetAction).GetName())
		if err != nil {
			return true, nil, err
		}
		if copies, _, _ := unstructured.NestedInt64(stored.(*unstructured.Unstructured).Object, "spec", "copies"); copies == 2 {
			return true, stored, nil
		}
		again := withCopiesCondition(copiesObject(ptr.To[int64](1), ptr.To[int64](1)), "True", "Healthy", "1 of 1 copies are healthy.")
		again.SetUID("uid-2")
		return true, again, nil
	})
	_, _, err := setServiceCopies(t, dyn, 1, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db was deleted and created again while waiting")
}

func setSharedCopies(t *testing.T, dyn *dynamicfake.FakeDynamicClient, want int32) error {
	t.Helper()
	quickCopiesPolling(t)
	return setCopies(context.Background(), io.Discard, dyn, copiesChange{
		gvr: manifest.ServiceGVR, namespace: "shop", name: "db", want: want, shared: true, wait: true, infoCommand: "kip volume list",
	})
}

// The controller never reports a shared volume with two copies as healthy
// while RWX copies are not qualified, so waiting for health would time out.
func TestTwoCopiesOfASharedVolumeAreRefusedEvenWhenItHasThem(t *testing.T) {
	if platform.RWXCopiesQualified {
		t.Skip("shared volumes may have two copies")
	}
	err := setSharedCopies(t, copiesDyn(copiesObject(ptr.To[int64](1), ptr.To[int64](2))), 2)
	assert.ErrorContains(t, err, "shared volumes cannot have two copies")

	already := withCopiesCondition(copiesObject(ptr.To[int64](2), ptr.To[int64](2)), "False", "RWXNotQualified", "Shared volumes keep one copy in this release.")
	start := time.Now()
	err = setSharedCopies(t, copiesDyn(already), 2)
	assert.ErrorContains(t, err, "Shared volumes keep one copy in this release.")
	assert.Less(t, time.Since(start), copiesWaitLimit, "the wait ends at once")
}
