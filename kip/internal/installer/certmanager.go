package installer

import (
	"encoding/json"
	"fmt"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

const certManagerVersion = "v1.17.2"

const clusterIssuerManifest = `apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: "%s"
    privateKeySecretRef:
      name: letsencrypt-prod
    solvers:
      - http01:
          ingress:
            class: traefik
`

// InstallCertManager installs cert-manager from its upstream manifest
// and creates a Let's Encrypt ClusterIssuer for automatic TLS.
//
// Also patches the cert-manager controller Deployment's `dnsConfig` so
// HTTP-01 self-checks resolve through the cluster's configured DNS
// resolvers directly, bypassing CoreDNS's forward chain and negative
// cache. On a freshly-created A record CoreDNS can serve a cached
// NXDOMAIN long enough to make the self-check fail even though Let's
// Encrypt reaches the challenge URL from outside. Witnessed on
// acme-tools 2026-05-17 when adding DNS for grafana.console.example.com.
// Using the same resolvers the operator chose (via --dns-resolver, or
// the public defaults) keeps this consistent with cluster DNS policy for
// private and corporate setups.
//
// Giving cert-manager its own dnsConfig + dnsPolicy=None bypasses
// CoreDNS for this one pod's outbound lookups, so the self-check
// succeeds as soon as the DNS record is live. Other workloads keep using
// CoreDNS — the smallest possible surface that fixes the bug.
func InstallCertManager(client *ssh.Client, email string, dnsResolvers []string) error {
	url := fmt.Sprintf(
		"https://github.com/cert-manager/cert-manager/releases/download/%s/cert-manager.yaml",
		certManagerVersion,
	)

	applyCmd := fmt.Sprintf("kubectl apply -f %s", url)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying cert-manager manifest: %w", err)
	}

	// Wait for cert-manager webhook to be ready before creating the ClusterIssuer
	waitCmd := "kubectl -n cert-manager rollout status deployment/cert-manager-webhook --timeout=120s"
	if _, err := client.Run(waitCmd); err != nil {
		return fmt.Errorf("waiting for cert-manager: %w", err)
	}

	if err := patchCertManagerDNSConfig(client, dnsResolvers); err != nil {
		return fmt.Errorf("configuring cert-manager DNS: %w", err)
	}

	issuer := fmt.Sprintf(clusterIssuerManifest, email)
	issuerCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", issuer)
	if _, err := client.Run(issuerCmd); err != nil {
		return fmt.Errorf("creating ClusterIssuer: %w", err)
	}

	return nil
}

// patchCertManagerDNSConfig sets `dnsPolicy: None` and a `dnsConfig` on
// the cert-manager controller Deployment so its pod resolves DNS through
// the cluster's configured resolvers directly, bypassing the CoreDNS
// chain. See the InstallCertManager docstring for why. When resolvers is
// empty it falls back to the public defaults.
//
// Idempotent: a strategic merge patch sets the same fields each time.
func patchCertManagerDNSConfig(client *ssh.Client, resolvers []string) error {
	patch, err := certManagerDNSPatch(resolvers)
	if err != nil {
		return err
	}
	cmd := fmt.Sprintf(
		`kubectl -n cert-manager patch deploy cert-manager --type=strategic --patch '%s'`,
		patch,
	)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("patching cert-manager Deployment: %w", err)
	}
	if _, err := client.Run("kubectl -n cert-manager rollout status deployment/cert-manager --timeout=120s"); err != nil {
		return fmt.Errorf("waiting for cert-manager rollout after dnsConfig patch: %w", err)
	}
	return nil
}

// certManagerDNSPatch builds the strategic merge patch that pins the
// cert-manager controller pod to the given resolvers. The list is run
// through resolveDNSResolvers so the upgrade path — which passes the
// persisted config value straight through — cannot render an invalid
// dnsConfig (IPv6, hostnames, over the nameserver limit) from a
// hand-edited config. An empty list falls back to the public defaults.
func certManagerDNSPatch(resolvers []string) (string, error) {
	resolved, err := resolveDNSResolvers(resolvers)
	if err != nil {
		return "", err
	}
	nameservers, err := json.Marshal(resolved)
	if err != nil {
		return "", fmt.Errorf("encoding cert-manager nameservers: %w", err)
	}
	return fmt.Sprintf(`{"spec":{"template":{"spec":{"dnsPolicy":"None","dnsConfig":{"nameservers":%s,"options":[{"name":"ndots","value":"5"}]}}}}}`, nameservers), nil
}
