package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
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
}

type resourcesRequest struct {
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
}

// ResourceKind picks which CR type the handler operates on. Apps and
// functions share the same wire format but live on different CRs, so
// the handler routes by kind to the right Get/Update path.
type ResourceKind string

const (
	ResourceKindApp      ResourceKind = "app"
	ResourceKindFunction ResourceKind = "function"
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
			respondJSON(w, http.StatusOK, resourcesResponse{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get resources")
		return
	}
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

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Capture previous values so the telemetry log can show "from" → "to".
	previous, _ := res.readResources(ctx, project, name, kind)

	// If the client sent only a limit, mirror it to the request (and vice versa)
	// so a single-value setting still produces Guaranteed QoS as before.
	cpuReq, cpuLim := pairOrPassThrough(req.CPURequest, req.CPULimit)
	memReq, memLim := pairOrPassThrough(req.MemoryRequest, req.MemoryLimit)

	// Preflight the change against the namespace quota with the same projection
	// the auto resource controller uses, so an over-quota request gets a
	// deterministic 409 here instead of a rollout that wedges at admission. The
	// App and Function reconcilers both back the workload with a Deployment
	// named after the CR.
	change := quotapkg.Change{CPURequest: cpuReq, CPULimit: cpuLim, MemoryRequest: memReq, MemoryLimit: memLim}
	if kind == ResourceKindFunction {
		// The Function reconciler replaces the pod template and defaults any
		// unpinned dimension (functionResources), so an omitted dimension
		// reconciles to the function default rather than the live value. The
		// App reconciler preserves unpinned live values, so its omitted
		// dimensions correctly project as unchanged. Fill the function defaults
		// here so a CPU-only change still accounts for the memory the rollout
		// will actually request.
		dCPUReq, dCPULim, dMemReq, dMemLim := functionDefaults()
		if change.CPURequest == "" {
			change.CPURequest = dCPUReq
		}
		if change.CPULimit == "" {
			change.CPULimit = dCPULim
		}
		if change.MemoryRequest == "" {
			change.MemoryRequest = dMemReq
		}
		if change.MemoryLimit == "" {
			change.MemoryLimit = dMemLim
		}
	}
	if pf, err := quotapkg.PreflightDeployment(ctx, res.Client, project, name, change); err == nil && !pf.Fits {
		respondError(w, http.StatusConflict, fmt.Sprintf("resource change needs %s of %s but the namespace quota caps at %s; raise the project tier or environment quota, or reduce other workloads", pf.Projected, pf.Dimension, pf.Hard))
		return
	}

	if err := res.writeResources(ctx, project, name, kind, cpuReq, cpuLim, memReq, memLim); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, string(kind)+" not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to update resources")
		return
	}

	scope := "app"
	if kind == ResourceKindFunction {
		scope = "function"
	}
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

// readResources collapses the CR-specific GET path into one place so
// the kind switch happens in exactly one spot. Returns the canonical
// response shape regardless of which CR backed it.
func (res *Resources) readResources(ctx context.Context, project, name string, kind ResourceKind) (resourcesResponse, error) {
	switch kind {
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

func (res *Resources) writeResources(ctx context.Context, project, name string, kind ResourceKind, cpuReq, cpuLim, memReq, memLim string) error {
	switch kind {
	case ResourceKindFunction:
		var fn kipperv1.Function
		if err := res.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: name}, &fn); err != nil {
			return err
		}
		fn.Spec.Resources.CPURequest = cpuReq
		fn.Spec.Resources.CPULimit = cpuLim
		fn.Spec.Resources.MemoryRequest = memReq
		fn.Spec.Resources.MemoryLimit = memLim
		return res.CRClient.Update(ctx, &fn)
	default:
		var app kipperv1.App
		if err := res.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: name}, &app); err != nil {
			return err
		}
		app.Spec.Resources.CPURequest = cpuReq
		app.Spec.Resources.CPULimit = cpuLim
		app.Spec.Resources.MemoryRequest = memReq
		app.Spec.Resources.MemoryLimit = memLim
		app.Spec.Resources.Profile = "custom"
		return res.CRClient.Update(ctx, &app)
	}
}

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
		{"cpu_request", req.CPURequest},
		{"cpu_limit", req.CPULimit},
		{"memory_request", req.MemoryRequest},
		{"memory_limit", req.MemoryLimit},
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
