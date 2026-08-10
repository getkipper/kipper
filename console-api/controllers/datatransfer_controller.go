package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

const (
	dataTransferFinalizer = "kipper.run/datatransfer-cleanup"

	// progressPollInterval paces status mirroring while a transfer runs.
	progressPollInterval = 5 * time.Second
)

// DataTransferReconciler drives one migration data transfer: it runs an
// export mover pod against the transfer's source, mirrors the mover's
// progress into the CR status, retries failed attempts, and cleans up
// source-side resources. Target-side resources belong to the target
// cluster's console-api and are never touched from here.
type DataTransferReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// APIReader reads mover pods straight from the API server: the
	// manager's pod cache is restricted to the monitoring namespace, so a
	// cached read would never observe pods in project namespaces.
	APIReader client.Reader

	// DatamoverImage is the kipper-datamover image reference, pinned per
	// release so both ends of a session speak the same wire protocol.
	DatamoverImage string

	// HTTPClient polls mover progress endpoints. Swappable for tests.
	HTTPClient *http.Client
}

// moverProgress mirrors the datamover ingest /progress payload.
type moverProgress struct {
	BytesDone   int64  `json:"bytesDone"`
	TotalBytes  int64  `json:"totalBytes"`
	ChunksDone  int64  `json:"chunksDone"`
	TotalChunks int64  `json:"totalChunks"`
	Phase       string `json:"phase"`
}

func (r *DataTransferReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var dt kipperv1.DataTransfer
	if err := r.Get(ctx, req.NamespacedName, &dt); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	if !dt.DeletionTimestamp.IsZero() {
		if err := r.cleanupSource(ctx, &dt); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning up transfer resources: %w", err)
		}
		controllerutil.RemoveFinalizer(&dt, dataTransferFinalizer)
		return ctrl.Result{}, r.Update(ctx, &dt)
	}

	if !controllerutil.ContainsFinalizer(&dt, dataTransferFinalizer) {
		controllerutil.AddFinalizer(&dt, dataTransferFinalizer)
		if err := r.Update(ctx, &dt); err != nil {
			return ctrl.Result{}, err
		}
	}

	switch dt.Status.Phase {
	case "Completed", "Failed":
		return ctrl.Result{}, nil
	}

	// The orchestrator provisions the per-transfer token Secret alongside
	// the CR; until it exists the mover cannot authenticate.
	var tokenSecret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Name: tokenSecretName(&dt), Namespace: dt.Namespace}, &tokenSecret); err != nil {
		if errors.IsNotFound(err) {
			return r.setPhase(ctx, &dt, "Pending", "waiting for transfer token secret")
		}
		return ctrl.Result{}, err
	}

	pod, err := r.ensureMoverPod(ctx, &dt)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring mover pod: %w", err)
	}
	transferToken := string(tokenSecret.Data["token"])

	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return r.completeAttempt(ctx, &dt, transferToken)
	case corev1.PodFailed:
		return r.retryOrFail(ctx, &dt, pod)
	case corev1.PodRunning:
		r.mirrorProgress(ctx, &dt, transferToken)
		if dt.Status.Phase != "Transferring" || dt.Status.Attempt == 0 {
			dt.Status.Phase = "Transferring"
			if dt.Status.Attempt == 0 {
				dt.Status.Attempt = 1
			}
		}
		if err := r.Status().Update(ctx, &dt); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: progressPollInterval}, nil
	default:
		// Pending / ContainerCreating: check back shortly.
		logger.V(1).Info("mover pod not running yet", "pod", pod.Name, "phase", pod.Status.Phase)
		return ctrl.Result{RequeueAfter: progressPollInterval}, nil
	}
}

// completeAttempt records a successful mover run. The mover only exits 0
// after the target's finalize report matched the manifest, so success here
// means verified-on-target.
func (r *DataTransferReconciler) completeAttempt(ctx context.Context, dt *kipperv1.DataTransfer, transferToken string) (ctrl.Result, error) {
	r.mirrorProgress(ctx, dt, transferToken)
	now := metav1.Now()
	dt.Status.Phase = "Completed"
	dt.Status.LastSyncedAt = &now
	dt.Status.LastError = ""
	if err := r.Status().Update(ctx, dt); err != nil {
		return ctrl.Result{}, err
	}
	// The pod is kept until CR deletion so its logs stay inspectable; the
	// migration session deletes the CR (and thereby the pod) on completion.
	return ctrl.Result{}, nil
}

// retryOrFail restarts a failed attempt with the completed-chunk state on
// the target providing resume, or marks the transfer Failed once attempts
// are exhausted.
func (r *DataTransferReconciler) retryOrFail(ctx context.Context, dt *kipperv1.DataTransfer, pod *corev1.Pod) (ctrl.Result, error) {
	maxAttempts := dt.Spec.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 3
	}

	reason := podFailureReason(pod)
	if dt.Status.Attempt >= maxAttempts {
		dt.Status.Phase = "Failed"
		dt.Status.LastError = reason
		return ctrl.Result{}, r.Status().Update(ctx, dt)
	}

	if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("deleting failed mover pod: %w", err)
	}
	dt.Status.Attempt++
	dt.Status.LastError = reason
	dt.Status.Phase = "Transferring"
	if err := r.Status().Update(ctx, dt); err != nil {
		return ctrl.Result{}, err
	}
	// Generous spacing between attempts: transient causes like an image
	// pull or a node reshuffle need time, and resume makes retries cheap.
	return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
}

// mirrorProgress copies the receiving side's live counters into the CR
// status, read from the target ingest's authenticated /progress endpoint.
// Poll failures are ignored: progress is cosmetic, completion is decided
// by the pod exit code.
func (r *DataTransferReconciler) mirrorProgress(ctx context.Context, dt *kipperv1.DataTransfer, transferToken string) {
	if transferToken == "" {
		return
	}
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}
	url := dt.Spec.TargetBaseURL + "/kipper-transfer/" + dt.Name + "/progress"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+transferToken)
	resp, err := httpClient.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return
	}
	var p moverProgress
	if err := json.Unmarshal(body, &p); err != nil {
		return
	}
	dt.Status.TotalBytes = p.TotalBytes
	dt.Status.BytesDone = p.BytesDone
	dt.Status.TotalChunks = p.TotalChunks
	dt.Status.CompletedChunks = p.ChunksDone
}

// cleanupSource removes the mover pod. The token Secret is owned by the
// migration session (the orchestrator deletes it with the session); target
// resources are reaped by the target's own lease sweep.
func (r *DataTransferReconciler) cleanupSource(ctx context.Context, dt *kipperv1.DataTransfer) error {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: moverPodName(dt), Namespace: dt.Namespace}}
	if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

func (r *DataTransferReconciler) setPhase(ctx context.Context, dt *kipperv1.DataTransfer, phase, lastError string) (ctrl.Result, error) {
	dt.Status.Phase = phase
	dt.Status.LastError = lastError
	if err := r.Status().Update(ctx, dt); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: progressPollInterval}, nil
}

func podFailureReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Terminated != nil && cs.State.Terminated.ExitCode != 0 {
			msg := cs.State.Terminated.Message
			if msg == "" {
				msg = cs.State.Terminated.Reason
			}
			return fmt.Sprintf("mover exited %d: %s", cs.State.Terminated.ExitCode, msg)
		}
	}
	return "mover pod failed"
}

func moverPodName(dt *kipperv1.DataTransfer) string {
	return "dt-" + dt.Name + "-export"
}

func tokenSecretName(dt *kipperv1.DataTransfer) string {
	return "dt-" + dt.Name + "-token"
}

func (r *DataTransferReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kipperv1.DataTransfer{}).
		Complete(r)
}
