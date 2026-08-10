package installer

import (
	"fmt"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// InstallLoki installs Grafana Loki in single-binary mode with filesystem
// storage. Provides persistent, searchable log aggregation for all pods.
// Memory limits come from the supplied profile.
//
// The upgrade path uses InstallLokiWithResources directly to honor any
// per-component overrides recorded on the PlatformConfig CR.
func InstallLoki(client *ssh.Client, profile string) error {
	return InstallLokiWithResources(client, platform.ResourcesForProfile(profile))
}

// InstallLokiWithResources installs Loki using the supplied memory resources.
// Use this when the effective limits depend on more than the profile alone
// (e.g. an upgrade reading PlatformConfig.spec.components overrides).
func InstallLokiWithResources(client *ssh.Client, res platform.Resources) error {
	manifest := platform.LokiHelmChart(res)

	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying Loki HelmChart: %w", err)
	}

	// Increase inotify limits for Promtail log file watching
	_, _ = client.Run("sysctl -w fs.inotify.max_user_instances=512")
	_, _ = client.Run("grep -q max_user_instances /etc/sysctl.d/99-kipper.conf 2>/dev/null || echo 'fs.inotify.max_user_instances=512' >> /etc/sysctl.d/99-kipper.conf")

	// Install Promtail to ship container logs to Loki
	promtailCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%s\nKIPEOF", platform.PromtailHelmChart())
	if _, err := client.Run(promtailCmd); err != nil {
		return fmt.Errorf("applying Promtail HelmChart: %w", err)
	}

	return nil
}

// InstallPrometheusGrafanaWithResources installs the kube-prometheus-stack
// HelmChart with explicit memory resources. Used by the upgrade path to
// preserve PlatformConfig overrides that the simple profile-only signature
// would otherwise overwrite.
func InstallPrometheusGrafanaWithResources(client *ssh.Client, res platform.Resources) error {
	// The chart reads Grafana's admin password from this Secret
	// (grafana.admin.existingSecret), so it must exist before the release
	// installs or Grafana starts with a dangling reference.
	if err := ensureGrafanaAdminSecret(client); err != nil {
		return err
	}

	manifest := platform.KubePrometheusStackHelmChart(res)
	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying kube-prometheus-stack HelmChart: %w", err)
	}

	// The helm-controller installs the stack asynchronously, so the CRDs appear
	// some time after the HelmChart apply. Wait for them before applying
	// anything that uses them.
	//
	// Established rather than merely present: a CRD exists before the API server
	// is serving it, and an apply in that window fails with "no matches for
	// kind". kubectl wait returns non-zero while the CRD is absent too, so the
	// loop covers both states.
	waitCRD := monitoringCRDWaitScript()
	if _, err := client.Run(waitCRD); err != nil {
		return fmt.Errorf("waiting for the monitoring crds: %w", err)
	}
	smCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", platform.TraefikServiceMonitor())
	if _, err := client.Run(smCmd); err != nil {
		return fmt.Errorf("applying traefik servicemonitor: %w", err)
	}

	// The controller manager's own metrics, and the alerts that read them. A
	// reconcile failing on every pass is invisible without both: the scrape
	// gives the counter somewhere to go, the rule gives it somewhere to arrive.
	apiSMCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", platform.ConsoleAPIServiceMonitor())
	if _, err := client.Run(apiSMCmd); err != nil {
		return fmt.Errorf("applying console-api servicemonitor: %w", err)
	}
	rulesCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", platform.KipperControllerAlerts())
	if _, err := client.Run(rulesCmd); err != nil {
		return fmt.Errorf("applying kipper controller alerts: %w", err)
	}
	return nil
}

// ensureGrafanaAdminSecret provisions the Grafana admin credentials into the
// grafana-admin Secret, generating a random password on first install and
// reusing the existing one on every re-run. Grafana reads it via
// grafana.admin.existingSecret, so a re-run never rotates the password and
// invalidates a saved login. The password is fed over stdin, so it never
// appears in the command string or the process table.
func ensureGrafanaAdminSecret(client *ssh.Client) error {
	if _, err := client.Run(ensureNamespaceCmd(platform.MonitoringNamespace)); err != nil {
		return fmt.Errorf("ensuring monitoring namespace: %w", err)
	}

	existing, err := readSecretValue(client, platform.MonitoringNamespace,
		platform.GrafanaAdminSecretName, platform.GrafanaAdminPasswordKey)
	if err != nil {
		return fmt.Errorf("reading grafana admin secret: %w", err)
	}
	if existing != "" {
		return nil
	}

	password, err := generateSecret(24)
	if err != nil {
		return fmt.Errorf("generating grafana admin password: %w", err)
	}
	cmd := applySecretWithLiteralCmd(platform.MonitoringNamespace, platform.GrafanaAdminSecretName,
		platform.GrafanaAdminUserKey, platform.GrafanaAdminUser, platform.GrafanaAdminPasswordKey)
	if _, err := client.RunStdin(cmd, strings.NewReader(password)); err != nil {
		return fmt.Errorf("creating grafana admin secret: %w", err)
	}
	return nil
}

// monitoringCRDWaitScript blocks until the CRDs the monitoring manifests need
// are being served.
//
// Established rather than merely present: a CRD exists before the API server is
// serving it, and an apply in that window fails with "no matches for kind".
// kubectl wait returns non-zero while the CRD is absent too, so one loop covers
// both states.
func monitoringCRDWaitScript() string {
	return `for i in $(seq 1 60); do kubectl wait --for=condition=established --timeout=5s crd/servicemonitors.monitoring.coreos.com crd/prometheusrules.monitoring.coreos.com >/dev/null 2>&1 && exit 0; sleep 5; done; echo "timed out waiting for the monitoring CRDs to be established" >&2; exit 1`
}
