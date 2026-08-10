package installer

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// ExistingIdentity is the serving identity read from an existing cluster's
// ClusterIdentity CR before the installer decides what to render. Empty host
// fields mean "derived from the domain by convention", matching the CR.
type ExistingIdentity struct {
	Domain          string
	ConsoleHost     string
	ConsoleAPIHost  string
	DexHost         string
	KipperRunDomain string
}

// ReadExistingClusterIdentity returns the serving identity of an existing
// cluster, plus whether a k3s installation was already present. A fresh host
// returns (nil, false, nil); an existing cluster without the CR (a legacy or
// half-installed cluster the create-if-absent step will adopt) returns
// (nil, true, nil). An existing cluster whose identity cannot be read is an
// error: proceeding would let the installer render an identity the cluster
// may have moved away from, which the reconciler's issuer guard can never
// converge back from.
func ReadExistingClusterIdentity(client *ssh.Client) (*ExistingIdentity, bool, error) {
	freshness, err := client.Run("test -d /var/lib/rancher/k3s && echo existing || echo fresh")
	k3sPreexisting := k3sPreexistingFromSample(freshness, err)
	if !k3sPreexisting {
		return nil, false, nil
	}

	out, err := client.Run("kubectl get clusteridentity cluster -o json")
	if err != nil {
		if identityAbsentFromKubectl(out + " " + err.Error()) {
			return nil, true, nil
		}
		return nil, true, fmt.Errorf("this host already runs a cluster but its serving identity could not be read (fix the apiserver or kubectl first, or wipe the host with 'kip cluster uninstall'): %w", err)
	}
	identity, err := ParseExistingIdentity(out)
	if err != nil {
		return nil, true, err
	}
	return identity, true, nil
}

// identityAbsentFromKubectl reports whether a failed kubectl read means the
// ClusterIdentity simply does not exist (no CR yet, or a cluster predating
// the CRD), as opposed to a transport or apiserver failure.
func identityAbsentFromKubectl(combined string) bool {
	return strings.Contains(combined, "NotFound") ||
		strings.Contains(combined, "doesn't have a resource type") ||
		strings.Contains(combined, "the server could not find the requested resource")
}

// ParseExistingIdentity decodes the identity fields from a
// `kubectl get clusteridentity -o json` body. Any leading kubectl warnings
// before the JSON document are skipped. A CR with an in-flight transition is
// an error: during a transition the spec already names the destination while
// login may still use the outgoing identity, so rendering the spec would cut
// the cluster over outside the phase machine — exactly what reading the CR is
// meant to prevent.
func ParseExistingIdentity(kubectlOutput string) (*ExistingIdentity, error) {
	start := strings.Index(kubectlOutput, "{")
	if start < 0 {
		return nil, fmt.Errorf("no JSON in the ClusterIdentity read: %q", strings.TrimSpace(kubectlOutput))
	}
	var doc struct {
		Spec struct {
			Domain string `json:"domain"`
			Hosts  struct {
				Console    string `json:"console"`
				ConsoleAPI string `json:"consoleAPI"`
				Dex        string `json:"dex"`
			} `json:"hosts"`
			Gateway struct {
				KipperRunDomain string `json:"kipperRunDomain"`
			} `json:"gateway"`
		} `json:"spec"`
		Status struct {
			Transition *struct {
				Phase string `json:"phase"`
			} `json:"transition"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(kubectlOutput[start:]), &doc); err != nil {
		return nil, fmt.Errorf("decoding the ClusterIdentity: %w", err)
	}
	if t := doc.Status.Transition; t != nil {
		return nil, fmt.Errorf("a domain change is in flight (phase %s); finish it with 'kip cluster domain --sync' or roll it back before reinstalling", t.Phase)
	}
	if doc.Spec.Domain == "" {
		return nil, fmt.Errorf("the ClusterIdentity carries no domain; the cluster is in a broken state, repair it with 'kip cluster domain --sync' before reinstalling")
	}
	return &ExistingIdentity{
		Domain:          doc.Spec.Domain,
		ConsoleHost:     doc.Spec.Hosts.Console,
		ConsoleAPIHost:  doc.Spec.Hosts.ConsoleAPI,
		DexHost:         doc.Spec.Hosts.Dex,
		KipperRunDomain: doc.Spec.Gateway.KipperRunDomain,
	}, nil
}

// AdoptIdentity validates the install flags against an existing cluster's
// identity and returns the identity the install must render. Changing a
// serving identity is the reconciler's job ('kip cluster domain'), which
// keeps login available through the change; the installer therefore refuses
// any flag that names a different identity rather than silently overwriting
// the one being served.
func AdoptIdentity(existing *ExistingIdentity, optDomain, optConsole, optConsoleAPI, optDex string) (*ExistingIdentity, error) {
	if optDomain != "" && optDomain != existing.Domain {
		return nil, fmt.Errorf("this cluster already serves %s; a reinstall keeps that identity, so change the domain with 'kip cluster domain %s' instead", existing.Domain, optDomain)
	}
	for _, c := range []struct{ flag, opt, current string }{
		{"--console-domain", optConsole, existing.ConsoleHost},
		{"--console-api-domain", optConsoleAPI, existing.ConsoleAPIHost},
		{"--dex-domain", optDex, existing.DexHost},
	} {
		if c.opt != "" && c.opt != c.current {
			return nil, fmt.Errorf("%s=%s conflicts with the cluster's serving identity; host changes go through 'kip cluster domain'", c.flag, c.opt)
		}
	}
	return existing, nil
}
