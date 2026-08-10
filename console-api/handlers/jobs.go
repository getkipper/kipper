package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"strings"

	"github.com/go-chi/chi/v5"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
)

// Jobs provides handlers for job and cronjob management.
type Jobs struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
}

type createJobRequest struct {
	Name          string `json:"name"`
	Image         string `json:"image"`
	Command       string `json:"command"`
	Schedule      string `json:"schedule"`
	Namespace     string `json:"namespace"`
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

type jobResponse struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Schedule string `json:"schedule"`
	Last     string `json:"last"`
	Status   string `json:"status"`
	Image    string `json:"image"`
}

// Create creates a new job or scheduled job.
// POST /api/v1/jobs
func (j *Jobs) Create(w http.ResponseWriter, r *http.Request) {
	var req createJobRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || req.Image == "" {
		respondError(w, http.StatusBadRequest, "name and image are required")
		return
	}

	namespace := req.Namespace
	if namespace == "" {
		namespace = "default"
	}
	if !enforceProjectRole(w, r, namespace, middleware.ProjectRoleDeployer) {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var command []string
	if req.Command != "" {
		command = strings.Fields(req.Command)
	}

	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: namespace,
			Labels: map[string]string{
				"app":       req.Name,
				kipperLabel: kipperValue,
			},
		},
		Spec: kipperv1.JobSpec{
			Image:    req.Image,
			Schedule: req.Schedule,
			Command:  command,
			Resources: kipperv1.JobResources{
				CPURequest:    req.CPURequest,
				CPULimit:      req.CPULimit,
				MemoryRequest: req.MemoryRequest,
				MemoryLimit:   req.MemoryLimit,
			},
		},
	}

	if err := j.CRClient.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			respondError(w, http.StatusConflict, fmt.Sprintf("job %q already exists", req.Name))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to create job")
		return
	}

	jobType := "job"
	if req.Schedule != "" {
		jobType = "cronjob"
	}

	respondJSON(w, http.StatusCreated, jobResponse{
		Name:     req.Name,
		Type:     jobType,
		Schedule: req.Schedule,
		Status:   "pending",
		Image:    req.Image,
	})
}

// List returns all Kipper-managed jobs.
// GET /api/v1/jobs
func (j *Jobs) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Read from Job CRs
	var jobList kipperv1.JobList
	if err := j.CRClient.List(ctx, &jobList); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}

	result := make([]jobResponse, 0, len(jobList.Items))
	for _, job := range jobList.Items {
		// Only surface jobs in projects the caller belongs to.
		if !canAccessNamespace(r, job.Namespace) {
			continue
		}

		jobType := "job"
		if job.Spec.Schedule != "" {
			jobType = "cronjob"
		}

		status := job.Status.Phase
		if status == "" {
			status = "pending"
		}

		last := "never"
		if job.Status.LastRun != nil {
			last = timeSince(job.Status.LastRun.Time)
		}

		result = append(result, jobResponse{
			Name:     job.Name,
			Type:     jobType,
			Schedule: job.Spec.Schedule,
			Last:     last,
			Status:   status,
			Image:    job.Spec.Image,
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// History returns execution history for a named job.
// GET /api/v1/jobs/{name}/history
func (j *Jobs) History(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	jobs, err := j.Client.BatchV1().Jobs("").List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s,%s=%s", name, kipperLabel, kipperValue),
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list job history")
		return
	}

	var result []jobResponse
	for _, job := range jobs.Items {
		// Only include runs from projects the caller belongs to.
		if !canAccessNamespace(r, job.Namespace) {
			continue
		}
		result = append(result, jobResponse{
			Name:   job.Name,
			Type:   "job",
			Last:   timeSince(job.CreationTimestamp.Time),
			Status: jobStatus(job),
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// Trigger manually runs a cronjob now by creating a Job from its template.
// POST /api/v1/jobs/{name}/trigger
func (j *Jobs) Trigger(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Find the cronjob across all namespaces
	cronJobs, err := j.Client.BatchV1().CronJobs("").List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s,%s=%s", name, kipperLabel, kipperValue),
	})
	if err != nil || len(cronJobs.Items) == 0 {
		respondError(w, http.StatusNotFound, fmt.Sprintf("cronjob %q not found", name))
		return
	}

	// Names are unique per namespace, so two projects can each own a cronjob
	// with this name. Pick the one in a project the caller belongs to.
	var cj *batchv1.CronJob
	for i := range cronJobs.Items {
		if canAccessNamespace(r, cronJobs.Items[i].Namespace) {
			cj = &cronJobs.Items[i]
			break
		}
	}
	if cj == nil {
		respondError(w, http.StatusForbidden, "you do not have access to this job")
		return
	}
	if !enforceProjectRole(w, r, cj.Namespace, middleware.ProjectRoleDeployer) {
		return
	}
	backoff := int32(0)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name + "-manual-" + time.Now().Format("20060102-150405"),
			Namespace: cj.Namespace,
			Labels: map[string]string{
				kipperLabel:               kipperValue,
				"kipper.run/job-type":     "job",
				"app":                     name,
				"kipper.run/triggered-by": "console",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template:     *cj.Spec.JobTemplate.Spec.Template.DeepCopy(),
		},
	}

	if _, err := j.Client.BatchV1().Jobs(cj.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to trigger job")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "triggered", "job": job.Name})
}

func jobStatus(j batchv1.Job) string {
	if j.Status.Succeeded > 0 {
		return "completed"
	}
	if j.Status.Failed > 0 {
		return "failed"
	}
	return "running"
}

func timeSince(t time.Time) string {
	d := time.Since(t).Truncate(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
