package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// storageMoveWaitInterval is how often a project waiting on a storage move
// checks whether the move has ended, since namespace label changes do not
// trigger reconciliation.
const storageMoveWaitInterval = 30 * time.Second

const reasonStorageMoveUnfinished = "StorageMoveUnfinished"

// holdsStorageMove reports whether a namespace's labels mark an unfinished
// storage move. The kipper-storage-move-namespaces admission policy refuses to
// delete such a namespace.
func holdsStorageMove(labels map[string]string) bool {
	return labels[platform.StorageMoveNamespaceLabel] == "true"
}

// refusedForStorageMove reports whether err is the namespaces policy refusing
// to delete a namespace that holds an unfinished storage move.
func refusedForStorageMove(err error) bool {
	return err != nil && strings.Contains(err.Error(), "ValidatingAdmissionPolicy '"+platform.StorageMoveNamespacesPolicy+"'")
}

// setStorageMoveCondition sets WaitingForStorageMove on project, naming the
// services whose moves prevent deletion of the waiting namespaces. It removes
// the condition when none are waiting and reports whether conditions changed.
// It reads services directly from the API server and propagates read errors.
func (r *ProjectReconciler) setStorageMoveCondition(ctx context.Context, project *kipperv1.Project, waiting []string) (bool, error) {
	if len(waiting) == 0 {
		return apimeta.RemoveStatusCondition(&project.Status.Conditions, kipperv1.ConditionWaitingForStorageMove), nil
	}
	waiting = slices.Sorted(slices.Values(waiting))
	sentences := make([]string, 0, len(waiting))
	for _, ns := range waiting {
		var services kipperv1.ServiceList
		if err := r.APIReader.List(ctx, &services, client.InNamespace(ns)); err != nil {
			return false, fmt.Errorf("reading the services moving in %s: %w", ns, err)
		}
		var moving []string
		for _, s := range services.Items {
			if s.Annotations[platform.StorageMoveAnnotation] != "" {
				moving = append(moving, s.Name)
			}
		}
		slices.Sort(moving)
		switch len(moving) {
		case 0:
			sentences = append(sentences, fmt.Sprintf("Namespace %s waits for a storage move to finish or roll back.", ns))
		case 1:
			sentences = append(sentences, fmt.Sprintf("Namespace %s waits for the storage move of service %s to finish or roll back.", ns, moving[0]))
		default:
			sentences = append(sentences, fmt.Sprintf("Namespace %s waits for the storage moves of services %s to finish or roll back.", ns, strings.Join(moving, ", ")))
		}
	}
	return apimeta.SetStatusCondition(&project.Status.Conditions, metav1.Condition{
		Type:               kipperv1.ConditionWaitingForStorageMove,
		Status:             metav1.ConditionTrue,
		Reason:             reasonStorageMoveUnfinished,
		Message:            strings.Join(sentences, " "),
		ObservedGeneration: project.Generation,
	}), nil
}
