// Package job provides the kip CLI's view of jobs and scheduled tasks.
//
// Jobs are modelled as Kipper Job CRs (jobs.kipper.run). The CLI is
// a thin client: it builds a CR from the user's flags, applies it via
// the dynamic client, and lets the JobReconciler in the console-api do
// the work of creating native batchv1 CronJobs (for scheduled jobs) or
// batchv1 Jobs (for one-off runs).
package job

import (
	"context"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

const (
	kipperLabel = "app.kubernetes.io/managed-by"
	kipperValue = "kipper"
)

// Options describes a job to create.
type Options struct {
	Name      string
	Namespace string
	Image     string
	Command   []string
	Schedule  string // cron expression, empty for one-off
}

// Status holds summary info for a job as displayed in `kip job list`.
type Status struct {
	Name     string
	Type     string // "cronjob" or "job"
	Schedule string
	Last     string
	Status   string
}

// Manager creates and manages jobs through the Job CR.
//
// Scheduled jobs (Schedule set) are reconciled into a native CronJob by
// the JobReconciler in the console-api. One-off runs (Schedule empty)
// are reconciled into a single native Job. Native Job listing uses the
// typed Clientset because batchv1 is a built-in K8s resource and the
// CLI already has it in hand for log streaming and history.
type Manager struct {
	Dynamic dynamic.Interface
	Client  kubernetes.Interface
}

// Run creates a one-off job by writing a Job CR with no schedule. The
// CR name is suffixed with a timestamp so multiple runs of the same
// task each get their own audit record.
func (m *Manager) Run(ctx context.Context, opts Options) (string, error) {
	if m.Dynamic == nil {
		return "", fmt.Errorf("job manager is not configured with a dynamic client")
	}
	runName := opts.Name + "-" + time.Now().Format("20060102-150405")
	cr := buildJobCR(runName, opts.Namespace, "", opts.Image, opts.Command, opts.Name)
	if err := m.applyCR(ctx, cr); err != nil {
		return "", err
	}
	return runName, nil
}

// Schedule creates a scheduled Job CR. Existing CRs of the same name are
// updated in place rather than failing.
func (m *Manager) Schedule(ctx context.Context, opts Options) error {
	if m.Dynamic == nil {
		return fmt.Errorf("job manager is not configured with a dynamic client")
	}
	if opts.Schedule == "" {
		return fmt.Errorf("schedule is required for scheduled jobs")
	}
	cr := buildJobCR(opts.Name, opts.Namespace, opts.Schedule, opts.Image, opts.Command, opts.Name)
	return m.applyCR(ctx, cr)
}

// List returns all jobs in a namespace by reading Job CRs.
func (m *Manager) List(ctx context.Context, namespace string) ([]Status, error) {
	if m.Dynamic == nil {
		return nil, fmt.Errorf("job manager is not configured with a dynamic client")
	}
	crList, err := m.Dynamic.Resource(manifest.JobGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing job CRs: %w", err)
	}

	result := make([]Status, 0, len(crList.Items))
	for i := range crList.Items {
		result = append(result, jobStatusFromCR(&crList.Items[i]))
	}
	return result, nil
}

// History returns recent job executions for a scheduled job by listing
// native Jobs labeled with the parent app name.
func (m *Manager) History(ctx context.Context, namespace, name string) ([]Status, error) {
	if m.Client == nil {
		return nil, fmt.Errorf("job manager is not configured with a clientset")
	}
	jobs, err := m.Client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s,%s=%s", name, kipperLabel, kipperValue),
	})
	if err != nil {
		return nil, fmt.Errorf("listing job history: %w", err)
	}

	result := make([]Status, 0, len(jobs.Items))
	for _, j := range jobs.Items {
		result = append(result, Status{
			Name:   j.Name,
			Type:   "job",
			Last:   time.Since(j.CreationTimestamp.Time).Truncate(time.Second).String() + " ago",
			Status: nativeJobStatus(j),
		})
	}
	return result, nil
}

// Delete removes the Job CR. The controller's finalizer cascades the
// deletion to all owned workloads (CronJob, Job).
func (m *Manager) Delete(ctx context.Context, namespace, name string) error {
	if m.Dynamic == nil {
		return fmt.Errorf("job manager is not configured with a dynamic client")
	}
	err := m.Dynamic.Resource(manifest.JobGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if errors.IsNotFound(err) {
		return fmt.Errorf("job %q not found", name)
	}
	return err
}

// applyCR creates the CR or, if it already exists, updates it in place.
func (m *Manager) applyCR(ctx context.Context, cr *unstructured.Unstructured) error {
	gvr := manifest.JobGVR
	ns := cr.GetNamespace()
	name := cr.GetName()
	_, err := m.Dynamic.Resource(gvr).Namespace(ns).Create(ctx, cr, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		existing, getErr := m.Dynamic.Resource(gvr).Namespace(ns).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("getting existing job: %w", getErr)
		}
		cr.SetResourceVersion(existing.GetResourceVersion())
		_, err = m.Dynamic.Resource(gvr).Namespace(ns).Update(ctx, cr, metav1.UpdateOptions{})
	}
	if err != nil {
		return fmt.Errorf("applying job CR: %w", err)
	}
	return nil
}

// buildJobCR maps Manager.Options to the JobSpec map. Must stay aligned
// with JobSpec in console-api/api/v1alpha1/job_types.go.
func buildJobCR(name, namespace, schedule, image string, command []string, app string) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"image": image,
	}
	if schedule != "" {
		spec["schedule"] = schedule
	}
	if len(command) > 0 {
		c := make([]interface{}, 0, len(command))
		for _, p := range command {
			c = append(c, p)
		}
		spec["command"] = c
	}

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Job",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
				"labels": map[string]interface{}{
					kipperLabel: kipperValue,
					"app":       app,
				},
			},
			"spec": spec,
		},
	}
}

// jobStatusFromCR derives the CLI's display status from a Job CR.
func jobStatusFromCR(cr *unstructured.Unstructured) Status {
	schedule, _, _ := unstructured.NestedString(cr.Object, "spec", "schedule")
	jobType := "job"
	if schedule != "" {
		jobType = "cronjob"
	}

	phase, _, _ := unstructured.NestedString(cr.Object, "status", "phase")
	if phase == "" {
		phase = "Pending"
	}

	last := "never"
	if lastRun, found, _ := unstructured.NestedString(cr.Object, "status", "lastRun"); found && lastRun != "" {
		if t, err := time.Parse(time.RFC3339, lastRun); err == nil {
			last = time.Since(t).Truncate(time.Second).String() + " ago"
		}
	}

	return Status{
		Name:     cr.GetName(),
		Type:     jobType,
		Schedule: schedule,
		Last:     last,
		Status:   strings.ToLower(phase),
	}
}

func nativeJobStatus(j batchv1.Job) string {
	if j.Status.Succeeded > 0 {
		return "completed"
	}
	if j.Status.Failed > 0 {
		return "failed"
	}
	return "running"
}
