package installer

import (
	"fmt"
)

// InstallSecurityHardening creates rate-limit middleware and configures baseline
// Pod Security warnings and auditing in namespaces selected by the installer.
func InstallSecurityHardening(client CommandRunner) error {
	if err := installRateLimiting(client); err != nil {
		return err
	}
	if err := installPodSecurityStandards(client); err != nil {
		return err
	}
	return nil
}

func installRateLimiting(client CommandRunner) error {
	// Rate limiting middleware — 100 requests per second per IP.
	// Protects against brute force attacks and basic DDoS.
	manifest := `apiVersion: traefik.io/v1alpha1
kind: Middleware
metadata:
  name: rate-limit
  namespace: traefik
spec:
  rateLimit:
    average: 100
    burst: 200
    period: 1s
    sourceCriterion:
      ipStrategy:
        depth: 1
`

	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying rate limiting middleware: %w", err)
	}

	return nil
}

func installPodSecurityStandards(client CommandRunner) error {
	// Configure baseline warnings and auditing in existing namespaces, excluding
	// the system components listed below. The project controller sets baseline
	// enforcement separately when creating project namespaces.
	labelCmd := `for ns in $(kubectl get namespaces -o name | grep -v 'kube-system\|kube-public\|kube-node-lease\|traefik\|cert-manager\|longhorn-system\|dex\|kipper-system\|monitoring\|velero\|keda'); do
  kubectl label --overwrite $ns pod-security.kubernetes.io/warn=baseline pod-security.kubernetes.io/audit=baseline
done 2>/dev/null || true`

	if _, err := client.Run(labelCmd); err != nil {
		return fmt.Errorf("applying pod security labels: %w", err)
	}

	return nil
}
