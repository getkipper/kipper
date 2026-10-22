package controllers

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/controller/pkg/workload"
)

const jobFinalizer = "kipper.run/job-cleanup"

// runOutcomeFinalizer preserves the native Job until its outcome is recorded
// on the CR, preventing another run after cleanup.
const runOutcomeFinalizer = "kipper.run/run-outcome"

// JobReconciler reconciles a Job CR.
type JobReconciler struct {
	client.Client
	// APIReader is an uncached reader, used where deciding an object is
	// unreferenced must not rest on a cache that has not caught up.
	APIReader client.Reader
	Scheme    *runtime.Scheme
	// Recorder puts a reconcile failure on the Job itself, where `kubectl
	// describe` finds it without anyone tailing the controller's log. Nil in
	// unit tests.
	Recorder record.EventRecorder
}

// hostReader returns the uncached reader, falling back to the cached client when
// none is wired (unit tests construct the reconciler without one).
func (r *JobReconciler) hostReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *JobReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, retErr error) {
	logger := log.FromContext(ctx)

	var job kipperv1.Job
	if err := r.Get(ctx, req.NamespacedName, &job); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	statusAtEntry := job.Status.DeepCopy()

	// A failed pass can leave the job partially reconciled, so record the
	// failure on the object for visibility. Conflicts are handled by retries.
	// The event recorder counts repeated events with the same reason and message
	// while cached, limiting duplicate events when a failure recurs.
	defer func() { recordReconcileFailure(r.Recorder, &job, retErr) }()

	if !job.DeletionTimestamp.IsZero() {
		logger.Info("cleaning up job resources", "job", job.Name)
		if err := r.releaseRun(ctx, &job); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(&job, jobFinalizer)
		return ctrl.Result{}, r.Update(ctx, &job)
	}

	// See the Function reconciler. A Job matters most here: it shares no child
	// object with an App, so nothing further down would ever refuse the pair,
	// and its pods carry the label the App's Service selects.
	if heldBy, claimErr := reconcileNameClaim(ctx, r.Client, r.hostReader(), r.Scheme, &job, "job"); claimErr != nil {
		return ctrl.Result{}, claimErr
	} else if heldBy != "" {
		taken := workload.NameTakenError{Name: job.Name, Kind: heldBy}
		apimeta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{
			Type:               kipperv1.ConditionChildrenAdopted,
			Status:             metav1.ConditionFalse,
			Reason:             "NameHeldByAnotherWorkload",
			Message:            blockedMessage(job.Name, heldBy),
			ObservedGeneration: job.Generation,
		})
		job.Status.Phase = "Failed"
		r.writeStatusIfChanged(ctx, &job, statusAtEntry)
		return ctrl.Result{}, taken
	}

	if !controllerutil.ContainsFinalizer(&job, jobFinalizer) {
		controllerutil.AddFinalizer(&job, jobFinalizer)
		if err := r.Update(ctx, &job); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Before either workload: a one-off Job's pod template is immutable once
	// created, so a Secret rendered afterwards would never reach that run.
	generation, err := r.reconcileEnvSecret(ctx, &job)
	if recErr := recordPublication(ctx, r.Client, &job, &job.Status.Conditions,
		&job.Status.PublishedEnv, job.Generation, generation, err); recErr != nil {
		logger.Error(recErr, "recording the published environment")
	}
	if err != nil {
		if statusErr := r.Status().Update(ctx, &job); statusErr != nil {
			logger.Error(statusErr, "recording a failed environment publication")
		}
		return ctrl.Result{}, fmt.Errorf("reconciling env secret: %w", err)
	}

	if job.Spec.Schedule != "" {
		if err := r.releaseRun(ctx, &job); err != nil {
			return ctrl.Result{}, fmt.Errorf("releasing the one-off run: %w", err)
		}
		if err := r.reconcileCronJob(ctx, &job, generation); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling cronjob: %w", err)
		}
	} else {
		if err := r.reconcileOneOffJob(ctx, &job, generation); err != nil {
			return ctrl.Result{}, fmt.Errorf("reconciling one-off job: %w", err)
		}
	}

	// Cleared on the way through, as the App and Function reconcilers do. A job
	// whose collision was resolved by deleting the other workload would
	// otherwise report a healthy phase beside a condition still saying another
	// workload holds its name.
	apimeta.SetStatusCondition(&job.Status.Conditions, metav1.Condition{
		Type:               kipperv1.ConditionChildrenAdopted,
		Status:             metav1.ConditionTrue,
		Reason:             "AllChildrenAdopted",
		Message:            "every child this workload owns reconciled",
		ObservedGeneration: job.Generation,
	})

	// Before the status write, for the same reason the other two do it there.
	retryIn := r.sweepEnv(ctx, &job, generation)

	if err := r.updateStatus(ctx, &job); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: retryIn}, nil
}

// sweepEnv retires what this job has moved off. A Job binds no services, so it
// has no projections to keep and nothing to compute a keep-set from.
func (r *JobReconciler) sweepEnv(ctx context.Context, job *kipperv1.Job, generation string) time.Duration {
	retryIn, legacyReaders, err := retireEnvSecrets(ctx, r.Client, r.hostReader(), job, secretname.KindJob, generation, nil)
	if err != nil {
		log.FromContext(ctx).Error(err, "retiring superseded environments", "job", job.Name)
		return envRetirementRetry
	}
	applyEnvPublishedConditionWithConversion(&job.Status.Conditions, job.Generation, nil, legacyReaders)
	return retryIn
}

// reconcileEnvSecret materialises Job.Spec.Env into the Secret both the CronJob
// and the one-off Job pod templates reference, resolving each ${NAME} as the
// App and Function renders do. Without it the reference is dangling: it is
// Optional, so pods start normally and run with none of the environment the Job
// declares.
//
// A Job binds no services and declares no links, so its own env is everything
// there is to resolve against. A reference to a credential stays literal, which
// is the truth about a Job today rather than a limit of the resolver.
func (r *JobReconciler) reconcileEnvSecret(ctx context.Context, job *kipperv1.Job) (string, error) {
	sources := jobEnvSources(job)
	table, err := effectiveEnv(ctx, r.Client, job.Namespace, sources)
	if err != nil {
		return "", err
	}
	resolved, diag := renderEnv(job.Spec.Env, table)
	// Recorded in memory; updateStatus persists it at the end of the pass.
	applyEnvResolvedCondition(&job.Status.Conditions, job.Generation, len(job.Spec.Env), diag)

	// A native Job's pod template is immutable, so naming a generation is what
	// finally makes a retry rerun the environment the run started with. Editing
	// the CR mid-run used to rewrite the Secret the retry pod then read, so one
	// Kubernetes Job could execute two configurations.
	published, err := publishedEnv(ctx, r.Client, job.Namespace, sources, resolved)
	if err != nil {
		return "", err
	}
	generation, err := publishEnvGeneration(ctx, r.Client, r.Scheme, job, secretname.KindJob,
		published, jobLabels(job))
	if err != nil {
		return "", err
	}

	if err := updateLegacyEnvSecret(ctx, r.Client, r.Scheme, job,
		secretname.Env(secretname.KindJob, job.Name), resolved, jobLabels(job)); err != nil {
		return "", err
	}
	return generation, nil
}

func (r *JobReconciler) reconcileCronJob(ctx context.Context, job *kipperv1.Job, generation string) error {
	labels := jobLabels(job)
	backoffLimit := int32(3)
	if job.Spec.BackoffLimit != nil {
		backoffLimit = *job.Spec.BackoffLimit
	}

	container := corev1.Container{
		Name:  job.Name,
		Image: job.Spec.Image,
	}

	if len(job.Spec.Command) > 0 {
		container.Command = job.Spec.Command
	}

	container.Resources = jobResources(job.Spec.Resources)

	container.EnvFrom = envFrom(generation)

	restartPolicy := corev1.RestartPolicyNever

	// Same chokepoint as Apps and Functions: validates a cluster-registry
	// image belongs to this project and stages the scoped third-party pull
	// Secret the pod references.
	pullSecrets, err := ensureImagePullSecret(ctx, r.Client, r.Scheme, job, job.Spec.Image)
	if err != nil {
		return err
	}

	desired := &batchv1.CronJob{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name,
			Namespace: job.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.CronJobSpec{
			Schedule: job.Spec.Schedule,
			JobTemplate: batchv1.JobTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: batchv1.JobSpec{
					BackoffLimit: &backoffLimit,
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{Labels: labels},
						Spec: corev1.PodSpec{
							Containers:       []corev1.Container{container},
							RestartPolicy:    restartPolicy,
							ImagePullSecrets: pullSecrets,
						},
					},
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(job, desired, r.Scheme); err != nil {
		return err
	}

	var existing batchv1.CronJob
	err = r.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	if err := adoptChild("CronJob", &existing, jobOwner(job), r.Scheme); err != nil {
		return err
	}
	existing.Spec.Schedule = desired.Spec.Schedule
	existing.Spec.JobTemplate = desired.Spec.JobTemplate
	existing.Labels = desired.Labels
	return r.Update(ctx, &existing)
}

func (r *JobReconciler) reconcileOneOffJob(ctx context.Context, job *kipperv1.Job, generation string) error {
	labels := jobLabels(job)
	backoffLimit := int32(3)
	if job.Spec.BackoffLimit != nil {
		backoffLimit = *job.Spec.BackoffLimit
	}

	container := corev1.Container{
		Name:  job.Name,
		Image: job.Spec.Image,
	}

	if len(job.Spec.Command) > 0 {
		container.Command = job.Spec.Command
	}

	container.Resources = jobResources(job.Spec.Resources)

	container.EnvFrom = envFrom(generation)

	ttl := int32(86400)

	// Validation and staging must run before creation: native Job pod
	// templates are immutable and an existing run is never patched, so this
	// is the only gate a one-off Job's image passes.
	pullSecrets, err := ensureImagePullSecret(ctx, r.Client, r.Scheme, job, job.Spec.Image)
	if err != nil {
		return err
	}

	desired := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:       job.Name,
			Namespace:  job.Namespace,
			Labels:     labels,
			Finalizers: []string{runOutcomeFinalizer},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					Containers:       []corev1.Container{container},
					RestartPolicy:    corev1.RestartPolicyNever,
					ImagePullSecrets: pullSecrets,
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(job, desired, r.Scheme); err != nil {
		return err
	}

	var existing batchv1.Job
	err = r.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &existing)
	if errors.IsNotFound(err) {
		// LastResult preserves the one-off run's outcome after the native Job is deleted.
		if job.Status.LastResult != "" {
			return nil
		}
		// Check the server for a recorded outcome to avoid rerunning from stale cache data.
		var current kipperv1.Job
		if err := r.hostReader().Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &current); err != nil {
			return err
		}
		if current.Status.LastResult != "" {
			job.Status = current.Status
			return nil
		}
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	// Preserve the run's immutable pod template while repairing ownership
	// for garbage collection and protecting any unrecorded outcome.
	changed := false
	if !metav1.IsControlledBy(&existing, job) {
		if err := adoptChild("Job", &existing, jobOwner(job), r.Scheme); err != nil {
			return err
		}
		changed = true
	}
	if job.Status.LastResult == "" && existing.DeletionTimestamp.IsZero() &&
		controllerutil.AddFinalizer(&existing, runOutcomeFinalizer) {
		changed = true
	}
	if changed {
		return r.Update(ctx, &existing)
	}
	return nil
}

func jobConditionTrue(nj *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range nj.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// releaseRun removes outcome protection when the CR is deleted or becomes scheduled.
func (r *JobReconciler) releaseRun(ctx context.Context, job *kipperv1.Job) error {
	// Read from the server so cache lag cannot leave a run stuck with its finalizer
	// after the CR is deleted.
	var nj batchv1.Job
	err := r.hostReader().Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &nj)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if controllerutil.RemoveFinalizer(&nj, runOutcomeFinalizer) {
		return r.Update(ctx, &nj)
	}
	return nil
}

func (r *JobReconciler) updateStatus(ctx context.Context, job *kipperv1.Job) error {
	if job.Spec.Schedule != "" {
		var cj batchv1.CronJob
		err := r.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &cj)
		if errors.IsNotFound(err) {
			job.Status.Phase = "Pending"
			return r.Status().Update(ctx, job)
		}
		if err != nil {
			return err
		}

		if cj.Status.LastScheduleTime != nil {
			job.Status.LastRun = cj.Status.LastScheduleTime
		}

		if len(cj.Status.Active) > 0 {
			job.Status.Phase = "Running"
		} else {
			job.Status.Phase = "Scheduled"
		}

		return r.Status().Update(ctx, job)
	}

	var nj batchv1.Job
	err := r.Get(ctx, types.NamespacedName{Name: job.Name, Namespace: job.Namespace}, &nj)
	if errors.IsNotFound(err) {
		// Preserve the recorded outcome while the native Job is absent from the cache.
		return r.Status().Update(ctx, job)
	}
	if err != nil {
		return err
	}

	// Terminal conditions determine the outcome; failed pods may still be retried.
	switch {
	case jobConditionTrue(&nj, batchv1.JobComplete):
		job.Status.Phase = "Completed"
		job.Status.LastResult = "Succeeded"
	case jobConditionTrue(&nj, batchv1.JobFailed):
		job.Status.Phase = "Failed"
		job.Status.LastResult = "Failed"
	case !nj.DeletionTimestamp.IsZero():
		// Treat deletion before a terminal condition as failure.
		job.Status.Phase = "Failed"
		job.Status.LastResult = "Failed"
	case nj.Status.Active > 0:
		job.Status.Phase = "Running"
	default:
		// Use an empty phase while pending; the schema excludes "Pending".
		job.Status.Phase = ""
	}
	if !nj.CreationTimestamp.IsZero() {
		t := nj.CreationTimestamp
		job.Status.LastRun = &t
	}
	if err := r.Status().Update(ctx, job); err != nil {
		return err
	}
	if job.Status.LastResult != "" && controllerutil.RemoveFinalizer(&nj, runOutcomeFinalizer) {
		return r.Update(ctx, &nj)
	}
	return nil
}

func (r *JobReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&kipperv1.Job{}).
		Owns(&batchv1.CronJob{}).
		Owns(&batchv1.Job{}).
		// A pod's only env source is not optional, so a published environment
		// deleted out of band stops every scheduled run. App and Function have
		// had this watch; without it the Job waits for an unrelated event
		// before republishing.
		Owns(&corev1.Secret{}).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.enqueueForRegistryCredentials)).
		Complete(r)
}

func jobLabels(job *kipperv1.Job) map[string]string {
	return map[string]string{
		"app":       job.Name,
		kipperLabel: kipperValue,
		// Without this a Job's children are labelled exactly like an App's, so
		// a same-named App in the namespace would accept an ownerless CronJob
		// of this Job's as its own.
		resourceTypeLabel: "job",
	}
}

func jobResources(res kipperv1.JobResources) corev1.ResourceRequirements {
	cpuReq, cpuLim := pairOrDefault(res.CPURequest, res.CPULimit, "100m")
	memReq, memLim := pairOrDefault(res.MemoryRequest, res.MemoryLimit, "128Mi")
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuReq),
			corev1.ResourceMemory: resource.MustParse(memReq),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpuLim),
			corev1.ResourceMemory: resource.MustParse(memLim),
		},
	}
}

// pairOrDefault returns (request, limit). If either is empty it inherits
// from the other; if both are empty, both default to fallback.
func pairOrDefault(req, lim, fallback string) (string, string) {
	switch {
	case req == "" && lim == "":
		return fallback, fallback
	case req == "":
		return lim, lim
	case lim == "":
		return req, req
	default:
		return req, lim
	}
}

// writeStatusIfChanged persists status only when this pass actually changed it.
//
// The controller watches Jobs without a status-change predicate, so an
// unconditional write on a permanently blocked job would enqueue itself again
// on every pass and spin rather than back off. The App and Function reconcilers
// carry the same guard for the same reason.
func (r *JobReconciler) writeStatusIfChanged(ctx context.Context, job *kipperv1.Job, before *kipperv1.JobStatus) {
	if equality.Semantic.DeepEqual(before, &job.Status) {
		return
	}
	if err := r.Status().Update(ctx, job); err != nil {
		log.FromContext(ctx).Error(err, "recording reconcile status", "job", job.Name, "namespace", job.Namespace)
	}
}
