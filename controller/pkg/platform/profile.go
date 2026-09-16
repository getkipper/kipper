// Package platform is the single source of truth for system component sizing
// across Kipper. It defines the named sizing profiles (nano/small/medium/
// large/xlarge) and the per-component memory targets each profile implies.
// Both the kip CLI (at install time) and the console-api reconciler (at
// runtime) read from here so that a "the small profile gets 512Mi for
// Prometheus" decision only lives in one place.
package platform

import "k8s.io/apimachinery/pkg/api/resource"

// Profile names. The installer picks one based on node memory; users can
// change it later via the console or `kip platform profile set`.
const (
	ProfileNano   = "nano"
	ProfileSmall  = "small"
	ProfileMedium = "medium"
	ProfileLarge  = "large"
	ProfileXLarge = "xlarge"
)

// Resources captures the memory request/limit pairs the platform sizing
// profile drives. Only Prometheus and Loki are profile-sensitive today —
// other system components have small, near-flat footprints across all
// profiles and are not exposed here.
type Resources struct {
	PrometheusMemoryRequest string
	PrometheusMemoryLimit   string
	LokiMemoryRequest       string
	LokiMemoryLimit         string
}

// EffectiveResources applies non-empty memory-limit overrides to profile
// defaults and clamps requests to the new limits. Both reconciliation and upgrade
// rendering use this rule. overrides maps component names to limits; nil uses
// profile defaults.
func EffectiveResources(profile string, overrides map[string]string) Resources {
	res := ResourcesForProfile(profile)
	if v, ok := overrides[ComponentPrometheus]; ok && v != "" {
		res.PrometheusMemoryLimit = v
		res.PrometheusMemoryRequest = clampRequest(res.PrometheusMemoryRequest, v)
	}
	if v, ok := overrides[ComponentLoki]; ok && v != "" {
		res.LokiMemoryLimit = v
		res.LokiMemoryRequest = clampRequest(res.LokiMemoryRequest, v)
	}
	return res
}

// clampRequest returns the lower of request and limit when both parse as
// valid quantities, so the rendered pod spec always satisfies request <=
// limit. If either side fails to parse, the original request is returned
// and the downstream chart layer surfaces the error rather than the
// reconciler silently rewriting the user's input.
func clampRequest(request, limit string) string {
	rq, err := resource.ParseQuantity(request)
	if err != nil {
		return request
	}
	lim, err := resource.ParseQuantity(limit)
	if err != nil {
		return request
	}
	if rq.Cmp(lim) > 0 {
		return limit
	}
	return request
}

// Component identifiers shared between kip, console-api, and docs. Hardcoding
// these strings in three places was a recurring source of typo bugs.
const (
	ComponentPrometheus = "prometheus"
	ComponentLoki       = "loki"
)

// ResourcesForProfile returns per-component memory targets for the active
// profile.
//
// nano is intentionally not listed: the install path skips Prometheus and
// Loki on that profile, so this function should never run with ProfileNano.
// If it does, the small profile's conservative numbers are returned as a
// safe fallback rather than rendering an empty resources block.
func ResourcesForProfile(profile string) Resources {
	switch profile {
	case ProfileXLarge:
		return Resources{
			PrometheusMemoryRequest: "1Gi",
			PrometheusMemoryLimit:   "2Gi",
			LokiMemoryRequest:       "512Mi",
			LokiMemoryLimit:         "1Gi",
		}
	case ProfileMedium, ProfileLarge:
		return Resources{
			PrometheusMemoryRequest: "512Mi",
			PrometheusMemoryLimit:   "1Gi",
			LokiMemoryRequest:       "256Mi",
			LokiMemoryLimit:         "512Mi",
		}
	default:
		return Resources{
			PrometheusMemoryRequest: "256Mi",
			PrometheusMemoryLimit:   "512Mi",
			LokiMemoryRequest:       "128Mi",
			LokiMemoryLimit:         "384Mi",
		}
	}
}
