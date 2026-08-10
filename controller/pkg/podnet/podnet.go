// Package podnet decides what a workload's egress policy may except, from the
// only evidence that settles it: the pod network's address family.
//
// Both the tenant egress policy and the build-isolation policy allow public
// egress through a single IPv4 ipBlock, and both need the same answer to the
// same question — can a pod here originate traffic the ipBlock cannot describe?
// Asking it in two places got it wrong in two places: each read the node's own
// addresses, and every node on ordinary hosting has a public IPv6 address while
// its pods are IPv4-only. One treated that as a reason to refuse installing
// isolation at all; the other silently dropped the public-egress rule and cut
// every tenant workload off from the internet.
package podnet

import (
	"fmt"
	"net"
)

// internalExcepts are the ranges a workload must never reach on egress: RFC1918
// covers the default pod (10.42/16) and service (10.43/16) CIDRs so a pod cannot
// reach another tenant's pods or cluster-internal services; 169.254.0.0/16 blocks
// the cloud instance-metadata endpoint (IAM credential theft); 100.64.0.0/10 is
// CGNAT. Node addresses are added on top, because on bare metal — Kipper's
// primary target — node IPs are public, so the static ranges alone would leave
// kubelet and the apiserver reachable.
var internalExcepts = []string{
	"10.0.0.0/8",
	"172.16.0.0/12",
	"192.168.0.0/16",
	"169.254.0.0/16",
	"100.64.0.0/10",
}

// Node is the per-node evidence the decision rests on. Addresses holds the
// InternalIP and ExternalIP values only; anything else on Node status is not an
// address a pod could route to.
type Node struct {
	Name      string
	PodCIDRs  []string
	Addresses []string
}

// EgressExcepts returns the CIDRs an IPv4 public-egress ipBlock must exclude.
//
// It fails unless every node positively reports IPv4-only pod CIDRs, because a
// pod holding an IPv6 address escapes an IPv4 ipBlock entirely and a node that
// has not published its pod CIDRs has told us nothing to rely on. A node's own
// IPv6 address is a different matter: with an IPv4-only pod network no workload
// has an IPv6 address to send from, so that address is unreachable from a pod and
// is left out of the list rather than treated as a reason to refuse.
//
// Callers must fail closed on an error — deny external egress, or decline to
// install — never fall back to an unconstrained policy.
func EgressExcepts(nodes []Node) ([]string, error) {
	if len(nodes) == 0 {
		return nil, fmt.Errorf("no nodes reported, so the pod address family is unknown")
	}
	excepts := append([]string(nil), internalExcepts...)
	for _, n := range nodes {
		if len(n.PodCIDRs) == 0 {
			return nil, fmt.Errorf("node %s has not published a pod CIDR, so its pod address family is unknown; retry once it is Ready", n.Name)
		}
		for _, cidr := range n.PodCIDRs {
			if !isIPv4CIDR(cidr) {
				return nil, fmt.Errorf("node %s does not report an IPv4 pod network (%q); an IPv4 ipBlock cannot constrain what its pods can reach", n.Name, cidr)
			}
		}
		for _, addr := range n.Addresses {
			// Only a parseable IPv4 address can go in an IPv4 ipBlock. Skipping
			// anything else is safe for IPv6 — an IPv4-only pod cannot reach it —
			// and the pod-CIDR check above is what establishes that.
			if !isIPv4Address(addr) {
				continue
			}
			excepts = append(excepts, addr+"/32")
		}
	}
	return excepts, nil
}

// isIPv4CIDR reports whether s parses as an IPv4 CIDR. Anything unparseable is
// not positive evidence of an IPv4-only pod network, so it must not pass: the
// whole point of this package is that only positive evidence permits public
// egress.
func isIPv4CIDR(s string) bool {
	ip, _, err := net.ParseCIDR(s)
	return err == nil && ip.To4() != nil
}

func isIPv4Address(s string) bool {
	ip := net.ParseIP(s)
	return ip != nil && ip.To4() != nil
}
