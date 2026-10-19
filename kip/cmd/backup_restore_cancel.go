package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	typedappsv1 "k8s.io/client-go/kubernetes/typed/apps/v1"
)

var dataDownloadGVR = schema.GroupVersionResource{Group: "velero.io", Version: "v2alpha1", Resource: "datadownloads"}

// Cancellation and restart waits are variables so tests can shorten them.
var (
	restoreCancelPoll  = 3 * time.Second
	restoreCancelWait  = 5 * time.Minute
	veleroRestartWait  = 5 * time.Minute
	veleroScaleTimeout = 30 * time.Second
)

// cancelRestore asks node-agent to cancel a file-system restore's unfinished
// volume restores, including those left after the restore has ended. If the
// restore remains InProgress after restoreCancelWait, it restarts Velero to
// mark the restore Failed on startup. The restart also fails InProgress backups
// and other InProgress restores, so these block a restart unless force is set.
// Success requires a finished restore with no volume restores still running.
// The Restore object is retained as a record.
func cancelRestore(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, name string, force bool) error {
	restore, err := dyn.Resource(restoreGVR).Namespace("velero").Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading restore %s: %w", name, err)
	}
	phase, _, _ := unstructured.NestedString(restore.Object, "status", "phase")
	uid := restore.GetUID()
	if restoreFinished(restore) {
		// A restore can end while its volume restores still run, for example
		// after an interrupted cancel, so those are still canceled and awaited.
		check, cancel := context.WithTimeout(ctx, veleroScaleTimeout)
		err := refuseDataDownloads(check, dyn, uid)
		running := 0
		if err == nil {
			running, err = cancelVolumeRestores(check, dyn, uid)
		}
		cancel()
		if err != nil {
			return err
		}
		if running == 0 {
			return fmt.Errorf("restore %s has already finished (%s), and none of its volume restores is running. There is nothing to cancel", name, phase)
		}
	} else if phase != "InProgress" {
		return fmt.Errorf("restore %s is %s. kip cancels only a restore that is InProgress", name, phaseOrNew(phase))
	}

	fmt.Printf("  ·  Asking node-agent to cancel the volume restores for %s\n", name)
	phase, done, err := waitWhileCanceling(ctx, dyn, name, uid, restoreCancelWait)
	if err != nil {
		return err
	}
	restarted := false
	if !done && restorePhaseFinal(phase) {
		return fmt.Errorf("restore %s ended with phase %s. Some of its volume restores are still running. Run kip backup restore --cancel %s again to keep waiting for them", name, phase, name)
	}
	if !done {
		// Startup fails only InProgress restores, so later phases must finish
		// through normal processing.
		if phase != "InProgress" {
			return fmt.Errorf("restore %s is %s now. A Velero restart only ends InProgress restores. Wait for it to finish", name, phase)
		}
		if err := refuseCollateralRestart(ctx, dyn, name, uid, force); err != nil {
			return err
		}
		fmt.Printf("  ·  Restore %s is still InProgress. Restarting the Velero server, which marks it Failed when it starts\n", name)
		if err := restartVeleroServer(ctx, clientset); err != nil {
			return err
		}
		restarted = true
		if phase, done, err = waitWhileCanceling(ctx, dyn, name, uid, restoreCancelWait); err != nil {
			return err
		} else if !done {
			return fmt.Errorf("restore %s is still %s after Velero restarted, or its volume restores are still running. Run kip backup restore --cancel %s again to continue waiting for the restore and its volume restores to finish", name, phase, name)
		}
	}
	return reportCanceledRestore(name, phase, restarted)
}

func phaseOrNew(phase string) string {
	if phase == "" {
		return "New"
	}
	return phase
}

// refuseDataDownloads rejects restores with any DataDownload objects because
// CSI data movement cancellation is unsupported. A missing DataDownload API
// is treated as an empty list.
func refuseDataDownloads(ctx context.Context, dyn dynamic.Interface, uid types.UID) error {
	list, err := dyn.Resource(dataDownloadGVR).Namespace("velero").List(ctx, metav1.ListOptions{LabelSelector: "velero.io/restore-uid=" + string(uid)})
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("listing the restore's data downloads: %w", err)
	}
	if len(list.Items) > 0 {
		return errors.New("this restore moves volumes with CSI data movement, which kip does not cancel. Wait for it to finish")
	}
	return nil
}

// waitWhileCanceling repeatedly requests cancellation of unfinished volume
// restores, including newly created ones. It returns done when the restore has
// ended and no volume restores are running. When limit expires, it returns the
// last observed phase, false and nil. Parent context cancellation, API errors,
// unsupported volume restores and replacement of the named restore return errors.
func waitWhileCanceling(ctx context.Context, dyn dynamic.Interface, name string, uid types.UID, limit time.Duration) (phase string, done bool, err error) {
	wait, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	// Expiry allows a restart attempt, while interruption must stop cancellation.
	expired := func(err error) bool {
		return wait.Err() != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded)
	}
	for {
		if err := refuseDataDownloads(wait, dyn, uid); err != nil {
			if expired(err) {
				return phase, false, nil
			}
			return "", false, err
		}
		running, err := cancelVolumeRestores(wait, dyn, uid)
		if err != nil {
			if expired(err) {
				return phase, false, nil
			}
			return "", false, err
		}
		r, err := dyn.Resource(restoreGVR).Namespace("velero").Get(wait, name, metav1.GetOptions{})
		if err != nil {
			if expired(err) {
				return phase, false, nil
			}
			return "", false, fmt.Errorf("reading restore %s: %w", name, err)
		}
		if r.GetUID() != uid {
			return "", false, fmt.Errorf("restore %s was replaced by another restore of the same name", name)
		}
		phase, _, _ = unstructured.NestedString(r.Object, "status", "phase")
		if restoreFinished(r) && running == 0 {
			return phase, true, nil
		}
		select {
		case <-ctx.Done():
			return phase, false, ctx.Err()
		case <-wait.Done():
			return phase, false, nil
		case <-time.After(restoreCancelPoll):
		}
	}
}

// cancelVolumeRestores requests cancellation of unfinished PodVolumeRestores
// belonging to uid and returns their count before the requests. It returns an
// error on encountering a restic restore because restic ignores cancellation.
// Missing or conflicting objects are left for the next poll.
func cancelVolumeRestores(ctx context.Context, dyn dynamic.Interface, uid types.UID) (int, error) {
	pvrs := dyn.Resource(podVolumeRestoreGVR).Namespace("velero")
	list, err := pvrs.List(ctx, metav1.ListOptions{LabelSelector: "velero.io/restore-uid=" + string(uid)})
	if err != nil {
		return 0, fmt.Errorf("listing the restore's volume restores: %w", err)
	}
	running := 0
	for i := range list.Items {
		pvr := &list.Items[i]
		switch phase, _, _ := unstructured.NestedString(pvr.Object, "status", "phase"); phase {
		case "Completed", "Failed", "Canceled":
			continue
		}
		if uploader, _, _ := unstructured.NestedString(pvr.Object, "spec", "uploaderType"); uploader == "restic" {
			return 0, fmt.Errorf("volume restore %s uses restic, which Velero cannot cancel. Wait for it to finish", pvr.GetName())
		}
		running++
		if wanted, _, _ := unstructured.NestedBool(pvr.Object, "spec", "cancel"); wanted {
			continue
		}
		// The UID makes the patch fail on a replacement of the same name.
		patch := []byte(fmt.Sprintf(`{"metadata":{"uid":%q},"spec":{"cancel":true}}`, pvr.GetUID()))
		if _, err := pvrs.Patch(ctx, pvr.GetName(), types.MergePatchType, patch, metav1.PatchOptions{}); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
			return 0, fmt.Errorf("canceling volume restore %s: %w", pvr.GetName(), err)
		}
	}
	return running, nil
}

// refuseCollateralRestart requires the target UID to remain InProgress before
// restarting Velero. It names other InProgress backups and restores that the
// restart would fail and refuses the restart unless force is set. Work that
// starts after these checks can still be affected.
func refuseCollateralRestart(ctx context.Context, dyn dynamic.Interface, name string, target types.UID, force bool) error {
	restores, err := dyn.Resource(restoreGVR).Namespace("velero").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing restores: %w", err)
	}
	var running []string
	targetPhase := ""
	for _, r := range restores.Items {
		phase, _, _ := unstructured.NestedString(r.Object, "status", "phase")
		switch {
		case r.GetUID() == target:
			targetPhase = phaseOrNew(phase)
		case phase == "InProgress":
			running = append(running, "restore "+r.GetName())
		}
	}
	if targetPhase != "InProgress" {
		if targetPhase == "" {
			return fmt.Errorf("restore %s is gone, so Velero is not restarted", name)
		}
		return fmt.Errorf("restore %s is %s now, so Velero is not restarted. Run kip backup restore --cancel %s again if its volume restores keep running", name, targetPhase, name)
	}
	backups, err := dyn.Resource(backupGVR).Namespace("velero").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing backups: %w", err)
	}
	for _, b := range backups.Items {
		if phase, _, _ := unstructured.NestedString(b.Object, "status", "phase"); phase == "InProgress" {
			running = append(running, "backup "+b.GetName())
		}
	}
	if len(running) == 0 {
		return nil
	}
	if !force {
		return fmt.Errorf("restarting Velero would also fail %s. Wait for them to finish, or pass --force to restart anyway", strings.Join(running, ", "))
	}
	fmt.Printf("  ⚠  Restarting Velero also fails %s\n", strings.Join(running, ", "))
	return nil
}

// restartVeleroServer scales the velero Deployment to zero and waits for its
// pods to disappear so the old server stops processing the restore. It restores
// the original replica count and waits for readiness. If scaling or shutdown
// fails, it attempts to restore the replica count even if ctx was canceled,
// and reports any recovery error alongside the original error.
func restartVeleroServer(ctx context.Context, clientset kubernetes.Interface) (err error) {
	deployments := clientset.AppsV1().Deployments("velero")
	readCtx, cancel := context.WithTimeout(ctx, veleroScaleTimeout)
	scale, err := deployments.GetScale(readCtx, "velero", metav1.GetOptions{})
	cancel()
	if err != nil {
		return fmt.Errorf("reading the velero Deployment: %w", err)
	}
	replicas := scale.Spec.Replicas
	if replicas == 0 {
		return errors.New("the velero Deployment is scaled to zero, so there is no running server to restart")
	}
	scaledUp := false
	defer func() {
		if scaledUp {
			return
		}
		// Recovery needs its own context because the caller may have canceled ctx.
		if upErr := setVeleroReplicas(context.Background(), deployments, replicas); upErr != nil {
			err = fmt.Errorf("%w. Scaling Velero back up also failed (%v), so backups are stopped until kip upgrade restores it", err, upErr)
		}
	}()
	if err := setVeleroReplicas(ctx, deployments, 0); err != nil {
		return err
	}
	if err := waitForVeleroPods(ctx, clientset, func(_ *appsv1.Deployment, pods int) bool { return pods == 0 }); err != nil {
		return fmt.Errorf("waiting for the old Velero server to stop: %w", err)
	}
	if err := setVeleroReplicas(ctx, deployments, replicas); err != nil {
		return err
	}
	scaledUp = true
	if err := waitForVeleroPods(ctx, clientset, func(d *appsv1.Deployment, _ int) bool {
		return d.Status.ObservedGeneration >= d.Generation && d.Status.ReadyReplicas >= replicas && d.Status.UpdatedReplicas >= replicas
	}); err != nil {
		return fmt.Errorf("waiting for Velero to start again: %w", err)
	}
	return nil
}

// setVeleroReplicas sets the velero Deployment's replica count, bounded by
// veleroScaleTimeout so a stalled API call cannot hang the restart.
func setVeleroReplicas(ctx context.Context, deployments typedappsv1.DeploymentInterface, replicas int32) error {
	ctx, cancel := context.WithTimeout(ctx, veleroScaleTimeout)
	defer cancel()
	scale, err := deployments.GetScale(ctx, "velero", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading the velero Deployment: %w", err)
	}
	scale.Spec.Replicas = replicas
	if _, err := deployments.UpdateScale(ctx, "velero", scale, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("scaling the velero Deployment to %d: %w", replicas, err)
	}
	return nil
}

// waitForVeleroPods polls the velero Deployment and its matching pods, including
// terminating pods, until ready returns true. API errors, context cancellation
// and expiry of veleroRestartWait return errors.
func waitForVeleroPods(ctx context.Context, clientset kubernetes.Interface, ready func(*appsv1.Deployment, int) bool) error {
	ctx, cancel := context.WithTimeout(ctx, veleroRestartWait)
	defer cancel()
	for {
		d, err := clientset.AppsV1().Deployments("velero").Get(ctx, "velero", metav1.GetOptions{})
		if err != nil {
			return err
		}
		selector, err := metav1.LabelSelectorAsSelector(d.Spec.Selector)
		if err != nil {
			return err
		}
		pods, err := clientset.CoreV1().Pods("velero").List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return err
		}
		if ready(d, len(pods.Items)) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(restoreCancelPoll):
		}
	}
}

func reportCanceledRestore(name, phase string, restarted bool) error {
	how := "its volume restores were canceled"
	if restarted {
		how = "Velero was restarted"
	}
	fmt.Printf("\n  ✔  Restore %s ended with phase %s after %s. None of its volume restores is running.\n", name, phase, how)
	fmt.Printf("     Objects and volume data restored so far remain in place and may be incomplete.\n")
	fmt.Printf("     If it was a drill, remove the drill namespace with kip backup drill <namespace> --clean.\n\n")
	return nil
}
