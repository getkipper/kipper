package installer

import (
	"fmt"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

const (
	kedaVersion        = "2.16.1"
	kedaHTTPAddonChart = "0.14.1"
)

// InstallKEDA installs the KEDA event-driven autoscaler from its upstream Helm chart
// via the k3s HelmChart CRD. KEDA enables scale-to-zero for Kipper functions.
func InstallKEDA(client *ssh.Client) error {
	manifest := fmt.Sprintf(`apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: keda
  namespace: kube-system
spec:
  repo: https://kedacore.github.io/charts
  chart: keda
  version: %s
  targetNamespace: keda
  createNamespace: true
  valuesContent: |-
    resources:
      operator:
        requests:
          cpu: 50m
          memory: 64Mi
        limits:
          memory: 256Mi
      metricServer:
        requests:
          cpu: 25m
          memory: 32Mi
        limits:
          memory: 128Mi
`, kedaVersion)

	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying KEDA HelmChart: %w", err)
	}

	// Wait for KEDA operator to be ready
	waitCmd := "kubectl -n keda rollout status deployment/keda-operator --timeout=180s 2>/dev/null || true"
	if _, err := client.Run(waitCmd); err != nil {
		return fmt.Errorf("waiting for KEDA: %w", err)
	}

	// Install the HTTP Add-on for scale-to-zero HTTP functions.
	// scaler.replicas: 1 because the chart's default of 3 is overkill
	// for single-node Kipper clusters.
	httpAddonManifest := fmt.Sprintf(`apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: keda-http-addon
  namespace: kube-system
spec:
  repo: https://kedacore.github.io/charts
  chart: keda-add-ons-http
  version: %s
  targetNamespace: keda
  createNamespace: true
  valuesContent: |-
    interceptor:
      resources:
        requests:
          cpu: 25m
          memory: 32Mi
        limits:
          memory: 128Mi
    scaler:
      replicas: 1
      resources:
        requests:
          cpu: 25m
          memory: 32Mi
        limits:
          memory: 64Mi
    operator:
      resources:
        requests:
          cpu: 25m
          memory: 32Mi
        limits:
          memory: 64Mi
`, kedaHTTPAddonChart)
	httpAddonCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", httpAddonManifest)
	if _, err := client.Run(httpAddonCmd); err != nil {
		return fmt.Errorf("applying KEDA HTTP Add-on: %w", err)
	}

	return nil
}
