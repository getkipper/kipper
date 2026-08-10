package installer

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/pubip"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// dnsHostnamePattern matches a DNS hostname. Validating against it before the
// value reaches the YAML manifest or the kubectl heredoc closes both YAML
// injection and heredoc-terminator escapes: a valid hostname has no whitespace,
// newlines, or YAML/shell metacharacters.
var dnsHostnamePattern = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func validateHost(field, value string, required bool) error {
	if value == "" {
		if required {
			return fmt.Errorf("%s is required", field)
		}
		return nil
	}
	if !dnsHostnamePattern.MatchString(value) {
		return fmt.Errorf("invalid %s %q: must be a DNS hostname", field, value)
	}
	return nil
}

// validateClusterHost checks the address the gateway will register, against the
// gateway's own policy: a routable public IP. A name, or a private or otherwise
// non-global address, would install cleanly and then fail every heartbeat.
// Requiring an address also keeps the value free of anything that could break the
// YAML manifest or the kubectl heredoc.
func validateClusterHost(clusterHost string) error {
	if clusterHost == "" {
		return nil
	}
	if !pubip.IsPublic(clusterHost) {
		return fmt.Errorf("invalid cluster host %q: the gateway registers routable public IP addresses only", clusterHost)
	}
	return nil
}

// ClusterIdentityManifest builds the singleton ClusterIdentity CR the reconciler
// adopts. Host overrides are included only when set — an empty override means
// "derive from the domain by convention" — and the gateway block only for a
// *.kipper.run registration. The gateway block carries clusterHost so the
// reconciler can re-render CLUSTER_HOST instead of blanking it.
func ClusterIdentityManifest(domain, consoleOverride, consoleAPIOverride, dexOverride, kipperRunDomain, clusterHost string) string {
	var b strings.Builder
	b.WriteString("apiVersion: kipper.run/v1alpha1\n")
	b.WriteString("kind: ClusterIdentity\n")
	b.WriteString("metadata:\n  name: cluster\n")
	b.WriteString("spec:\n")
	fmt.Fprintf(&b, "  domain: %s\n", domain)
	if consoleOverride != "" || consoleAPIOverride != "" || dexOverride != "" {
		b.WriteString("  hosts:\n")
		if consoleOverride != "" {
			fmt.Fprintf(&b, "    console: %s\n", consoleOverride)
		}
		if consoleAPIOverride != "" {
			fmt.Fprintf(&b, "    consoleAPI: %s\n", consoleAPIOverride)
		}
		if dexOverride != "" {
			fmt.Fprintf(&b, "    dex: %s\n", dexOverride)
		}
	}
	if kipperRunDomain != "" {
		b.WriteString("  gateway:\n")
		fmt.Fprintf(&b, "    kipperRunDomain: %s\n", kipperRunDomain)
		if clusterHost != "" {
			fmt.Fprintf(&b, "    clusterHost: %s\n", clusterHost)
		}
		b.WriteString("    register: true\n")
	}
	return b.String()
}

// validateIdentityInputs checks everything that reaches the manifest. The hosts
// are DNS names; clusterHost is an address and is only checked when a gateway
// block will actually carry it, since a custom-domain install registers nothing
// and is legitimately reached through a hostname.
func validateIdentityInputs(domain, consoleOverride, consoleAPIOverride, dexOverride, kipperRunDomain, clusterHost string) error {
	for _, v := range []struct {
		field, value string
		required     bool
	}{
		{"domain", domain, true},
		{"console domain", consoleOverride, false},
		{"console-api domain", consoleAPIOverride, false},
		{"dex domain", dexOverride, false},
		{"kipper.run domain", kipperRunDomain, false},
	} {
		if err := validateHost(v.field, v.value, v.required); err != nil {
			return err
		}
	}
	if kipperRunDomain == "" {
		return nil
	}
	return validateClusterHost(clusterHost)
}

// InstallClusterIdentity creates the singleton ClusterIdentity CR if it does not
// already exist, so the console-api reconciler adopts the serving identity on
// first boot. Create-if-absent preserves an existing CR, so re-running install
// never stomps a custom domain a cluster has since moved to.
func InstallClusterIdentity(client *ssh.Client, domain, consoleOverride, consoleAPIOverride, dexOverride, kipperRunDomain, clusterHost string) error {
	if err := validateIdentityInputs(domain, consoleOverride, consoleAPIOverride, dexOverride, kipperRunDomain, clusterHost); err != nil {
		return err
	}

	if _, err := client.Run("kubectl wait --for=condition=Established crd/clusteridentities.kipper.run --timeout=60s"); err != nil {
		return fmt.Errorf("waiting for ClusterIdentity CRD: %w", err)
	}
	manifest := ClusterIdentityManifest(domain, consoleOverride, consoleAPIOverride, dexOverride, kipperRunDomain, clusterHost)
	cmd := fmt.Sprintf(
		"kubectl get clusteridentity cluster >/dev/null 2>&1 || cat <<'KIPEOF' | kubectl create -f -\n%sKIPEOF",
		manifest,
	)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("creating ClusterIdentity: %w", err)
	}
	return nil
}
