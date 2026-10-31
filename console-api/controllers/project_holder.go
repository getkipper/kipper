package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"
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

func (e envHolder) message() string {
	return fmt.Sprintf("Environment %s: %s", e.env, e.cond.Message)
}

// reported reports whether the project condition c describes e's holder.
func (e envHolder) reported(c *metav1.Condition) bool {
	return strings.HasPrefix(c.Message, fmt.Sprintf("Environment %s: ", e.env))
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
	// Starting from the environment's recorded condition keeps its restore
	// warning while the replacement holder starts.
	var conditions []metav1.Condition
	if c := environmentHolder(project, env); c != nil {
		conditions = []metav1.Condition{*c}
	}
	st := holderReport{conditions: &conditions, generation: project.Generation}
	st.save = func(ctx context.Context) error {
		if len(conditions) == 0 || !restoreWarning(conditions[0].Reason) {
			return nil
		}
		return r.saveHolderWarning(ctx, project, envHolder{env: env, cond: conditions[0]})
	}
	retry, err := ops.reconcile(ctx, t, &pvc, st)
	if err != nil || len(conditions) == 0 {
		return nil, retry, err
	}
	return &envHolder{env: env, cond: conditions[0]}, retry, nil
}

// saveHolderWarning persists holder's condition before replacing its pod.
// Waiting for the final status write after reconciling all environments risks
// losing the warning if that write fails after the pod is deleted. It also sets
// the condition on project so the final write includes the same warning.
func (r *ProjectReconciler) saveHolderWarning(ctx context.Context, project *kipperv1.Project, holder envHolder) error {
	var live kipperv1.Project
	if err := r.apiReader().Get(ctx, client.ObjectKeyFromObject(project), &live); err != nil {
		return fmt.Errorf("reading the project: %w", err)
	}
	if live.UID != project.UID {
		return errProjectReplaced
	}
	read := live.ResourceVersion
	adoptSummaryWarning(&live)
	r.holderReport(&live).setFalse(holder.cond.Reason, holder.message())
	setEnvironmentHolder(&live, holder)
	if err := r.Status().Update(ctx, &live); err != nil {
		return fmt.Errorf("saving the backup holder warning: %w", err)
	}
	apimeta.SetStatusCondition(&project.Status.Conditions, *apimeta.FindStatusCondition(live.Status.Conditions, conditionBackupHolderReady))
	setEnvironmentHolder(project, holder)
	// If no intervening write changed the project, use the saved version so
	// the final status write carrying this warning avoids a conflict.
	if project.ResourceVersion == read {
		project.ResourceVersion = live.ResourceVersion
	}
	return nil
}

// environmentHolder returns env's recorded BackupHolderReady condition, or nil
// if the environment or condition is absent.
func environmentHolder(project *kipperv1.Project, env string) *metav1.Condition {
	for _, s := range project.Status.Environments {
		if s.Name == env {
			return apimeta.FindStatusCondition(s.Conditions, conditionBackupHolderReady)
		}
	}
	return nil
}

// adoptSummaryWarning copies a legacy project restore warning to the named
// environment if it has no BackupHolderReady condition. Older controllers
// stored warnings only on the project, so this preserves the warning before
// a new one replaces the project condition.
func adoptSummaryWarning(project *kipperv1.Project) {
	summary := apimeta.FindStatusCondition(project.Status.Conditions, conditionBackupHolderReady)
	if summary == nil || !restoreWarning(summary.Reason) {
		return
	}
	for _, env := range ProjectEnvironments(project) {
		e := envHolder{env: env.Name}
		if !e.reported(summary) || environmentHolder(project, env.Name) != nil {
			continue
		}
		c := *summary
		c.Message = strings.TrimPrefix(c.Message, e.message())
		setEnvironmentHolder(project, envHolder{env: env.Name, cond: c})
	}
}

// setEnvironmentHolder records holder's condition on its environment's status,
// adding the environment when it has no status yet.
func setEnvironmentHolder(project *kipperv1.Project, holder envHolder) {
	for i := range project.Status.Environments {
		if s := &project.Status.Environments[i]; s.Name == holder.env {
			apimeta.SetStatusCondition(&s.Conditions, holder.cond)
			return
		}
	}
	project.Status.Environments = append(project.Status.Environments,
		kipperv1.EnvironmentStatus{Name: holder.env, Conditions: []metav1.Condition{holder.cond}})
}

// setEnvironmentHolders records each environment's BackupHolderReady
// condition. It removes that condition from environments absent from envs
// and removes status entries left with no conditions.
func setEnvironmentHolders(project *kipperv1.Project, envs []envHolder) {
	for i := range project.Status.Environments {
		apimeta.RemoveStatusCondition(&project.Status.Environments[i].Conditions, conditionBackupHolderReady)
	}
	for _, e := range envs {
		setEnvironmentHolder(project, e)
	}
	project.Status.Environments = slices.DeleteFunc(project.Status.Environments, func(s kipperv1.EnvironmentStatus) bool {
		return len(s.Conditions) == 0
	})
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
// A recorded restore warning takes priority while that environment's holder
// still reports a restore warning or its replacement is starting. Otherwise,
// the first restore warning takes priority over the first non-ready holder,
// both in input order. An empty input removes the condition.
func (r *ProjectReconciler) setProjectHolderCondition(project *kipperv1.Project, envs []envHolder) {
	if len(envs) == 0 {
		apimeta.RemoveStatusCondition(&project.Status.Conditions, conditionBackupHolderReady)
		return
	}
	st := r.holderReport(project)
	recorded := apimeta.FindStatusCondition(project.Status.Conditions, conditionBackupHolderReady)
	if recorded != nil && restoreWarning(recorded.Reason) {
		for _, e := range envs {
			if e.reported(recorded) && (restoreWarning(e.cond.Reason) || e.cond.Reason == "Starting") {
				st.setFalse(e.cond.Reason, e.message())
				return
			}
		}
	}
	for _, e := range envs {
		if restoreWarning(e.cond.Reason) {
			st.setFalse(e.cond.Reason, e.message())
			return
		}
	}
	if recorded != nil && restoreWarning(recorded.Reason) {
		// Clear the old warning once its holder runs, disappears or reports a
		// different state, so another environment's Starting cannot preserve it.
		apimeta.RemoveStatusCondition(&project.Status.Conditions, conditionBackupHolderReady)
	}
	for _, e := range envs {
		if e.cond.Status != metav1.ConditionTrue {
			st.setFalse(e.cond.Reason, e.message())
			return
		}
	}
	st.setReady()
}

// holderReport configures reporting of project's BackupHolderReady condition
// and warning events when a recorder is available.
func (r *ProjectReconciler) holderReport(project *kipperv1.Project) holderReport {
	st := holderReport{conditions: &project.Status.Conditions, generation: project.Generation}
	if r.Recorder != nil {
		st.event = func(reason, message string) {
			r.Recorder.Event(project, corev1.EventTypeWarning, "BackupHolder"+reason, message)
		}
	}
	return st
}

func (r *ProjectReconciler) apiReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}
