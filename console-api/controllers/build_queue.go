package controllers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// defaultMaxConcurrentBuilds is how many builds run at once when the cluster
// sets no BUILD_MAX_CONCURRENT. A build may use its whole memory limit, 2 GiB
// by default, so a small node fits one next to its apps.
const defaultMaxConcurrentBuilds = 1

// maxConcurrentBuilds reads BUILD_MAX_CONCURRENT, falling back to the default
// for an unset or unusable value.
func maxConcurrentBuilds() int {
	if n, err := strconv.Atoi(os.Getenv("BUILD_MAX_CONCURRENT")); err == nil && n > 0 {
		return n
	}
	return defaultMaxConcurrentBuilds
}

// queueRecheckInterval controls polling while a deleting Job or a leftover pod
// holds a slot. Pod completion alone produces no build event.
const queueRecheckInterval = 10 * time.Second

// stuckPodAfter limits how long a terminating build pod or Job counts toward
// slot usage after its own deletion timestamp.
const stuckPodAfter = 5 * time.Minute

// releaseQueuedBuilds starts waiting builds in a namespace, oldest first,
// until as many run as the cluster allows. Builds are created suspended, so
// this is the only place one starts. Pods retain slots after their Jobs are
// deleted until they stop or exceed stuckPodAfter past their deletion timestamp.
// An unfinished, unsuspended Job retains its slot independently of its pods
// until it exceeds stuckPodAfter past its own deletion timestamp.
// Creation-time ties are ordered by Job name. Waiting builds rejected by
// buildStillWanted are deleted, including when all slots are occupied.
// Per-build errors are returned together for retry while processing continues
// for the others. Each attempted start holds a slot for the rest of the pass,
// even if the update returns an error. recheck is true when a
// wanted build is waiting for a slot and a deleting Job or leftover pod holds
// at least one slot.
func (r *BuildReconciler) releaseQueuedBuilds(ctx context.Context, namespace string) (recheck bool, err error) {
	// A just-released build can still read as suspended in the cache, which
	// would release a second one into the same slot.
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var jobs batchv1.JobList
	if err := reader.List(ctx, &jobs, client.InNamespace(namespace), client.MatchingLabels{buildLabelKey: "true"}); err != nil {
		return false, fmt.Errorf("listing builds: %w", err)
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{buildLabelKey: "true"}); err != nil {
		return false, fmt.Errorf("listing build pods: %w", err)
	}

	// Slots are counted per build, by its Job's name, so a running build's
	// Job and pod take one slot between them.
	occupied := map[string]bool{}
	leftover := false
	liveJobs := map[string]bool{}
	newest := map[string]*batchv1.Job{}
	var waiting []*batchv1.Job
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.DeletionTimestamp == nil {
			if n := newest[buildIdentity(job)]; n == nil || createdAfter(job, n) {
				newest[buildIdentity(job)] = job
			}
		}
		if jobFinished(job) {
			continue
		}
		suspended := job.Spec.Suspend != nil && *job.Spec.Suspend
		switch {
		case job.DeletionTimestamp != nil:
			// Bound the wait because foreground deletion can stall on a pod
			// that is stuck terminating.
			if !suspended && !deletedLongAgo(job.DeletionTimestamp) {
				occupied[job.Name] = true
				leftover = true
			}
		case suspended:
			waiting = append(waiting, job)
		default:
			occupied[job.Name] = true
			liveJobs[job.Name] = true
		}
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		// Bound the wait for terminating pods, which can linger after their
		// deletion deadline if a node stops responding or a finalizer remains.
		if deletedLongAgo(pod.DeletionTimestamp) {
			continue
		}
		owner := pod.Labels[batchv1.JobNameLabel]
		if owner == "" {
			owner = pod.Name
		}
		occupied[owner] = true
		if !liveJobs[owner] {
			leftover = true
		}
	}
	running := len(occupied)
	sort.Slice(waiting, func(i, j int) bool {
		return createdAfter(waiting[j], waiting[i])
	})

	full := false
	var errs []error
	// Check every waiting build so obsolete builds are removed even when all
	// slots are occupied.
	for _, job := range waiting {
		wanted, discard, err := r.buildStillWanted(ctx, reader, job, newest)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !wanted {
			// Record the discard reason first so the App shows it even if
			// deletion fails and needs a retry.
			if discard != "" {
				if err := r.recordDiscard(ctx, reader, job, discard); err != nil {
					errs = append(errs, err)
					continue
				}
			}
			err := r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
			if client.IgnoreNotFound(err) != nil {
				errs = append(errs, fmt.Errorf("dropping build %s: %w", job.Name, err))
			}
			continue
		}
		if running >= maxConcurrentBuilds() {
			full = true
			continue
		}
		patch := client.MergeFrom(job.DeepCopy())
		job.Spec.Suspend = ptr.To(false)
		// Reserve the slot until the next pass lists the Jobs again because
		// the update may succeed even if its response is lost.
		running++
		if err := r.Patch(ctx, job, patch); err != nil {
			errs = append(errs, fmt.Errorf("starting build %s: %w", job.Name, err))
		}
	}
	return full && leftover, errors.Join(errs...)
}

// deletedLongAgo reports whether a non-nil deletion timestamp is more than
// stuckPodAfter in the past.
func deletedLongAgo(deleted *metav1.Time) bool {
	return deleted != nil && time.Since(deleted.Time) > stuckPodAfter
}

// buildIdentity groups builds by source namespace, App name and App UID.
func buildIdentity(job *batchv1.Job) string {
	return job.Labels[sourceNamespaceLabelKey] + "/" + job.Labels[appLabelKey] + "/" + job.Labels[appUIDLabelKey]
}

// createdAfter orders builds by creation time, then by name.
func createdAfter(a, b *batchv1.Job) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return b.CreationTimestamp.Before(&a.CreationTimestamp)
	}
	return a.Name > b.Name
}

// buildStillWanted reports whether a waiting build could still produce the
// App's image, so obsolete builds can release their slots for other builds.
// It rejects builds whose App is gone or has a different UID, whose source
// fails staleSourceReason, or whose newest build has a later creation time
// and matches the current source. Equal creation times keep both builds
// because random Job names cannot establish request order. Missing namespace
// or App labels skip all checks. A missing UID skips the App identity check.
// Lookup errors other than NotFound are returned. discard contains the source
// rejection reason only when the App's status tracks the Job by name or has
// a Pending build with no Job name and the same nonempty commit. Otherwise,
// discard is empty.
func (r *BuildReconciler) buildStillWanted(ctx context.Context, reader client.Reader, job *batchv1.Job, newest map[string]*batchv1.Job) (wanted bool, discard string, err error) {
	namespace, name := job.Labels[sourceNamespaceLabelKey], job.Labels[appLabelKey]
	if namespace == "" || name == "" {
		return true, "", nil
	}
	var app kipperv1.App
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &app); err != nil {
		if apierrors.IsNotFound(err) {
			return false, "", nil
		}
		return false, "", fmt.Errorf("looking up the app of build %s: %w", job.Name, err)
	}
	if uid := job.Labels[appUIDLabelKey]; uid != "" && uid != string(app.UID) {
		return false, "", nil
	}
	if reason := staleSourceReason(job, app.Spec.Git); reason != "" {
		if tracksBuild(app.Status.Build, job) {
			return false, reason, nil
		}
		return false, "", nil
	}
	later := newest[buildIdentity(job)]
	replaced := later != nil && job.CreationTimestamp.Before(&later.CreationTimestamp) &&
		staleSourceReason(later, app.Spec.Git) == ""
	return !replaced, "", nil
}

// recordDiscard records why a build is being discarded in its App's status.
// It rereads the App on each conflict retry and updates its status only if the
// App matches the Job's UID label, when present, and still tracks the Job.
// This preserves the status of a newer attempt. A missing App is ignored.
// An existing discard for the same Job is preserved so deletion retries keep
// the original completion time.
func (r *BuildReconciler) recordDiscard(ctx context.Context, reader client.Reader, job *batchv1.Job, reason string) error {
	key := client.ObjectKey{Namespace: job.Labels[sourceNamespaceLabelKey], Name: job.Labels[appLabelKey]}
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var app kipperv1.App
		if err := reader.Get(ctx, key, &app); err != nil {
			return client.IgnoreNotFound(err)
		}
		if uid := job.Labels[appUIDLabelKey]; uid != "" && uid != string(app.UID) {
			return nil
		}
		if !tracksBuild(app.Status.Build, job) {
			return nil
		}
		if app.Status.Build.Phase == "Discarded" && app.Status.Build.Build == job.Name {
			return nil
		}
		setBuildStatus(&app, job, "Discarded", reason)
		return r.Status().Update(ctx, &app)
	})
	if err != nil {
		return fmt.Errorf("writing Discarded build status for %s: %w", job.Name, err)
	}
	return nil
}

// tracksBuild reports whether status names the Job or has a Pending build
// with no Job name and the same nonempty commit. Matching by commit lets the
// queue record a discard before the creator records the Job name.
func tracksBuild(status *kipperv1.AppBuildStatus, job *batchv1.Job) bool {
	if status == nil {
		return false
	}
	if status.Build == job.Name {
		return true
	}
	return status.Build == "" && status.Phase == "Pending" && status.Commit != "" &&
		status.Commit == job.Annotations["kipper.run/commit"]
}

// jobFinished reports whether a Job has a succeeded or failed pod, or a
// terminal completion or failure condition.
func jobFinished(job *batchv1.Job) bool {
	if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
		return true
	}
	for _, c := range job.Status.Conditions {
		if (c.Type == batchv1.JobComplete || c.Type == batchv1.JobFailed) && c.Status == "True" {
			return true
		}
	}
	return false
}
