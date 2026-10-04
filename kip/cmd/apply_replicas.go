package cmd

import (
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/controller/pkg/capacity"
	"github.com/getkipper/kipper/kip/internal/deployer"
)

// withResolvedReplicas fills an omitted App replica count when the policy has
// valid bounds. It keeps an in-range stored count (defaulting to one if absent),
// otherwise uses the minimum. A nil live spec selects the minimum for creation.
// It preserves explicit counts and leaves both input maps unchanged.
func withResolvedReplicas(spec, live map[string]interface{}) map[string]interface{} {
	if _, declared := spec["replicas"]; declared || spec == nil {
		return spec
	}
	policy := deployer.SpecPolicy(spec)
	if capacity.Validate(policy, nil) != nil {
		return spec
	}
	lo, hi, ok := policy.Bounds()
	if !ok {
		return spec
	}
	n := int64(lo)
	if live != nil {
		stored := int64(1)
		if v, found, _ := unstructured.NestedInt64(live, "replicas"); found {
			stored = v
		}
		if stored >= int64(lo) && stored <= int64(hi) {
			n = stored
		}
	}
	out := make(map[string]interface{}, len(spec)+1)
	for k, v := range spec {
		out[k] = v
	}
	out["replicas"] = n
	return out
}
