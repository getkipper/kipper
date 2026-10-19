package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func veleroObject(kind, name, phase string, uid types.UID) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1", "kind": kind,
		"metadata": map[string]interface{}{"name": name, "namespace": "velero", "uid": string(uid)},
		"status":   map[string]interface{}{"phase": phase},
	}}
}

func volumeRestore(name, restoreUID, phase, uploader string, cancel bool) *unstructured.Unstructured {
	u := veleroObject("PodVolumeRestore", name, phase, types.UID(name+"-uid"))
	u.SetLabels(map[string]string{"velero.io/restore-uid": restoreUID})
	u.Object["spec"] = map[string]interface{}{"uploaderType": uploader, "cancel": cancel}
	return u
}

func cancelFakes(objs ...k8sruntime.Object) *dynamicfake.FakeDynamicClient {
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(k8sruntime.NewScheme(), map[schema.GroupVersionResource]string{
		restoreGVR: "RestoreList", backupGVR: "BackupList", podVolumeRestoreGVR: "PodVolumeRestoreList", dataDownloadGVR: "DataDownloadList",
	}, objs...)
}

func shortCancelWaits(t *testing.T) {
	t.Helper()
	poll, wait, restart := restoreCancelPoll, restoreCancelWait, veleroRestartWait
	restoreCancelPoll, restoreCancelWait, veleroRestartWait = time.Millisecond, 100*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { restoreCancelPoll, restoreCancelWait, veleroRestartWait = poll, wait, restart })
}

func setPhase(dyn *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource, name, phase string) {
	r, err := dyn.Resource(gvr).Namespace("velero").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		return
	}
	_ = unstructured.SetNestedField(r.Object, phase, "status", "phase")
	_, _ = dyn.Resource(gvr).Namespace("velero").Update(context.Background(), r, metav1.UpdateOptions{})
}

func phaseOf(t *testing.T, dyn *dynamicfake.FakeDynamicClient, gvr schema.GroupVersionResource, name string) string {
	t.Helper()
	r, err := dyn.Resource(gvr).Namespace("velero").Get(context.Background(), name, metav1.GetOptions{})
	require.NoError(t, err)
	phase, _, _ := unstructured.NestedString(r.Object, "status", "phase")
	return phase
}

// nodeAgent simulates cancellation by marking each patched volume restore
// Canceled and the named restore PartiallyFailed.
func nodeAgent(dyn *dynamicfake.FakeDynamicClient, restore string) {
	dyn.PrependReactor("patch", "podvolumerestores", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		name := action.(k8stesting.PatchAction).GetName()
		go func() {
			setPhase(dyn, podVolumeRestoreGVR, name, "Canceled")
			setPhase(dyn, restoreGVR, restore, "PartiallyFailed")
		}()
		return false, nil, nil
	})
}

// veleroServer creates a fake Deployment with one pod and records replica
// changes. Scaling to zero removes the pod only when podLeaves is true.
// Scaling up marks the named restore Failed.
func veleroServer(t *testing.T, dyn *dynamicfake.FakeDynamicClient, restore string, podLeaves bool) (*kubefake.Clientset, *[]int32) {
	t.Helper()
	replicas := int32(1)
	clientset := kubefake.NewClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "velero", Namespace: "velero", Generation: 1},
			Spec:       appsv1.DeploymentSpec{Replicas: &replicas, Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"name": "velero"}}},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1, UpdatedReplicas: 1},
		},
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "velero-1", Namespace: "velero", Labels: map[string]string{"name": "velero"}}},
	)
	var scaledTo []int32
	clientset.PrependReactor("get", "deployments", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		return true, &autoscalingv1.Scale{ObjectMeta: metav1.ObjectMeta{Name: "velero", Namespace: "velero"}, Spec: autoscalingv1.ScaleSpec{Replicas: replicas}}, nil
	})
	clientset.PrependReactor("update", "deployments", func(action k8stesting.Action) (bool, k8sruntime.Object, error) {
		if action.GetSubresource() != "scale" {
			return false, nil, nil
		}
		s := action.(k8stesting.UpdateAction).GetObject().(*autoscalingv1.Scale)
		scaledTo = append(scaledTo, s.Spec.Replicas)
		replicas = s.Spec.Replicas
		if replicas == 0 && podLeaves {
			_ = clientset.Tracker().Delete(schema.GroupVersionResource{Version: "v1", Resource: "pods"}, "velero", "velero-1")
		}
		if replicas > 0 {
			setPhase(dyn, restoreGVR, restore, "Failed")
		}
		return true, s, nil
	})
	return clientset, &scaledTo
}

func TestAFinishedRestoreIsNotCanceled(t *testing.T) {
	dyn := cancelFakes(veleroObject("Restore", "r1", "Completed", "u1"))
	err := cancelRestore(context.Background(), kubefake.NewClientset(), dyn, "r1", false)
	assert.ErrorContains(t, err, "already finished")
}

func TestCancelingTheVolumeRestoresReleasesTheRestoreWithoutARestart(t *testing.T) {
	shortCancelWaits(t)
	dyn := cancelFakes(veleroObject("Restore", "r1", "InProgress", "u1"),
		volumeRestore("pvr-a", "u1", "InProgress", "kopia", false),
		volumeRestore("pvr-b", "u1", "Completed", "kopia", false),
		volumeRestore("pvr-other", "u2", "InProgress", "kopia", false))
	nodeAgent(dyn, "r1")
	clientset := kubefake.NewClientset()

	require.NoError(t, cancelRestore(context.Background(), clientset, dyn, "r1", false))

	var patched []string
	for _, a := range dyn.Actions() {
		if p, ok := a.(k8stesting.PatchAction); ok {
			patched = append(patched, p.GetName())
			assert.JSONEq(t, `{"metadata":{"uid":"pvr-a-uid"},"spec":{"cancel":true}}`, string(p.GetPatch()), "the UID keeps a same-named replacement safe")
		}
	}
	assert.Equal(t, []string{"pvr-a"}, patched, "only the target's unfinished volume restores are canceled")
	assert.Equal(t, "InProgress", phaseOf(t, dyn, podVolumeRestoreGVR, "pvr-other"))
	assert.Empty(t, clientset.Actions(), "a released restore needs no Velero restart")
}

func TestARestoreIsNotReportedCanceledWhileAVolumeRestoreStillRuns(t *testing.T) {
	shortCancelWaits(t)
	dyn := cancelFakes(veleroObject("Restore", "r1", "InProgress", "u1"), volumeRestore("pvr-a", "u1", "Canceling", "kopia", true))
	dyn.PrependReactor("list", "podvolumerestores", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		go setPhase(dyn, restoreGVR, "r1", "PartiallyFailed")
		return false, nil, nil
	})

	err := cancelRestore(context.Background(), kubefake.NewClientset(), dyn, "r1", false)

	assert.ErrorContains(t, err, "still running")
}

func TestAResticVolumeRestoreIsRefusedEvenWhenCancelWasRequested(t *testing.T) {
	for _, requested := range []bool{false, true} {
		dyn := cancelFakes(veleroObject("Restore", "r1", "InProgress", "u1"), volumeRestore("pvr-a", "u1", "InProgress", "restic", requested))
		err := cancelRestore(context.Background(), kubefake.NewClientset(), dyn, "r1", false)
		assert.ErrorContains(t, err, "restic", "cancel already requested: %v", requested)
	}
}

func TestADataMoverRestoreIsRefused(t *testing.T) {
	shortCancelWaits(t)
	dd := veleroObject("DataDownload", "dd-1", "InProgress", "dd")
	dd.SetAPIVersion("velero.io/v2alpha1")
	dd.SetLabels(map[string]string{"velero.io/restore-uid": "u1"})
	dyn := cancelFakes(veleroObject("Restore", "r1", "InProgress", "u1"), dd)
	err := cancelRestore(context.Background(), kubefake.NewClientset(), dyn, "r1", false)
	assert.ErrorContains(t, err, "CSI data movement")
}

func TestARestartThatWouldFailOtherWorkNeedsForce(t *testing.T) {
	shortCancelWaits(t)
	dyn := cancelFakes(veleroObject("Restore", "r1", "InProgress", "u1"),
		veleroObject("Backup", "nightly", "InProgress", "b1"),
		veleroObject("Restore", "r2", "InProgress", "u2"))
	clientset := kubefake.NewClientset()

	err := cancelRestore(context.Background(), clientset, dyn, "r1", false)

	assert.ErrorContains(t, err, "restore r2, backup nightly")
	assert.Empty(t, clientset.Actions(), "Velero must not be touched")
}

func TestAStuckRestoreIsEndedByRestartingVelero(t *testing.T) {
	shortCancelWaits(t)
	dyn := cancelFakes(veleroObject("Restore", "r1", "InProgress", "u1"))
	clientset, scaledTo := veleroServer(t, dyn, "r1", true)

	require.NoError(t, cancelRestore(context.Background(), clientset, dyn, "r1", false))

	assert.Equal(t, []int32{0, 1}, *scaledTo, "the old server stops before the new one starts")
	assert.Equal(t, "Failed", phaseOf(t, dyn, restoreGVR, "r1"), "the restore stays as the record")
}

func TestVeleroIsScaledBackWhenItsOldPodDoesNotStop(t *testing.T) {
	shortCancelWaits(t)
	dyn := cancelFakes(veleroObject("Restore", "r1", "InProgress", "u1"))
	clientset, scaledTo := veleroServer(t, dyn, "r1", false)

	err := cancelRestore(context.Background(), clientset, dyn, "r1", false)

	assert.ErrorContains(t, err, "old Velero server to stop")
	assert.Equal(t, []int32{0, 1}, *scaledTo, "backups must never be left without a server")
}

func TestCancelFlagRules(t *testing.T) {
	run := func(args ...string) error {
		cmd := &cobra.Command{Args: backupRestoreCmd.Args, RunE: func(*cobra.Command, []string) error { return nil }, SilenceErrors: true, SilenceUsage: true}
		cmd.Flags().String("cancel", "", "")
		cmd.Flags().Bool("force", false, "")
		cmd.SetArgs(args)
		return cmd.Execute()
	}
	assert.NoError(t, run("--cancel", "r1"))
	assert.Error(t, run("--cancel", ""), "an empty name must not fall back to starting a restore")
	assert.Error(t, run("--cancel", "r1", "backup-1"), "cancel takes no backup name")
	assert.Error(t, run("backup-1", "--force"), "--force only means something with --cancel")
	assert.NoError(t, run("backup-1"))
}

func TestADataMoverAppearingDuringTheWaitIsRefused(t *testing.T) {
	shortCancelWaits(t)
	dyn := cancelFakes(veleroObject("Restore", "r1", "InProgress", "u1"))
	polls := 0
	dyn.PrependReactor("list", "datadownloads", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		polls++
		if polls == 3 {
			dd := veleroObject("DataDownload", "dd-1", "InProgress", "dd")
			dd.SetAPIVersion("velero.io/v2alpha1")
			dd.SetLabels(map[string]string{"velero.io/restore-uid": "u1"})
			_ = dyn.Tracker().Create(dataDownloadGVR, dd, "velero")
		}
		return false, nil, nil
	})
	clientset := kubefake.NewClientset()

	err := cancelRestore(context.Background(), clientset, dyn, "r1", false)

	assert.ErrorContains(t, err, "CSI data movement")
	assert.Empty(t, clientset.Actions(), "Velero must not be restarted under a data mover restore")
}

func TestARestoreThatMovedOnBeforeTheRestartIsNotRestarted(t *testing.T) {
	shortCancelWaits(t)
	dyn := cancelFakes(veleroObject("Restore", "r1", "InProgress", "u1"))
	dyn.PrependReactor("list", "restores", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		// The fake holds its lock while reactors run, so this edits the tracker directly.
		obj, err := dyn.Tracker().Get(restoreGVR, "velero", "r1")
		if err == nil {
			r := obj.(*unstructured.Unstructured)
			_ = unstructured.SetNestedField(r.Object, "Finalizing", "status", "phase")
			_ = dyn.Tracker().Update(restoreGVR, r, "velero")
		}
		return false, nil, nil
	})
	clientset := kubefake.NewClientset()

	err := cancelRestore(context.Background(), clientset, dyn, "r1", true)

	assert.ErrorContains(t, err, "Finalizing now")
	assert.Empty(t, clientset.Actions(), "--force overrides collateral, never the target's own state")
}

func TestAFinishedRestoreWithAVolumeRestoreStillRunningIsAwaited(t *testing.T) {
	shortCancelWaits(t)
	dyn := cancelFakes(veleroObject("Restore", "r1", "Failed", "u1"), volumeRestore("pvr-a", "u1", "InProgress", "kopia", false))
	nodeAgent(dyn, "r1")

	require.NoError(t, cancelRestore(context.Background(), kubefake.NewClientset(), dyn, "r1", false))

	assert.Equal(t, "Canceled", phaseOf(t, dyn, podVolumeRestoreGVR, "pvr-a"))
}

func TestARestartWaitsForTheNewServerToBeReady(t *testing.T) {
	shortCancelWaits(t)
	dyn := cancelFakes(veleroObject("Restore", "r1", "InProgress", "u1"))
	clientset, _ := veleroServer(t, dyn, "r1", true)
	d, err := clientset.AppsV1().Deployments("velero").Get(context.Background(), "velero", metav1.GetOptions{})
	require.NoError(t, err)
	d.Status.ReadyReplicas = 0
	_, err = clientset.AppsV1().Deployments("velero").UpdateStatus(context.Background(), d, metav1.UpdateOptions{})
	require.NoError(t, err)

	err = cancelRestore(context.Background(), clientset, dyn, "r1", false)

	assert.ErrorContains(t, err, "Velero to start again", "a server that never becomes ready is not a finished restart")
}

func TestAFinishedDataMoverRestoreIsRefused(t *testing.T) {
	shortCancelWaits(t)
	dd := veleroObject("DataDownload", "dd-1", "InProgress", "dd")
	dd.SetAPIVersion("velero.io/v2alpha1")
	dd.SetLabels(map[string]string{"velero.io/restore-uid": "u1"})
	dyn := cancelFakes(veleroObject("Restore", "r1", "Failed", "u1"), dd, volumeRestore("pvr-a", "u1", "InProgress", "kopia", false))
	err := cancelRestore(context.Background(), kubefake.NewClientset(), dyn, "r1", false)
	assert.ErrorContains(t, err, "CSI data movement")
}
