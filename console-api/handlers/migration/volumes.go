package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// migrateVolumes exports Volume CRs and their data from the source cluster
// and transfers them to the target.
func (h *Handler) migrateVolumes(ctx context.Context, session *Session, token *Token, namespace string) error {
	var volumeList kipperv1.VolumeList
	if err := h.CRClient.List(ctx, &volumeList, crclient.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing volumes: %w", err)
	}

	if len(volumeList.Items) == 0 {
		return nil
	}

	for _, vol := range volumeList.Items {
		if session.IsCancelled() {
			return fmt.Errorf("migration cancelled")
		}

		// Create Volume CR on target
		stepName := fmt.Sprintf("Creating volume %s/%s on target", namespace, vol.Name)
		session.AddStep(Step{
			Name:   stepName,
			Phase:  "data",
			Status: StepRunning,
		})

		specJSON, _ := json.Marshal(vol.Spec)
		var specMap map[string]interface{}
		_ = json.Unmarshal(specJSON, &specMap)

		if err := h.sendToTarget(token, fmt.Sprintf("/api/v1/migrate-target/%s/resource", session.ID), map[string]interface{}{
			"kind":      "Volume",
			"name":      vol.Name,
			"namespace": namespace,
			"spec":      specMap,
		}); err != nil {
			session.UpdateStep(stepName, func(s *Step) {
				s.Status = StepFailed
				s.Error = err.Error()
			})
			return fmt.Errorf("creating volume %s on target: %w", vol.Name, err)
		}

		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepCompleted
			now := time.Now()
			s.CompletedAt = &now
		})

		// Transfer volume data
		if err := h.transferVolumeData(ctx, session, token, namespace, &vol); err != nil {
			return err
		}
	}

	return nil
}

// transferVolumeData moves one shared volume's contents through the
// CR-driven chunked transfer: a DataTransfer CR drives an export mover
// against the PVC, the target's import mover verifies and commits, and the
// session step mirrors the CR's progress. There is no size cap; a failure
// retries from the last completed chunk.
func (h *Handler) transferVolumeData(ctx context.Context, session *Session, token *Token, namespace string, vol *kipperv1.Volume) error {
	dt := &kipperv1.DataTransfer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      transferID(session.ID, namespace, vol.Name),
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				"kipper.run/migration-session": session.ID,
			},
		},
		Spec: kipperv1.DataTransferSpec{
			SessionID:     session.ID,
			Kind:          "volume",
			Source:        kipperv1.DataTransferEndpoint{Volume: vol.Name},
			Target:        kipperv1.DataTransferEndpoint{Volume: vol.Name},
			TargetBaseURL: fmt.Sprintf("%s/api/v1/migrate-target/%s/transfer/%s", token.Endpoint, session.ID, transferID(session.ID, namespace, vol.Name)),
		},
	}
	stepName := fmt.Sprintf("Transferring volume data (%s)", vol.Name)
	if err := h.runDataTransfer(ctx, session, token, dt, stepName); err != nil {
		return fmt.Errorf("transferring volume data: %w", err)
	}
	return nil
}

// transferServicePVCData moves a stateful service's PVC bytes through the
// CR-driven transfer while the statefulset is scaled to zero on both
// clusters. Moving the native on-disk layout keeps engines like minio
// byte-identical without speaking their object protocol.
func (h *Handler) transferServicePVCData(ctx context.Context, session *Session, token *Token, namespace string, svc *kipperv1.Service) error {
	stepName := fmt.Sprintf("Transferring service data (%s)", svc.Name)

	// Record the running size before pausing, so a restart on any path
	// (defer, sweep, or lease reaper) restores the same replica count.
	sourceReplicas := h.statefulSetReplicas(ctx, namespace, svc.Name)
	if err := h.scaleStatefulSet(ctx, namespace, svc.Name, 0); err != nil {
		session.AddStep(Step{Name: stepName, Phase: "data", Status: StepFailed, Error: fmt.Sprintf("stopping service for transfer: %v", err)})
		return fmt.Errorf("stopping service %s for transfer: %w", svc.Name, err)
	}
	// The source service comes back up whatever happens to the transfer.
	defer func() {
		_ = h.restoreStatefulSet(context.WithoutCancel(ctx), namespace, svc.Name, sourceReplicas)
	}()

	id := transferID(session.ID, namespace, "svc-"+svc.Name)
	dt := &kipperv1.DataTransfer{
		ObjectMeta: metav1.ObjectMeta{
			Name:      id,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				"kipper.run/migration-session": session.ID,
			},
		},
		Spec: kipperv1.DataTransferSpec{
			SessionID:      session.ID,
			Kind:           "servicePVC",
			Source:         kipperv1.DataTransferEndpoint{Service: svc.Name},
			Target:         kipperv1.DataTransferEndpoint{Service: svc.Name},
			SourceReplicas: sourceReplicas,
			TargetBaseURL:  fmt.Sprintf("%s/api/v1/migrate-target/%s/transfer/%s", token.Endpoint, session.ID, id),
		},
	}
	if err := h.runDataTransfer(ctx, session, token, dt, stepName); err != nil {
		return fmt.Errorf("transferring service data: %w", err)
	}
	return nil
}

// createVolume applies a Volume CR on the target with create-or-update
// semantics, so a retried migration converges instead of failing on
// AlreadyExists.
func (h *Handler) createVolume(ctx context.Context, name, namespace string, spec map[string]interface{}) error {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return err
	}

	var volumeSpec kipperv1.VolumeSpec
	if err := json.Unmarshal(specJSON, &volumeSpec); err != nil {
		return err
	}

	volume := &kipperv1.Volume{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, h.CRClient, volume, func() error {
		setLabel(&volume.ObjectMeta, "app.kubernetes.io/managed-by", "kipper")
		volume.Spec = volumeSpec
		return nil
	})
	return err
}
