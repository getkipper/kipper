package controllers

import (
	"context"
	"fmt"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/controller/pkg/secretname"
)

const (
	// unreferencedSinceAnnotation records when a workload's environment was
	// first seen with nothing reading it.
	//
	// The mark lives on the object rather than in the controller, so a restart
	// or a second controller process cannot count one observation twice, which
	// an in-memory counter would. Annotations on an immutable Secret are
	// writable; only its data and the immutable flag itself are sealed.
	unreferencedSinceAnnotation = "kipper.run/unreferenced-since"

	// envRetirementRescan is how soon to look again while an environment is kept
	// alive only by something outside this workload's own children.
	//
	// A StatefulSet reading an App's old generation is honoured, and nothing
	// makes the App reconcile when that StatefulSet goes away: it is not owned
	// and not watched. Without a timer of its own the generation would wait for
	// an unrelated event that may never come.
	envRetirementRescan = time.Hour

	// envRetirementRetry is how soon to look again after a retirement pass
	// failed. Retirement owns the only timer that brings it back, so swallowing
	// the error without one would leave a marked object waiting for an
	// unrelated event.
	envRetirementRetry = 5 * time.Minute

	// envRetirementGrace is how long an environment must go unread before it is
	// deleted.
	//
	// The delay is the protection, not the bookkeeping. A CronJob controller can
	// create a native Job from a template this sweep has already listed, and
	// with a non-optional env reference that Job could never start. An hour is
	// far longer than that window and costs nothing but a Secret.
	envRetirementGrace = time.Hour
)

// envConsumers is every Secret name something in this namespace still reads
// through envFrom.
//
// It scans templates rather than walking ownership, which is both simpler and
// safer: a Job created by a CronJob is a Job like any other, so the
// grandchildren that an ownership walk has to be taught about are already here.
// A pod counts for as long as it exists, because a container restart re-reads
// envFrom, and a pod that has finished cannot restart.
//
// The reads are authoritative. A cached list that has not caught up says exactly
// what an empty one says, and only one of them means it.
func envConsumers(ctx context.Context, reader client.Reader, namespace string) (map[string]bool, error) {
	names := map[string]bool{}

	add := func(spec corev1.PodSpec) {
		for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
			for _, ef := range c.EnvFrom {
				if ef.SecretRef != nil {
					names[ef.SecretRef.Name] = true
				}
			}
		}
	}

	var stateful appsv1.StatefulSetList
	if err := reader.List(ctx, &stateful, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing statefulsets: %w", err)
	}
	for i := range stateful.Items {
		add(stateful.Items[i].Spec.Template.Spec)
	}

	var daemons appsv1.DaemonSetList
	if err := reader.List(ctx, &daemons, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing daemonsets: %w", err)
	}
	for i := range daemons.Items {
		add(daemons.Items[i].Spec.Template.Spec)
	}

	var rcs corev1.ReplicationControllerList
	if err := reader.List(ctx, &rcs, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing replicationcontrollers: %w", err)
	}
	for i := range rcs.Items {
		if rcs.Items[i].Spec.Template != nil {
			add(rcs.Items[i].Spec.Template.Spec)
		}
	}

	var deploys appsv1.DeploymentList
	if err := reader.List(ctx, &deploys, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing deployments: %w", err)
	}
	for i := range deploys.Items {
		add(deploys.Items[i].Spec.Template.Spec)
	}

	var sets appsv1.ReplicaSetList
	if err := reader.List(ctx, &sets, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing replicasets: %w", err)
	}
	for i := range sets.Items {
		add(sets.Items[i].Spec.Template.Spec)
	}

	var crons batchv1.CronJobList
	if err := reader.List(ctx, &crons, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing cronjobs: %w", err)
	}
	for i := range crons.Items {
		add(crons.Items[i].Spec.JobTemplate.Spec.Template.Spec)
	}

	var jobs batchv1.JobList
	if err := reader.List(ctx, &jobs, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing jobs: %w", err)
	}
	for i := range jobs.Items {
		add(jobs.Items[i].Spec.Template.Spec)
	}

	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	for i := range pods.Items {
		switch pods.Items[i].Status.Phase {
		case corev1.PodSucceeded, corev1.PodFailed:
			continue
		}
		add(pods.Items[i].Spec)
	}

	return names, nil
}

// retireEnvSecrets deletes the environments this workload published that nothing
// reads any more, and returns how long to wait before looking again.
//
// Deletion needs two things a single pass cannot both provide: the object seen
// unreferenced, and that observation older than the grace. So a pass either
// marks or deletes, never both, and a reference that reappears clears the mark
// rather than counting towards it.
//
// current is never a candidate, whatever the scan says. A pod template written
// this pass may not be visible to the reader yet, and deleting the environment
// it names would leave a workload that cannot start.
func retireEnvSecrets(ctx context.Context, c client.Client, reader client.Reader,
	owner client.Object, kind secretname.Kind, current string, keepProjections map[string]bool) (time.Duration, int, error) {
	consumers, err := envConsumers(ctx, reader, owner.GetNamespace())
	if err != nil {
		return 0, 0, err
	}

	var mine corev1.SecretList
	if err := reader.List(ctx, &mine, client.InNamespace(owner.GetNamespace())); err != nil {
		return 0, 0, fmt.Errorf("listing secrets: %w", err)
	}

	prefix := secretname.EnvGenerationPrefix(kind, owner.GetName())
	legacy := secretname.Env(kind, owner.GetName())

	var soonest time.Duration
	var doomed []*corev1.Secret
	// Objects from before the move that something still reads. While this is
	// above zero the workload has not finished converting.
	legacyReaders := 0

	for i := range mine.Items {
		s := &mine.Items[i]
		// Three kinds of object retire under one rule. A generation, the
		// pre-generation env Secret now that nothing recreates it, and a derived
		// credential projection whose binding has gone. A second rule for the
		// third would drift from this one, and the drifted one would be wrong.
		var mine, preGeneration bool
		switch {
		case s.Name == legacy:
			mine, preGeneration = metav1.IsControlledBy(s, owner), true
		case strings.HasPrefix(s.Name, prefix):
			mine = metav1.IsControlledBy(s, owner)
		case s.Labels[derivedBindingLabel] == "true" && !keepProjections[s.Name]:
			// derivedSecretBelongsTo rather than a controller check, because a
			// restored projection carries a reference to the workload it named
			// before the restore gave that workload a new UID.
			//
			// A projection still read by a pod is a pre-generation reader too:
			// once a workload has converted, nothing reads one directly and it
			// is only an ingredient of the published environment.
			mine, preGeneration = derivedSecretBelongsTo(s, owner, kind), true
		}
		if !mine {
			continue
		}

		// current is the strongest reference there is, so it clears a mark like
		// any other. Skipping it entirely would leave a mark from before a
		// rollback lying on the environment the rollback moved back to, and the
		// next roll-forward would delete it with no grace at all.
		if s.Name == current || consumers[s.Name] {
			// Only objects from before the move count. A retained ReplicaSet
			// naming the generation it rolled off is ordinary deployment
			// history, and counting it would leave the gate permanently open.
			if preGeneration && consumers[s.Name] {
				legacyReaders++
			}
			if err := clearUnreferencedMark(ctx, c, s); err != nil {
				return 0, 0, err
			}
			// Something outside this workload holds it open, and nothing brings
			// this controller back when that goes away. current is the
			// exception: it is this workload's own, and the next pass to change
			// it is what supersedes it.
			if s.Name != current && (soonest == 0 || envRetirementRescan < soonest) {
				soonest = envRetirementRescan
			}
			continue
		}

		wait, ripe, err := markOrDelete(ctx, c, s)
		if err != nil {
			return 0, 0, err
		}
		if ripe {
			doomed = append(doomed, s)
		}
		if wait > 0 && (soonest == 0 || wait < soonest) {
			soonest = wait
		}
	}

	if len(doomed) > 0 {
		preserved, err := deleteAfterRecheck(ctx, c, reader, owner.GetNamespace(), doomed)
		if err != nil {
			return 0, 0, err
		}
		// A consumer that appeared inside the recheck window needs the same
		// timer as one the first scan found. It is the same situation reached a
		// moment later, and the same nothing brings this controller back.
		if preserved && (soonest == 0 || envRetirementRescan < soonest) {
			soonest = envRetirementRescan
		}
	}
	return soonest, legacyReaders, nil
}

// deleteAfterRecheck looks once more, immediately before each delete.
//
// The hour a mark has to mature proves that an earlier pass found no reference.
// It does not make a set of LIST calls and a DELETE one operation, and it cannot:
// a CronJob controller holding a template version read before the scan can
// create a Job in between, and that Job would name an environment that no longer
// exists. Looking again does not close that window either, it narrows it to the
// gap between the last read and the write, which is the smallest it can be made
// without a lock nothing here can take.
//
// The scan is per candidate rather than per batch, because one scan for a batch
// puts the whole of every earlier deletion inside the window of every later one,
// and that part is avoidable.
// It reports whether it spared anything, because a candidate spared here is one
// the caller's own scan did not see and therefore did not schedule a rescan for.
func deleteAfterRecheck(ctx context.Context, c client.Client, reader client.Reader,
	namespace string, doomed []*corev1.Secret) (bool, error) {
	spared := false
	for _, s := range doomed {
		consumers, err := envConsumers(ctx, reader, namespace)
		if err != nil {
			return spared, err
		}
		if consumers[s.Name] {
			// Something appeared during the pass. Clearing the mark gives it the
			// full grace again rather than deleting it on the next look.
			if err := clearUnreferencedMark(ctx, c, s); err != nil {
				return spared, err
			}
			spared = true
			continue
		}
		if err := c.Delete(ctx, s); err != nil && !errors.IsNotFound(err) {
			return spared, fmt.Errorf("deleting retired environment %q: %w", s.Name, err)
		}
	}
	return spared, nil
}

// markOrDelete stamps an unreferenced environment, or deletes one whose stamp
// has matured. It returns how long is left on an immature stamp, so a pass that
// finds one requeues for it — including a pass that did not write it, which is
// what keeps an object stamped before a controller restart from waiting on an
// unrelated event.
func markOrDelete(ctx context.Context, c client.Client, s *corev1.Secret) (time.Duration, bool, error) {
	stamped, ok := s.Annotations[unreferencedSinceAnnotation]
	if !ok {
		if s.Annotations == nil {
			s.Annotations = map[string]string{}
		}
		s.Annotations[unreferencedSinceAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		if err := c.Update(ctx, s); err != nil && !errors.IsNotFound(err) {
			return 0, false, fmt.Errorf("marking %q unreferenced: %w", s.Name, err)
		}
		return envRetirementGrace, false, nil
	}

	since, err := time.Parse(time.RFC3339Nano, stamped)
	if err != nil {
		// An unreadable stamp is rewritten rather than trusted, which restarts
		// the grace. Deleting on the strength of a value nothing wrote is the
		// one outcome worth avoiding here.
		s.Annotations[unreferencedSinceAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
		if updErr := c.Update(ctx, s); updErr != nil && !errors.IsNotFound(updErr) {
			return 0, false, fmt.Errorf("rewriting the mark on %q: %w", s.Name, updErr)
		}
		return envRetirementGrace, false, nil
	}

	if left := envRetirementGrace - time.Since(since); left > 0 {
		return left, false, nil
	}
	return 0, true, nil
}

// clearUnreferencedMark removes a stamp from an environment something reads
// again, so the grace starts over rather than resuming where it left off.
func clearUnreferencedMark(ctx context.Context, c client.Client, s *corev1.Secret) error {
	if _, ok := s.Annotations[unreferencedSinceAnnotation]; !ok {
		return nil
	}
	delete(s.Annotations, unreferencedSinceAnnotation)
	if err := c.Update(ctx, s); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("clearing the mark on %q: %w", s.Name, err)
	}
	return nil
}
