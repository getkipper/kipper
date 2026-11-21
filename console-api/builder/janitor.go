package builder

import (
	"context"
	"log"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// RunBuildJanitor sweeps build Jobs and Secrets immediately and every interval
// until ctx is canceled. The age threshold is the greater of maxAge and the
// configured build timeout plus one hour. Jobs are eligible once their start
// time is older than that threshold. Secrets older than the same threshold
// are eligible unless a retained Job owns or references them. Queued builds
// and their credentials are retained. This supplements finished-Job TTL cleanup
// for hung Jobs and orphaned credentials. The interval must be positive.
func RunBuildJanitor(ctx context.Context, client kubernetes.Interface, interval, maxAge time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		sweepBuildNamespace(ctx, client, maxAge)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func sweepBuildNamespace(ctx context.Context, client kubernetes.Interface, maxAge time.Duration) {
	// A build may run for its whole time limit, which the cluster can set
	// beyond maxAge.
	cutoff := time.Now().Add(-max(maxAge, buildTimeout()+time.Hour))
	selector := metav1.ListOptions{LabelSelector: buildLabel + "=true"}

	jobs, err := client.BatchV1().Jobs(buildsNamespace).List(ctx, selector)
	if err != nil {
		// Without the Jobs, a waiting build's credentials look orphaned.
		log.Printf("build janitor: listing jobs: %v", err)
		return
	}
	liveJobs := map[types.UID]bool{}
	inUse := map[string]bool{}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		// The Job controller sets StartTime when the queue releases the build,
		// so a queued build has none and the wait does not count.
		if job.Status.StartTime == nil || !job.Status.StartTime.Time.Before(cutoff) {
			liveJobs[job.UID] = true
			// Ownership is set after the Job is created, so a restart in
			// between leaves a secret the kept Job needs with no owner.
			for _, name := range referencedSecrets(&job.Spec.Template.Spec) {
				inUse[name] = true
			}
			continue
		}
		if err := client.BatchV1().Jobs(buildsNamespace).Delete(ctx, job.Name, metav1.DeleteOptions{
			PropagationPolicy: propagationBackground(),
		}); err != nil {
			log.Printf("build janitor: deleting job %s: %v", job.Name, err)
		}
	}

	// Sweep ephemeral secrets too: ownerRef GC covers the ones whose Job still
	// exists, but a secret whose Job never got created is only cleaned up here.
	secrets, err := client.CoreV1().Secrets(buildsNamespace).List(ctx, selector)
	if err != nil {
		log.Printf("build janitor: listing secrets: %v", err)
		return
	}
	for i := range secrets.Items {
		secret := &secrets.Items[i]
		if secret.CreationTimestamp.Time.Before(cutoff) && !inUse[secret.Name] && !ownedByLiveJob(secret, liveJobs) {
			if err := client.CoreV1().Secrets(buildsNamespace).Delete(ctx, secret.Name, metav1.DeleteOptions{}); err != nil {
				log.Printf("build janitor: deleting secret %s: %v", secret.Name, err)
			}
		}
	}
}

// ownedByLiveJob reports whether a secret belongs to a build Job the sweep
// keeps.
func ownedByLiveJob(secret *corev1.Secret, liveJobs map[types.UID]bool) bool {
	for _, ref := range secret.OwnerReferences {
		if ref.Kind == "Job" && liveJobs[ref.UID] {
			return true
		}
	}
	return false
}

// referencedSecrets returns Secret volume names and Secret references in the
// environment of regular and init containers. It preserves duplicates and omits
// projected volumes and image pull secrets.
func referencedSecrets(spec *corev1.PodSpec) []string {
	var names []string
	for _, v := range spec.Volumes {
		if v.Secret != nil {
			names = append(names, v.Secret.SecretName)
		}
	}
	for _, c := range append(append([]corev1.Container{}, spec.InitContainers...), spec.Containers...) {
		for _, e := range c.Env {
			if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil {
				names = append(names, e.ValueFrom.SecretKeyRef.Name)
			}
		}
		for _, from := range c.EnvFrom {
			if from.SecretRef != nil {
				names = append(names, from.SecretRef.Name)
			}
		}
	}
	return names
}
