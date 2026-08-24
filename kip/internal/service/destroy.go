package service

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/getkipper/kipper/controller/pkg/datavolume"
)

// destroyDataTimeout and destroyDataInterval bound the wait for the workload to
// go. Deletion is asynchronous: the Service CR goes first and its StatefulSet
// follows through the owner reference, which takes as long as garbage collection
// takes.
const (
	destroyDataTimeout  = 90 * time.Second
	destroyDataInterval = 2 * time.Second
)

// DataVolumes are the volumes a service keeps its data on, as they stood when
// they were read.
type DataVolumes []corev1.PersistentVolumeClaim

// DataVolumes finds the volumes the service's StatefulSet created.
//
// Read them before deleting anything. A name belongs to nobody once its service
// is gone, so a set read afterwards can describe a service someone has made
// since, holding data nobody asked to lose.
//
// Which claims those are is the datavolume package's to say, because the Service
// finalizer destroys the same ones and the two must not drift on it.
func (m *Manager) DataVolumes(ctx context.Context, namespace, name string) (DataVolumes, error) {
	return m.dataVolumes(ctx, namespace, name, destroyDataTimeout)
}

// DestroyVolumes removes volumes read earlier, once the workload holding them
// has gone.
//
// This is the irreversible half of deleting a service, and it is separate
// because the two do not happen at the same moment. The volume can only go once
// the workload holding it has, so this waits for that and then deletes.
func (m *Manager) DestroyVolumes(ctx context.Context, namespace, name string, volumes DataVolumes) error {
	return m.destroyVolumes(ctx, namespace, name, volumes, destroyDataTimeout, destroyDataInterval)
}

// destroyVolumes is the pollable core, split out so the wait is testable without
// a live cluster.
func (m *Manager) destroyVolumes(ctx context.Context, namespace, name string, volumes DataVolumes, timeout, interval time.Duration) error {
	if err := m.waitForWorkloadGone(ctx, namespace, name, timeout, interval); err != nil {
		return err
	}
	return m.deleteVolumes(ctx, namespace, volumes, timeout, interval)
}

// waitForWorkloadGone blocks until the service's StatefulSet is no longer there.
//
// Deleting the claim first achieves nothing: the StatefulSet controller writes
// one back from its volumeClaimTemplate as soon as it goes, so the operator
// would be left with a fresh empty volume and the data exactly where it was.
func (m *Manager) waitForWorkloadGone(ctx context.Context, namespace, name string, timeout, interval time.Duration) error {
	waiting, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	for {
		_, err := m.Client.AppsV1().StatefulSets(namespace).Get(waiting, name, metav1.GetOptions{})
		switch {
		case errors.IsNotFound(err):
			return nil
		case waiting.Err() != nil:
			return workloadStillRunning(ctx, name, timeout)
		case err != nil:
			return fmt.Errorf("waiting for the workload of %s to go: %w", name, err)
		}

		select {
		case <-waiting.Done():
			return workloadStillRunning(ctx, name, timeout)
		case <-time.After(interval):
		}
	}
}

// workloadStillRunning tells the operator's own cancellation apart from the
// timeout, so a Ctrl-C does not come back as a report about the workload.
func workloadStillRunning(ctx context.Context, name string, timeout time.Duration) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("the workload of service %s was still running after %s, so its volume was left in place; delete the service again with --delete-data once it has gone", name, timeout)
}

// dataVolumes carries the timeout, so a request the API server accepts and never
// answers is bounded by it the same way the polling is.
func (m *Manager) dataVolumes(ctx context.Context, namespace, name string, timeout time.Duration) (DataVolumes, error) {
	reading, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	claims, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).List(reading, metav1.ListOptions{
		LabelSelector: datavolume.Selector(name),
	})
	if err != nil {
		return nil, fmt.Errorf("listing the volumes of %s: %w", name, err)
	}

	var mine DataVolumes
	for _, claim := range claims.Items {
		if datavolume.Belongs(name, claim.Name) {
			mine = append(mine, claim)
		}
	}
	return mine, nil
}

// deleteVolumes removes the volumes that were read, and only while they are
// still the same volumes.
//
// By the time the workload has gone the claim standing under a given name can
// belong to a service made since. The UID is what tells the two apart, so it is
// checked here and pinned on the delete itself, leaving the API server to refuse
// a claim that changed in between.
func (m *Manager) deleteVolumes(ctx context.Context, namespace string, volumes DataVolumes, timeout, interval time.Duration) error {
	deleting, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	pvcs := m.Client.CoreV1().PersistentVolumeClaims(namespace)
	for _, volume := range volumes {
		uid := volume.UID
		current, err := pvcs.Get(deleting, volume.Name, metav1.GetOptions{})
		switch {
		case errors.IsNotFound(err):
			continue
		case err != nil:
			return fmt.Errorf("reading volume %s: %w", volume.Name, err)
		case current.UID != uid:
			continue
		}

		err = pvcs.Delete(deleting, volume.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &uid},
		})
		switch {
		case err == nil, errors.IsForbidden(err):
			// Forbidden is the ordinary answer for a project's own operators,
			// who may not delete a volume themselves. The service was marked
			// before it went, so the cluster is removing it; what matters is
			// whether it goes, which is what the wait is for. If it does not,
			// the refusal is carried into the message rather than left as a
			// volume that mysteriously stayed.
			if waitErr := m.waitForVolumeGone(deleting, namespace, volume.Name, uid, interval); waitErr != nil {
				if errors.IsForbidden(err) {
					return fmt.Errorf("%w: this command may not delete the volume itself (%s), and nothing else removed it", waitErr, err)
				}
				return waitErr
			}
		case errors.IsNotFound(err):
		case errors.IsConflict(err):
			gone, checkErr := m.volumeIsGone(deleting, namespace, volume.Name, uid)
			if checkErr != nil {
				return checkErr
			}
			if !gone {
				return fmt.Errorf("deleting volume %s: %w", volume.Name, err)
			}
		default:
			return fmt.Errorf("deleting volume %s: %w", volume.Name, err)
		}
	}
	return nil
}

// waitForVolumeGone blocks until the volume that was read has left.
//
// An accepted delete is a request, not an act. A finalizer holds the claim while
// a pod that mounted it is still terminating, and one that never clears holds it
// for good, so the data can still be on disk long after the API server said yes.
func (m *Manager) waitForVolumeGone(ctx context.Context, namespace, name string, uid types.UID, interval time.Duration) error {
	stillThere := fmt.Errorf("the delete of volume %s was accepted but the volume was still there when the command stopped waiting, so the data may still be on disk", name)
	for {
		gone, err := m.volumeIsGone(ctx, namespace, name, uid)
		switch {
		case ctx.Err() != nil:
			return stillThere
		case err != nil:
			return err
		case gone:
			return nil
		}

		select {
		case <-ctx.Done():
			return stillThere
		case <-time.After(interval):
		}
	}
}

// volumeIsGone reports whether the volume that was read has left, whatever now
// stands under its name.
//
// A refused delete usually means the name belongs to something made since, and
// there is nothing of the operator's left to destroy. It can also mean the
// delete simply did not happen, and saying it did would be a lie about data.
func (m *Manager) volumeIsGone(ctx context.Context, namespace, name string, uid types.UID) (bool, error) {
	current, err := m.Client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading volume %s: %w", name, err)
	}
	return current.UID != uid, nil
}
