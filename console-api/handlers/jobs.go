package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"strings"

	"github.com/go-chi/chi/v5"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/controller/pkg/capability"
)

// Jobs provides handlers for job and cronjob management.
type Jobs struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
	// Resources serves the job's own resource verbs, so what a job runs with is
	// read and written the same way an app's and a function's are.
	Resources *Resources
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
	Name string `json:"name"`
	Type string `json:"type"`
	// The namespace the job runs in, which is half of its identity: a name is
	// unique inside one and nowhere wider. Every client addresses a job through
	// this, and it also names the project whose capabilities decide what may be
	// offered for the row.
	Namespace string `json:"namespace"`
	Schedule  string `json:"schedule"`
	Last      string `json:"last"`
	Status    string `json:"status"`
	Image     string `json:"image"`
}

// resolveJob finds the Job a bare-name request means.
//
// The bare-name routes carry no namespace and a job name is unique only within
// one, so the candidate set is the jobs of that name the caller can see,
// narrowed to the ones the verb's capability admits. Exactly one is acted on.
// Nothing visible is 404, nothing eligible is 403, and more than one is 409
// naming the namespaces: choosing on the caller's behalf is how one project's
// job came to answer for another's.
func (j *Jobs) resolveJob(ctx context.Context, w http.ResponseWriter, r *http.Request, name string, required capability.Name) (*kipperv1.Job, bool) {
	var list kipperv1.JobList
	if err := j.CRClient.List(ctx, &list); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list jobs")
		return nil, false
	}

	var visible, eligible []kipperv1.Job
	for _, job := range list.Items {
		if job.Name != name || !canAccessNamespace(r, job.Namespace) {
			continue
		}
		visible = append(visible, job)
		if holdsCapability(r, job.Namespace, required) {
			eligible = append(eligible, job)
		}
	}

	switch {
	case len(visible) == 0:
		respondError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", name))
		return nil, false
	case len(eligible) == 0:
		respondError(w, http.StatusForbidden, "you do not have access to this project")
		return nil, false
	case len(eligible) > 1:
		namespaces := make([]string, 0, len(eligible))
		for _, job := range eligible {
			namespaces = append(namespaces, job.Namespace)
		}
		sort.Strings(namespaces)
		respondError(w, http.StatusConflict, fmt.Sprintf(
			"more than one project has a job called %q (%s), so this request does not say which one to act on; name the project",
			name, strings.Join(namespaces, ", ")))
		return nil, false
	}

	return &eligible[0], true
}

// jobInNamespace reads the Job a namespaced route names. The router has already
// resolved the project and enforced the capability, so this only has to find it.
func (j *Jobs) jobInNamespace(ctx context.Context, w http.ResponseWriter, namespace, name string) (*kipperv1.Job, bool) {
	var job kipperv1.Job
	if err := j.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &job); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", name))
			return nil, false
		}
		respondError(w, http.StatusInternalServerError, "failed to read job")
		return nil, false
	}
	return &job, true
}

// jobCronJob reads the CronJob the reconciler keeps for a scheduled job.
//
// Three answers, and they need different words. A nil CronJob with occupied
// false is nothing to act on, which is a one-off job and a job the reconciler
// has not caught up with. occupied true is a CronJob sitting on this job's key
// that belongs to something else, which is why that job will not reconcile and
// is a conflict the caller can fix. An error is a read that did not happen, and
// reading it as emptiness is how an outage comes to look like an unset field.
func (j *Jobs) jobCronJob(ctx context.Context, job *kipperv1.Job) (cronJob *batchv1.CronJob, occupied bool, err error) {
	cj, err := j.Client.BatchV1().CronJobs(job.Namespace).Get(ctx, job.Name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}

	// The name is not provenance, and neither is the managed-by label on its
	// own: a Function's cron trigger creates a CronJob called <function>-cron,
	// which is a name a Job may also have. The reconciler's own rule is the one
	// that separates them.
	if !controllers.JobOwnsChild(cj, job) {
		return nil, true, nil
	}

	return cj, false, nil
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
	if !enforceCapability(w, r, namespace, "kipper.write") {
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

	release, ok := reserveWorkloadName(ctx, w, j.CRClient, namespace, req.Name, "job")
	if !ok {
		return
	}

	if err := j.CRClient.Create(ctx, job); err != nil {
		// See Apps.Create: AlreadyExists proves the workload is there, so the
		// reservation just made is its own first claim and must stand.
		if !errors.IsAlreadyExists(err) {
			release()
		}
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
		Name:      req.Name,
		Type:      jobType,
		Namespace: namespace,
		Schedule:  req.Schedule,
		Status:    "pending",
		Image:     req.Image,
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
			Name:      job.Name,
			Type:      jobType,
			Namespace: job.Namespace,
			Schedule:  job.Spec.Schedule,
			Last:      last,
			Status:    status,
			Image:     job.Spec.Image,
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// History returns execution history for a named job.
// GET /api/v1/jobs/{name}/history
func (j *Jobs) History(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	job, ok := j.resolveJob(ctx, w, r, chi.URLParam(r, "name"), "kipper.read")
	if !ok {
		return
	}
	j.history(ctx, w, job)
}

// HistoryInNamespace returns execution history for a job named by its project.
// GET /api/v1/projects/{name}/jobs/{job}/history
func (j *Jobs) HistoryInNamespace(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	job, ok := j.jobInNamespace(ctx, w, chi.URLParam(r, "name"), chi.URLParam(r, "job"))
	if !ok {
		return
	}
	j.history(ctx, w, job)
}

func (j *Jobs) history(ctx context.Context, w http.ResponseWriter, job *kipperv1.Job) {
	runs, err := j.Client.BatchV1().Jobs(job.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s,%s=%s", job.Name, kipperLabel, kipperValue),
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list job history")
		return
	}

	result := make([]jobResponse, 0, len(runs.Items))
	for _, run := range runs.Items {
		result = append(result, jobResponse{
			Name:      run.Name,
			Type:      "job",
			Namespace: run.Namespace,
			Last:      timeSince(run.CreationTimestamp.Time),
			Status:    jobStatus(run),
		})
	}

	respondJSON(w, http.StatusOK, result)
}

// Trigger manually runs a scheduled job now by creating a Job from its template.
// POST /api/v1/jobs/{name}/trigger
func (j *Jobs) Trigger(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	job, ok := j.resolveJob(ctx, w, r, chi.URLParam(r, "name"), "kipper.write")
	if !ok {
		return
	}
	j.trigger(ctx, w, job)
}

// TriggerInNamespace manually runs a scheduled job named by its project.
// POST /api/v1/projects/{name}/jobs/{job}/trigger
func (j *Jobs) TriggerInNamespace(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	job, ok := j.jobInNamespace(ctx, w, chi.URLParam(r, "name"), chi.URLParam(r, "job"))
	if !ok {
		return
	}
	j.trigger(ctx, w, job)
}

func (j *Jobs) trigger(ctx context.Context, w http.ResponseWriter, job *kipperv1.Job) {
	// A one-off job has no CronJob behind it, so there is no schedule to run
	// ahead of and no template to copy.
	if job.Spec.Schedule == "" {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("job %q runs once and has no schedule to bring forward", job.Name))
		return
	}

	cj, occupied, err := j.jobCronJob(ctx, job)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read the job's schedule")
		return
	}
	if occupied {
		respondError(w, http.StatusConflict, occupiedChildMessage(job.Name))
		return
	}
	if cj == nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("cronjob %q not found", job.Name))
		return
	}

	backoff := int32(0)
	run := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      job.Name + "-manual-" + time.Now().Format("20060102-150405"),
			Namespace: job.Namespace,
			Labels: map[string]string{
				kipperLabel:               kipperValue,
				"kipper.run/job-type":     "job",
				"app":                     job.Name,
				"kipper.run/triggered-by": "console",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			Template:     *cj.Spec.JobTemplate.Spec.Template.DeepCopy(),
		},
	}

	if _, err := j.Client.BatchV1().Jobs(job.Namespace).Create(ctx, run, metav1.CreateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to trigger job")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "triggered", "job": run.Name})
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

// occupiedChildMessage says what a caller can do about a CronJob key another
// workload holds, which is the same thing stopping the job from reconciling.
func occupiedChildMessage(name string) string {
	return fmt.Sprintf("another workload already owns the cronjob %q in this namespace, so this job cannot reconcile; rename one of them", name)
}
