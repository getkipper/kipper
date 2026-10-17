package installer

import (
	"fmt"
)

const (
	kedaVersion        = "2.16.1"
	kedaHTTPAddonChart = "0.14.1"
)

// InstallKEDA applies the upstream KEDA and HTTP Add-on charts via k3s HelmChart
// resources. The minimum HTTP interceptor count is one for ProfileNano and three
// for all other profile values. A successful return means both resources were
// applied, not that their workloads are ready.
func InstallKEDA(client CommandRunner, profile string) error {
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
    nodeSelector:
      node-role.kubernetes.io/control-plane: "true"
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

	// Rollout failures are ignored so the HTTP Add-on can still be applied.
	waitCmd := "kubectl -n keda rollout status deployment/keda-operator --timeout=180s 2>/dev/null || true"
	if _, err := client.Run(waitCmd); err != nil {
		return fmt.Errorf("waiting for KEDA: %w", err)
	}

	// A single scaler limits resource use on the control-plane node.
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
      replicas:
        min: %d
      nodeSelector:
        node-role.kubernetes.io/control-plane: "true"
      resources:
        requests:
          cpu: 25m
          memory: 32Mi
        limits:
          memory: 128Mi
    scaler:
      replicas: 1
      nodeSelector:
        node-role.kubernetes.io/control-plane: "true"
      resources:
        requests:
          cpu: 25m
          memory: 32Mi
        limits:
          memory: 64Mi
    operator:
      nodeSelector:
        node-role.kubernetes.io/control-plane: "true"
      resources:
        requests:
          cpu: 25m
          memory: 32Mi
        limits:
          memory: 64Mi
`, kedaHTTPAddonChart, interceptorReplicas(profile))
	httpAddonCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", httpAddonManifest)
	if _, err := client.Run(httpAddonCmd); err != nil {
		return fmt.Errorf("applying KEDA HTTP Add-on: %w", err)
	}

	return nil
}

// interceptorReplicas lowers the minimum replica count for nano to save memory.
func interceptorReplicas(profile string) int {
	if profile == ProfileNano {
		return 1
	}
	return 3
}
