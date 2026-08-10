package installer

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseRouteSrc(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want string
	}{
		{
			name: "typical ip route get output",
			out:  "1.1.1.1 via 203.0.113.1 dev eth0 src 203.0.113.20 uid 0 \\    cache",
			want: "203.0.113.20",
		},
		{
			name: "on-link route",
			out:  "10.0.0.5 dev eth0 src 10.0.0.20 uid 0 \\    cache",
			want: "10.0.0.20",
		},
		{
			name: "no src field",
			out:  "1.1.1.1 dev eth0 uid 0",
			want: "",
		},
		{
			name: "empty",
			out:  "",
			want: "",
		},
	}
	for _, tt := range tests {
		if got := parseRouteSrc(tt.out); got != tt.want {
			t.Errorf("%s: parseRouteSrc = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestParseConfigNodeIP(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   string
	}{
		{"no config", "", ""},
		{"no node-ip key", "token: abc\nwrite-kubeconfig-mode: \"0644\"\n", ""},
		{"scalar", "node-ip: 10.1.2.3\n", "10.1.2.3"},
		{"quoted scalar", "node-ip: \"10.1.2.3\"\n", "10.1.2.3"},
		{"indented", "  node-ip: 10.1.2.3\n", "10.1.2.3"},
		{"with inline comment", "node-ip: 10.1.2.3 # cluster-facing\n", "10.1.2.3"},
		{"comma list takes first", "node-ip: 10.1.2.3,10.1.2.4\n", "10.1.2.3"},
		{"inline yaml list takes first", "node-ip: [10.1.2.3, 10.1.2.4]\n", "10.1.2.3"},
		{"ipv6 scalar", "node-ip: 2001:db8::5\n", "2001:db8::5"},
		{"commented-out key is ignored", "#node-ip: 10.1.2.3\n", ""},
		{"unrelated key with node-ip substring", "kubelet-arg: something\n", ""},
		{"non-ip value falls through", "node-ip: not-an-ip\n", ""},
		{"block sequence falls through to route", "node-ip:\n  - 10.1.2.3\n", ""},
		// k3s merges config.yaml then config.yaml.d/*.yaml and the last
		// assignment wins; the files are concatenated in that order.
		{"a later drop-in overrides the main file", "node-ip: 192.0.2.10\nnode-ip: 10.20.30.40\n", "10.20.30.40"},
		{"a later block sequence clears an earlier scalar", "node-ip: 192.0.2.10\nnode-ip:\n  - 10.20.30.40\n", ""},
		{"a later non-ip clears an earlier scalar", "node-ip: 192.0.2.10\nnode-ip: not-an-ip\n", ""},
		// WorkerNodeIP emits a newline after each file so boundaries are kept.
		// Even if a boundary were lost, a fused token is not a valid IP, so the
		// parser falls through to the default route rather than returning garbage.
		{"a fused file boundary is not a valid IP", "node-ip: 192.0.2.10node-ip: 10.20.30.40\n", ""},
	}
	for _, tt := range tests {
		if got := parseConfigNodeIP(tt.config); got != tt.want {
			t.Errorf("%s: parseConfigNodeIP = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestNodeReportsAddress(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		address string
		want    bool
	}{
		{
			name:    "a node reports the worker's InternalIP",
			out:     "InternalIP=10.0.0.1\nHostname=master-1\nInternalIP=203.0.113.20\nHostname=worker-1\n",
			address: "203.0.113.20",
			want:    true,
		},
		{
			name:    "a node reports the worker's ExternalIP",
			out:     "ExternalIP=203.0.113.20\nHostname=worker-1\n",
			address: "203.0.113.20",
			want:    true,
		},
		{
			name:    "the worker's IP appears only as a Hostname, not an IP address type",
			out:     "InternalIP=10.0.0.1\nHostname=203.0.113.20\n",
			address: "203.0.113.20",
			want:    false,
		},
		{
			// A shared bridge address reported by an unrelated node must not match:
			// the worker is matched by its routable node IP, not a bridge address.
			name:    "an unrelated node's bridge address does not match the worker IP",
			out:     "InternalIP=10.0.0.1\nInternalIP=172.17.0.1\n",
			address: "203.0.113.20",
			want:    false,
		},
		{
			name:    "no node reports the worker's IP",
			out:     "InternalIP=10.0.0.1\nHostname=master-1\n",
			address: "203.0.113.20",
			want:    false,
		},
		{
			name:    "empty address matches nothing",
			out:     "InternalIP=203.0.113.20\n",
			address: "",
			want:    false,
		},
	}
	for _, tt := range tests {
		if got := nodeReportsAddress(tt.out, tt.address); got != tt.want {
			t.Errorf("%s: nodeReportsAddress = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// The Node object can appear before kubelet publishes its address; the wait must
// keep polling until a node reports the worker's IP, not stop earlier.
func TestWaitForNodeAddress_WaitsForAddress(t *testing.T) {
	calls := 0
	run := func(string) (string, error) {
		calls++
		if calls < 3 {
			// The worker exists but has only a Hostname so far, no IP.
			return "InternalIP=10.0.0.1\nHostname=worker-1\n", nil
		}
		return "InternalIP=10.0.0.1\nInternalIP=203.0.113.20\n", nil
	}

	if err := waitForNodeAddress(run, "203.0.113.20", 2*time.Second, time.Millisecond); err != nil {
		t.Fatalf("expected success once the node publishes the worker's IP, got %v", err)
	}
	if calls < 3 {
		t.Errorf("expected polling to continue until the address appeared, got %d calls", calls)
	}
}

// An idempotent re-run against an already-registered worker must refresh
// isolation immediately, not time out waiting for a "new" node.
func TestWaitForNodeAddress_AlreadyRegistered(t *testing.T) {
	run := func(string) (string, error) {
		return "InternalIP=10.0.0.1\nInternalIP=203.0.113.20\nHostname=worker-1\n", nil
	}

	if err := waitForNodeAddress(run, "203.0.113.20", 20*time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("an already-registered worker must be matched at once, got %v", err)
	}
}

// Unrelated node churn must not satisfy the wait: only the worker's own IP does.
func TestWaitForNodeAddress_IgnoresUnrelatedNodes(t *testing.T) {
	run := func(string) (string, error) {
		// Master plus an unrelated node, neither reporting the worker's IP.
		return "InternalIP=10.0.0.1\nInternalIP=198.51.100.9\nInternalIP=172.17.0.1\n", nil
	}

	err := waitForNodeAddress(run, "203.0.113.20", 20*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected a timeout: no node reports the worker's IP")
	}
	if !strings.Contains(err.Error(), "did not publish its address") {
		t.Errorf("expected a registration timeout error, got %v", err)
	}
}

// A transient list error must not end the wait: the loop retries and still
// succeeds once the node reports the worker's IP.
func TestWaitForNodeAddress_RetriesOnRunError(t *testing.T) {
	calls := 0
	run := func(string) (string, error) {
		calls++
		if calls == 1 {
			return "", fmt.Errorf("connection refused")
		}
		return "InternalIP=203.0.113.20\n", nil
	}

	if err := waitForNodeAddress(run, "203.0.113.20", time.Second, time.Millisecond); err != nil {
		t.Fatalf("expected success after a transient error, got %v", err)
	}
}

// nodesJSON renders a `kubectl get nodes -o json` snapshot from per-node pod
// CIDRs and addresses, so the tests below read as the cluster state they mean.
func nodesJSON(nodes ...map[string][]string) string {
	items := make([]string, 0, len(nodes))
	for i, n := range nodes {
		cidrs, _ := json.Marshal(n["podCIDRs"])
		addrs := make([]string, 0, len(n["addresses"]))
		for _, a := range n["addresses"] {
			typ, addr, _ := strings.Cut(a, "=")
			addrs = append(addrs, fmt.Sprintf(`{"type":%q,"address":%q}`, typ, addr))
		}
		if n["podCIDRs"] == nil {
			cidrs = []byte("null")
		}
		items = append(items, fmt.Sprintf(
			`{"metadata":{"name":"node-%d"},"spec":{"podCIDRs":%s},"status":{"addresses":[%s]}}`,
			i+1, cidrs, strings.Join(addrs, ",")))
	}
	return fmt.Sprintf(`{"items":[%s]}`, strings.Join(items, ","))
}

// A node with a public IPv6 address but an IPv4-only pod network is the normal
// case on the hosting this runs on. The pods have no IPv6 address to send from,
// so the IPv4 ipBlock still constrains every build, and refusing here would
// leave the cluster with no kipper-builds namespace and every build failing.
func TestEgressExceptsSkipsNodeIPv6OnAnIPv4PodNetwork(t *testing.T) {
	excepts, err := egressExcepts(nodesJSON(map[string][]string{
		"podCIDRs":  {"10.42.0.0/24"},
		"addresses": {"InternalIP=203.0.113.10", "ExternalIP=203.0.113.10", "InternalIP=2001:db8::1", "Hostname=node-1"},
	}))
	if err != nil {
		t.Fatalf("an IPv4-only pod network must be accepted: %v", err)
	}
	joined := strings.Join(excepts, ",")
	if !strings.Contains(joined, "203.0.113.10/32") {
		t.Errorf("the node's IPv4 address must be excepted, got %v", excepts)
	}
	if strings.Contains(joined, ":") {
		t.Errorf("an IPv6 address cannot appear in an IPv4 ipBlock, got %v", excepts)
	}
	for _, want := range []string{"10.0.0.0/8", "169.254.0.0/16", "100.64.0.0/10"} {
		if !strings.Contains(joined, want) {
			t.Errorf("except list must keep %s, got %v", want, excepts)
		}
	}
}

// Every case where the pod address family is not positively IPv4-only has to
// stop: the policy's single IPv4 ipBlock cannot constrain a pod that holds an
// IPv6 address, and a node that has published no pod CIDR has told us nothing.
func TestEgressExceptsFailsClosedUnlessPodNetworkIsKnownIPv4(t *testing.T) {
	cases := []struct {
		name  string
		nodes string
		want  string
	}{
		{
			name: "dual-stack pod network",
			nodes: nodesJSON(map[string][]string{
				"podCIDRs":  {"10.42.0.0/24", "2001:db8:42::/64"},
				"addresses": {"InternalIP=203.0.113.10"},
			}),
			want: "does not report an IPv4 pod network",
		},
		{
			name: "IPv6-only pod network",
			nodes: nodesJSON(map[string][]string{
				"podCIDRs":  {"2001:db8:42::/64"},
				"addresses": {"InternalIP=203.0.113.10"},
			}),
			want: "does not report an IPv4 pod network",
		},
		{
			name: "a node that has not published a pod CIDR yet",
			nodes: nodesJSON(map[string][]string{
				"addresses": {"InternalIP=203.0.113.10"},
			}),
			want: "has not published a pod CIDR",
		},
		{
			name: "a second node joins dual-stack while the first is IPv4",
			nodes: nodesJSON(
				map[string][]string{"podCIDRs": {"10.42.0.0/24"}, "addresses": {"InternalIP=203.0.113.10"}},
				map[string][]string{"podCIDRs": {"10.42.1.0/24", "2001:db8:42::/64"}, "addresses": {"InternalIP=203.0.113.11"}},
			),
			want: "does not report an IPv4 pod network",
		},
		{
			name:  "no nodes at all",
			nodes: `{"items":[]}`,
			want:  "no nodes reported",
		},
		{
			name:  "unparseable output",
			nodes: "not json",
			want:  "parsing nodes",
		},
	}
	for _, tc := range cases {
		_, err := egressExcepts(tc.nodes)
		if err == nil {
			t.Errorf("%s: must be refused", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error must name the reason %q, got %v", tc.name, tc.want, err)
		}
	}
}

// Every node's addresses reach the except list, not just the first node's: a
// build pod must not be able to reach a second node on 80/443.
func TestEgressExceptsCoversEveryNode(t *testing.T) {
	excepts, err := egressExcepts(nodesJSON(
		map[string][]string{"podCIDRs": {"10.42.0.0/24"}, "addresses": {"InternalIP=203.0.113.10"}},
		map[string][]string{"podCIDRs": {"10.42.1.0/24"}, "addresses": {"InternalIP=203.0.113.11", "ExternalIP=198.51.100.7"}},
	))
	if err != nil {
		t.Fatalf("two IPv4-only nodes must be accepted: %v", err)
	}
	joined := strings.Join(excepts, ",")
	for _, want := range []string{"203.0.113.10/32", "203.0.113.11/32", "198.51.100.7/32"} {
		if !strings.Contains(joined, want) {
			t.Errorf("except list must contain %s, got %v", want, excepts)
		}
	}
}

// The seal must overwrite the policy the isolation manifest installs, not add a
// second one: NetworkPolicies union their allowances, so a separate deny-all
// alongside the original would leave every allowance in place.
func TestBuildEgressSealOverwritesTheIsolationPolicy(t *testing.T) {
	if !strings.Contains(buildEgressSealManifest, "name: kipper-builds-egress") {
		t.Error("the seal must carry the isolation policy's name to replace it")
	}
	if !strings.Contains(buildEgressSealManifest, "egress: []") || !strings.Contains(buildEgressSealManifest, "ingress: []") {
		t.Error("the seal must allow nothing in either direction")
	}
	if !strings.Contains(buildIsolationManifest, "name: kipper-builds-egress") {
		t.Error("the isolation manifest's policy name changed; the seal no longer replaces it")
	}
}
