package handlers

import (
	"context"
	goerrors "errors"
	"fmt"
	"log"
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
)

// projectQuotaObjectName matches the ResourceQuota the project reconciler
// manages in every environment namespace.
const projectQuotaObjectName = kipperv1.ProjectQuotaName

// Quota serves and mutates a project's tier and per-environment quota.
type Quota struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
}

// QuotaDimensions carries one value per quota dimension, as quantity strings.
type QuotaDimensions struct {
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

// EnvironmentQuota is the per-environment slice of the quota response.
type EnvironmentQuota struct {
	Environment string           `json:"environment"`
	Namespace   string           `json:"namespace"`
	Source      string           `json:"source"` // "tier" or "override"
	Hard        QuotaDimensions  `json:"hard"`
	Used        *QuotaDimensions `json:"used,omitempty"` // nil until the quota object reports status
	// OverQuota is whether any dimension is used beyond its hard cap, and nil
	// when nothing compared them. The three states are distinct: an update whose
	// post-commit usage read failed has not established that a project is within
	// its quota, and answering false there asserts a comparison that never ran.
	OverQuota *bool `json:"over_quota"`
}

// QuotaResponse is the JSON shape for GET /projects/{name}/quota.
type QuotaResponse struct {
	Tier         string                     `json:"tier"`
	Tiers        map[string]QuotaDimensions `json:"tiers"`
	Environments []EnvironmentQuota         `json:"environments"`
	// EnvLimit is the effective environment cap (MaxEnvironments override, else
	// the tier default). EnvCount is how many environments the project has.
	EnvLimit        int  `json:"env_limit"`
	EnvCount        int  `json:"env_count"`
	MaxEnvironments *int `json:"max_environments,omitempty"`
}

// quotaUpdateRequest mutates the tier and/or per-environment overrides.
// Environments listed with a null quota clear their override back to the
// tier default; environments not listed keep their current state.
type quotaUpdateRequest struct {
	// Tier follows pointer semantics: nil leaves the tier unchanged, an empty
	// string clears it (tierless, no quota objects), a tier name sets it.
	Tier         *string                  `json:"tier"`
	Environments []quotaEnvironmentUpdate `json:"environments,omitempty"`
	// MaxEnvironments overrides the tier's environment cap. A positive value
	// sets the override; 0 clears it back to the tier default; omitting the
	// field leaves it unchanged.
	MaxEnvironments *int `json:"max_environments"`
	// Force applies the change even when a new cap is below current usage or
	// the new environment limit is below the current environment count.
	Force bool `json:"force,omitempty"`
}

type quotaEnvironmentUpdate struct {
	Name  string           `json:"name"`
	Quota *QuotaDimensions `json:"quota"`
}

// toEnvQuota converts the API shape into the CR shape.
func (d QuotaDimensions) toEnvQuota() *kipperv1.EnvQuota {
	return &kipperv1.EnvQuota{
		CPURequest:    d.CPURequest,
		CPULimit:      d.CPULimit,
		MemoryRequest: d.MemoryRequest,
		MemoryLimit:   d.MemoryLimit,
	}
}

// QuotaWarning flags one dimension of one environment where the new cap is
// below what the namespace currently uses. Applying it does not evict
// anything, but the next pod that dimension admits will be rejected.
type QuotaWarning struct {
	Environment string `json:"environment"`
	Dimension   string `json:"dimension"`
	Used        string `json:"used"`
	NewCap      string `json:"new_cap"`
}

func validateTier(tier string) error {
	switch tier {
	case "", kipperv1.TierSmall, kipperv1.TierMedium, kipperv1.TierLarge:
		return nil
	}
	return fmt.Errorf("invalid tier %q: must be small, medium or large", tier)
}

// Get returns the project's tier, the tier catalogue, and per-environment
// caps with live usage from each namespace's ResourceQuota status.
func (h *Quota) Get(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var project kipperv1.Project
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Name: name}, &project); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", name))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to load project")
		return
	}

	resp, err := h.quotaView(ctx, name, &project, true)
	if err != nil {
		log.Printf("quota: building the view for project %s: %v", name, err)
		respondError(w, http.StatusInternalServerError, "reading quota")
		return
	}
	respondJSON(w, http.StatusOK, resp)
}

// quotaView is the project's caps, and its live usage when withUsage is set.
//
// Set builds its response from this after the update has committed, with usage
// off. A read that fails there must not turn a write that succeeded into a 500:
// the caller would retry against state that had already changed. Get can fail,
// because nothing has happened yet when it does.
func (h *Quota) quotaView(ctx context.Context, name string, project *kipperv1.Project, withUsage bool) (QuotaResponse, error) {
	resp := QuotaResponse{
		Tier: project.Spec.Tier,
		Tiers: map[string]QuotaDimensions{
			kipperv1.TierSmall:  toDimensions(kipperv1.TierQuota(kipperv1.TierSmall)),
			kipperv1.TierMedium: toDimensions(kipperv1.TierQuota(kipperv1.TierMedium)),
			kipperv1.TierLarge:  toDimensions(kipperv1.TierQuota(kipperv1.TierLarge)),
		},
		Environments:    []EnvironmentQuota{},
		EnvLimit:        project.EffectiveEnvLimit(),
		EnvCount:        len(controllers.ProjectEnvironments(project)),
		MaxEnvironments: project.Spec.MaxEnvironments,
	}

	for _, env := range controllers.ProjectEnvironments(project) {
		ns := controllers.ResolveNamespace(name, env.Name)
		entry := EnvironmentQuota{
			Environment: env.Name,
			Namespace:   ns,
			Source:      "tier",
		}
		switch {
		case env.Quota != nil:
			entry.Source = "override"
			entry.Hard = toDimensions(*env.Quota)
		case project.Spec.Tier == "":
			// Tierless environments carry no quota objects; only cluster-wide
			// limits apply, so there are no caps to report.
			entry.Source = "none"
		default:
			entry.Hard = toDimensions(kipperv1.TierQuota(project.Spec.Tier))
		}

		// The caps above come from the Project's own spec and are the caller's
		// to see. What is running against them does not: a declared environment
		// whose namespace another project holds would otherwise report that
		// project's usage.
		// Ownership decides whether the read happens at all. Reading first and
		// discarding the result on a condition would still have issued the
		// request: a Go if-statement runs its initializer before it evaluates
		// the condition, so the privileged GET went out either way.
		//
		// A namespace that is absent or somebody else's is skipped and the
		// environment still reported, with its declared caps and no usage. A
		// check that could not run is neither of those: answering 200 there
		// would report a healthy environment with no usage, which is what a
		// namespace whose quota has not published status yet looks like.
		ownErr := error(nil)
		if withUsage {
			ownErr = namespaceBelongsTo(ctx, h.Client, ns, name)
			var foreign *foreignNamespaceError
			if ownErr != nil && !goerrors.As(ownErr, &foreign) {
				return QuotaResponse{}, fmt.Errorf("establishing ownership of %s: %w", ns, ownErr)
			}
		}
		if withUsage && ownErr == nil {
			quota, err := h.Client.CoreV1().ResourceQuotas(ns).Get(ctx, projectQuotaObjectName, metav1.GetOptions{})
			switch {
			case errors.IsNotFound(err):
				// No quota object here. A tierless project has none, and a new
				// environment has none until its reconcile runs, so this is an
				// ordinary state rather than a failure: the declared caps stand
				// and usage stays unknown.
			case err != nil:
				// A read that failed is not an absent object, and treating the
				// two alike was the whole defect. It reported a healthy 200 with
				// no usage — the same answer a quota that has not published
				// status yet gives — so a 403 or a 503 on the quota object was
				// indistinguishable from nothing being wrong.
				return QuotaResponse{}, fmt.Errorf("reading the quota of %s: %w", ns, err)
			default:
				// The live object is authoritative for hard values (the
				// reconciler may lag a spec edit) and is the only source of used.
				entry.Hard = fromResourceList(quota.Spec.Hard)
				if len(quota.Status.Used) > 0 {
					used := fromResourceList(quota.Status.Used)
					entry.Used = &used
					over := anyDimensionOver(quota.Status.Used, quota.Status.Hard)
					entry.OverQuota = &over
				}
			}
		}
		resp.Environments = append(resp.Environments, entry)
	}

	return resp, nil
}

// Set updates the tier and/or per-environment overrides. When any new cap is
// below the namespace's current usage the change is rejected with 409 and
// the offending dimensions, unless force is set.
func (h *Quota) Set(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var req quotaUpdateRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Tier != nil {
		if err := validateTier(*req.Tier); err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	for _, env := range req.Environments {
		if env.Quota == nil {
			continue
		}
		if _, err := env.Quota.toEnvQuota().Parsed(); err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("environment %s: %v", env.Name, err))
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var project kipperv1.Project
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Name: name}, &project); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", name))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to load project")
		return
	}

	// Apply the request to an in-memory copy first so the warning check
	// sees the final effective caps.
	updated := project.DeepCopy()
	if req.Tier != nil {
		updated.Spec.Tier = *req.Tier
	}
	if req.MaxEnvironments != nil {
		if *req.MaxEnvironments <= 0 {
			updated.Spec.MaxEnvironments = nil
		} else {
			v := *req.MaxEnvironments
			updated.Spec.MaxEnvironments = &v
		}
	}
	if len(updated.Spec.Environments) == 0 {
		updated.Spec.Environments = controllers.ProjectEnvironments(updated)
	}

	// A tier or limit change that drops the effective environment cap below the
	// current environment count does not evict anything, but blocks new
	// environments. Surface it like the below-usage warning, forceable.
	if !req.Force {
		if envCount, newLimit := len(controllers.ProjectEnvironments(&project)), updated.EffectiveEnvLimit(); envCount > newLimit {
			respondJSON(w, http.StatusConflict, map[string]any{
				"message":   fmt.Sprintf("project has %d environments but the new limit is %d; existing environments keep running, new ones are blocked", envCount, newLimit),
				"env_count": envCount,
				"env_limit": newLimit,
			})
			return
		}
	}
	for _, envReq := range req.Environments {
		var quota *kipperv1.EnvQuota
		if envReq.Quota != nil {
			quota = envReq.Quota.toEnvQuota()
		}
		found := false
		for i := range updated.Spec.Environments {
			if updated.Spec.Environments[i].Name == envReq.Name {
				updated.Spec.Environments[i].Quota = quota
				found = true
				break
			}
		}
		if !found {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("unknown environment %q", envReq.Name))
			return
		}
	}

	if !req.Force {
		warnings, err := h.belowUsageWarnings(ctx, updated)
		if err != nil {
			// Redacted. This path now reads namespaces to establish ownership,
			// so the error it can carry is a Kubernetes one — an apiserver
			// address, an RBAC diagnostic, an etcd message — and none of that
			// is the caller's to read. It goes to the log instead.
			log.Printf("quota: checking current usage for project %s: %v", name, err)
			respondError(w, http.StatusInternalServerError, "checking current usage")
			return
		}
		if len(warnings) > 0 {
			respondJSON(w, http.StatusConflict, map[string]any{
				"message":  "new caps are below current usage; nothing is evicted, but new pods in these dimensions will be rejected",
				"warnings": warnings,
			})
			return
		}
	}

	project.Spec.Tier = updated.Spec.Tier
	project.Spec.Environments = updated.Spec.Environments
	project.Spec.MaxEnvironments = updated.Spec.MaxEnvironments
	if err := h.CRClient.Update(ctx, &project); err != nil {
		if errors.IsConflict(err) {
			respondError(w, http.StatusConflict, "project was modified concurrently; reload and retry")
			return
		}
		log.Printf("quota: updating project %s: %v", name, err)
		respondError(w, http.StatusInternalServerError, "failed to update project")
		return
	}

	// The same live view a GET would return, so a successful update answers
	// with what is actually running. Only when that cannot be read does this
	// fall back, and it falls back rather than failing: the update has
	// committed, and reporting it as failed would send the caller to retry
	// against state that had already changed.
	//
	// The fallback leaves usage and over_quota unset, so over_quota is null
	// rather than false: the comparison did not run, and saying a project is
	// within its quota because nothing could check is the one answer an outage
	// must not give.
	resp, err := h.quotaView(ctx, name, &project, true)
	if err != nil {
		log.Printf("quota: reading live usage for project %s after a successful update: %v", name, err)
		resp, err = h.quotaView(ctx, name, &project, false)
		if err != nil {
			log.Printf("quota: building the view for project %s after a successful update: %v", name, err)
			respondJSON(w, http.StatusOK, map[string]string{"tier": project.Spec.Tier})
			return
		}
	}
	respondJSON(w, http.StatusOK, resp)
}

// belowUsageWarnings compares each environment's new effective caps against
// the live ResourceQuota usage.
func (h *Quota) belowUsageWarnings(ctx context.Context, project *kipperv1.Project) ([]QuotaWarning, error) {
	var warnings []QuotaWarning
	for _, env := range controllers.ProjectEnvironments(project) {
		if project.Spec.Tier == "" && env.Quota == nil {
			// Tierless environments have no caps, so there is nothing new
			// usage could exceed.
			continue
		}
		effective := kipperv1.TierQuota(project.Spec.Tier)
		if env.Quota != nil {
			effective = *env.Quota
		}
		values, err := effective.Parsed()
		if err != nil {
			return nil, err
		}

		ns := controllers.ResolveNamespace(project.Name, env.Name)
		// A namespace this project does not own is skipped like a missing one.
		// This warns about usage that would exceed a new cap, and somebody
		// else's usage is neither the caller's to see nor governed by it.
		if err := namespaceBelongsTo(ctx, h.Client, ns, project.Name); err != nil {
			var foreign *foreignNamespaceError
			if goerrors.As(err, &foreign) {
				continue
			}
			return nil, err
		}
		quota, err := h.Client.CoreV1().ResourceQuotas(ns).Get(ctx, projectQuotaObjectName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}

		for _, dim := range []struct {
			name   string
			key    corev1.ResourceName
			newCap resource.Quantity
		}{
			{"requests.cpu", corev1.ResourceRequestsCPU, values.CPURequest},
			{"limits.cpu", corev1.ResourceLimitsCPU, values.CPULimit},
			{"requests.memory", corev1.ResourceRequestsMemory, values.MemoryRequest},
			{"limits.memory", corev1.ResourceLimitsMemory, values.MemoryLimit},
		} {
			used, ok := quota.Status.Used[dim.key]
			if !ok || dim.newCap.Cmp(used) >= 0 {
				continue
			}
			warnings = append(warnings, QuotaWarning{
				Environment: env.Name,
				Dimension:   dim.name,
				Used:        used.String(),
				NewCap:      dim.newCap.String(),
			})
		}
	}
	return warnings, nil
}

func toDimensions(q kipperv1.EnvQuota) QuotaDimensions {
	return QuotaDimensions{
		CPURequest:    q.CPURequest,
		CPULimit:      q.CPULimit,
		MemoryRequest: q.MemoryRequest,
		MemoryLimit:   q.MemoryLimit,
	}
}

func fromResourceList(list corev1.ResourceList) QuotaDimensions {
	get := func(key corev1.ResourceName) string {
		if v, ok := list[key]; ok {
			return v.String()
		}
		return ""
	}
	return QuotaDimensions{
		CPURequest:    get(corev1.ResourceRequestsCPU),
		CPULimit:      get(corev1.ResourceLimitsCPU),
		MemoryRequest: get(corev1.ResourceRequestsMemory),
		MemoryLimit:   get(corev1.ResourceLimitsMemory),
	}
}

func anyDimensionOver(used, hard corev1.ResourceList) bool {
	for key, u := range used {
		if h, ok := hard[key]; ok && u.Cmp(h) > 0 {
			return true
		}
	}
	return false
}
