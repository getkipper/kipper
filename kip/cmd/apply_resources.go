package cmd

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/provenance"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// linkNamespace defaults cross-project links to the apply environment.
func linkNamespace(cluster *config.Cluster, environment string) manifest.LinkNamespace {
	return func(project, linkEnvironment string) string {
		if linkEnvironment == "" {
			linkEnvironment = environment
		}
		return cluster.ResolveNamespace(project, linkEnvironment)
	}
}

// withLiveAutomaticResources preserves omitted CPU and memory values classified
// as automatic, matching export's classification. It copies the spec so carried
// values remain separate from declared values used to claim user ownership.
func withLiveAutomaticResources(spec, live map[string]interface{}, owners []metav1.ManagedFieldsEntry) map[string]interface{} {
	liveResources, _ := live["resources"].(map[string]interface{})
	if spec == nil || len(liveResources) == 0 {
		return spec
	}
	declared, _ := spec["resources"].(map[string]interface{})
	var carried map[string]interface{}
	for _, field := range []string{"cpuRequest", "cpuLimit", "memoryRequest", "memoryLimit"} {
		if _, set := declared[field]; set {
			continue
		}
		value, _ := liveResources[field].(string)
		if provenance.ClassifyApp(value, owners, field) != provenance.Automatic {
			continue
		}
		if carried == nil {
			carried = make(map[string]interface{}, len(declared)+4)
			for k, v := range declared {
				carried[k] = v
			}
		}
		carried[field] = value
	}
	if carried == nil {
		return spec
	}
	out := make(map[string]interface{}, len(spec)+1)
	for k, v := range spec {
		out[k] = v
	}
	out["resources"] = carried
	return out
}
