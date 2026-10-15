package installer

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

type placementHost struct {
	listings  map[string]string
	missing   []string
	unchanged []string
	commands  []string
}

func (h *placementHost) Run(cmd string) (string, error) {
	h.commands = append(h.commands, cmd)
	for _, m := range h.missing {
		if strings.Contains(cmd, "patch") && (strings.Contains(cmd, " "+m+" ") || strings.Contains(cmd, "/"+m+" ")) {
			return `Error from server (NotFound): deployments.apps "` + m + `" not found`, errors.New("exit status 1")
		}
	}
	if strings.Contains(cmd, " patch ") {
		for _, u := range h.unchanged {
			if strings.Contains(cmd, " "+u+" ") {
				return u + " patched (no change)", nil
			}
		}
		return "patched", nil
	}
	if strings.Contains(cmd, " get deployments,statefulsets ") {
		for ns, out := range h.listings {
			if strings.Contains(cmd, "-n "+ns+" ") {
				return out, nil
			}
		}
	}
	return "", nil
}

func (h *placementHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

func (h *placementHost) patched() []string {
	var out []string
	for _, c := range h.commands {
		if strings.Contains(c, " patch ") {
			out = append(out, c)
		}
	}
	return out
}

func TestPinPlatformToServerPinsUnownedPlatformWorkloads(t *testing.T) {
	h := &placementHost{listings: map[string]string{
		"kipper-system": "Deployment console \nDeployment zot \n",
		"monitoring":    "StatefulSet loki \nStatefulSet prometheus-kube-prometheus-stack-prometheus Prometheus\n",
	}}

	require.NoError(t, PinPlatformToServer(h))

	all := strings.Join(h.patched(), "\n")
	assert.Contains(t, all, "kubectl -n kipper-system patch deployment/console --type strategic")
	assert.Contains(t, all, "kubectl -n kipper-system patch deployment/zot --type strategic")
	assert.Contains(t, all, "kubectl -n monitoring patch statefulset/loki --type strategic")
	assert.NotContains(t, all, "patch statefulset/prometheus-kube-prometheus-stack-prometheus",
		"placement must be set on the Prometheus resource so the operator preserves it")
	assert.Contains(t, all, "patch prometheuses.monitoring.coreos.com kube-prometheus-stack-prometheus --type merge")
	assert.Contains(t, all, "kubectl -n kube-system patch deployment/coredns")
	assert.Contains(t, all, "kubectl -n kube-system patch deployment/metrics-server")
	assert.Contains(t, all, `"nodeSelector":{"node-role.kubernetes.io/control-plane":"true"}`)
}

func TestPinPlatformToServerLabelsTheServerForTheServiceLoadBalancer(t *testing.T) {
	h := &placementHost{}

	require.NoError(t, PinPlatformToServer(h))

	assert.Contains(t, h.commands[0], "kubectl label node -l node-role.kubernetes.io/control-plane=true svccontroller.k3s.cattle.io/enablelb=true --overwrite",
		"labeling the server restricts ServiceLB to labeled nodes")
}

func TestPinPlatformToServerSkipsAbsentAddonsAndPrometheus(t *testing.T) {
	h := &placementHost{missing: []string{"metrics-server", "kube-prometheus-stack-prometheus"}}

	require.NoError(t, PinPlatformToServer(h), "Prometheus is optional, and k3s addons may still be starting")
}

func TestPinPlatformToServerReportsAFailedPatch(t *testing.T) {
	h := &failingPatchHost{}

	err := PinPlatformToServer(h)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "deployment/coredns in kube-system")
}

type failingPatchHost struct{ placementHost }

func (h *failingPatchHost) Run(cmd string) (string, error) {
	if strings.Contains(cmd, " patch ") {
		return "The connection to the server was refused", errors.New("exit status 1")
	}
	return h.placementHost.Run(cmd)
}

func TestPinPlatformToServerWaitsForEveryPinnedWorkload(t *testing.T) {
	h := &placementHost{
		listings:  map[string]string{"dex": "Deployment dex \n", "velero": "Deployment minio \n"},
		unchanged: []string{"deployment/minio"},
	}

	require.NoError(t, PinPlatformToServer(h))

	all := strings.Join(h.commands, "\n")
	assert.Contains(t, all, "kubectl -n dex rollout status deployment/dex --timeout=300s",
		"placement must wait for each workload to finish rolling out")
	assert.Contains(t, all, "kubectl -n velero rollout status deployment/minio --timeout=300s",
		"unchanged selectors still require a rollout check on retries")
	assert.Contains(t, all, "kubectl -n kube-system rollout status deployment/coredns --timeout=300s")
}

func TestPinPlatformToServerWaitsForTheOperatorToPinPrometheus(t *testing.T) {
	h := &placementHost{}

	require.NoError(t, PinPlatformToServer(h))

	all := strings.Join(h.commands, "\n")
	waitSelector := strings.Index(all, `get statefulset/prometheus-kube-prometheus-stack-prometheus -o jsonpath='{.spec.template.spec.nodeSelector.node-role\.kubernetes\.io/control-plane}'`)
	waitRollout := strings.Index(all, "kubectl -n monitoring rollout status statefulset/prometheus-kube-prometheus-stack-prometheus --timeout=300s")
	require.NotEqual(t, -1, waitSelector, "wait for the operator to apply the selector before checking the rollout")
	require.NotEqual(t, -1, waitRollout)
	assert.Less(t, waitSelector, waitRollout)
}

func TestPinPlatformToServerReportsARolloutThatDoesNotFinish(t *testing.T) {
	h := &stuckRolloutHost{placementHost{listings: map[string]string{"dex": "Deployment dex \n"}}}

	err := PinPlatformToServer(h)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "deployment/dex in dex failed to complete its rollout on the server")
}

type stuckRolloutHost struct{ placementHost }

func (h *stuckRolloutHost) Run(cmd string) (string, error) {
	if strings.Contains(cmd, "rollout status deployment/dex") {
		return "error: timed out waiting for the condition", errors.New("exit status 1")
	}
	return h.placementHost.Run(cmd)
}

// Templates preserve server placement when controllers recreate workloads
// or charts finish installing after the placement step.
func TestPlatformManifestsPinToTheServer(t *testing.T) {
	res := platform.EffectiveResources(ProfileMedium, nil)
	keda := &recordingRunner{}
	require.NoError(t, InstallKEDA(keda))
	minio := &stdinRecorder{}
	require.NoError(t, installInClusterMinIO(minio, "pw"))

	for name, tc := range map[string]struct {
		manifest string
		want     int
	}{
		"loki (single binary, gateway)": {platform.LokiHelmChart(res), 2},
		"kube-prometheus-stack (operator, webhook job, prometheus, grafana, kube-state-metrics)": {platform.KubePrometheusStackHelmChart(res), 5},
		"keda (all components, then interceptor, scaler, operator)":                              {strings.Join(keda.commands, "\n"), 4},
		"velero server":   {veleroManifest(nil, false), 1},
		"dex":             {renderDexManifest("dex.example.com", "console.example.com", "example.com", "hash"), 1},
		"console and api": {renderConsoleManifest("dex", "console", "console-api", "example.com", "", ""), 2},
		"authz":           {authzManifest, 1},
		"zot":             {renderZotRuntimeManifest("zot-config"), 1},
		"minio":           {minio.stdin.String(), 1},
	} {
		assert.Equal(t, tc.want, strings.Count(tc.manifest, serverNodeLabel+`: "true"`), name)
	}
}

type stdinRecorder struct{ stdin strings.Builder }

func (r *stdinRecorder) Run(string) (string, error) { return "", nil }

func (r *stdinRecorder) RunStdin(_ string, in io.Reader) (string, error) {
	b, err := io.ReadAll(in)
	r.stdin.Write(b)
	return "", err
}

func TestLokiGatewayCanRollOutOnOneServer(t *testing.T) {
	values := platform.LokiHelmChart(platform.EffectiveResources(ProfileMedium, nil))
	assert.Contains(t, values, "    gateway:\n      # ", "the strategy belongs to the gateway block")
	assert.Contains(t, values, "      deploymentStrategy:\n        type: RollingUpdate\n        rollingUpdate:\n          maxSurge: 0\n          maxUnavailable: 1\n",
		"a surge pod can never schedule next to the old one, and the rollout stalls")
}
