package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

// consoleManifestDoc returns the first rendered document of the given kind and
// name as a navigable map.
func consoleManifestDoc(t *testing.T, kind, name string) map[string]any {
	t.Helper()
	manifest := renderConsoleManifest(
		"dex--acme.kipper.run", "console--acme.kipper.run", "console-api--acme.kipper.run",
		"acme.kipper.run", "acme.kipper.run", "203.0.113.10")
	return decodeDoc(t, manifest, kind, name)
}

func decodeDoc(t *testing.T, manifest, kind, name string) map[string]any {
	t.Helper()
	dec := yaml.NewDecoder(strings.NewReader(manifest))
	for {
		var doc map[string]any
		if err := dec.Decode(&doc); err != nil {
			break
		}
		if doc["kind"] != kind {
			continue
		}
		meta, _ := doc["metadata"].(map[string]any)
		if meta["name"] == name {
			return doc
		}
	}
	t.Fatalf("%s %q not found in the manifest", kind, name)
	return nil
}

// A ServiceMonitor that names a port the Service does not have scrapes nothing,
// says nothing about it, and the first anyone knows is that an alert which
// should have fired never did. The two are written in different files and in
// different modules, so nothing but this compares them.
func TestConsoleAPIServiceMonitorMatchesTheService(t *testing.T) {
	svc := consoleManifestDoc(t, "Service", "console-api")

	selector := dig(t, svc, "spec", "selector").(map[string]any)
	ports, ok := dig(t, svc, "spec", "ports").([]any)
	require.True(t, ok, "the console-api Service must declare ports")

	var metricsPortName string
	for _, p := range ports {
		port, _ := p.(map[string]any)
		if name, _ := port["name"].(string); name == "metrics" {
			metricsPortName = name
		}
	}
	require.Equal(t, "metrics", metricsPortName,
		"the Service must expose a port named metrics, because the ServiceMonitor selects it by that name")

	var sm map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(platform.ConsoleAPIServiceMonitor()), &sm))

	smSelector := dig(t, sm, "spec", "selector", "matchLabels").(map[string]any)
	for k, v := range smSelector {
		assert.Equal(t, v, selector[k],
			"the ServiceMonitor selects %s=%v, which must match the Service's own selector or it scrapes nothing", k, v)
	}

	endpoints := dig(t, sm, "spec", "endpoints").([]any)
	require.NotEmpty(t, endpoints)
	first, _ := endpoints[0].(map[string]any)
	assert.Equal(t, metricsPortName, first["port"],
		"the ServiceMonitor's endpoint must name a port the Service actually declares")

	namespaces := dig(t, sm, "spec", "namespaceSelector", "matchNames").([]any)
	assert.Contains(t, namespaces, "kipper-system",
		"the ServiceMonitor must look in the namespace the Service is in")
}

// The Service forwards to a container port by name, so the container has to
// declare that name. Getting this wrong yields a Service with no endpoints,
// which is as quiet as the mismatch above.
func TestConsoleAPIContainerDeclaresTheMetricsPort(t *testing.T) {
	dep := consoleManifestDoc(t, "Deployment", "console-api")
	containers := dig(t, dep, "spec", "template", "spec", "containers").([]any)
	require.NotEmpty(t, containers)
	first, _ := containers[0].(map[string]any)

	ports, ok := first["ports"].([]any)
	require.True(t, ok, "the console-api container must declare its ports")

	found := false
	for _, p := range ports {
		port, _ := p.(map[string]any)
		if name, _ := port["name"].(string); name == "metrics" {
			found = true
			assert.Equal(t, consoleAPIMetricsPort, port["containerPort"],
				"the metrics container port must be the one the manager binds")
		}
	}
	assert.True(t, found, "the container must declare a port named metrics for the Service to target")

	// And the Service must forward to that name rather than to a number, or the
	// two drift the moment either side moves.
	svc := consoleManifestDoc(t, "Service", "console-api")
	svcPorts := dig(t, svc, "spec", "ports").([]any)
	for _, p := range svcPorts {
		port, _ := p.(map[string]any)
		if name, _ := port["name"].(string); name == "metrics" {
			assert.Equal(t, "metrics", port["targetPort"],
				"the Service should target the container port by name, so renaming one breaks a test rather than the scrape")
			assert.Equal(t, consoleAPIMetricsPort, port["port"])
		}
	}
}

// consoleAPIMetricsPort is the port console-api's manager binds. It is asserted
// on both sides of a boundary the compiler cannot see: this module renders the
// manifest, and console-api decides what to listen on.
const consoleAPIMetricsPort = 8081

// An alert whose expression names a metric nothing exports is an alert that
// never fires, and it looks exactly like an alert with nothing to report.
func TestControllerAlertsReadMetricsControllerRuntimeExports(t *testing.T) {
	var rule map[string]any
	require.NoError(t, yaml.Unmarshal([]byte(platform.KipperControllerAlerts()), &rule))

	groups := dig(t, rule, "spec", "groups").([]any)
	require.NotEmpty(t, groups)

	var expressions []string
	for _, g := range groups {
		group, _ := g.(map[string]any)
		rules, _ := group["rules"].([]any)
		for _, r := range rules {
			row, _ := r.(map[string]any)
			expr, _ := row["expr"].(string)
			expressions = append(expressions, expr)
			assert.NotEmpty(t, row["for"], "an alert with no for: fires on a single scrape")
			annotations, _ := row["annotations"].(map[string]any)
			assert.NotEmpty(t, annotations["summary"], "an alert nobody can read is not an alert")
		}
	}

	joined := strings.Join(expressions, " ")
	// Both are exported by controller-runtime's own metrics registry, which is
	// what the ServiceMonitor above scrapes.
	assert.Contains(t, joined, "controller_runtime_reconcile_errors_total")

	// The window has to outlast the retry backoff. controller-runtime caps a
	// failed reconcile's retry at 1000 seconds, so a workload that has been
	// stuck for a while errors three or four times an hour and not at all in
	// most five-minute windows: a rate over five minutes reads zero for exactly
	// the case this alert exists to catch.
	assert.Contains(t, joined, "increase(controller_runtime_reconcile_errors_total[1h])",
		"the error window must be longer than the 1000s retry backoff ceiling")
	assert.NotContains(t, joined, "rate(controller_runtime_reconcile_errors_total[5m])",
		"a five-minute rate cannot see a reconcile failing at the backoff ceiling")

	// Queue depth says work is arriving, which a busy cluster does honestly. It
	// cannot see a pass that went in and never came out, because that item has
	// already left the queue.
	assert.Contains(t, joined, "workqueue_longest_running_processor_seconds",
		"detecting a wedged reconcile needs the processor gauge, not queue depth")
	assert.NotContains(t, joined, "workqueue_depth",
		"queue depth false-positives on healthy sustained traffic")
}

// The CRDs arrive asynchronously after the HelmChart apply, and a CRD exists
// before the API server serves it. Applying a ServiceMonitor in that window
// fails with "no matches for kind", which is a install failure with a confusing
// message rather than a retry.
func TestObservabilityWaitsForTheMonitoringCRDsToBeEstablished(t *testing.T) {
	script := monitoringCRDWaitScript()

	assert.Contains(t, script, "--for=condition=established",
		"waiting for the CRD to merely exist admits the window where the API server is not yet serving it")
	assert.Contains(t, script, "servicemonitors.monitoring.coreos.com")
	assert.Contains(t, script, "prometheusrules.monitoring.coreos.com",
		"the PrometheusRule is applied in the same step and needs its CRD too")
	assert.Contains(t, script, "exit 1",
		"a wait that gives up must fail the install rather than carry on to an apply that cannot work")
}
