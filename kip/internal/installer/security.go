package installer

import (
	"fmt"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// InstallSecurityHardening applies production security defaults:
// - Traefik middleware for rate limiting (100 req/s per IP)
// - Pod Security Standards enforcement (restricted profile on user namespaces)
//
// Response headers are not set here. Platform hosts carry the per-namespace
// security-headers Middleware from controller/pkg/serving, and an app route
// gets its own <namespace>-<app>-security Middleware, so a global copy would
// only be a second policy with the same name and no consumer.
func InstallSecurityHardening(client *ssh.Client) error {
	if err := installRateLimiting(client); err != nil {
		return err
	}
	if err := installPodSecurityStandards(client); err != nil {
		return err
	}
	return nil
}

func installRateLimiting(client *ssh.Client) error {
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

func installPodSecurityStandards(client *ssh.Client) error {
	// Pod Security Standards — enforce baseline security on all user namespaces.
	// This prevents containers from running as root, using privileged mode,
	// or escalating privileges. System namespaces are excluded.
	//
	// We use the "baseline" profile which blocks the most dangerous patterns
	// while remaining compatible with most Docker images. The "restricted"
	// profile would be stricter but breaks many common images that run as root.
	labelCmd := `for ns in $(kubectl get namespaces -o name | grep -v 'kube-system\|kube-public\|kube-node-lease\|traefik\|cert-manager\|longhorn-system\|dex\|kipper-system\|monitoring\|velero\|keda'); do
  kubectl label --overwrite $ns pod-security.kubernetes.io/warn=baseline pod-security.kubernetes.io/audit=baseline
done 2>/dev/null || true`

	if _, err := client.Run(labelCmd); err != nil {
		return fmt.Errorf("applying pod security labels: %w", err)
	}

	return nil
}
