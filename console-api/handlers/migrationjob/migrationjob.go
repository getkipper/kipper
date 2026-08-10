// Package migrationjob runs one-shot data-migration Jobs against
// Kipper-managed services. It is the framework layer beneath every per-
// type migration (postgres, mysql, mongodb, …) — each type plugs in by
// building a Spec for its dump+restore command. The package owns Job
// lifecycle (create, mirror source credentials, poll status, tear down).
//
// Why a separate package: keeps the Kubernetes-Job machinery isolated
// from the per-type business logic and from the HTTP handlers, so the
// handlers stay thin and the per-type builders stay testable as pure
// functions.
package migrationjob

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Spec describes a single migration Job. Per-type builders (postgres,
// mysql, …) construct this; the handler hands it to Submit.
type Spec struct {
	// Namespace is the target namespace where the Job runs. Data writes
	// land here; source data is read over cluster DNS.
	Namespace string

	// JobName is the Job object name. Must be unique per migration; the
	// caller is responsible for collision-free naming (typically
	// "migrate-<service>-<unix-ts>").
	JobName string

	// Image is the container image. Should match the target service's
	// version so the client tools speak the right protocol version.
	Image string

	// Command runs as the container's entrypoint. Typically a single
	// `sh -c "..."` invocation that pipes a dump tool into a restore
	// tool over network. Exit non-zero on any failure.
	Command []string

	// EnvFrom mounts already-existing secrets in the target namespace as
	// prefixed env vars. Use this for the target service's own
	// credentials.
	EnvFrom []corev1.EnvFromSource

	// Mirrors lists secrets to copy from another namespace into the
	// target namespace before the Job runs. The mirrored copies are
	// owned by the Job and GC'd when the Job is deleted (TTL).
	Mirrors []MirrorSpec

	// ActiveDeadlineSeconds caps how long the Job may run before K8s
	// kills it. Defaults to one hour.
	ActiveDeadlineSeconds *int64

	// TTLSecondsAfterFinished controls how long completed Jobs stick
	// around for status reads before K8s GCs them. Defaults to 1 hour.
	TTLSecondsAfterFinished *int32

	// Labels are added to the Job and its pods. The package adds its
	// own bookkeeping label (`kipper.run/migration=true`); callers
	// can add their own (e.g. `kipper.run/service=backend`).
	Labels map[string]string
}

// MirrorSpec copies a secret from one namespace to another. The mirrored
// copy is owned by the Job that uses it, so deleting the Job
// (post-TTL or manually) cascades to the mirror.
type MirrorSpec struct {
	SourceNamespace string
	SourceName      string
	// TargetName is what the mirrored secret is called in the target
	// namespace. The caller picks this so EnvFrom references match.
	TargetName string
}

// Phase mirrors Kubernetes Job conditions in a UI-friendlier shape.
type Phase string

const (
	PhasePending   Phase = "Pending"
	PhaseRunning   Phase = "Running"
	PhaseSucceeded Phase = "Succeeded"
	PhaseFailed    Phase = "Failed"
)

// Status is what the polling handler returns to the frontend.
type Status struct {
	JobName     string     `json:"job_name"`
	Phase       Phase      `json:"phase"`
	StartedAt   *time.Time `json:"started_at,omitempty"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
	Message     string     `json:"message,omitempty"`
	Logs        []string   `json:"logs,omitempty"`
}

// Submit creates the mirrored secrets and the Job. Returns the Job's
// resolved name (mirrors what was passed in unless K8s rewrote it).
//
// On any error during mirroring or Job creation, Submit attempts a best-
// effort rollback of whatever it managed to create. The caller treats
// the operation as atomic.
func Submit(ctx context.Context, client kubernetes.Interface, spec Spec) (string, error) {
	if spec.Namespace == "" || spec.JobName == "" || spec.Image == "" || len(spec.Command) == 0 {
		return "", fmt.Errorf("namespace, job name, image and command are required")
	}

	deadline := int64(60 * 60)
	if spec.ActiveDeadlineSeconds != nil {
		deadline = *spec.ActiveDeadlineSeconds
	}
	ttl := int32(60 * 60)
	if spec.TTLSecondsAfterFinished != nil {
		ttl = *spec.TTLSecondsAfterFinished
	}

	jobLabels := map[string]string{"kipper.run/migration": "true"}
	for k, v := range spec.Labels {
		jobLabels[k] = v
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      spec.JobName,
			Namespace: spec.Namespace,
			Labels:    jobLabels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            int32Ptr(0),
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: jobLabels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "migrate",
							Image:   spec.Image,
							Command: spec.Command,
							EnvFrom: spec.EnvFrom,
							// Dump/restore pipes stream row-by-row, so memory
							// stays flat; the limit is headroom for client
							// tools that buffer large rows.
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1000m"),
									corev1.ResourceMemory: resource.MustParse("1Gi"),
								},
							},
						},
					},
				},
			},
		},
	}

	createdJob, err := client.BatchV1().Jobs(spec.Namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("creating job: %w", err)
	}

	// Mirror source secrets into the target namespace, with OwnerRefs
	// pointing at the Job. K8s will GC the mirrors when the Job is
	// deleted (after TTL).
	for _, m := range spec.Mirrors {
		if err := mirrorSecret(ctx, client, m, createdJob); err != nil {
			// Roll back: delete the Job and any mirrors we created so far.
			_ = client.BatchV1().Jobs(spec.Namespace).Delete(ctx, createdJob.Name, metav1.DeleteOptions{})
			return "", fmt.Errorf("mirroring secret %s/%s: %w", m.SourceNamespace, m.SourceName, err)
		}
	}

	return createdJob.Name, nil
}

func mirrorSecret(ctx context.Context, client kubernetes.Interface, m MirrorSpec, job *batchv1.Job) error {
	src, err := client.CoreV1().Secrets(m.SourceNamespace).Get(ctx, m.SourceName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	mirror := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      m.TargetName,
			Namespace: job.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				"kipper.run/migration":         "true",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: "batch/v1",
					Kind:       "Job",
					Name:       job.Name,
					UID:        job.UID,
				},
			},
		},
		Type: src.Type,
		Data: copyByteMap(src.Data),
	}
	_, err = client.CoreV1().Secrets(job.Namespace).Create(ctx, mirror, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		// A previous run left it behind; overwrite. Owner ref attaches
		// it to the new Job so cleanup still works.
		existing, getErr := client.CoreV1().Secrets(job.Namespace).Get(ctx, m.TargetName, metav1.GetOptions{})
		if getErr != nil {
			return getErr
		}
		existing.Data = mirror.Data
		existing.OwnerReferences = mirror.OwnerReferences
		_, err = client.CoreV1().Secrets(job.Namespace).Update(ctx, existing, metav1.UpdateOptions{})
	}
	return err
}

// GetStatus reads the Job state and tails the most recent pod's logs.
// Returns a NotFound-friendly value when the Job has been TTL-cleaned.
func GetStatus(ctx context.Context, client kubernetes.Interface, namespace, jobName string) (Status, error) {
	job, err := client.BatchV1().Jobs(namespace).Get(ctx, jobName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return Status{JobName: jobName, Phase: PhaseSucceeded, Message: "job has been cleaned up"}, nil
	}
	if err != nil {
		return Status{}, err
	}

	status := Status{JobName: jobName}
	if job.Status.StartTime != nil {
		t := job.Status.StartTime.Time
		status.StartedAt = &t
	}
	if job.Status.CompletionTime != nil {
		t := job.Status.CompletionTime.Time
		status.CompletedAt = &t
	}

	switch {
	case job.Status.Succeeded > 0:
		status.Phase = PhaseSucceeded
	case hasFailedCondition(job):
		status.Phase = PhaseFailed
		status.Message = failedConditionMessage(job)
	case job.Status.Active > 0:
		status.Phase = PhaseRunning
	default:
		status.Phase = PhasePending
	}

	// Tail the last 50 lines from the most recent pod for visibility.
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("job-name=%s", jobName),
	})
	if err == nil && len(pods.Items) > 0 {
		latest := pods.Items[0]
		for i := range pods.Items {
			if pods.Items[i].CreationTimestamp.After(latest.CreationTimestamp.Time) {
				latest = pods.Items[i]
			}
		}
		logs, _ := readPodLogs(ctx, client, namespace, latest.Name, 50)
		status.Logs = logs
	}

	return status, nil
}

func hasFailedCondition(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func failedConditionMessage(job *batchv1.Job) string {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			if c.Message != "" {
				return c.Message
			}
			return c.Reason
		}
	}
	return "job failed"
}

func readPodLogs(ctx context.Context, client kubernetes.Interface, namespace, pod string, tail int64) ([]string, error) {
	req := client.CoreV1().Pods(namespace).GetLogs(pod, &corev1.PodLogOptions{TailLines: &tail})
	stream, err := req.Stream(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = stream.Close() }()

	var lines []string
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil && err != io.EOF {
		return lines, err
	}
	return lines, nil
}

func copyByteMap(in map[string][]byte) map[string][]byte {
	if in == nil {
		return nil
	}
	out := make(map[string][]byte, len(in))
	for k, v := range in {
		cp := make([]byte, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

func int32Ptr(v int32) *int32 { return &v }

// SuggestJobName builds a deterministic per-service Job name with a
// timestamp suffix. Callers can pass it as Spec.JobName.
func SuggestJobName(prefix string) string {
	return fmt.Sprintf("%s-%d", strings.ToLower(prefix), time.Now().Unix())
}
