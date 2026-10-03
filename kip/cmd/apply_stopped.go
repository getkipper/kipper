package cmd

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// stampStops records who stops an app through a manifest, and when. For an
// app that is already stopped, withLiveStopRecord puts the live record back,
// so this applies only to stops the manifest introduces.
func stampStops(resources []manifest.Resource, by string, at time.Time) {
	for _, res := range resources {
		if res.GVR != deployer.AppGVR {
			continue
		}
		stopped, found, _ := unstructured.NestedMap(res.Object.Object, "spec", "stopped")
		if !found {
			continue
		}
		stopped["by"] = by
		stopped["at"] = at.UTC().Format(time.RFC3339)
		_ = unstructured.SetNestedMap(res.Object.Object, stopped, "spec", "stopped")
	}
}

// manifestsStop reports whether any resource stops an app.
func manifestsStop(resources []manifest.Resource) bool {
	for _, res := range resources {
		if _, found, _ := unstructured.NestedMap(res.Object.Object, "spec", "stopped"); found && res.GVR == deployer.AppGVR {
			return true
		}
	}
	return false
}

// withLiveStopRecord returns spec with the live stop's by and at in place of
// the manifest's, so neither a diff nor a write rewrites who stopped the app
// or when. spec is left alone.
func withLiveStopRecord(spec, live map[string]interface{}) map[string]interface{} {
	stopped, isStopped := spec["stopped"].(map[string]interface{})
	liveStopped, liveIsStopped := live["stopped"].(map[string]interface{})
	if !isStopped || !liveIsStopped {
		return spec
	}
	record := make(map[string]interface{}, len(stopped))
	for k, v := range stopped {
		if k != "by" && k != "at" {
			record[k] = v
		}
	}
	for _, k := range []string{"by", "at"} {
		if v, has := liveStopped[k]; has {
			record[k] = v
		}
	}
	out := make(map[string]interface{}, len(spec))
	for k, v := range spec {
		out[k] = v
	}
	out["stopped"] = record
	return out
}

// sendsStop reports whether an App write carries a stop the cluster must keep.
func sendsStop(res manifest.Resource) bool {
	if res.GVR != deployer.AppGVR {
		return false
	}
	_, found, _ := unstructured.NestedMap(res.Object.Object, "spec", "stopped")
	return found
}
