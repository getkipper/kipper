package builder

import (
	"context"
	"log"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// RunBuildJanitor deletes build-labeled Jobs and Secrets older than maxAge.
// It supplements finished-Job TTL cleanup by covering hung Jobs and orphaned
// credentials. Age uses creationTimestamp, so cleanup survives restarts.
// Set maxAge beyond the longest legitimate build; active Jobs are eligible too.
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
	cutoff := time.Now().Add(-maxAge)
	selector := metav1.ListOptions{LabelSelector: buildLabel + "=true"}

	jobs, err := client.BatchV1().Jobs(buildsNamespace).List(ctx, selector)
	if err != nil {
		log.Printf("build janitor: listing jobs: %v", err)
	} else {
		for i := range jobs.Items {
			if jobs.Items[i].CreationTimestamp.Time.Before(cutoff) {
				if err := client.BatchV1().Jobs(buildsNamespace).Delete(ctx, jobs.Items[i].Name, metav1.DeleteOptions{
					PropagationPolicy: propagationBackground(),
				}); err != nil {
					log.Printf("build janitor: deleting job %s: %v", jobs.Items[i].Name, err)
				}
			}
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
		if secrets.Items[i].CreationTimestamp.Time.Before(cutoff) {
			if err := client.CoreV1().Secrets(buildsNamespace).Delete(ctx, secrets.Items[i].Name, metav1.DeleteOptions{}); err != nil {
				log.Printf("build janitor: deleting secret %s: %v", secrets.Items[i].Name, err)
			}
		}
	}
}
