package podnet

import (
	"strings"
	"testing"
)

// A node with a public IPv6 address and an IPv4-only pod network is the ordinary
// case on the hosting Kipper runs on. Reading it as dual-stack is what cut every
// tenant workload off from the internet and refused to install build isolation.
func TestEgressExceptsAcceptsAnIPv6NodeOnAnIPv4PodNetwork(t *testing.T) {
	excepts, err := EgressExcepts([]Node{{
		Name:      "node-1",
		PodCIDRs:  []string{"10.42.0.0/24"},
		Addresses: []string{"203.0.113.10", "2001:db8::1"},
	}})
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
			t.Errorf("the internal ranges must be kept, %s missing from %v", want, excepts)
		}
	}
}

func TestEgressExceptsCoversEveryNode(t *testing.T) {
	excepts, err := EgressExcepts([]Node{
		{Name: "node-1", PodCIDRs: []string{"10.42.0.0/24"}, Addresses: []string{"203.0.113.10"}},
		{Name: "node-2", PodCIDRs: []string{"10.42.1.0/24"}, Addresses: []string{"203.0.113.11", "198.51.100.7"}},
	})
	if err != nil {
		t.Fatalf("two IPv4-only nodes must be accepted: %v", err)
	}
	joined := strings.Join(excepts, ",")
	for _, want := range []string{"203.0.113.10/32", "203.0.113.11/32", "198.51.100.7/32"} {
		if !strings.Contains(joined, want) {
			t.Errorf("every node address must be excepted, %s missing from %v", want, excepts)
		}
	}
}

// Anything short of positively-known IPv4-only pod addressing has to fail, and
// the caller then denies external egress rather than guessing.
func TestEgressExceptsFailsClosedWhenThePodFamilyIsNotKnownIPv4(t *testing.T) {
	cases := []struct {
		name  string
		nodes []Node
		want  string
	}{
		{
			name:  "dual-stack pod network",
			nodes: []Node{{Name: "n", PodCIDRs: []string{"10.42.0.0/24", "2001:db8:42::/64"}}},
			want:  "does not report an IPv4 pod network",
		},
		{
			name:  "IPv6-only pod network",
			nodes: []Node{{Name: "n", PodCIDRs: []string{"2001:db8:42::/64"}}},
			want:  "does not report an IPv4 pod network",
		},
		{
			name:  "a node that has not published a pod CIDR",
			nodes: []Node{{Name: "joining", Addresses: []string{"203.0.113.10"}}},
			want:  "has not published a pod CIDR",
		},
		{
			name: "a second node joins dual-stack",
			nodes: []Node{
				{Name: "n1", PodCIDRs: []string{"10.42.0.0/24"}},
				{Name: "n2", PodCIDRs: []string{"10.42.1.0/24", "2001:db8:42::/64"}},
			},
			want: "does not report an IPv4 pod network",
		},
		{
			name:  "no nodes",
			nodes: nil,
			want:  "no nodes reported",
		},
		{
			// Unparseable is not evidence of anything. Only positive IPv4 evidence
			// may permit public egress, so garbage must fail rather than slip past
			// a "contains no colon" test.
			name:  "a pod CIDR that does not parse",
			nodes: []Node{{Name: "n", PodCIDRs: []string{"not-a-cidr"}}},
			want:  "does not report an IPv4 pod network",
		},
		{
			name:  "a bare IP where a CIDR belongs",
			nodes: []Node{{Name: "n", PodCIDRs: []string{"10.42.0.1"}}},
			want:  "does not report an IPv4 pod network",
		},
	}
	for _, tc := range cases {
		_, err := EgressExcepts(tc.nodes)
		if err == nil {
			t.Errorf("%s: must be refused", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: the error must name the reason %q, got %v", tc.name, tc.want, err)
		}
	}
}

// The returned slice must not alias the package's own range list, or one caller
// appending node addresses would corrupt the next call.
func TestEgressExceptsDoesNotAliasTheInternalRanges(t *testing.T) {
	first, err := EgressExcepts([]Node{{Name: "n", PodCIDRs: []string{"10.42.0.0/24"}, Addresses: []string{"203.0.113.10"}}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := EgressExcepts([]Node{{Name: "n", PodCIDRs: []string{"10.42.0.0/24"}, Addresses: []string{"198.51.100.9"}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(second, ","), "203.0.113.10") {
		t.Errorf("the second call leaked the first call's node address: %v", second)
	}
	if len(first) != len(second) {
		t.Errorf("both calls must return the same shape, got %d and %d", len(first), len(second))
	}

	// A caller editing what it was given must not reach the package's own list.
	first[0] = "0.0.0.0/0"
	third, err := EgressExcepts([]Node{{Name: "n", PodCIDRs: []string{"10.42.0.0/24"}}})
	if err != nil {
		t.Fatal(err)
	}
	if third[0] == "0.0.0.0/0" {
		t.Error("mutating a returned slice reached the package's internal ranges")
	}
}
