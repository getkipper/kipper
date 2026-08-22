package cmd

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/datavolume"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

// The service under test is "db" in "shop-prod" throughout, as the fixtures
// around it are.
func serviceToDelete() *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": manifest.ServiceGVR.GroupVersion().String(),
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name": "db", "namespace": "shop-prod", "uid": "the-service-being-deleted",
			"resourceVersion": "1",
		},
		"spec": map[string]interface{}{"type": "postgres"},
	}}
}

// No StatefulSet: the wait for the workload to go is the internal package's to
// test, and a fixture carrying one would make every case here sit out the real
// timeout.
func deleteFixture(t *testing.T) (*service.Manager, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	objects := []runtime.Object{&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-db-0", Namespace: "shop-prod", Labels: map[string]string{"app": "db"},
	}}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"},
		serviceToDelete(),
	)
	return &service.Manager{Client: k8sfake.NewSimpleClientset(objects...), Dynamic: dyn}, dyn //nolint:staticcheck
}

func claimIsThere(t *testing.T, mgr *service.Manager) bool {
	t.Helper()
	_, err := mgr.Client.CoreV1().PersistentVolumeClaims("shop-prod").
		Get(context.Background(), "data-db-0", metav1.GetOptions{})
	return err == nil
}

// The flag was read and then ignored on this path: the CR went, the command said
// the service was deleted, and the volume stayed. An operator who asked for the
// data to go believed it had.
func TestServiceDelete_DestroysTheVolumeWhenAsked(t *testing.T) {
	mgr, dyn := deleteFixture(t)

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", true))

	assert.False(t, claimIsThere(t, mgr), "the volume survived a delete that asked for it to go")
	assert.Contains(t, out.String(), "db")
}

// Without the flag the volume stays, which is the useful default, and the
// command says so. A volume nobody knows about is what a service of the same
// name lands on later, and the reconciler then refuses it for having data and no
// password.
func TestServiceDelete_SaysTheVolumeWasKept(t *testing.T) {
	mgr, dyn := deleteFixture(t)

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", false))

	assert.True(t, claimIsThere(t, mgr), "the volume was destroyed without being asked")
	printed := out.String()
	assert.Contains(t, printed, "volume", "nothing told the operator the data is still there")
	assert.Contains(t, printed, "--delete-data", "nothing said what removes it")
}

// The CR is gone and the volume is not, which is what the old behaviour left
// behind. Running the delete again with the flag has to clear it.
func TestServiceDelete_ClearsAVolumeLeftByAnEarlierDelete(t *testing.T) {
	mgr, _ := deleteFixture(t)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"},
	)

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", true))

	assert.False(t, claimIsThere(t, mgr), "the leftover volume survived")
}

// A service that is not there and no volume either is not an error worth
// inventing, but it must not claim to have deleted anything.
func TestServiceDelete_RefusesToDestroyWithoutTheFlagWhenTheServiceIsGone(t *testing.T) {
	mgr, _ := deleteFixture(t)
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"},
	)

	var out bytes.Buffer
	err := deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", false)

	require.Error(t, err, "destroying data must stay behind the flag")
	assert.True(t, strings.Contains(err.Error(), "--delete-data"), "the message names the flag: %v", err)
	assert.True(t, claimIsThere(t, mgr))
}

// The CR goes first and cannot be brought back, so a failure after that point
// has to say what already happened. An error naming only the volume reads like
// nothing was deleted, and the operator is left guessing whether the service is
// still there.
func TestServiceDelete_SaysTheServiceWentWhenTheVolumeCouldNot(t *testing.T) {
	mgr, dyn := deleteFixture(t)
	refusing := mgr.Client.(*k8sfake.Clientset)
	refusing.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("the storage backend is unreachable")
	})

	var out bytes.Buffer
	err := deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", true)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "deleted", "nothing said the service itself has already gone")
	assert.Contains(t, err.Error(), "volume", "nothing said which half failed")
	assert.NotContains(t, out.String(), "and its volume with it", "a failed destroy was reported as a success")
}

// The volumes have to be read before the CR is deleted, because the name is
// free from that moment on. A service created under the same name while this
// one is finishing keeps its own data.
func TestServiceDelete_LeavesTheVolumeOfAServiceMadeSince(t *testing.T) {
	mine := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-db-0", Namespace: "shop-prod", UID: "the-service-being-deleted",
		Labels: map[string]string{"app": "db"},
	}}
	replacement := mine.DeepCopy()
	replacement.UID = "a-service-made-since"

	client := k8sfake.NewSimpleClientset(mine) //nolint:staticcheck
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"},
		serviceToDelete(),
	)
	dyn.PrependReactor("delete", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		tracker := client.Tracker()
		_ = tracker.Delete(corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), "shop-prod", "data-db-0")
		_ = tracker.Add(replacement)
		return false, nil, nil
	})
	mgr := &service.Manager{Client: client, Dynamic: dyn}

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", true))

	assert.True(t, claimIsThere(t, mgr), "the volume of a service created during the delete was destroyed with the old one's data")
}

// A volume this does not recognise as the service's is left alone, and the
// command has to report that rather than say the data went. The label may have
// been stripped by hand, or the volume may have gone in an earlier delete;
// either way nobody can check the claim afterwards.
func TestServiceDelete_SaysWhenItFoundNoVolumeToRemove(t *testing.T) {
	unrecognised := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-db-0", Namespace: "shop-prod",
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"},
		serviceToDelete(),
	)
	mgr := &service.Manager{Client: k8sfake.NewSimpleClientset(unrecognised), Dynamic: dyn} //nolint:staticcheck

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", true))

	assert.Contains(t, out.String(), "No volume", "the command claimed a volume went that it never touched")
	assert.True(t, claimIsThere(t, mgr), "a volume with nothing tying it to this service was destroyed")
}

// A project's own operators may delete their services but not the volumes
// underneath them: kipper:project-owner and kipper:project-deployer carry
// get, list and watch on claims and no delete at all. The mark is what gets
// their data removed, by the cluster, once the CR has gone.
func TestServiceDelete_AsksTheClusterToRemoveTheVolume(t *testing.T) {
	mgr, dyn := deleteFixture(t)
	var marked string
	dyn.PrependReactor("patch", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		marked = string(action.(k8stesting.PatchAction).GetPatch())
		return false, nil, nil
	})

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", true))

	assert.Contains(t, marked, datavolume.DeleteAnnotation,
		"nothing asked the cluster to remove the volume, so an operator who may not delete one loses their data")
	assert.Contains(t, marked, "true")
}

// The volume delete is refused for exactly the operators who are meant to be
// able to do this, so a refusal is not the end of it: the cluster was already
// asked, and what matters is whether the volume goes.
func TestServiceDelete_LeavesTheVolumeToTheClusterWhenItMayNotDeleteIt(t *testing.T) {
	mgr, dyn := deleteFixture(t)
	refused := mgr.Client.(*k8sfake.Clientset)
	refused.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			corev1.Resource("persistentvolumeclaims"), "data-db-0", fmt.Errorf("not allowed"))
	})
	// What the cluster does about the mark, on its own schedule. Through the
	// tracker, because the refusal above is the operator's and not the
	// controller's.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = refused.Tracker().Delete(
			corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims"), "shop-prod", "data-db-0")
	}()

	var out bytes.Buffer
	err := deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", true)

	require.NoError(t, err, "a volume the cluster removed was reported as a failure")
	assert.False(t, claimIsThere(t, mgr), "the volume is still there")
}

// A name ending -git derives the Secret an app on the older naming keeps its git
// token in. Creating such a service is refused for that reason, and deleting one
// that was never there must not take the app's token instead.
func TestServiceDelete_LeavesAnAppsGitTokenAlone(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": deployer.AppGVR.GroupVersion().String(),
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "shop", "namespace": "shop-prod"},
		"spec": map[string]interface{}{
			"git": map[string]interface{}{"credentialsSecret": secretname.LegacyGitCredential("shop")},
		},
	}}
	token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: secretname.ServiceCredentials("shop-git"), Namespace: "shop-prod",
	}}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			manifest.ServiceGVR: "ServiceList",
			deployer.AppGVR:     "AppList",
		},
		app,
	)
	mgr := &service.Manager{Client: k8sfake.NewSimpleClientset(token), Dynamic: dyn} //nolint:staticcheck

	var out bytes.Buffer
	err := deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "shop-git", true)

	require.Error(t, err, "the app's git token was deleted as though it were a service's credentials")
	assert.Contains(t, err.Error(), "shop")

	_, getErr := mgr.Client.CoreV1().Secrets("shop-prod").
		Get(context.Background(), secretname.ServiceCredentials("shop-git"), metav1.GetOptions{})
	assert.NoError(t, getErr, "the app's git token is gone, so its next build cannot clone")
}

// The mark and the delete both have to name the service that was read, or a
// service somebody creates under the same name in between is the one that goes.
func TestServiceDelete_PinsTheMarkAndTheDeleteToOneService(t *testing.T) {
	mgr, dyn := deleteFixture(t)
	var markedWithLock, deletedWithUID bool
	dyn.PrependReactor("patch", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		markedWithLock = strings.Contains(string(action.(k8stesting.PatchAction).GetPatch()), "resourceVersion")
		return false, nil, nil
	})
	dyn.PrependReactor("delete", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deletedWithUID = action.(k8stesting.DeleteActionImpl).DeleteOptions.Preconditions != nil
		return false, nil, nil
	})

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", true))

	assert.True(t, markedWithLock, "the mark was not pinned to the service that was read")
	assert.True(t, deletedWithUID, "the delete was not pinned to the service that was marked")
}

// A mark can outlive the delete that set it: the patch lands and the delete that
// should have followed does not. Left there, the next delete says the volume was
// kept while the finalizer destroys it. A delete that means to keep the volume
// says so by taking the mark off first.
func TestServiceDelete_ClearsAStaleMarkWhenItKeepsTheVolume(t *testing.T) {
	mgr, _ := deleteFixture(t)
	stale := serviceToDelete()
	stale.SetAnnotations(map[string]string{datavolume.DeleteAnnotation: "true"})
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"},
		stale,
	)
	var cleared bool
	dyn.PrependReactor("patch", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		patch := string(action.(k8stesting.PatchAction).GetPatch())
		if strings.Contains(patch, datavolume.DeleteAnnotation) && strings.Contains(patch, "null") {
			cleared = true
		}
		return false, nil, nil
	})

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", false))

	assert.True(t, cleared,
		"a mark left by an earlier delete still stands, so the volume this said it kept is destroyed")
	assert.Contains(t, out.String(), "volume was kept")
}

// A delete already under way settled what happens to the volume, and the
// finalizer may be part way through it. Neither half of this command has
// anything to say about that, and saying the volume was kept over the top of a
// destruction already running is the one thing it must not do.
func TestServiceDelete_RefusesAServiceThatIsAlreadyGoing(t *testing.T) {
	for _, keepData := range []bool{true, false} {
		mgr, _ := deleteFixture(t)
		going := serviceToDelete()
		going.SetDeletionTimestamp(&metav1.Time{Time: time.Now()})
		going.SetFinalizers([]string{"kipper.run/service-cleanup"})
		dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
			runtime.NewScheme(),
			map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"},
			going,
		)

		var out bytes.Buffer
		err := deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", keepData)

		require.Error(t, err, "delete-data=%v walked into a delete that was already running", keepData)
		assert.Contains(t, err.Error(), "already being deleted")
		assert.NotContains(t, out.String(), "volume was kept",
			"a destruction already under way was reported as a volume kept")
	}
}

// The service is read before its volumes, so the mark cannot land on one that
// took the name in between. Reading the volumes first would leave the command
// holding one service's volumes and another service's identity.
func TestServiceDelete_ReadsTheServiceBeforeItsVolumes(t *testing.T) {
	mgr, dyn := deleteFixture(t)
	var order []string
	dyn.PrependReactor("get", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "read the service")
		return false, nil, nil
	})
	client := mgr.Client.(*k8sfake.Clientset)
	client.PrependReactor("list", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		order = append(order, "read the volumes")
		return false, nil, nil
	})

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", true))

	require.GreaterOrEqual(t, len(order), 2)
	assert.Equal(t, "read the service", order[0],
		"the volumes were read before the service they are supposed to belong to")
}

// A service record that was not there when the delete started is not this
// command's to delete. Whatever holds the name now belongs to whoever made it.
func TestServiceDelete_NeverDeletesARecordItDidNotRead(t *testing.T) {
	mgr, _ := deleteFixture(t)
	empty := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"},
	)
	var deleted bool
	empty.PrependReactor("delete", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		deleted = true
		return false, nil, nil
	})

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, empty, "shop-prod", "db", true))

	assert.False(t, deleted,
		"a service record that appeared after the read was deleted by name alone")
}

// A write nobody needs is one more chance for this and a concurrent delete to
// disagree, so a service with no mark on it is left alone.
func TestServiceDelete_DoesNotWriteToAServiceWithNothingToClear(t *testing.T) {
	mgr, dyn := deleteFixture(t)
	var patched bool
	dyn.PrependReactor("patch", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		patched = true
		return false, nil, nil
	})

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", false))

	assert.False(t, patched, "a service with no mark was written to for nothing")
	assert.Contains(t, out.String(), "volume was kept")
}

// The delete is pinned to the version the mark was decided on, so a mark that
// lands in between cannot turn a delete that says it kept the volume into one
// that destroyed it.
func TestServiceDelete_PinsTheDeleteToTheDecisionItMade(t *testing.T) {
	mgr, dyn := deleteFixture(t)
	var pinned *metav1.Preconditions
	dyn.PrependReactor("delete", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pinned = action.(k8stesting.DeleteActionImpl).DeleteOptions.Preconditions
		return false, nil, nil
	})

	var out bytes.Buffer
	require.NoError(t, deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", true))

	require.NotNil(t, pinned, "the delete carried no preconditions at all")
	require.NotNil(t, pinned.UID)
	assert.Equal(t, types.UID("the-service-being-deleted"), *pinned.UID)
	require.NotNil(t, pinned.ResourceVersion,
		"the delete is pinned to the service but not to the mark, so somebody else's mark can land in between")
	assert.NotEmpty(t, *pinned.ResourceVersion)
}

// A conflict says this delete was not the one the API server took, and nothing
// more. Another may have been accepted a moment earlier and be running now, so
// the message must not promise that nothing was deleted.
func TestServiceDelete_DoesNotPromiseNothingWentOnAConflict(t *testing.T) {
	mgr, dyn := deleteFixture(t)
	dyn.PrependReactor("delete", "services", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			manifest.ServiceGVR.GroupResource(), "db", fmt.Errorf("the object has changed"))
	})

	var out bytes.Buffer
	err := deleteService(context.Background(), &out, mgr, dyn, "shop-prod", "db", true)

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "nothing was deleted",
		"a delete that may well have happened elsewhere was reported as nothing having happened")
	assert.Contains(t, err.Error(), "did not delete it")
}
