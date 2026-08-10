package installer

import (
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// traefikManifestTemplate carries one %s per entrypoint for the rendered
// trustedIPs list. The web entrypoint redirects everything to websecure:
// gated routes carry API keys in headers, and a key accepted over cleartext
// HTTP would undo the hashed, reveal-once key design. Let's Encrypt HTTP-01
// follows the redirect, so certificate issuance is unaffected.
const traefikManifestTemplate = `apiVersion: v1
kind: Namespace
metadata:
  name: traefik
---
apiVersion: helm.cattle.io/v1
kind: HelmChart
metadata:
  name: traefik
  namespace: kube-system
spec:
  repo: https://traefik.github.io/charts
  chart: traefik
  targetNamespace: traefik
  version: 41.0.2
  valuesContent: |-
    # Pin the proxy image by digest to v3.7.8. The chart's own appVersion is
    # v3.7.6, which still carries GHSA-cxjq-mrr5-89rv (path-traversal auth
    # bypass in ReplacePathRegex) and GHSA-8rxv-jg7p-wvg3, both fixed in the
    # v3.7.7/v3.7.8 patch line; a path-gating bypass is exactly the class this
    # entrypoint must not reproduce. digest is what actually gets pulled
    # (repository@digest, tag ignored for the ref). versionOverride tells the
    # chart's version-gated logic the real version, which it cannot derive
    # from a digest — the chart documents setting it alongside image.digest.
    versionOverride: v3.7.8
    image:
      tag: v3.7.8
      digest: sha256:4299bbed850421258fc5448c2e0e6ad350981d4d335a68de11b92448aedbefe5
    ingressRoute:
      dashboard:
        enabled: false
    providers:
      kubernetesIngress:
        allowEmptyServices: true
    # X-Forwarded-* is honoured only from the listed proxies: the kipper.run
    # gateway (resolved at install/upgrade time) and any --trusted-proxy
    # entries. An empty list means forwarded headers are ignored, which is
    # the safe default when nothing sits in front of Traefik.
    ports:
      web:
        http:
          redirections:
            entryPoint:
              to: websecure
              scheme: https
              permanent: true
        forwardedHeaders:
          trustedIPs: %s
      websecure:
        forwardedHeaders:
          trustedIPs: %s
    # Ingress runs on the server node, because that is the address everything
    # points at: the cluster's DNS records, its gateway registration, and the
    # loopback pin the API server resolves its OIDC issuer through. With
    # externalTrafficPolicy Local, kube-proxy serves the NodePort only on a node
    # that has a Traefik pod, and k3s's service LB sends :443 to its own node's
    # NodePort — so a Traefik scheduled onto a worker leaves the published
    # address answering nothing, and takes operator authentication with it,
    # because the pin resolves to a port on the server that now drops traffic.
    # One replica has to be somewhere; this makes it the somewhere the rest of
    # the system already assumes.
    nodeSelector:
      node-role.kubernetes.io/control-plane: "true"
    tolerations:
      - key: node-role.kubernetes.io/control-plane
        operator: Exists
        effect: NoSchedule
      - key: CriticalAddonsOnly
        operator: Exists
    service:
      spec:
        # Preserves the client address, which the per-app rate limits and the
        # forwarded-header trust list above both depend on. It is also why the
        # nodeSelector is required rather than merely tidy.
        externalTrafficPolicy: Local
    # Prometheus metrics are on by default in the chart; the service label
    # (namespace-app-port) is what maps request counts back to Kipper apps.
    # Router labels stay off: router names embed hosts/paths and grow
    # unbounded with custom domains. The dedicated metrics Service is a
    # plain Service (no CRD dependency); the ServiceMonitor that scrapes it
    # ships with the observability stack, which owns that CRD.
    metrics:
      prometheus:
        addServicesLabels: true
        service:
          enabled: true
`

// renderTrustedIPs validates every entry as an IP or CIDR and renders the
// YAML flow list for the chart values. Bare IPs are normalised to host
// prefixes. Validation doubles as heredoc safety: only canonical prefix
// strings ever reach the manifest.
func renderTrustedIPs(proxies []string) (string, error) {
	if len(proxies) == 0 {
		return "[]", nil
	}
	rendered := make([]string, 0, len(proxies))
	seen := map[string]bool{}
	for _, p := range proxies {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(p)
		if err != nil {
			addr, addrErr := netip.ParseAddr(p)
			if addrErr != nil {
				return "", fmt.Errorf("invalid trusted proxy %q: must be an IP address or CIDR", p)
			}
			prefix = netip.PrefixFrom(addr, addr.BitLen())
		}
		canon := prefix.String()
		if seen[canon] {
			continue
		}
		seen[canon] = true
		rendered = append(rendered, `"`+canon+`"`)
	}
	if len(rendered) == 0 {
		return "[]", nil
	}
	return "[" + strings.Join(rendered, ", ") + "]", nil
}

// lookupIP is swapped in tests; DNS is otherwise the real resolver.
var lookupIP = net.LookupIP

// ResolveTrustedProxies returns the forwarded-header trust list for a
// cluster: the resolved addresses of its own *.kipper.run domain (the
// wildcard points at the central gateway, whose reverse proxy appends the
// real client IP) plus any operator-supplied entries. The gateway addresses
// are resolved fresh on every install and upgrade rather than persisted, so
// a gateway IP change cannot pin stale trust. Resolution failure warns and
// continues: the cluster still works, requests just carry the gateway's
// address as the client until the next upgrade resolves it.
func ResolveTrustedProxies(domain string, extras []string) []string {
	proxies := append([]string{}, extras...)
	if strings.HasSuffix(domain, ".kipper.run") {
		ips, err := lookupIP(domain)
		if err != nil {
			fmt.Printf("  ⚠  could not resolve %s to trust the kipper.run gateway for client IPs: %v\n", domain, err)
			return proxies
		}
		for _, ip := range ips {
			proxies = append(proxies, ip.String())
		}
	}
	return proxies
}

// InstallTraefik installs Traefik as the ingress controller using the
// k3s HelmChart CRD. We disable the k3s-bundled Traefik in the k3s
// config and install our own with custom settings. trustedProxies is the
// forwarded-header trust list from ResolveTrustedProxies.
func InstallTraefik(client *ssh.Client, trustedProxies []string) error {
	trusted, err := renderTrustedIPs(trustedProxies)
	if err != nil {
		return err
	}
	manifest := fmt.Sprintf(traefikManifestTemplate, trusted, trusted)

	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying traefik manifest: %w", err)
	}

	// The HelmChart controller needs time to pull the chart and create
	// the deployment. On a fresh server, downloading the chart and
	// pulling the container image can take several minutes. Wait for
	// the deployment to exist before checking rollout status.
	waitExistCmd := `for i in $(seq 1 60); do kubectl -n traefik get deployment/traefik >/dev/null 2>&1 && break; sleep 5; done`
	if _, err := client.Run(waitExistCmd); err != nil {
		return fmt.Errorf("waiting for traefik deployment to be created: %w", err)
	}

	waitCmd := "kubectl -n traefik rollout status deployment/traefik --timeout=300s"
	if _, err := client.Run(waitCmd); err != nil {
		return fmt.Errorf("waiting for traefik: %w", err)
	}

	return nil
}
