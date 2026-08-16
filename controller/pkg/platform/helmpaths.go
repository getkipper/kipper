package platform

import (
	"fmt"
	"sort"

	"k8s.io/apimachinery/pkg/api/resource"
)

// ComponentPaths locates a single platform component's memory request and
// limit fields inside a HelmChart's valuesContent, and supplies the default
// values used when the user has not set a per-component override.
//
// Multiple components can live in one HelmChart — for example the
// kube-prometheus-stack chart carries both Prometheus and Grafana, each at
// its own path. The reconciler groups patches by ChartName so a single
// HelmChart Update covers every component that lives inside it.
//
// Profile-sensitive components (Prometheus, Loki) leave the default fields
// empty: their resolved value comes from ResourcesForProfile via
// EffectiveLimit / EffectiveRequest. Flat components (Grafana, Promtail,
// Traefik, KEDA, Velero) declare fixed defaults that mirror the values the
// installer seeded into the chart's valuesContent.
type ComponentPaths struct {
	// ChartName is the HelmChart resource name in HelmChartNamespace.
	ChartName string
	// MemoryRequestPath is the segmented YAML path to the request value.
	MemoryRequestPath []string
	// MemoryLimitPath is the segmented YAML path to the limit value.
	MemoryLimitPath []string
	// DefaultMemoryRequest is used when EffectiveRequest resolves a flat
	// component with no override. Empty for profile-sensitive components.
	DefaultMemoryRequest string
	// DefaultMemoryLimit is used when EffectiveLimit resolves a flat
	// component with no override. Empty for profile-sensitive components.
	DefaultMemoryLimit string
	// MemoryMin / MemoryMax bracket the allowable override range so the
	// API can reject obviously-wrong values (256Mi for Prometheus, 16Gi
	// for cert-manager). Stored as Kubernetes quantity strings; both must
	// parse cleanly.
	MemoryMin string
	MemoryMax string
	// Toggle describes how the component's effective enabled state is
	// resolved. The reconciler today only honours per-component Enabled
	// for Prometheus and Loki (ToggleSelf); Grafana and Promtail follow
	// their chart sibling; Traefik / KEDA / Velero are always on if
	// installed (ToggleAlwaysOn). The PATCH endpoint rejects `enabled`
	// updates for anything other than ToggleSelf so users can't persist
	// overrides the reconciler will silently ignore.
	Toggle ToggleMode
}

// ToggleMode captures whether and how a component's Enabled override is
// honoured. Encoded as a typed const so adding a new mode (per-component
// toggle for KEDA, say) becomes a one-line change.
type ToggleMode int

const (
	// ToggleAlwaysOn — the chart is installed by kip install and never
	// torn down by the reconciler. Enabled overrides are rejected.
	ToggleAlwaysOn ToggleMode = iota
	// ToggleSelf — the component's own Enabled override is honoured.
	ToggleSelf
	// ToggleFollowsPrometheus — the component lives inside the
	// kube-prometheus-stack chart, so its enabled state mirrors
	// Prometheus' (the chart is deleted/re-created together).
	ToggleFollowsPrometheus
	// ToggleFollowsLoki — same idea for promtail vs loki.
	ToggleFollowsLoki
)

// Component identifiers covered by the path table. The original consts
// (ComponentPrometheus, ComponentLoki) live alongside these in profile.go
// and are re-exported transparently via Go's package scope.
const (
	ComponentGrafana          = "grafana"
	ComponentKubeStateMetrics = "kube-state-metrics"
	ComponentPromtail         = "promtail"
	ComponentTraefik          = "traefik"
	ComponentKeda             = "keda"
	ComponentVelero           = "velero"
)

// HelmChart resource names in HelmChartNamespace. Centralised so they
// can't drift between the installer (which creates them) and the
// reconciler (which patches them).
const (
	chartKubePrometheusStack = "kube-prometheus-stack"
	chartLoki                = "loki"
	chartPromtail            = "promtail"
	chartTraefik             = "traefik"
	chartKeda                = "keda"
	chartVelero              = "velero"
)

// componentPathsByName is the source of truth for how each platform
// component is patched. Keys are component identifiers; values bundle the
// chart name, the YAML paths, and the flat default values where
// applicable. Profile-sensitive entries leave defaults empty so callers
// know to consult ResourcesForProfile.
var componentPathsByName = map[string]ComponentPaths{
	ComponentPrometheus: {
		ChartName:         chartKubePrometheusStack,
		MemoryRequestPath: []string{"prometheus", "prometheusSpec", "resources", "requests", "memory"},
		MemoryLimitPath:   []string{"prometheus", "prometheusSpec", "resources", "limits", "memory"},
		MemoryMin:         "256Mi",
		MemoryMax:         "8Gi",
		Toggle:            ToggleSelf,
	},
	ComponentGrafana: {
		ChartName:            chartKubePrometheusStack,
		MemoryRequestPath:    []string{"grafana", "resources", "requests", "memory"},
		MemoryLimitPath:      []string{"grafana", "resources", "limits", "memory"},
		DefaultMemoryRequest: "64Mi",
		DefaultMemoryLimit:   "128Mi",
		MemoryMin:            "64Mi",
		MemoryMax:            "512Mi",
		Toggle:               ToggleFollowsPrometheus,
	},
	// Sized for the re-list an API server restart forces, not for the steady
	// state; see the constants in manifests.go. Tunable because the peak scales
	// with how many objects a cluster holds, and a cluster that outgrows the
	// default should not have to wait for a Kipper release.
	ComponentKubeStateMetrics: {
		ChartName:            chartKubePrometheusStack,
		MemoryRequestPath:    []string{"kube-state-metrics", "resources", "requests", "memory"},
		MemoryLimitPath:      []string{"kube-state-metrics", "resources", "limits", "memory"},
		DefaultMemoryRequest: kubeStateMetricsMemoryRequest,
		DefaultMemoryLimit:   kubeStateMetricsMemoryLimit,
		MemoryMin:            "64Mi",
		MemoryMax:            "1Gi",
		Toggle:               ToggleFollowsPrometheus,
	},
	ComponentLoki: {
		ChartName:         chartLoki,
		MemoryRequestPath: []string{"singleBinary", "resources", "requests", "memory"},
		MemoryLimitPath:   []string{"singleBinary", "resources", "limits", "memory"},
		MemoryMin:         "128Mi",
		MemoryMax:         "2Gi",
		Toggle:            ToggleSelf,
	},
	ComponentPromtail: {
		ChartName:            chartPromtail,
		MemoryRequestPath:    []string{"resources", "requests", "memory"},
		MemoryLimitPath:      []string{"resources", "limits", "memory"},
		DefaultMemoryRequest: "32Mi",
		DefaultMemoryLimit:   "128Mi",
		MemoryMin:            "64Mi",
		MemoryMax:            "512Mi",
		Toggle:               ToggleFollowsLoki,
	},
	ComponentTraefik: {
		ChartName:            chartTraefik,
		MemoryRequestPath:    []string{"resources", "requests", "memory"},
		MemoryLimitPath:      []string{"resources", "limits", "memory"},
		DefaultMemoryRequest: "64Mi",
		DefaultMemoryLimit:   "256Mi",
		MemoryMin:            "64Mi",
		MemoryMax:            "512Mi",
		Toggle:               ToggleAlwaysOn,
	},
	ComponentKeda: {
		ChartName:            chartKeda,
		MemoryRequestPath:    []string{"resources", "operator", "requests", "memory"},
		MemoryLimitPath:      []string{"resources", "operator", "limits", "memory"},
		DefaultMemoryRequest: "64Mi",
		DefaultMemoryLimit:   "256Mi",
		MemoryMin:            "64Mi",
		MemoryMax:            "256Mi",
		Toggle:               ToggleAlwaysOn,
	},
	ComponentVelero: {
		ChartName:            chartVelero,
		MemoryRequestPath:    []string{"resources", "requests", "memory"},
		MemoryLimitPath:      []string{"resources", "limits", "memory"},
		DefaultMemoryRequest: "256Mi",
		DefaultMemoryLimit:   "512Mi",
		MemoryMin:            "128Mi",
		MemoryMax:            "1Gi",
		Toggle:               ToggleAlwaysOn,
	},
}

// IsToggleable reports whether the user's Enabled override is honoured by
// the reconciler for this component. Only ToggleSelf entries qualify. The
// PATCH endpoint uses this to reject `enabled` updates on follow-* or
// always-on components, which the reconciler would silently ignore.
func IsToggleable(component string) bool {
	p, ok := PathFor(component)
	return ok && p.Toggle == ToggleSelf
}

// EffectiveEnabled resolves a component's effective running state.
// Follow-* components mirror their chart sibling, always-on components
// are unconditionally true, self-toggleable components honour their own
// override and fall back to "off on nano, on elsewhere".
//
// enabledOverrides may contain a nil entry to mean "no explicit value".
func EffectiveEnabled(component string, enabledOverrides map[string]*bool, profile string) bool {
	p, ok := PathFor(component)
	if !ok {
		return false
	}
	switch p.Toggle {
	case ToggleAlwaysOn:
		return true
	case ToggleFollowsPrometheus:
		return resolveSelfEnabled(ComponentPrometheus, enabledOverrides, profile)
	case ToggleFollowsLoki:
		return resolveSelfEnabled(ComponentLoki, enabledOverrides, profile)
	case ToggleSelf:
		return resolveSelfEnabled(component, enabledOverrides, profile)
	}
	return true
}

func resolveSelfEnabled(name string, enabledOverrides map[string]*bool, profile string) bool {
	if v, ok := enabledOverrides[name]; ok && v != nil {
		return *v
	}
	return profile != ProfileNano
}

// ValidateMemoryLimit parses the override and confirms it sits within the
// component's [MemoryMin, MemoryMax] range. Returns a descriptive error
// the API can pass straight back to the caller — there's no useful clamp
// behaviour at this layer because the user is the one picking the value;
// silently snapping their input to a different number would surprise them
// at the next reconcile.
func ValidateMemoryLimit(component, value string) error {
	p, ok := PathFor(component)
	if !ok {
		return fmt.Errorf("unknown component %q", component)
	}
	q, err := resource.ParseQuantity(value)
	if err != nil {
		return fmt.Errorf("memory_limit %q is not a valid Kubernetes quantity", value)
	}
	if p.MemoryMin != "" {
		minQ, perr := resource.ParseQuantity(p.MemoryMin)
		if perr == nil && q.Cmp(minQ) < 0 {
			return fmt.Errorf("memory_limit %s is below %s minimum for %s", value, p.MemoryMin, component)
		}
	}
	if p.MemoryMax != "" {
		maxQ, perr := resource.ParseQuantity(p.MemoryMax)
		if perr == nil && q.Cmp(maxQ) > 0 {
			return fmt.Errorf("memory_limit %s is above %s maximum for %s", value, p.MemoryMax, component)
		}
	}
	return nil
}

// PathFor returns the ComponentPaths entry for the given component name.
// The bool is false for unknown components (typo, or a component the
// install path manages via a raw Deployment rather than a HelmChart —
// dex, longhorn, zot, etc. fall into that bucket today).
func PathFor(component string) (ComponentPaths, bool) {
	p, ok := componentPathsByName[component]
	return p, ok
}

// SupportedComponents lists every component the path table covers, sorted
// alphabetically. The CLI uses it for `kip platform resize` validation
// and the API uses it for PATCH validation.
func SupportedComponents() []string {
	out := make([]string, 0, len(componentPathsByName))
	for name := range componentPathsByName {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ComponentsByChart groups supported components by HelmChart resource
// name so the reconciler can send one Update per chart even when several
// components share one (Prometheus + Grafana → kube-prometheus-stack).
// Component slices are sorted so test fixtures are stable.
func ComponentsByChart() map[string][]string {
	out := make(map[string][]string)
	for name, paths := range componentPathsByName {
		out[paths.ChartName] = append(out[paths.ChartName], name)
	}
	for _, names := range out {
		sort.Strings(names)
	}
	return out
}

// EffectiveLimit resolves the memory limit that should land in the chart
// for the named component. Override wins; otherwise profile-sensitive
// components defer to ResourcesForProfile and flat components fall through
// to their DefaultMemoryLimit. Returns "" if the component is not in the
// path table — callers should treat that as a programming error.
func EffectiveLimit(component, profile, override string) string {
	if override != "" {
		return override
	}
	switch component {
	case ComponentPrometheus:
		return ResourcesForProfile(profile).PrometheusMemoryLimit
	case ComponentLoki:
		return ResourcesForProfile(profile).LokiMemoryLimit
	}
	if p, ok := componentPathsByName[component]; ok {
		return p.DefaultMemoryLimit
	}
	return ""
}

// EffectiveRequest mirrors EffectiveLimit but clamps the result so request
// never exceeds the corresponding limit — Kubernetes rejects pods where
// request > limit, so a user override that lowers the limit below the
// profile-default request would otherwise wedge the chart's next rollout.
func EffectiveRequest(component, profile, override string) string {
	req := defaultRequest(component, profile)
	lim := EffectiveLimit(component, profile, override)
	return clampRequest(req, lim)
}

func defaultRequest(component, profile string) string {
	switch component {
	case ComponentPrometheus:
		return ResourcesForProfile(profile).PrometheusMemoryRequest
	case ComponentLoki:
		return ResourcesForProfile(profile).LokiMemoryRequest
	}
	if p, ok := componentPathsByName[component]; ok {
		return p.DefaultMemoryRequest
	}
	return ""
}
