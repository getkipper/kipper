package migration

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// dataTransferDeadline bounds one CR-driven transfer end to end. Transfers
// resume chunk-by-chunk across retries, so this is a stuck-transfer
// backstop, not a size cap.
const dataTransferDeadline = 12 * time.Hour

var transferSlugRe = regexp.MustCompile(`[^a-z0-9-]`)

// transferID builds the deterministic, DNS-safe identifier shared by both
// clusters for one transfer. The readable slug may truncate, so a hash of
// the full identity keeps distinct items from ever colliding. Pod names
// derived from the ID append at most 10 characters, so 48 keeps headroom
// under the 63-character label limit.
func transferID(sessionID, namespace, name string) string {
	sum := sha256.Sum256([]byte(sessionID + "\x00" + namespace + "\x00" + name))
	suffix := hex.EncodeToString(sum[:4])
	slug := transferSlugRe.ReplaceAllString(strings.ToLower(namespace+"-"+name), "-")
	if len(slug) > 39 {
		slug = slug[:39]
	}
	return strings.Trim(slug, "-") + "-" + suffix
}

// runDataTransfer drives one CR-driven data transfer to completion: it
// stands up the receiver on the target, provisions the per-transfer token,
// creates the DataTransfer CR for the reconciler, and mirrors CR progress
// into the session step until the transfer settles. On success all transfer
// resources on both clusters are cleaned up; on failure they stay for
// inspection and the reaper's lease collects the target side.
func (h *Handler) runDataTransfer(ctx context.Context, session *Session, token *Token, dt *kipperv1.DataTransfer, stepName string) error {
	session.AddStep(Step{Name: stepName, Phase: "data", Status: StepRunning})

	fail := func(err error) error {
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepFailed
			s.Error = err.Error()
		})
		// Tear down both sides on failure instead of waiting for the target
		// lease: this restarts any paused target service and reaps the
		// receiver promptly. Runs on a fresh context so a cancelled parent
		// does not skip cleanup.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		h.cleanupDataTransfer(cleanupCtx, session, token, dt, false)
		return err
	}

	// Receiver first: the export mover's very first request must land on a
	// listening import side.
	receiverReq := map[string]interface{}{
		"transferID": dt.Name,
		"namespace":  dt.Namespace,
		"kind":       dt.Spec.Kind,
		"target": map[string]string{
			"volume":  dt.Spec.Target.Volume,
			"service": dt.Spec.Target.Service,
		},
	}
	ensureReceiver := func() error {
		return h.sendToTarget(token, fmt.Sprintf("/api/v1/migrate-target/%s/transfer", session.ID), receiverReq)
	}
	// Repairing a receiver that died is a different operation from standing one
	// up: it only replaces a finished import pod and never touches the Service,
	// the destination service, or anything else the setup owns.
	repairReceiver := func() error {
		return h.sendToTarget(token, fmt.Sprintf("/api/v1/migrate-target/%s/transfer/%s/ensure", session.ID, dt.Name), nil)
	}
	if err := ensureReceiver(); err != nil {
		return fail(fmt.Errorf("creating receiver on target: %w", err))
	}

	derived, err := deriveTransferToken(token.Secret, dt.Name)
	if err != nil {
		return fail(err)
	}

	// Gate on the receiver actually answering through the full ingest path
	// before the exporter launches: a fresh cluster pulls the datamover
	// image on first use, and burning transfer attempts on that pull would
	// fail healthy first migrations.
	if err := h.waitReceiverReady(ctx, dt, derived, 5*time.Minute); err != nil {
		return fail(fmt.Errorf("receiver for %s never became ready: %w", dt.Name, err))
	}
	tokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dt-" + dt.Name + "-token",
			Namespace: dt.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				"kipper.run/resource-type":     "datatransfer-token",
				transferIDLabel:                dt.Name,
			},
		},
		StringData: map[string]string{"token": derived},
	}
	if _, err := h.Client.CoreV1().Secrets(dt.Namespace).Create(ctx, tokenSecret, metav1.CreateOptions{}); err != nil && !errors.IsAlreadyExists(err) {
		return fail(fmt.Errorf("creating transfer token secret: %w", err))
	}

	if err := h.CRClient.Create(ctx, dt); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fail(fmt.Errorf("creating DataTransfer: %w", err))
		}
	}

	lastAttempt := int32(0)
	deadline := time.NewTimer(dataTransferDeadline)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-deadline.C:
			return fail(fmt.Errorf("transfer %s exceeded the %s deadline", dt.Name, dataTransferDeadline))
		case <-tick.C:
		}

		var current kipperv1.DataTransfer
		if err := h.CRClient.Get(ctx, types.NamespacedName{Name: dt.Name, Namespace: dt.Namespace}, &current); err != nil {
			continue
		}

		// The controller retries by relaunching the exporter, but the importer
		// lives on the target and is not part of that. If it was evicted or
		// OOM-killed, every remaining attempt would upload to a dead backend
		// and the transfer would burn its retries without moving a byte. So
		// each new attempt re-ensures the receiver first; the target treats it
		// as idempotent and only replaces a pod that has actually finished.
		// lastAttempt only advances once the repair has actually succeeded. A
		// target that is briefly unreachable would otherwise consume the one
		// chance this attempt had, and the exporter would relaunch against the
		// dead importer anyway; retrying on the next poll costs nothing and the
		// controller waits before recreating the exporter.
		if current.Status.Attempt > lastAttempt {
			if err := repairReceiver(); err == nil {
				lastAttempt = current.Status.Attempt
			}
		}

		session.UpdateStep(stepName, func(s *Step) {
			s.BytesDone = current.Status.BytesDone
			if current.Status.TotalChunks > 0 {
				s.Detail = fmt.Sprintf("%d/%d chunks", current.Status.CompletedChunks, current.Status.TotalChunks)
				if current.Status.Attempt > 1 {
					s.Detail += fmt.Sprintf(" (attempt %d)", current.Status.Attempt)
				}
			}
		})

		switch current.Status.Phase {
		case "Completed":
			session.UpdateStep(stepName, func(s *Step) {
				s.Status = StepCompleted
				s.BytesDone = current.Status.BytesDone
				now := time.Now()
				s.CompletedAt = &now
			})
			h.cleanupDataTransfer(ctx, session, token, &current, true)
			return nil
		case "Failed":
			return fail(fmt.Errorf("transfer %s failed: %s", dt.Name, current.Status.LastError))
		}
	}
}

// SweepAbandonedTransfers restores source-side state for DataTransfer CRs
// whose owning migration session is no longer running — the durable
// replacement for in-process defers lost to a console-api restart. It runs
// at start and periodically: paused source services scale back up, and the
// orphaned CR and its token secret are removed.
func (h *Handler) SweepAbandonedTransfers(ctx context.Context, interval time.Duration) {
	h.sweepAbandonedTransfersOnce(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h.sweepAbandonedTransfersOnce(ctx)
		}
	}
}

func (h *Handler) sweepAbandonedTransfersOnce(ctx context.Context) {
	var transfers kipperv1.DataTransferList
	if err := h.CRClient.List(ctx, &transfers); err != nil {
		return
	}
	for i := range transfers.Items {
		dt := &transfers.Items[i]
		session, ok := h.Sessions.Get(dt.Spec.SessionID)
		if st := session.CurrentStatus(); ok && (st == SessionRunning || st == SessionVerifying) {
			continue
		}
		// Tear down the target receiver too when the session is still known,
		// so its paused service restarts now rather than on the 24h lease.
		// Without the session secret only the source can be recovered; the
		// target then falls back to its own lease reaper.
		if ok && session.TargetAPI != "" && session.Secret != "" {
			token := &Token{Endpoint: session.TargetAPI, Secret: session.Secret}
			dctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = h.sendToTargetDeleteCtx(dctx, token, fmt.Sprintf("/api/v1/migrate-target/%s/transfer/%s", dt.Spec.SessionID, dt.Name))
			cancel()
		}
		if dt.Spec.Kind == "servicePVC" && dt.Spec.Source.Service != "" {
			_ = h.restoreStatefulSet(ctx, dt.Namespace, dt.Spec.Source.Service, dt.Spec.SourceReplicas)
		}
		_ = h.CRClient.Delete(ctx, dt)
		_ = h.Client.CoreV1().Secrets(dt.Namespace).Delete(ctx, "dt-"+dt.Name+"-token", metav1.DeleteOptions{})
	}
}

// waitReceiverReady polls the transfer's ingest progress endpoint through
// the target's proxy until the import mover answers, proving the whole
// chain (route, service, pod, token) before any data is sent.
func (h *Handler) waitReceiverReady(ctx context.Context, dt *kipperv1.DataTransfer, transferToken string, budget time.Duration) error {
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: refuseRedirect,
	}
	deadline := time.Now().Add(budget)
	url := dt.Spec.TargetBaseURL + "/kipper-transfer/" + dt.Name + "/progress"
	var lastErr error
	for time.Now().Before(deadline) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+transferToken)
		resp, err := httpClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("ingest answered %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return lastErr
}

// sendToTargetDeleteCtx issues an authenticated DELETE bound to ctx, so a
// teardown deadline actually caps the call rather than the client's timeout.
func (h *Handler) sendToTargetDeleteCtx(ctx context.Context, token *Token, path string) error {
	status, body, err := h.callTargetRawCtx(ctx, token, "DELETE", path, nil)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("target returned %d: %s", status, string(body))
	}
	return nil
}

// cleanupDataTransfer removes a completed transfer's resources on both
// clusters. Failures here are logged into the step detail at most; a
// leftover receiver is reaped by its lease.
// cleanupDataTransfer tears down one transfer on both clusters. transferred
// says the transfer finished and verified, which is the only state in which the
// destination is known whole.
func (h *Handler) cleanupDataTransfer(ctx context.Context, session *Session, token *Token, dt *kipperv1.DataTransfer, completed bool) {
	// The importer applies files to the live destination in place, so an
	// interrupted commit leaves a tree that is part source and part target.
	// Whether it was interrupted cannot be observed reliably from here: the
	// source only samples the importer's phase over HTTP every few seconds, and
	// a pod can begin and abandon finalization inside one interval. So the
	// service is restarted only on a positively completed transfer, and every
	// other outcome leaves it stopped for an operator to judge.
	path := fmt.Sprintf("/api/v1/migrate-target/%s/transfer/%s", session.ID, dt.Name)
	if completed {
		path += "?restoreService=true"
	}
	_ = h.sendToTargetDeleteCtx(ctx, token, path)
	_ = h.CRClient.Delete(ctx, dt)
	_ = h.Client.CoreV1().Secrets(dt.Namespace).Delete(ctx, "dt-"+dt.Name+"-token", metav1.DeleteOptions{})
}

// scaleStatefulSet sets a statefulset's replicas and, when scaling down,
// waits for its pods to terminate so the PVC is safe to mount elsewhere.
// statefulSetReplicas returns a statefulset's current desired replicas, or 1
// when it is missing or unset, so a pause can be undone to the same size.
func (h *Handler) statefulSetReplicas(ctx context.Context, namespace, name string) int32 {
	sts, err := h.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil || sts.Spec.Replicas == nil || *sts.Spec.Replicas < 1 {
		return 1
	}
	return *sts.Spec.Replicas
}

// restoreStatefulSet scales a paused statefulset back to its recorded size,
// treating a zero/unknown record as one replica.
func (h *Handler) restoreStatefulSet(ctx context.Context, namespace, name string, replicas int32) error {
	if replicas < 1 {
		replicas = 1
	}
	return h.scaleStatefulSet(ctx, namespace, name, replicas)
}

func (h *Handler) scaleStatefulSet(ctx context.Context, namespace, name string, replicas int32) error {
	sts, err := h.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if sts.Spec.Replicas != nil && *sts.Spec.Replicas == replicas {
		if replicas > 0 {
			return nil
		}
	} else {
		sts.Spec.Replicas = &replicas
		if _, err := h.Client.AppsV1().StatefulSets(namespace).Update(ctx, sts, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	if replicas > 0 {
		return nil
	}
	for i := 0; i < 60; i++ {
		pods, err := h.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "app=" + name,
		})
		if err == nil && len(pods.Items) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("pods of statefulset %s/%s still running after scale to zero", namespace, name)
}

// listCRVolumes returns the namespace's Volume CRs.
func (h *Handler) listCRVolumes(ctx context.Context, namespace string) ([]kipperv1.Volume, error) {
	var volumeList kipperv1.VolumeList
	if err := h.CRClient.List(ctx, &volumeList, crclient.InNamespace(namespace)); err != nil {
		return nil, err
	}
	return volumeList.Items, nil
}
