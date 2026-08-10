package installer

import (
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/getkipper/kipper/controller/pkg/podnet"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// Build isolation moves image builds out of tenant namespaces into a single
// installer-owned namespace (kipper-builds) so a build's credentials never sit
// on a tenant-readable surface. The namespace enforces PodSecurity baseline
// rather than restricted: Kaniko must run as root to unpack image layers, which
// restricted forbids. baseline still blocks the escape vectors (privileged,
// hostPath, host namespaces, added capabilities); the clone and push containers
// are hardened individually in the build Job spec.

// buildIsolationManifest is rendered with the egress except-list (%s) before
// apply. The NetworkPolicy is default-deny both ways: no ingress (concurrent
// tenant builds cannot reach each other's pods), and egress only to DNS, the
// cluster registry, and public IPv4 on 80/443 with internal ranges excepted.
const buildIsolationManifest = `apiVersion: v1
kind: Namespace
metadata:
  name: kipper-builds
  labels:
    app.kubernetes.io/managed-by: kipper
    pod-security.kubernetes.io/enforce: baseline
    pod-security.kubernetes.io/enforce-version: latest
    pod-security.kubernetes.io/warn: restricted
    pod-security.kubernetes.io/audit: restricted
---
apiVersion: v1
kind: ServiceAccount
metadata:
  name: kipper-builder
  namespace: kipper-builds
  labels:
    app.kubernetes.io/managed-by: kipper
# Build pods run untrusted Dockerfile RUN steps, so their identity has no
# Kubernetes API access at all and the token is never mounted.
automountServiceAccountToken: false
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kipper-builds-egress
  namespace: kipper-builds
  labels:
    app.kubernetes.io/managed-by: kipper
spec:
  podSelector: {}
  policyTypes:
    - Ingress
    - Egress
  ingress: []
  egress:
    # DNS to the cluster CoreDNS pods (matched by pod, not ClusterIP, since the
    # ClusterIP falls inside the excepted RFC1918 range).
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kube-system
          podSelector:
            matchLabels:
              k8s-app: kube-dns
      ports:
        - protocol: UDP
          port: 53
        - protocol: TCP
          port: 53
    # The cluster registry, for the no-user-code push container's skopeo push.
    - to:
        - namespaceSelector:
            matchLabels:
              kubernetes.io/metadata.name: kipper-system
          podSelector:
            matchLabels:
              app: zot
      ports:
        - protocol: TCP
          port: 5000
    # Public egress for git clone and public base-image pulls. Ports are
    # restricted to 80/443: on bare-metal the node IPs are public, so the
    # port limit (not the RFC1918 except) is what closes kubelet:10250 and
    # apiserver:6443; the node IPs themselves are added to the except list so
    # a build cannot reach a node-bound service (ingress, host-port) on 80/443.
    # 169.254.0.0/16 blocks cloud instance-metadata credential theft.
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
            except:
%s
      ports:
        - protocol: TCP
          port: 80
        - protocol: TCP
          port: 443
---
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: console-api-builds
  namespace: kipper-builds
  labels:
    app.kubernetes.io/managed-by: kipper
rules:
  - apiGroups: ["batch"]
    resources: ["jobs"]
    verbs: ["get", "list", "watch", "create", "delete"]
  - apiGroups: [""]
    resources: ["secrets"]
    verbs: ["get", "list", "create", "update", "delete"]
  - apiGroups: [""]
    resources: ["pods"]
    verbs: ["get", "list", "watch"]
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: RoleBinding
metadata:
  name: console-api-builds
  namespace: kipper-builds
  labels:
    app.kubernetes.io/managed-by: kipper
subjects:
  - kind: ServiceAccount
    name: console-api
    namespace: kipper-system
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: Role
  name: console-api-builds
`

// InstallBuildIsolation creates the kipper-builds namespace, its
// zero-permission builder ServiceAccount, the default-deny egress
// NetworkPolicy, and the Role/RoleBinding letting console-api drive builds
// there. The egress except-list is derived at install time so it covers the
// cluster's actual node addresses, which are public on bare-metal targets.
func InstallBuildIsolation(client *ssh.Client) error {
	excepts, err := buildEgressExcepts(client)
	if err != nil {
		// Returning here leaves any policy already on the cluster in force, and
		// on the case this refuses — a cluster that has become dual-stack since
		// install — that policy permits everything a build pod can now reach over
		// IPv6. Close it before reporting.
		return fmt.Errorf("%w%s", err, sealBuildEgress(client))
	}
	manifest := fmt.Sprintf(buildIsolationManifest, renderExceptList(excepts))
	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Errorf("applying build-isolation manifest: %w", err)
	}
	return nil
}

// sealBuildEgress replaces the build namespace's egress policy with one that
// allows nothing, and returns a sentence describing what it did for the caller
// to append to the error that triggered it.
//
// It overwrites the existing policy rather than adding a second one: policies
// union their allowances, so a new deny-everything policy alongside the old one
// would change nothing. Builds fail loudly afterwards, which is the point — the
// alternative is builds running under a policy that no longer constrains them.
func sealBuildEgress(client *ssh.Client) string {
	// --ignore-not-found separates the three cases: a definite absence exits 0
	// with no output, a present namespace names itself, and only a real failure
	// (API unreachable, credentials) errors. Treating that failure as absence
	// would leave a permissive policy in force and say nothing about it.
	out, err := client.Run("kubectl get namespace " + buildNamespace + " --ignore-not-found -o name")
	switch {
	case err != nil:
		return fmt.Sprintf(" — and whether build egress in %s is still open could not be determined (%v); check it by hand", buildNamespace, err)
	case strings.TrimSpace(out) == "":
		// No build namespace, so there is no policy to close and nothing can run.
		return ""
	}
	applyCmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", buildEgressSealManifest)
	if _, err := client.Run(applyCmd); err != nil {
		return fmt.Sprintf(" — and the existing build egress policy could NOT be closed (%v), so builds in %s are running unconstrained until this is resolved", err, buildNamespace)
	}
	return fmt.Sprintf(" — build egress in %s has been closed until this is resolved, so builds will fail", buildNamespace)
}

// buildEgressSealManifest replaces the egress policy with a default-deny that
// permits nothing. It carries the same name as the policy in
// buildIsolationManifest so applying it overwrites the allowances there; a
// later successful InstallBuildIsolation restores them the same way.
const buildEgressSealManifest = `apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: kipper-builds-egress
  namespace: kipper-builds
  labels:
    app.kubernetes.io/managed-by: kipper
spec:
  podSelector: {}
  policyTypes:
    - Ingress
    - Egress
  ingress: []
  egress: []
`

// buildNamespace is the namespace build pods run in.
const buildNamespace = "kipper-builds"

// nodeAddressTypeQuery lists every "type=address" of every node, one per line.
const nodeAddressTypeQuery = `kubectl get nodes -o jsonpath='{range .items[*]}{range .status.addresses[*]}{.type}={.address}{"` + "\n" + `"}{end}{end}'`

// WorkerNodeIP returns the worker's effective node IP: the address it will be
// registered under in Kubernetes, read from the worker itself. This identifies
// the specific worker without guessing its k3s node name and without a shared
// bridge/container address (e.g. docker0's 172.17.0.1) that several hosts report
// identically.
//
// An explicit node-ip in the k3s config is authoritative: on a multi-homed host
// the default route can differ from the cluster-facing address k3s publishes. A
// fresh kip join sets none, so the config value is absent for the common case
// and the source of the default route is used, which is the address k3s selects
// as the node's InternalIP by default and is routable and host-unique.
func WorkerNodeIP(client *ssh.Client) (string, error) {
	// Read the config files in k3s load order (main file, then drop-ins in the
	// shell's sorted glob order). A newline is emitted after each file so a file
	// without a trailing newline does not fuse its last line into the next
	// file's first, which would hide a later overriding node-ip.
	cfg, _ := client.Run(`for f in /etc/rancher/k3s/config.yaml /etc/rancher/k3s/config.yaml.d/*.yaml; do [ -f "$f" ] && { cat "$f"; echo; }; done 2>/dev/null`)
	if ip := parseConfigNodeIP(cfg); ip != "" {
		return ip, nil
	}
	// A routing-table lookup (no packets sent) toward a public address returns
	// the default route's source address whenever a default route exists.
	out, err := client.Run("ip -o route get 1.1.1.1")
	if err != nil {
		return "", fmt.Errorf("reading worker node IP: %w", err)
	}
	ip := parseRouteSrc(out)
	if ip == "" {
		return "", fmt.Errorf("could not determine the worker's node IP from %q", strings.TrimSpace(out))
	}
	return ip, nil
}

// parseConfigNodeIP returns the effective explicit scalar node-ip in k3s config
// text, or "" if none applies. k3s loads config.yaml then config.yaml.d/*.yaml
// in order and the last assignment of a key wins; callers concatenate the files
// in that order, so the last node-ip line is authoritative. A later assignment
// overrides an earlier one even to a form this parser does not model (a block
// sequence or a non-IP), which then falls through to the default-route source
// rather than resurrecting a superseded value.
func parseConfigNodeIP(config string) string {
	result := ""
	for _, line := range strings.Split(config, "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "node-ip:")
		if !ok {
			continue
		}
		// This assignment overrides any earlier one; clear first so an
		// unparsable later value does not fall back to an earlier IP.
		result = ""
		if i := strings.IndexByte(rest, '#'); i >= 0 {
			rest = rest[:i]
		}
		// Accept a scalar, a comma list, or an inline YAML list; take the first
		// entry and strip surrounding brackets or quotes.
		rest = strings.TrimPrefix(strings.TrimSpace(rest), "[")
		if first, _, found := strings.Cut(rest, ","); found {
			rest = first
		}
		rest = strings.Trim(strings.TrimSpace(rest), `[]"'`)
		if net.ParseIP(rest) != nil {
			result = rest
		}
	}
	return result
}

// parseRouteSrc extracts the address after "src" in `ip route get` output.
func parseRouteSrc(out string) string {
	fields := strings.Fields(out)
	for i, f := range fields {
		if f == "src" && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

// WaitForNodeAddress polls the API server until some node reports the given
// address as its InternalIP or ExternalIP, so build isolation can then be
// refreshed with that address actually present in the egress deny-list. Matching
// the worker's own node IP handles both a fresh registration and an idempotent
// re-run against an already-registered worker, and waits for the address kubelet
// publishes (not mere Node existence, which precedes it). An unrelated node
// cannot satisfy the match because the address is the worker's routable node IP.
// Returns an error if no node reports the address within timeout.
func WaitForNodeAddress(client *ssh.Client, address string, timeout time.Duration) error {
	return waitForNodeAddress(client.Run, address, timeout, 3*time.Second)
}

// waitForNodeAddress is the pollable core, split out so the poll behaviour is
// testable without a live SSH host.
func waitForNodeAddress(run func(command string) (string, error), address string, timeout, interval time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		out, err := run(nodeAddressTypeQuery)
		if err == nil && nodeReportsAddress(out, address) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node %s did not publish its address within %s", address, timeout)
		}
		time.Sleep(interval)
	}
}

// nodeReportsAddress reports whether out (lines of "type=address") contains an
// InternalIP or ExternalIP — the address types buildEgressExcepts consumes —
// whose value equals address.
func nodeReportsAddress(out, address string) bool {
	if address == "" {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		typ, addr, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		if (typ == "InternalIP" || typ == "ExternalIP") && strings.TrimSpace(addr) == address {
			return true
		}
	}
	return false
}

// buildEgressExcepts returns the CIDRs a build pod must not reach: the
// standard internal ranges (RFC1918 covers the default 10.42/16 pod and
// 10.43/16 service CIDRs, link-local blocks cloud IMDS, CGNAT) plus every
// IPv4 node address as a /32. It reads the nodes once, so the pod CIDRs the
// decision rests on and the addresses that end up in the policy describe the
// same set of nodes.
func buildEgressExcepts(client *ssh.Client) ([]string, error) {
	out, err := client.Run("kubectl get nodes -o json")
	if err != nil {
		return nil, fmt.Errorf("reading nodes: %w", err)
	}
	return egressExcepts(out)
}

// egressExcepts derives the except-list from a `kubectl get nodes -o json`
// snapshot. The rule itself lives in controller/pkg/podnet, shared with the
// tenant egress policy the console-api reconciler installs: both allow public
// egress through one IPv4 ipBlock and both need the same answer about the pod
// network, and asking it separately got it wrong in both places.
func egressExcepts(nodesJSON string) ([]string, error) {
	var nodes struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				PodCIDRs []string `json:"podCIDRs"`
			} `json:"spec"`
			Status struct {
				Addresses []struct {
					Type    string `json:"type"`
					Address string `json:"address"`
				} `json:"addresses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(nodesJSON), &nodes); err != nil {
		return nil, fmt.Errorf("parsing nodes: %w", err)
	}
	inputs := make([]podnet.Node, 0, len(nodes.Items))
	for _, node := range nodes.Items {
		n := podnet.Node{Name: node.Metadata.Name, PodCIDRs: node.Spec.PodCIDRs}
		for _, addr := range node.Status.Addresses {
			if addr.Type == "InternalIP" || addr.Type == "ExternalIP" {
				n.Addresses = append(n.Addresses, addr.Address)
			}
		}
		inputs = append(inputs, n)
	}
	excepts, err := podnet.EgressExcepts(inputs)
	if err != nil {
		return nil, fmt.Errorf("build isolation requires an IPv4-only pod network: %w", err)
	}
	return excepts, nil
}

// renderExceptList formats the CIDRs as an indented YAML sequence for the
// NetworkPolicy ipBlock.except field.
func renderExceptList(cidrs []string) string {
	var b strings.Builder
	for _, c := range cidrs {
		fmt.Fprintf(&b, "              - %s\n", c)
	}
	return strings.TrimRight(b.String(), "\n")
}
