package handlers

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
	quotapkg "github.com/getkipper/kipper/console-api/quota"
)

// Resources provides handlers for managing app CPU and memory limits.
type Resources struct {
	Client      kubernetes.Interface
	CRClient    crclient.Client
	Adjustments *Adjustments
}

type resourcesResponse struct {
	MemoryLimit   string `json:"memory_limit"`
	MemoryRequest string `json:"memory_request"`
	CPULimit      string `json:"cpu_limit"`
	CPURequest    string `json:"cpu_request"`
	// CPU and Memory say who set each value, how Kipper sizes the resource,
	// what the container runs with and what the auto-sizer recommends.
	CPU    *resourceDetail `json:"cpu,omitempty"`
	Memory *resourceDetail `json:"memory,omitempty"`
	// PartialEdits tells clients they can omit an entire CPU or memory pair.
	// Older servers replace all four values on every PUT.
	PartialEdits bool `json:"partial_edits"`
}

// resourcesRequest edits CPU and memory independently. Omitting both fields
// for a resource preserves it. A single nonempty value sets a fixed size;
// sending an empty pair returns that resource to automatic sizing.
type resourcesRequest struct {
	MemoryRequest *string `json:"memory_request"`
	MemoryLimit   *string `json:"memory_limit"`
	CPURequest    *string `json:"cpu_request"`
	CPULimit      *string `json:"cpu_limit"`
}

// pairEdit returns nil for an omitted resource, clears an empty pair,
// and mirrors a single value to set a fixed size.
func pairEdit(req, lim *string) *resourcebounds.PairEdit {
	if req == nil && lim == nil {
		return nil
	}
	r, l := pairOrPassThrough(deref(req), deref(lim))
	if r == "" {
		return &resourcebounds.PairEdit{Clear: true}
	}
	return &resourcebounds.PairEdit{Request: r, Limit: l}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// setValues returns an edit's request and limit, empty when it leaves the
// resource alone or clears it.
func setValues(e *resourcebounds.PairEdit) (string, string) {
	if e == nil || e.Clear {
		return "", ""
	}
	return e.Request, e.Limit
}

// ResourceKind picks which CR type the handler operates on. Apps and
// functions share the same wire format but live on different CRs, so
// the handler routes by kind to the right Get/Update path.
type ResourceKind string

const (
	ResourceKindApp      ResourceKind = "app"
	ResourceKindFunction ResourceKind = "function"
	ResourceKindJob      ResourceKind = "job"
)

// GetByParam returns a handler that reads the resource name from the
// given URL param and queries the matching CR kind.
func (res *Resources) GetByParam(param string, kind ResourceKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res.getResources(w, r, chi.URLParam(r, "name"), chi.URLParam(r, param), kind)
	}
}

// UpdateByParam returns a handler that reads the resource name from
// the given URL param and updates the matching CR kind.
func (res *Resources) UpdateByParam(param string, kind ResourceKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res.updateResources(w, r, chi.URLParam(r, "name"), chi.URLParam(r, param), kind)
	}
}

// Get returns current resource limits for an app.
func (res *Resources) Get(w http.ResponseWriter, r *http.Request) {
	res.getResources(w, r, chi.URLParam(r, "name"), chi.URLParam(r, "app"), ResourceKindApp)
}

// Update sets resource limits for an app.
func (res *Resources) Update(w http.ResponseWriter, r *http.Request) {
	res.updateResources(w, r, chi.URLParam(r, "name"), chi.URLParam(r, "app"), ResourceKindApp)
}

func (res *Resources) getResources(w http.ResponseWriter, r *http.Request, project, name string, kind ResourceKind) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	resp, err := res.readResources(ctx, project, name, kind)
	if err != nil {
		if errors.IsNotFound(err) {
			// A job route names its CR outright, so a missing one is a missing
			// job. For the others an absent CR still reads as nothing set,
			// which is the answer those screens have always had.
			if kind == ResourceKindJob {
				respondError(w, http.StatusNotFound, fmt.Sprintf("job %q not found", name))
				return
			}
			respondJSON(w, http.StatusOK, resourcesResponse{PartialEdits: true})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get resources")
		return
	}
	res.describeWorkload(ctx, project, name, kind, &resp)
	resp.PartialEdits = true
	respondJSON(w, http.StatusOK, resp)
}

func (res *Resources) updateResources(w http.ResponseWriter, r *http.Request, project, name string, kind ResourceKind) {
	var req resourcesRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateResourceQuantities(req); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateRequestWithinLimit(req); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Capture previous values so the telemetry log can show "from" → "to".
	previous, _ := res.readResources(ctx, project, name, kind)

	edits := resourcebounds.Edits{
		CPU:    pairEdit(req.CPURequest, req.CPULimit),
		Memory: pairEdit(req.MemoryRequest, req.MemoryLimit),
	}
	_, cpuLim := setValues(edits.CPU)
	_, memLim := setValues(edits.Memory)

	// Check the projected rollout against quota before saving the edit. App
	// and Function Deployments preserve omitted resources at their live size.
	// Jobs have no steady Deployment; admission checks their transient pods.
	if kind != ResourceKindJob {
		var live corev1.ResourceRequirements
		if deploy, err := res.Client.AppsV1().Deployments(project).Get(ctx, name, metav1.GetOptions{}); err == nil && len(deploy.Spec.Template.Spec.Containers) > 0 {
			live = deploy.Spec.Template.Spec.Containers[0].Resources
		}
		change := projectedChange(edits, live)
		if pf, err := quotapkg.PreflightDeployment(ctx, res.Client, project, name, change); err == nil && !pf.Fits {
			respondError(w, http.StatusConflict, fmt.Sprintf("resource change needs %s of %s but the namespace quota caps at %s; raise the project tier or environment quota, or reduce other workloads", pf.Projected, pf.Dimension, pf.Hard))
			return
		}
	}

	if err := res.writeResources(ctx, project, name, kind, edits); err != nil {
		if stderrors.Is(err, errJobRunsOnce) {
			respondError(w, http.StatusConflict, fmt.Sprintf("job %q runs once and uses the resources it was created with; create a new job to run it with different ones", name))
			return
		}
		if errors.IsConflict(err) {
			respondError(w, http.StatusConflict, fmt.Sprintf("this %s changed while the limits were being saved; read it again and reapply them", kind))
			return
		}
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, string(kind)+" not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to update resources")
		return
	}

	scope := adjustmentScope(kind)
	subject := SubjectFromRequest(r)
	if memLim != "" {
		res.Adjustments.Record(ctx, scope, project, name, "memory",
			previous.MemoryLimit, memLim, "", subject)
	}
	if cpuLim != "" {
		res.Adjustments.Record(ctx, scope, project, name, "cpu",
			previous.CPULimit, cpuLim, "", subject)
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// errJobRunsOnce rejects resource edits to one-off jobs. Their native Job
// template is created once; accepting an edit cannot guarantee it reaches the run.
var errJobRunsOnce = stderrors.New("a one-off job runs with the resources it was created with")

// adjustmentScope is the scope a resource change is recorded under. The values
// are a closed set in the ResourceAdjustment CRD, so a kind added here without
// the schema is a write the API server refuses.
func adjustmentScope(kind ResourceKind) string {
	switch kind {
	case ResourceKindFunction:
		return "function"
	case ResourceKindJob:
		return "job"
	default:
		return "app"
	}
}

// readResources collapses the CR-specific GET path into one place so
// the kind switch happens in exactly one spot. Returns the canonical
// response shape regardless of which CR backed it.
func (res *Resources) readResources(ctx context.Context, project, name string, kind ResourceKind) (resourcesResponse, error) {
	switch kind {
	case ResourceKindJob:
		var job kipperv1.Job
		if err := res.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: name}, &job); err != nil {
			return resourcesResponse{}, err
		}
		// jobResources in the reconciler falls back to the same pair, so an
		// unpinned job reads as what it will actually run with.
		cpuReq, cpuLim := controllers.ResolveResourcePair(job.Spec.Resources.CPURequest, job.Spec.Resources.CPULimit, jobDefaultCPU, jobDefaultCPU)
		memReq, memLim := controllers.ResolveResourcePair(job.Spec.Resources.MemoryRequest, job.Spec.Resources.MemoryLimit, jobDefaultMemory, jobDefaultMemory)
		return resourcesResponse{
			MemoryLimit: memLim, MemoryRequest: memReq,
			CPULimit: cpuLim, CPURequest: cpuReq,
		}, nil
	case ResourceKindFunction:
		var fn kipperv1.Function
		if err := res.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: name}, &fn); err != nil {
			return resourcesResponse{}, err
		}
		// Functions use a default profile of "lightweight" since they're
		// scale-to-zero and tend to be small. Resolve the same way the
		// function reconciler does so the response matches what runs: a
		// one-sided override mirrors to the other side.
		dCPUReq, dCPULim, dMemReq, dMemLim := functionDefaults()
		cpuReq, cpuLim := controllers.ResolveResourcePair(fn.Spec.Resources.CPURequest, fn.Spec.Resources.CPULimit, dCPUReq, dCPULim)
		memReq, memLim := controllers.ResolveResourcePair(fn.Spec.Resources.MemoryRequest, fn.Spec.Resources.MemoryLimit, dMemReq, dMemLim)
		return resourcesResponse{
			MemoryLimit: memLim, MemoryRequest: memReq,
			CPULimit: cpuLim, CPURequest: cpuReq,
		}, nil
	default:
		var app kipperv1.App
		if err := res.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: name}, &app); err != nil {
			return resourcesResponse{}, err
		}
		// Resolve the same way the app reconciler does so the response
		// matches what runs: a one-sided override mirrors to the other side,
		// and an unset field falls back to the profile baseline.
		pCPUReq, pCPULim, pMemReq, pMemLim := profileResources(app.Spec.Resources.Profile)
		cpuReq, cpuLim := controllers.ResolveResourcePair(app.Spec.Resources.CPURequest, app.Spec.Resources.CPULimit, pCPUReq, pCPULim)
		memReq, memLim := controllers.ResolveResourcePair(app.Spec.Resources.MemoryRequest, app.Spec.Resources.MemoryLimit, pMemReq, pMemLim)
		return resourcesResponse{
			MemoryLimit: memLim, MemoryRequest: memReq,
			CPULimit: cpuLim, CPURequest: cpuReq,
		}, nil
	}
}

// writeResources applies the edits. App and Function values are written as the
// console's own field manager, which is what marks them as the user's bounds
// for the auto-sizer.
func (res *Resources) writeResources(ctx context.Context, project, name string, kind ResourceKind, edits resourcebounds.Edits) error {
	switch kind {
	case ResourceKindJob:
		var job kipperv1.Job
		if err := res.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: name}, &job); err != nil {
			return err
		}
		// Decided on the object that is about to be written, and written back
		// carrying its resourceVersion, so a job that becomes a one-off between
		// the two cannot slip through as a scheduled one.
		if job.Spec.Schedule == "" {
			return errJobRunsOnce
		}
		if edits.CPU != nil {
			job.Spec.Resources.CPURequest, job.Spec.Resources.CPULimit = setValues(edits.CPU)
		}
		if edits.Memory != nil {
			job.Spec.Resources.MemoryRequest, job.Spec.Resources.MemoryLimit = setValues(edits.Memory)
		}
		return res.CRClient.Update(ctx, &job)
	case ResourceKindFunction:
		return resourcebounds.WriteQuantities(ctx, res.CRClient, &kipperv1.Function{}, project, name, resourcebounds.ConsoleManager, edits)
	default:
		if sets(edits.CPU) || sets(edits.Memory) {
			edits.Profile = "custom"
		}
		return resourcebounds.WriteQuantities(ctx, res.CRClient, &kipperv1.App{}, project, name, resourcebounds.ConsoleManager, edits)
	}
}

func sets(e *resourcebounds.PairEdit) bool {
	return e != nil && !e.Clear
}

// The job reconciler's fallback when nothing is pinned, from jobResources.
const (
	jobDefaultCPU    = "100m"
	jobDefaultMemory = "128Mi"
)

// functionDefaults mirrors the controller's default for a Function
// container when no resources are set explicitly. Keep these in sync
// with controllers/function_controller.go::functionResources.
func functionDefaults() (cpuReq, cpuLim, memReq, memLim string) {
	return "50m", "50m", "64Mi", "64Mi"
}

// profileResources returns the (cpuRequest, cpuLimit, memoryRequest, memoryLimit)
// for a profile. The jvm profile is burstable: low request, high limit so the
// node doesn't have to reserve a full core that's only needed during cold-start
// JIT.
func profileResources(profile string) (cpuReq, cpuLim, memReq, memLim string) {
	switch profile {
	case "lightweight":
		return "50m", "50m", "64Mi", "64Mi"
	case "standard":
		return "100m", "100m", "128Mi", "128Mi"
	case "compute-heavy":
		return "500m", "500m", "256Mi", "256Mi"
	case "memory-heavy":
		return "100m", "100m", "512Mi", "512Mi"
	case "jvm":
		return "100m", "1000m", "2Gi", "2Gi"
	default:
		return "100m", "100m", "128Mi", "128Mi"
	}
}

// validateResourceQuantities rejects unparseable CPU/memory strings before they
// reach a CR or container spec. The reconcilers and applyResources call
// resource.MustParse on these values, so an unparseable string would panic the
// reconcile (wedging the workload) or the request. Empty fields mean "unset"
// and are left for the caller to handle.
func validateResourceQuantities(req resourcesRequest) error {
	for _, f := range []struct {
		name string
		raw  string
	}{
		{"cpu_request", deref(req.CPURequest)},
		{"cpu_limit", deref(req.CPULimit)},
		{"memory_request", deref(req.MemoryRequest)},
		{"memory_limit", deref(req.MemoryLimit)},
	} {
		if f.raw == "" {
			continue
		}
		q, err := resource.ParseQuantity(f.raw)
		if err != nil {
			return fmt.Errorf("invalid %s %q", f.name, f.raw)
		}
		// ParseQuantity accepts negatives, which are meaningless for a request
		// or limit and would only fail later at pod admission.
		if q.Sign() < 0 {
			return fmt.Errorf("%s cannot be negative: %q", f.name, f.raw)
		}
	}
	return nil
}

// projectedChange estimates the allocation after the edit for quota checks.
// Bounds clamp the live request, so lowering a floor alone does not count as
// a reduction. Omitted or cleared resources keep their live size.
func projectedChange(edits resourcebounds.Edits, live corev1.ResourceRequirements) quotapkg.Change {
	var ch quotapkg.Change
	ch.CPURequest, ch.CPULimit = projectedPair(edits.CPU, live, corev1.ResourceCPU)
	ch.MemoryRequest, ch.MemoryLimit = projectedPair(edits.Memory, live, corev1.ResourceMemory)
	return ch
}

func projectedPair(e *resourcebounds.PairEdit, live corev1.ResourceRequirements, name corev1.ResourceName) (string, string) {
	if e == nil || e.Clear {
		return "", ""
	}
	req, errReq := resource.ParseQuantity(e.Request)
	lim, errLim := resource.ParseQuantity(e.Limit)
	if errReq != nil || errLim != nil {
		return e.Request, e.Limit
	}
	livePair, ok := resourcebounds.PairOf(&live, name)
	if !ok {
		livePair = resourcebounds.Pair{Request: req, Limit: lim}
	}
	p, _ := resourcebounds.Resolve(
		resourcebounds.Quantity{Value: req, Source: resourcebounds.User},
		resourcebounds.Quantity{Value: lim, Source: resourcebounds.User},
		nil, livePair, resourcebounds.AutoRange{})
	return p.Request.String(), p.Limit.String()
}

// validateRequestWithinLimit rejects a request above its limit: the request is
// the floor the auto-sizer keeps to and the limit the ceiling.
func validateRequestWithinLimit(req resourcesRequest) error {
	for _, p := range []struct {
		name     string
		req, lim *string
	}{
		{"cpu", req.CPURequest, req.CPULimit},
		{"memory", req.MemoryRequest, req.MemoryLimit},
	} {
		if deref(p.req) == "" || deref(p.lim) == "" {
			continue
		}
		r, l := resource.MustParse(*p.req), resource.MustParse(*p.lim)
		if r.Cmp(l) > 0 {
			return fmt.Errorf("%s_request %s is above %s_limit %s; the request is the floor and the limit the ceiling", p.name, *p.req, p.name, *p.lim)
		}
	}
	return nil
}

// pairOrPassThrough mirrors a single value to both sides if the other is empty.
// Both empty stays both empty (caller decides whether to clear or skip).
func pairOrPassThrough(req, lim string) (string, string) {
	switch {
	case req == "" && lim == "":
		return "", ""
	case req == "":
		return lim, lim
	case lim == "":
		return req, req
	default:
		return req, lim
	}
}
