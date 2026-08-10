package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// platformConfigName is the singleton CR the platform reconciler acts on.
// Keep this in sync with controllers.PlatformConfigName — the two packages
// don't share imports, but the name is a published API contract.
const platformConfigName = "platform"

// Platform serves the /api/v1/platform endpoints used by the console's
// Platform section and `kip platform`.
type Platform struct {
	CRClient    crclient.Client
	Adjustments *Adjustments
}

type platformSummaryResponse struct {
	Profile           string                  `json:"profile"`
	AvailableProfiles []string                `json:"available_profiles"`
	Components        []componentSummaryEntry `json:"components"`
}

type componentSummaryEntry struct {
	Name               string `json:"name"`
	Enabled            bool   `json:"enabled"`
	CurrentMemoryLimit string `json:"current_memory_limit,omitempty"`
	Phase              string `json:"phase,omitempty"`
	AtCeiling          bool   `json:"at_ceiling,omitempty"`
}

type componentDetailEntry struct {
	Name                string `json:"name"`
	Enabled             bool   `json:"enabled"`
	ProfileMemoryLimit  string `json:"profile_memory_limit,omitempty"`
	OverrideMemoryLimit string `json:"override_memory_limit,omitempty"`
	CurrentMemoryLimit  string `json:"current_memory_limit,omitempty"`
	// MemoryMin / MemoryMax bracket the allowable override range. The
	// frontend slider reads these so a small-cluster profile shows a
	// narrower memory range than xlarge would.
	MemoryMin      string `json:"memory_min,omitempty"`
	MemoryMax      string `json:"memory_max,omitempty"`
	Phase          string `json:"phase,omitempty"`
	RestartCount7d int32  `json:"restart_count_7d,omitempty"`
	LastBumpAt     string `json:"last_bump_at,omitempty"`
	LastBumpFrom   string `json:"last_bump_from,omitempty"`
	LastBumpTo     string `json:"last_bump_to,omitempty"`
	LastBumpReason string `json:"last_bump_reason,omitempty"`
	AtCeiling      bool   `json:"at_ceiling,omitempty"`
}

type componentsResponse struct {
	Profile    string                 `json:"profile"`
	Components []componentDetailEntry `json:"components"`
}

// componentPatchRequest is the PATCH body. Both fields are pointers so the
// caller can update only one of them without resetting the other. A nil
// Enabled means "leave as is"; an explicit true/false sets the override.
type componentPatchRequest struct {
	Enabled     *bool   `json:"enabled,omitempty"`
	MemoryLimit *string `json:"memory_limit,omitempty"`
}

// isKnownComponent gates the PATCH endpoint and the summary/detail
// listings against the shared path table in controller/pkg/platform.
// Centralising this means a new chart-managed component lands here for
// free as soon as it's added to helmpaths.go.
func isKnownComponent(name string) bool {
	_, ok := platform.PathFor(name)
	return ok
}

func availableProfiles() []string {
	return []string{
		platform.ProfileNano,
		platform.ProfileSmall,
		platform.ProfileMedium,
		platform.ProfileLarge,
		platform.ProfileXLarge,
	}
}

// Summary returns a compact view of platform state for dashboards.
// GET /api/v1/platform
func (p *Platform) Summary(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	pc, err := p.fetchPlatformConfig(ctx)
	if err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, platformSummaryResponse{
				AvailableProfiles: availableProfiles(),
			})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to load platform config")
		return
	}

	statusByName := indexStatus(pc.Status.Components)
	overrideByName := indexOverrides(pc.Spec.Components)

	enabledOverrides := enabledOverrideMap(overrideByName)

	summary := platformSummaryResponse{
		Profile:           pc.Spec.Profile,
		AvailableProfiles: availableProfiles(),
	}
	for _, name := range platform.SupportedComponents() {
		summary.Components = append(summary.Components, componentSummaryEntry{
			Name:               name,
			Enabled:            platform.EffectiveEnabled(name, enabledOverrides, pc.Spec.Profile),
			CurrentMemoryLimit: statusByName[name].CurrentMemoryLimit,
			Phase:              statusByName[name].Phase,
			AtCeiling:          statusByName[name].AtCeiling,
		})
	}
	respondJSON(w, http.StatusOK, summary)
}

// Components returns the detailed per-component list.
// GET /api/v1/platform/components
func (p *Platform) Components(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	pc, err := p.fetchPlatformConfig(ctx)
	if err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, componentsResponse{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to load platform config")
		return
	}

	statusByName := indexStatus(pc.Status.Components)
	overrideByName := indexOverrides(pc.Spec.Components)
	enabledOverrides := enabledOverrideMap(overrideByName)

	resp := componentsResponse{Profile: pc.Spec.Profile}
	for _, name := range platform.SupportedComponents() {
		override := overrideByName[name]
		paths, _ := platform.PathFor(name)
		entry := componentDetailEntry{
			Name:                name,
			Enabled:             platform.EffectiveEnabled(name, enabledOverrides, pc.Spec.Profile),
			ProfileMemoryLimit:  platform.EffectiveLimit(name, pc.Spec.Profile, ""),
			OverrideMemoryLimit: override.MemoryLimit,
			CurrentMemoryLimit:  statusByName[name].CurrentMemoryLimit,
			MemoryMin:           paths.MemoryMin,
			MemoryMax:           paths.MemoryMax,
			Phase:               statusByName[name].Phase,
			RestartCount7d:      statusByName[name].RestartCount7d,
			LastBumpFrom:        statusByName[name].LastBumpFrom,
			LastBumpTo:          statusByName[name].LastBumpTo,
			LastBumpReason:      statusByName[name].LastBumpReason,
			AtCeiling:           statusByName[name].AtCeiling,
		}
		if statusByName[name].LastBumpAt != nil {
			entry.LastBumpAt = statusByName[name].LastBumpAt.UTC().Format(time.RFC3339)
		}
		resp.Components = append(resp.Components, entry)
	}
	respondJSON(w, http.StatusOK, resp)
}

// UpdateComponent applies a partial override to spec.components[name].
// PATCH /api/v1/platform/components/{name}
func (p *Platform) UpdateComponent(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !isKnownComponent(name) {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("unknown component %q", name))
		return
	}

	var req componentPatchRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.MemoryLimit != nil && *req.MemoryLimit != "" {
		if err := platform.ValidateMemoryLimit(name, *req.MemoryLimit); err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	// Reject `enabled` updates on components the reconciler doesn't
	// listen to. Persisting these would leave the API reporting a
	// component as disabled while the chart keeps running, which is
	// exactly the silent-no-op problem step 4 fixed on the CLI side.
	if req.Enabled != nil && !platform.IsToggleable(name) {
		respondError(w, http.StatusBadRequest,
			fmt.Sprintf("component %q is not independently toggleable; only prometheus and loki accept enable/disable", name))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	pc, err := p.fetchPlatformConfig(ctx)
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "platform config not initialised; run kip install or recreate the PlatformConfig CR")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to load platform config")
		return
	}

	idx := -1
	for i, c := range pc.Spec.Components {
		if c.Name == name {
			idx = i
			break
		}
	}
	if idx == -1 {
		pc.Spec.Components = append(pc.Spec.Components, kipperv1.ComponentOverride{Name: name})
		idx = len(pc.Spec.Components) - 1
	}

	// Capture the previous limit so the telemetry log records "from".
	// Empty when the user is setting a first override; the recorder
	// skips noisy entries (to == from) elsewhere.
	previousMemoryLimit := pc.Spec.Components[idx].MemoryLimit

	if req.MemoryLimit != nil {
		pc.Spec.Components[idx].MemoryLimit = *req.MemoryLimit
	}
	if req.Enabled != nil {
		pc.Spec.Components[idx].Enabled = req.Enabled
	}

	if err := p.CRClient.Update(ctx, pc); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update platform config")
		return
	}

	if req.MemoryLimit != nil {
		p.Adjustments.Record(ctx,
			"platform", "", name, "memory",
			previousMemoryLimit, *req.MemoryLimit,
			"", SubjectFromRequest(r))
	}

	paths, _ := platform.PathFor(name)
	enabledOverrides := enabledOverrideMap(indexOverrides(pc.Spec.Components))
	respondJSON(w, http.StatusOK, componentDetailEntry{
		Name:                name,
		Enabled:             platform.EffectiveEnabled(name, enabledOverrides, pc.Spec.Profile),
		ProfileMemoryLimit:  platform.EffectiveLimit(name, pc.Spec.Profile, ""),
		OverrideMemoryLimit: pc.Spec.Components[idx].MemoryLimit,
		MemoryMin:           paths.MemoryMin,
		MemoryMax:           paths.MemoryMax,
	})
}

// enabledOverrideMap projects spec.components[].Enabled into the
// map[string]*bool shape platform.EffectiveEnabled expects, without
// leaking the CR type into the platform package.
func enabledOverrideMap(byName map[string]kipperv1.ComponentOverride) map[string]*bool {
	out := make(map[string]*bool, len(byName))
	for name, ov := range byName {
		out[name] = ov.Enabled
	}
	return out
}

func (p *Platform) fetchPlatformConfig(ctx context.Context) (*kipperv1.PlatformConfig, error) {
	var pc kipperv1.PlatformConfig
	if err := p.CRClient.Get(ctx, crclient.ObjectKey{Name: platformConfigName}, &pc); err != nil {
		return nil, err
	}
	return &pc, nil
}

func indexStatus(components []kipperv1.ComponentStatus) map[string]kipperv1.ComponentStatus {
	out := make(map[string]kipperv1.ComponentStatus, len(components))
	for _, c := range components {
		out[c.Name] = c
	}
	return out
}

func indexOverrides(components []kipperv1.ComponentOverride) map[string]kipperv1.ComponentOverride {
	out := make(map[string]kipperv1.ComponentOverride, len(components))
	for _, c := range components {
		out[c.Name] = c
	}
	return out
}
