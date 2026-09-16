package installer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

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

func TestWaitForNodeToPublishAddress_WaitsForTheNamedNode(t *testing.T) {
	calls := 0
	run := func(command string) (string, error) {
		calls++
		if !strings.Contains(command, "'worker-1'") {
			t.Fatalf("expected the named node to be asked about, got %q", command)
		}
		if calls < 3 {
			return "Hostname=worker-1\n", nil
		}
		return "Hostname=worker-1\nInternalIP=203.0.113.20\n", nil
	}

	if err := waitForNodeToPublishAddress(run, "worker-1", time.Second, time.Millisecond); err != nil {
		t.Fatalf("an address that appears on the third look should satisfy the wait: %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected the wait to keep asking, got %d calls", calls)
	}
}

func TestWaitForNodeToPublishAddress_TimesOutOnHostnameOnly(t *testing.T) {
	run := func(string) (string, error) { return "Hostname=worker-1\n", nil }

	err := waitForNodeToPublishAddress(run, "worker-1", 20*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("expected a node reporting only a hostname to time out")
	}
	if !strings.Contains(err.Error(), "worker-1") {
		t.Errorf("the failure should name the node, got %v", err)
	}
}

func TestWaitForNodeToPublishAddress_KeepsWaitingOnAnIPv6OnlyNode(t *testing.T) {
	// Build egress exclusions require IPv4 addresses.
	run := func(string) (string, error) {
		return "Hostname=worker-1\nInternalIP=2001:db8::5\n", nil
	}
	err := waitForNodeToPublishAddress(run, "worker-1", 20*time.Millisecond, 5*time.Millisecond)
	if err == nil {
		t.Fatal("an IPv6-only address reaches no ipBlock, so the wait must not end on it")
	}
}

func TestWaitForNodeToPublishAddress_KeepsWaitingOnAMalformedAddress(t *testing.T) {
	run := func(string) (string, error) {
		return "InternalIP=not-an-address\n", nil
	}
	err := waitForNodeToPublishAddress(run, "worker-1", 20*time.Millisecond, 5*time.Millisecond)
	if err == nil {
		t.Fatal("a value the deny-list drops must not end the wait")
	}
}

func TestWaitForNodeToPublishAddress_EndsOnIPv4PublishedAfterIPv6(t *testing.T) {
	calls := 0
	run := func(string) (string, error) {
		calls++
		if calls < 3 {
			return "Hostname=worker-1\nInternalIP=2001:db8::5\n", nil
		}
		return "Hostname=worker-1\nInternalIP=2001:db8::5\nInternalIP=203.0.113.9\n", nil
	}
	if err := waitForNodeToPublishAddress(run, "worker-1", time.Second, time.Millisecond); err != nil {
		t.Fatalf("the IPv4 address arrived, so the wait should end: %v", err)
	}
}

func TestWaitForNodeToPublishAddress_AlreadyPublished(t *testing.T) {
	calls := 0
	run := func(string) (string, error) {
		calls++
		return "Hostname=worker-1\nInternalIP=203.0.113.9\n", nil
	}
	if err := waitForNodeToPublishAddress(run, "worker-1", time.Second, time.Millisecond); err != nil {
		t.Fatalf("the address was already there: %v", err)
	}
	if calls != 1 {
		t.Errorf("asked %d times for an address that was already published", calls)
	}
}

func TestWaitForNodeToPublishAddress_RecoversFromATransientError(t *testing.T) {
	calls := 0
	run := func(string) (string, error) {
		calls++
		if calls == 1 {
			return "", fmt.Errorf("connection refused")
		}
		return "Hostname=worker-1\nInternalIP=203.0.113.9\n", nil
	}
	if err := waitForNodeToPublishAddress(run, "worker-1", time.Second, time.Millisecond); err != nil {
		t.Fatalf("the second poll answered, so the wait should end: %v", err)
	}
}

func TestWaitForNodeToPublishAddress_DoesNotBlameARecoveredError(t *testing.T) {
	calls := 0
	run := func(string) (string, error) {
		calls++
		if calls == 1 {
			return "", fmt.Errorf("connection refused")
		}
		return "Hostname=worker-1\n", nil
	}
	err := waitForNodeToPublishAddress(run, "worker-1", 30*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("no address was ever published, so the wait must time out")
	}
	if strings.Contains(err.Error(), "connection refused") {
		t.Errorf("blamed an error a later poll recovered from: %v", err)
	}
}

func TestWaitForNodeToPublishAddress_WrapsLastError(t *testing.T) {
	refused := errors.New("connection refused")
	run := func(string) (string, error) { return "", refused }

	err := waitForNodeToPublishAddress(run, "worker-1", 20*time.Millisecond, time.Millisecond)
	if !errors.Is(err, refused) {
		t.Fatalf("the timeout should carry the kubectl error, got %v", err)
	}
}
