package controllers

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/podnet"
)

const workloadEgressPolicyName = "kipper-workload-egress"

// egressRefreshInterval bounds how long a namespace's egress except-list can lag
// a Node change if its watch event was lost. The Node watch handles the common
// case immediately; this is the backstop.
const egressRefreshInterval = 30 * time.Minute

// egressExceptCIDRs returns the CIDRs a workload egress policy must exclude and
// whether public egress can be expressed at all. The decision rests on the pod
// network's address family, not on the node's own addresses — see
// controller/pkg/podnet, which the build-isolation policy shares. A List failure
// or an unusable pod network is not fatal here: the caller installs the deny
// baseline first and surfaces the error so the reconcile retries.
func (r *ProjectReconciler) egressExceptCIDRs(ctx context.Context) (excepts []string, publicEgressOK bool, err error) {
	var nodes corev1.NodeList
	if err := r.List(ctx, &nodes); err != nil {
		return nil, false, fmt.Errorf("listing nodes for egress policy: %w", err)
	}
	inputs := make([]podnet.Node, 0, len(nodes.Items))
	for i := range nodes.Items {
		n := podnet.Node{Name: nodes.Items[i].Name, PodCIDRs: nodes.Items[i].Spec.PodCIDRs}
		for _, addr := range nodes.Items[i].Status.Addresses {
			if addr.Type == corev1.NodeInternalIP || addr.Type == corev1.NodeExternalIP {
				n.Addresses = append(n.Addresses, addr.Address)
			}
		}
		inputs = append(inputs, n)
	}
	excepts, err = podnet.EgressExcepts(inputs)
	if err != nil {
		return nil, false, err
	}
	sort.Strings(excepts)
	return excepts, true, nil
}

// reconcileEgressPolicy applies a default-deny egress NetworkPolicy to a project
// namespace. Egress is allowed only to DNS, to pods in the same namespace (so an
// app reaches its own project's bound services), and — on an IPv4-only cluster —
// to the public internet with the internal ranges and node IPv4 addresses
// excluded. Ingress is deliberately left unrestricted so Traefik, KEDA, and
// Prometheus can still reach the workloads.
//
// The node addresses excluded here are those on Node status; a separate public
// API-server load-balancer VIP that is not a Node address is not auto-excluded.
// The reconcile installs a policy on every call (never returning before one
// exists), so a namespace is never left with unrestricted egress.
func (r *ProjectReconciler) reconcileEgressPolicy(ctx context.Context, ns string) error {
	// A Node-list failure is NOT allowed to leave the namespace unprotected: we
	// still install the deny baseline (DNS + same-namespace, no public egress)
	// and only then surface the error so the reconcile retries and upgrades the
	// baseline to the public-allow policy once the node set is readable. Until
	// then the namespace is isolated (no external egress), never fail-open.
	excepts, publicEgressOK, listErr := r.egressExceptCIDRs(ctx)

	udp := corev1.ProtocolUDP
	tcp := corev1.ProtocolTCP
	port53 := intstr.FromInt32(53)

	egress := []networkingv1.NetworkPolicyEgressRule{
		// DNS to the cluster CoreDNS pods, matched by pod because the
		// ClusterIP falls inside the excepted RFC1918 range.
		{
			To: []networkingv1.NetworkPolicyPeer{{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
				PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
			}},
			Ports: []networkingv1.NetworkPolicyPort{
				{Protocol: &udp, Port: &port53},
				{Protocol: &tcp, Port: &port53},
			},
		},
		// Same namespace: an app reaches its own project's bound services (a
		// peer with only a podSelector means this namespace).
		{
			To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}},
		},
	}
	if listErr == nil && publicEgressOK {
		// Public internet on any port, with the internal ranges and node IPs
		// excluded so a workload can call external services but not cloud
		// metadata, cluster-internal CIDRs, or node addresses. Omitted when the
		// pod network is not known to be IPv4-only, or the node set can't be
		// read, so external egress stays denied until it can be excepted safely.
		egress = append(egress, networkingv1.NetworkPolicyEgressRule{
			To: []networkingv1.NetworkPolicyPeer{{
				IPBlock: &networkingv1.IPBlock{CIDR: "0.0.0.0/0", Except: excepts},
			}},
		})
	}

	desired := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      workloadEgressPolicyName,
			Namespace: ns,
			Labels:    map[string]string{kipperLabel: kipperValue},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			Egress:      egress,
		},
	}

	var existing networkingv1.NetworkPolicy
	err := r.Get(ctx, types.NamespacedName{Name: workloadEgressPolicyName, Namespace: ns}, &existing)
	switch {
	case errors.IsNotFound(err):
		if cerr := r.Create(ctx, desired); cerr != nil {
			return cerr
		}
	case err != nil:
		return err
	default:
		existing.Spec = desired.Spec
		existing.Labels = desired.Labels
		if uerr := r.Update(ctx, &existing); uerr != nil {
			return uerr
		}
	}
	// Surface a node-enumeration failure only after the fail-closed baseline is
	// in place, so the reconcile retries and upgrades to the public-allow policy.
	return listErr
}

// enqueueProjectsForNode re-reconciles every Project when a Node changes, so a
// node added or removed after a project namespace was created is reflected in
// its egress policy's except list. Without this, a new node's (public, on
// bare-metal) IP would stay reachable by existing tenant workloads.
func (r *ProjectReconciler) enqueueProjectsForNode(ctx context.Context, _ client.Object) []reconcile.Request {
	var projects kipperv1.ProjectList
	if err := r.List(ctx, &projects); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(projects.Items))
	for i := range projects.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Name: projects.Items[i].Name}})
	}
	return reqs
}
