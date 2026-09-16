package controllers

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
)

const (
	buildLabelKey = "kipper.run/build"
	appLabelKey   = "kipper.run/app"
	// sourceNamespaceLabelKey carries the tenant namespace the App lives in.
	// Builds run in the shared kipper-builds namespace, so job.Namespace is no
	// longer the App's namespace — this label is.
	sourceNamespaceLabelKey = "kipper.run/source-namespace"
	// appUIDLabelKey carries the App's UID at build time, so a build that
	// finishes after its App was deleted and a same-named App recreated is not
	// applied to the new App.
	appUIDLabelKey      = "kipper.run/app-uid"
	deployHistoryAnnKey = "kipper.run/deploy-history"
	maxDeployHistory    = 10
	// buildsNamespace is the shared, installer-owned namespace builds run in.
	buildsNamespace = "kipper-builds"
)

// BuildReconciler watches Kaniko build Jobs and updates the App CR
// when a build succeeds or fails.
type BuildReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// APIReader reads straight from the API server, bypassing the manager's
	// cache. Two lookups need it: OOM attribution reads build pods the Pod
	// cache does not cover, and the supersession check reads build Jobs whose
	// cache can lag behind a just-created newer attempt.
	APIReader client.Reader
}

func (r *BuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var job batchv1.Job
	if err := r.Get(ctx, req.NamespacedName, &job); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Only handle kipper build jobs
	if job.Labels[buildLabelKey] != "true" {
		return ctrl.Result{}, nil
	}
	// A job on its way out drives nothing. The TTL controller deletes a
	// finished job with foreground propagation, which stamps a deletion
	// timestamp and produces an update event while the job is still readable,
	// so without this a job whose successor has already aged out gets one last
	// reconcile on a timer rather than only on a restart.
	if job.DeletionTimestamp != nil {
		return ctrl.Result{}, nil
	}

	appName := job.Labels[appLabelKey]
	if appName == "" {
		return ctrl.Result{}, nil
	}
	// The App lives in the source (tenant) namespace, not the build namespace
	// the Job now runs in.
	sourceNS := job.Labels[sourceNamespaceLabelKey]
	if sourceNS == "" {
		return ctrl.Result{}, nil
	}

	// Resolve the App up front so every path — Building, Succeeded, Failed — is
	// guarded by the identity and supersession checks below.
	var app kipperv1.App
	if err := r.Get(ctx, client.ObjectKey{Namespace: sourceNS, Name: appName}, &app); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	// If the App was deleted and a same-named App recreated while this build
	// ran, its UID no longer matches; do not write this build's image or status
	// onto the new App.
	if uid := job.Labels[appUIDLabelKey]; uid != "" && uid != string(app.UID) {
		return ctrl.Result{}, nil
	}
	// A newer build for this App supersedes this one, so a build that finishes
	// late cannot overwrite a newer build's image or status. If the lookup
	// cannot be resolved (transient list error), return the error so the
	// reconcile is requeued without mutating the App — proceeding here could
	// let a stale build overwrite the current image before any recheck.
	superseded, err := r.supersededByNewerBuild(ctx, &job)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("checking build supersession: %w", err)
	}
	if superseded {
		return ctrl.Result{}, nil
	}
	// Only publish artifacts from the app's current git source. Check the job's
	// source fingerprint here, since a source edit can leave an older job running
	// without creating a newer one. Legacy jobs lacking the fingerprint are also
	// discarded; eligible completed jobs receive Discarded status.
	if reason := staleSourceReason(&job, app.Spec.Git); reason != "" {
		// Record discarded completions once the job finishes. Detached apps leave
		// build-status cleanup to the App reconciler; already applied completions
		// retain their terminal status when old jobs are replayed.
		if app.Spec.Git != nil && (job.Status.Succeeded > 0 || job.Status.Failed > 0) &&
			!completionAlreadyApplied(app.Status.Build, &job) {
			// The job is terminal, so no further job event will arrive and
			// nothing else re-enqueues this reconcile. A write dropped here
			// leaves the app on Building for ever, which is what the phase
			// exists to prevent, so the failure has to be retried.
			if err := r.updateBuildStatus(ctx, sourceNS, appName, job.Labels[appUIDLabelKey], job.Name, "Discarded", "", reason); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Check if job is still running
	if job.Status.Succeeded == 0 && job.Status.Failed == 0 {
		// Update build phase to Building if pods are active
		if job.Status.Active > 0 {
			if err := r.updateBuildStatus(ctx, sourceNS, appName, job.Labels[appUIDLabelKey], job.Name, "Building", "", ""); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if job.Status.Succeeded > 0 {
		logger.Info("build succeeded", "app", appName, "job", job.Name)

		// Read the full commit SHA from the annotation set by the builder.
		// Falls back to extracting from the job name for older jobs.
		commit := job.Annotations["kipper.run/commit"]
		if commit == "" {
			commit = extractCommitFromJobName(job.Name, appName)
		}
		imageRef := builder.ImageRef(sourceNS, appName, commit)

		// Update the App CR image
		app.Spec.Image = imageRef

		// Record in deploy history
		r.recordDeployHistory(&app, job.Name, imageRef, commit)

		if err := r.Update(ctx, &app); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating app image: %w", err)
		}

		// Update build status
		now := metav1.Now()
		app.Status.Build = &kipperv1.AppBuildStatus{
			Phase:       "Succeeded",
			Commit:      commit,
			CompletedAt: &now,
			Build:       job.Name,
		}
		if err := r.Status().Update(ctx, &app); err != nil {
			// The job is terminal, so nothing re-enqueues this reconcile. A
			// write dropped here leaves the app on Building for ever.
			logger.Error(err, "failed to update build status")
			return ctrl.Result{}, fmt.Errorf("writing build status: %w", err)
		}

	} else if job.Status.Failed > 0 {
		logger.Info("build failed", "app", appName, "job", job.Name)

		message := "build failed"
		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobFailed {
				message = cond.Message
				break
			}
		}
		// An out-of-memory build shows up as a generic Job failure, which
		// tells the operator nothing actionable. Name it and point at the fix.
		if r.buildWasOOMKilled(ctx, &job) {
			message = "the build ran out of memory (OOMKilled). Raise it with `kip app deploy --build-memory <size>` (e.g. 6Gi), the app's git.buildResources.memory, or the cluster's BUILD_MEMORY_LIMIT."
		}

		// The annotation carries the full commit; the Job name may only hold
		// a digest of it. Extraction stays as the fallback for older Jobs.
		commit := job.Annotations["kipper.run/commit"]
		if commit == "" {
			commit = extractCommitFromJobName(job.Name, appName)
		}
		now := metav1.Now()
		app.Status.Build = &kipperv1.AppBuildStatus{
			Phase:       "Failed",
			Commit:      commit,
			CompletedAt: &now,
			Message:     message,
			Build:       job.Name,
		}
		if err := r.Status().Update(ctx, &app); err != nil {
			// The job is terminal, so nothing re-enqueues this reconcile. A
			// write dropped here leaves the app on Building for ever.
			logger.Error(err, "failed to update build status")
			return ctrl.Result{}, fmt.Errorf("writing build status: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

// supersededByNewerBuild checks jobs for the same namespace, app and UID,
// ordered by creation time and then name. Prefer the direct API reader to see
// new jobs promptly; propagate list errors before mutating the App.
// App resource-version conflicts guard concurrent completion writes.
func (r *BuildReconciler) supersededByNewerBuild(ctx context.Context, job *batchv1.Job) (bool, error) {
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var jobs batchv1.JobList
	if err := reader.List(ctx, &jobs, client.InNamespace(buildsNamespace), client.MatchingLabels{
		sourceNamespaceLabelKey: job.Labels[sourceNamespaceLabelKey],
		appLabelKey:             job.Labels[appLabelKey],
		buildLabelKey:           "true",
	}); err != nil {
		return false, err
	}
	for i := range jobs.Items {
		other := &jobs.Items[i]
		if other.Name == job.Name {
			continue
		}
		// Only the same App generation competes; a different UID is a
		// deleted-and-recreated App, already handled by the UID guard.
		if other.Labels[appUIDLabelKey] != job.Labels[appUIDLabelKey] {
			continue
		}
		if other.CreationTimestamp.After(job.CreationTimestamp.Time) {
			return true, nil
		}
		// Break ties on equal timestamps by name so the winner is stable.
		if other.CreationTimestamp.Equal(&job.CreationTimestamp) && other.Name > job.Name {
			return true, nil
		}
	}
	return false, nil
}

// completionAlreadyApplied reports whether a build status records the finished
// outcome of this job, which a later refusal must not relabel.
//
// The phase alone cannot answer it. A status can be terminal because it belongs
// to an *earlier* build whose result was never replaced — the current build's
// Pending write can be dropped on a conflict — and treating that as this job's
// own applied completion left a refused build with no verdict at all, and the
// previous build's success reported as its own.
func completionAlreadyApplied(status *kipperv1.AppBuildStatus, job *batchv1.Job) bool {
	if status == nil {
		return false
	}
	if status.Build == "" {
		// A status written before builds were named can only be the completion
		// of a job from before builds were named. A job carrying a source
		// fingerprint is not one of those, so this status is somebody else's
		// and does not stand in for the verdict this job is owed.
		if _, named := job.Annotations[builder.SourceFingerprintAnnotation]; named {
			return false
		}
	} else if status.Build != job.Name {
		return false
	}
	return status.Phase == "Succeeded" || status.Phase == "Failed" || status.Phase == "Discarded"
}

// staleSourceReason reports why a build job may not write to its App, or the
// empty string when the job belongs to the source the App declares now. The
// reason is shown to the operator, so it names what changed rather than the
// mechanism that caught it.
func staleSourceReason(job *batchv1.Job, git *kipperv1.AppGitSource) string {
	fingerprint, ok := job.Annotations[builder.SourceFingerprintAnnotation]
	if !ok {
		return "This build started before Kipper recorded which git source a build came from, so it cannot be matched against the source this app has now. Deploy again to build from the current source."
	}
	if fingerprint == builder.UnfingerprintableSource {
		return "Kipper could not record which git source this build came from, so its image was not deployed. Deploy again."
	}
	if git == nil {
		return "This app no longer builds from git, so the build was discarded and the image it is running was kept."
	}
	if fingerprint != builder.GitSourceFingerprint(git) {
		return "The git source changed while this build was running, so it built from settings the app no longer has. Deploy again to build from the current source."
	}
	return ""
}

// wantUID is the App the job was created for. The reconcile's own guards ran
// against an earlier read, so an App deleted and a same-named one recreated in
// between would otherwise be given a dead app's build status here.
func (r *BuildReconciler) updateBuildStatus(ctx context.Context, namespace, appName, wantUID, build, phase, commit, message string) error {
	var app kipperv1.App
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: appName}, &app); err != nil {
		return client.IgnoreNotFound(err)
	}
	if wantUID != "" && wantUID != string(app.UID) {
		return nil
	}

	if app.Status.Build != nil && app.Status.Build.Phase == phase && app.Status.Build.Build == build {
		return nil
	}

	if app.Status.Build == nil {
		app.Status.Build = &kipperv1.AppBuildStatus{}
	}
	app.Status.Build.Phase = phase
	app.Status.Build.Build = build
	if commit != "" {
		app.Status.Build.Commit = commit
	}
	if message != "" {
		app.Status.Build.Message = message
	}
	// Nothing else writes a completion time for a discarded build, so without
	// this the console shows the previous build's, next to a phase that is not
	// the previous build's.
	if phase == "Discarded" {
		now := metav1.Now()
		app.Status.Build.CompletedAt = &now
	}
	if err := r.Status().Update(ctx, &app); err != nil {
		// A rejected write leaves the app on a phase that will never resolve,
		// and a schema the cluster has not been upgraded to is one way to get
		// one. Silence here is what makes that look like the app never built.
		log.FromContext(ctx).Error(err, "writing build status", "app", appName, "phase", phase)
		return fmt.Errorf("writing %s build status: %w", phase, err)
	}
	return nil
}

type buildDeployEntry struct {
	Revision  int    `json:"revision"`
	Image     string `json:"image"`
	Commit    string `json:"commit,omitempty"`
	Trigger   string `json:"trigger"`
	Timestamp string `json:"timestamp"`
	// Build is the job the entry was recorded from. handlers.deployEntry and
	// webhook.DeployEntry in kip mirror this shape and round-trip the
	// annotation, so a field added here has to be added to both or it is
	// stripped the first time either of them writes.
	Build string `json:"build,omitempty"`
}

func (r *BuildReconciler) recordDeployHistory(app *kipperv1.App, build, image, commit string) {
	var history []buildDeployEntry
	if raw, ok := app.Annotations[deployHistoryAnnKey]; ok && raw != "" {
		_ = json.Unmarshal([]byte(raw), &history)
	}

	// The succeeded branch re-runs whole: a status write that conflicts
	// requeues it, and the informer replays a finished job for as long as its
	// TTL lasts. Appending again each time evicts real rollback targets.
	for i := range history {
		if history[i].Build != "" && history[i].Build == build {
			return
		}
	}

	nextRevision := 1
	if len(history) > 0 {
		nextRevision = history[0].Revision + 1
	}

	entry := buildDeployEntry{
		Revision:  nextRevision,
		Image:     image,
		Commit:    commit,
		Trigger:   "build",
		Timestamp: time.Now().Format(time.RFC3339),
		Build:     build,
	}

	history = append([]buildDeployEntry{entry}, history...)
	if len(history) > maxDeployHistory {
		history = history[:maxDeployHistory]
	}

	data, _ := json.Marshal(history)
	if app.Annotations == nil {
		app.Annotations = make(map[string]string)
	}
	app.Annotations[deployHistoryAnnKey] = string(data)
}

func extractCommitFromJobName(jobName, appName string) string {
	prefix := appName + "-build-"
	if len(jobName) > len(prefix) {
		return jobName[len(prefix):]
	}
	return ""
}

// buildWasOOMKilled reports whether the build container of any pod for this
// Job was terminated by the OOM killer, so a memory-starved build can be
// named instead of surfacing an opaque failure.
func (r *BuildReconciler) buildWasOOMKilled(ctx context.Context, job *batchv1.Job) bool {
	var pods corev1.PodList
	// Uncached: the manager's Pod cache only covers the monitoring namespace,
	// while build pods live in project namespaces.
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	if err := reader.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return false
	}
	for i := range pods.Items {
		// Match on the owning Job's UID: a pod from an earlier build can
		// outlive its Job during background deletion, and it must not be read
		// as this build's failure. A job created before build names carried a
		// random id shares this one's name, which is how that happens.
		if owner := metav1.GetControllerOf(&pods.Items[i]); owner == nil || owner.UID != job.UID {
			continue
		}
		// The Kaniko build runs as an init container (the push container is
		// the pod's main container), so both status lists are scanned.
		statuses := append(append([]corev1.ContainerStatus{},
			pods.Items[i].Status.InitContainerStatuses...),
			pods.Items[i].Status.ContainerStatuses...)
		for _, cs := range statuses {
			if cs.Name != "build" {
				continue
			}
			for _, st := range []*corev1.ContainerStateTerminated{
				stateTerminated(cs.State), stateTerminated(cs.LastTerminationState),
			} {
				if st != nil && st.Reason == "OOMKilled" {
					return true
				}
			}
		}
	}
	return false
}

func stateTerminated(s corev1.ContainerState) *corev1.ContainerStateTerminated {
	return s.Terminated
}

// SetupWithManager registers the build watcher with the controller manager.
func (r *BuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("build").
		For(&batchv1.Job{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(obj client.Object) bool {
			return obj.GetLabels()[buildLabelKey] == "true"
		})).
		Complete(r)
}
