package cmd

import (
	"bytes"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/controller/pkg/capacity"
	"github.com/getkipper/kipper/controller/pkg/provenance"
	"github.com/getkipper/kipper/kip/internal/deployer"
)

func sampleAppInfo() deployer.AppInfo {
	min, max, target := int32(2), int32(5), int32(70)
	desired, ready := int32(2), int32(2)
	scaled := time.Date(2026, 10, 10, 20, 56, 0, 0, time.UTC)
	return deployer.AppInfo{
		Status: deployer.AppStatus{Name: "api", Status: "running", Image: "ghcr.io/acme/api:v1", Replicas: 2, Ready: 2},
		Autoscale: deployer.AutoscaleStatus{
			Policy:        &capacity.Policy{Enabled: true, MinReplicas: &min, MaxReplicas: &max, CPUTarget: &target},
			Replicas:      2,
			LiveDesired:   &desired,
			Ready:         &ready,
			HPAExists:     true,
			CurrentMetric: map[string]int32{"cpu": 12},
			LastScaleTime: &scaled,
		},
		Profile:  "jvm",
		Template: deployer.TemplateRead,
		CPU: deployer.ResourceInfo{
			Mode:        provenance.ModeBounded,
			Request:     deployer.SpecValue{Value: "200m", Source: provenance.User},
			Limit:       deployer.SpecValue{Value: "1", Source: provenance.User},
			LiveRequest: "300m", LiveLimit: "1",
		},
		Memory: deployer.ResourceInfo{
			Mode:        provenance.ModeAutomatic,
			LiveRequest: "2Gi", LiveLimit: "2Gi",
			RecommendedRequest: "1536Mi", RecommendedLimit: "2Gi",
		},
	}
}

func TestWriteAppInfo(t *testing.T) {
	now := time.Date(2026, 10, 10, 23, 0, 0, 0, time.UTC)

	t.Run("shows status, scaling and resources", func(t *testing.T) {
		var out bytes.Buffer
		writeAppInfo(&out, "api", sampleAppInfo(), now)
		got := out.String()

		assert.Contains(t, got, "  App: api\n")
		assert.Contains(t, got, "  Status:  running, 2/2 ready\n")
		assert.Contains(t, got, "  Image:   ghcr.io/acme/api:v1\n")
		assert.Contains(t, got, "  Autoscaling: on (CPU 70%)")
		assert.Contains(t, got, "  Desired: 2 (set by autoscaling)   Min: 2   Max: 5")
		assert.Contains(t, got, "  Last scaled: 2026-10-10 20:56 UTC (2h ago)\n")
		assert.Contains(t, got, "  Resources (profile jvm, from the pod template)\n")
		assert.Contains(t, got, "  CPU:     request 300m, limit 1\n")
		assert.Contains(t, got, "           the autoscaler tracks CPU, so Kipper leaves the CPU request alone\n")
		assert.Contains(t, got, "  Memory:  request 2Gi, limit 2Gi\n")
		assert.Contains(t, got, "           sized by Kipper; latest recommendation: request 1536Mi, limit 2Gi\n")
	})

	t.Run("describes a bounded range when the autoscaler does not track the resource", func(t *testing.T) {
		info := sampleAppInfo()
		info.Autoscale.Policy.Enabled = false
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.Contains(t, out.String(), "           Kipper adjusts the request between 200m and 1\n")
	})

	t.Run("says when the autoscaler tracks a resource", func(t *testing.T) {
		var out bytes.Buffer
		writeAppInfo(&out, "api", sampleAppInfo(), now)

		assert.Contains(t, out.String(), "           the autoscaler tracks CPU, so Kipper leaves the CPU request alone\n")
	})

	t.Run("says memory is only raised after an out-of-memory kill when the autoscaler tracks memory", func(t *testing.T) {
		info := sampleAppInfo()
		target := int32(80)
		info.Autoscale.Policy.MemoryTarget = &target
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.Contains(t, out.String(), "           the autoscaler tracks memory, so Kipper only raises it after an out-of-memory kill\n")
	})

	t.Run("takes tracked metrics from the autoscaler when the policy is not usable", func(t *testing.T) {
		info := sampleAppInfo()
		six := int32(6)
		info.Autoscale.Policy.MinReplicas = &six
		info.Autoscale.HPAMetrics = map[string]bool{"memory": true}
		info.CPU.Mode = provenance.ModeAutomatic
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.NotContains(t, out.String(), "tracks CPU", "an unusable policy's CPU target is not what the autoscaler tracks")
		assert.Contains(t, out.String(), "tracks memory, so Kipper only raises it after an out-of-memory kill")
	})

	t.Run("claims no tracking for an unusable policy without an autoscaler", func(t *testing.T) {
		info := sampleAppInfo()
		six := int32(6)
		info.Autoscale.Policy.MinReplicas = &six
		info.Autoscale.HPAExists = false
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.NotContains(t, out.String(), "the autoscaler tracks")
	})

	t.Run("says tracking is unknown when the autoscaler cannot be read", func(t *testing.T) {
		info := sampleAppInfo()
		six := int32(6)
		info.Autoscale.Policy.MinReplicas = &six
		info.Autoscale.HPAExists = false
		info.Autoscale.HPAUnreadable = true
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.Contains(t, out.String(), "           CPU sizing unknown: permission denied when reading the autoscaler\n")
		assert.NotContains(t, out.String(), "Kipper adjusts the request")
	})

	t.Run("compares sizes as quantities", func(t *testing.T) {
		info := sampleAppInfo()
		info.Memory.RecommendedRequest, info.Memory.RecommendedLimit = "2048Mi", "2Gi"
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.NotContains(t, out.String(), "recommendation", "2048Mi and 2Gi are the same size")
	})

	t.Run("names fixed and held values", func(t *testing.T) {
		info := sampleAppInfo()
		info.CPU = deployer.ResourceInfo{Mode: provenance.ModeFixed, Request: deployer.SpecValue{Value: "500m", Source: provenance.User}, LiveRequest: "500m", LiveLimit: "500m"}
		info.Memory = deployer.ResourceInfo{Mode: provenance.ModeHeld, LiveRequest: "1Gi", LiveLimit: "1Gi"}
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.Contains(t, out.String(), "           fixed at the size you set\n")
		assert.Contains(t, out.String(), "           kept until you set them; original ownership is unknown\n")
	})

	t.Run("hides a recommendation that is already applied", func(t *testing.T) {
		info := sampleAppInfo()
		info.Memory.RecommendedRequest, info.Memory.RecommendedLimit = "2Gi", "2Gi"
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.Contains(t, out.String(), "           sized by Kipper\n")
		assert.NotContains(t, out.String(), "recommendation")
	})

	t.Run("says when the pod template does not exist yet", func(t *testing.T) {
		info := sampleAppInfo()
		info.Template = deployer.TemplateMissing
		info.Autoscale.LastScaleTime = nil
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.Contains(t, out.String(), "  CPU:     not deployed yet\n")
		assert.NotContains(t, out.String(), "Last scaled")
	})

	t.Run("says unknown when the pod template could not be read", func(t *testing.T) {
		info := sampleAppInfo()
		info.Template = deployer.TemplateUnreadable
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.Contains(t, out.String(), "  CPU:     unknown (the Deployment could not be read)\n")
		assert.NotContains(t, out.String(), "not deployed")
	})

	t.Run("names a value the pod template leaves unset", func(t *testing.T) {
		info := sampleAppInfo()
		info.CPU.LiveLimit = ""
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.Contains(t, out.String(), "  CPU:     request 300m, limit not set\n")
	})

	t.Run("warns that running pods may lag the template during a rollout", func(t *testing.T) {
		info := sampleAppInfo()
		info.Status.RolloutWaiting = "1 of 2 pods updated"
		var out bytes.Buffer
		writeAppInfo(&out, "api", info, now)

		assert.Contains(t, out.String(), "  Rollout incomplete: some pods may still use earlier values.\n")
	})
}
