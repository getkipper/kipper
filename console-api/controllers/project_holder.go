package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

const (
	// sharedUploadsClaim is the claim of a project's shared storage in each
	// environment namespace.
	sharedUploadsClaim = "shared-uploads"

	// projectHolderName differs from every Volume holder's name, because a
	// Volume named uploads has a claim of the same name.
	projectHolderName = "shared-uploads-holder"
)

func projectHolderTarget(project *kipperv1.Project, ns string) holderTarget {
	return holderTarget{
		owner: project, kind: "Project", namespace: ns, pod: projectHolderName, claim: sharedUploadsClaim,
		labels: map[string]string{
			kipperLabel:         kipperValue,
			labels.ResourceType: labels.ResourceTypeVolumeHolder,
			labels.Project:      project.Name,
		},
		claimOwned: func(pvc *corev1.PersistentVolumeClaim) (bool, string) { return projectClaimProvenance(pvc, project) },
	}
}

// projectClaimProvenance reports whether pvc is the shared storage claim the
// project controller created for project. It requires the project's labels and
// no controller reference, and returns a reason on failure.
func projectClaimProvenance(pvc *corev1.PersistentVolumeClaim, project *kipperv1.Project) (bool, string) {
	if ref := metav1.GetControllerOf(pvc); ref != nil {
		return false, fmt.Sprintf("is controlled by %s %q, not by this project", ref.Kind, ref.Name)
	}
	l := pvc.Labels
	if l[kipperLabel] != kipperValue || l[labels.ResourceType] != labels.ResourceTypeSharedVolume ||
		l[labels.Project] != project.Name || l[labels.VolumeName] != "" {
		return false, "was not created by Kipper as this project's shared storage"
	}
	return true, ""
}

// projectForHolder maps a project holder to its Project. Volume holders carry
// a volume name and belong to their Volume instead.
func projectForHolder(_ context.Context, obj client.Object) []reconcile.Request {
	l := obj.GetLabels()
	if l[kipperLabel] != kipperValue || l[labels.ResourceType] != labels.ResourceTypeVolumeHolder ||
		l[labels.VolumeName] != "" || l[labels.Project] == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: l[labels.Project]}}}
}

// envHolder is one environment's BackupHolderReady condition.
type envHolder struct {
	env  string
	cond metav1.Condition
}

// reconcileProjectHolder maintains the holder of the shared storage claim in
// ns while backups are configured, including for claims retained after shared
// storage is disabled. It returns the holder condition and retry delay. A
// missing or deleting claim, or disabled backups, removes the holder and
// returns a nil condition. Removing the mount lets claim deletion finish.
func (r *ProjectReconciler) reconcileProjectHolder(ctx context.Context, project *kipperv1.Project, env, ns string, backups bool) (*envHolder, time.Duration, error) {
	ops := holderOps{client: r.Client, reader: r.apiReader(), scheme: r.Scheme}
	t := projectHolderTarget(project, ns)
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Namespace: ns, Name: sharedUploadsClaim}, &pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, 0, ops.remove(ctx, t)
		}
		return nil, 0, err
	}
	if !backups || !pvc.DeletionTimestamp.IsZero() {
		return nil, 0, ops.remove(ctx, t)
	}
	if owned, _ := projectClaimProvenance(&pvc, project); !owned && !sharedStorageEnabled(project) {
		// An unrelated claim needs no shared storage warning when the feature is disabled.
		return nil, 0, nil
	}
	var conditions []metav1.Condition
	retry, err := ops.reconcile(ctx, t, &pvc, holderReport{conditions: &conditions, generation: project.Generation})
	if err != nil || len(conditions) == 0 {
		return nil, retry, err
	}
	return &envHolder{env: env, cond: conditions[0]}, retry, nil
}

func sharedStorageEnabled(project *kipperv1.Project) bool {
	return project.Spec.SharedStorage != nil && project.Spec.SharedStorage.Enabled
}

// projectForSharedClaim maps shared-uploads claim events to the labelled Project
// so reconciliation can release the holder promptly during claim deletion.
func projectForSharedClaim(_ context.Context, obj client.Object) []reconcile.Request {
	project := obj.GetLabels()[labels.Project]
	if obj.GetName() != sharedUploadsClaim || project == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: project}}}
}

// setProjectHolderCondition records the environments' holders as one
// BackupHolderReady condition. It is true when every reported holder is ready.
// Otherwise, the message names the first environment with a non-ready holder
// in input order. An empty input removes the condition.
func (r *ProjectReconciler) setProjectHolderCondition(project *kipperv1.Project, envs []envHolder) {
	if len(envs) == 0 {
		apimeta.RemoveStatusCondition(&project.Status.Conditions, conditionBackupHolderReady)
		return
	}
	st := holderReport{conditions: &project.Status.Conditions, generation: project.Generation}
	if r.Recorder != nil {
		st.event = func(reason, message string) {
			r.Recorder.Event(project, corev1.EventTypeWarning, "BackupHolder"+reason, message)
		}
	}
	for _, e := range envs {
		if e.cond.Status != metav1.ConditionTrue {
			st.setFalse(e.cond.Reason, fmt.Sprintf("Environment %s: %s", e.env, e.cond.Message))
			return
		}
	}
	st.setReady()
}

func (r *ProjectReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
