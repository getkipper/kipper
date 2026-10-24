package installer

import (
	"fmt"
	"regexp"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

// These queries distinguish absent objects from lookup failures so an unreadable
// StatefulSet cannot cause an update to its immutable claim template.
const (
	lokiTemplateQuery = `kubectl -n monitoring get statefulset loki --ignore-not-found ` +
		`-o jsonpath='{"loki-template["}{.spec.volumeClaimTemplates[0].spec.resources.requests.storage}{"]"}' 2>/dev/null`
	lokiClaimQuery = `kubectl -n monitoring get pvc storage-loki-0 --ignore-not-found ` +
		`-o jsonpath='{"loki-claim["}{.spec.resources.requests.storage}{"]"}' 2>/dev/null`
	// Read only after lokiTemplateQuery found the StatefulSet, so an empty
	// answer is an error rather than a missing object.
	lokiStatefulSetQuery = `kubectl -n monitoring get statefulset loki -o json`
)

var (
	lokiTemplatePayload = regexp.MustCompile(`loki-template\[([^\]]*)\]`)
	lokiClaimPayload    = regexp.MustCompile(`loki-claim\[([^\]]*)\]`)
)

// lokiStorageRequest picks the volume size the chart renders from the two
// lookups using platform.LokiStorageSize.
func lokiStorageRequest(templateOut, claimOut string) (string, error) {
	templateSize, err := markedSize(lokiTemplatePayload, templateOut)
	if err != nil {
		return "", err
	}
	claimSize, err := markedSize(lokiClaimPayload, claimOut)
	if err != nil {
		return "", err
	}
	return platform.LokiStorageSize(templateSize, claimSize), nil
}

// markedSize validates the first matched quantity. An absent or empty match
// returns an empty string. An invalid quantity returns an error.
func markedSize(payload *regexp.Regexp, out string) (string, error) {
	m := payload.FindStringSubmatch(out)
	if len(m) < 2 || m[1] == "" {
		return "", nil
	}
	if _, err := resource.ParseQuantity(m[1]); err != nil {
		return "", fmt.Errorf("reading the log volume size %q: %w", m[1], err)
	}
	return m[1], nil
}

// lokiStorage reads the existing volume size and storage class, picking them
// with platform.LokiStorageSize and platform.LokiStorageClass. Lookup and
// parsing failures return errors.
func lokiStorage(client CommandRunner, storageMode string) (string, *string, error) {
	templateOut, err := client.Run(lokiTemplateQuery)
	if err != nil {
		return "", nil, fmt.Errorf("reading the log volume size: %w", err)
	}
	claimOut, err := client.Run(lokiClaimQuery)
	if err != nil {
		return "", nil, fmt.Errorf("reading the log volume size: %w", err)
	}
	size, err := lokiStorageRequest(templateOut, claimOut)
	if err != nil {
		return "", nil, err
	}
	if lokiTemplatePayload.MatchString(templateOut) {
		var sts appsv1.StatefulSet
		if err := kubectlJSON(client, "the log StatefulSet", lokiStatefulSetQuery, &sts); err != nil {
			return "", nil, err
		}
		var templateClass *string
		if len(sts.Spec.VolumeClaimTemplates) > 0 {
			templateClass = sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName
		}
		return size, platform.LokiStorageClass(true, templateClass, "", storageMode), nil
	}
	claimClass, err := liveClaimClass(client, platform.MonitoringNamespace, "storage-loki-0", "")
	if err != nil {
		return "", nil, err
	}
	return size, platform.LokiStorageClass(false, nil, claimClass, storageMode), nil
}

// InstallLoki applies Loki with memory resources from the profile and adds Promtail.
// It preserves existing Loki storage through InstallLokiWithResources.
func InstallLoki(client CommandRunner, profile, storageMode string) error {
	return InstallLokiWithResources(client, platform.ResourcesForProfile(profile), storageMode)
}

// InstallLokiWithResources applies Loki with the supplied memory resources and
// adds Promtail. It preserves the existing Loki template or claim size and
// storage class, and gives a new log volume the single-copy class of
// storageMode. Lookup, parsing and apply failures return errors. It also
// attempts to raise the host inotify limit and returns without waiting for the
// charts to be ready.
func InstallLokiWithResources(client CommandRunner, res platform.Resources, storageMode string) error {
	// Preserve the immutable claim template when updating an existing StatefulSet.
	storage, class, err := lokiStorage(client, storageMode)
	if err != nil {
		return err
	}
	manifest := platform.LokiHelmChart(res, storage, class)

	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying Loki HelmChart: %w", err)
	}

	// Apply the Promtail limit during upgrades too. Workers receive it when joining.
	_ = raiseInotifyLimit(client)

	// Promtail supplies Loki with container logs.
	promtailCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%s\nKIPEOF", platform.PromtailHelmChart())
	if _, err := client.Run(promtailCmd); err != nil {
		return fmt.Errorf("applying Promtail HelmChart: %w", err)
	}

	return nil
}

// InstallNodeExporterTLS provisions node-exporter's TLS certificate and waits for readiness.
// Install and upgrade call it even when monitoring is disabled, so the required
// Secret is ready when monitoring is enabled later.
func InstallNodeExporterTLS(client CommandRunner) error {
	if _, err := client.Run(ensureNamespaceCmd(platform.MonitoringNamespace)); err != nil {
		return fmt.Errorf("ensuring monitoring namespace: %w", err)
	}
	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", platform.NodeExporterTLSManifests())
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying node-exporter certificate: %w", err)
	}
	waitCmd := fmt.Sprintf("kubectl wait --for=condition=Ready certificate/%s -n %s --timeout=180s",
		platform.NodeExporterCertName, platform.MonitoringNamespace)
	if _, err := client.Run(waitCmd); err != nil {
		return fmt.Errorf("waiting for the node-exporter certificate: %w", err)
	}
	return nil
}

// InstallPrometheusGrafanaWithResources installs the kube-prometheus-stack
// HelmChart with explicit memory resources. Used by the upgrade path to
// preserve PlatformConfig overrides that the simple profile-only signature
// would otherwise overwrite.
func InstallPrometheusGrafanaWithResources(client CommandRunner, res platform.Resources) error {
	// The chart reads Grafana's admin password from this Secret
	// (grafana.admin.existingSecret), so it must exist before the release
	// installs or Grafana starts with a dangling reference.
	if err := ensureGrafanaAdminSecret(client); err != nil {
		return err
	}
	// Restrict access to Grafana before starting it in auth-proxy mode.
	if err := applyGrafanaNetworkPolicy(client); err != nil {
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
func ensureGrafanaAdminSecret(client CommandRunner) error {
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
