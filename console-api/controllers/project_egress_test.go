package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// nodeWithIP builds a node as the clusters actually report one: an IPv4 pod CIDR
// plus whatever address the test cares about. The pod CIDR is what decides
// whether public egress can be expressed at all, so leaving it off would test a
// cluster that does not exist.
func nodeWithIP(name, ipType, ip string) *corev1.Node {
	n := nodeWithPodCIDRs(name, "10.42.0.0/24")
	n.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeAddressType(ipType), Address: ip}}
	return n
}

func nodeWithPodCIDRs(name string, podCIDRs ...string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       corev1.NodeSpec{PodCIDRs: podCIDRs},
	}
}

func TestReconcileEgressPolicy_DenyEgressWithMetadataAndNodeBlocked(t *testing.T) {
	scheme := testScheme()
	node := nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c}

	require.NoError(t, r.reconcileEgressPolicy(context.Background(), "project-test"))

	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: workloadEgressPolicyName, Namespace: "project-test"}, &np))

	// Egress-only: ingress must stay open for Traefik/KEDA/Prometheus.
	require.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
	assert.Empty(t, np.Spec.PodSelector.MatchLabels, "the policy must select every pod")

	// Find the public-egress rule and check its except list.
	var excepts []string
	sameNamespaceAllowed := false
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil && peer.IPBlock.CIDR == "0.0.0.0/0" {
				excepts = peer.IPBlock.Except
			}
			if peer.IPBlock == nil && peer.NamespaceSelector == nil && peer.PodSelector != nil && len(peer.PodSelector.MatchLabels) == 0 {
				sameNamespaceAllowed = true
			}
		}
	}
	require.NotNil(t, excepts, "a public-egress rule with 0.0.0.0/0 must exist")
	assert.Contains(t, excepts, "169.254.0.0/16", "the metadata endpoint must be blocked")
	assert.Contains(t, excepts, "10.0.0.0/8", "the pod and service CIDRs must be blocked")
	assert.Contains(t, excepts, "203.0.113.9/32", "the node IP must be blocked")
	assert.True(t, sameNamespaceAllowed, "a pod must still reach its own project's services")
}

func TestEgressExceptCIDRs_NodeIPsAndIPv6(t *testing.T) {
	scheme := testScheme()

	t.Run("includes node internal and external IPs, IPv4-only", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
			nodeWithIP("n1", "InternalIP", "10.1.2.3"),
			nodeWithIP("n2", "ExternalIP", "203.0.113.10"),
		).Build()
		r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c}
		excepts, publicEgressOK, err := r.egressExceptCIDRs(context.Background())
		require.NoError(t, err)
		assert.True(t, publicEgressOK)
		assert.Contains(t, excepts, "10.1.2.3/32")
		assert.Contains(t, excepts, "203.0.113.10/32")
		assert.Contains(t, excepts, "169.254.0.0/16")
	})

	// Every node on ordinary hosting has a public IPv6 address while its pods are
	// IPv4-only. Reading that as dual-stack dropped the public-egress rule and cut
	// every tenant workload off from the internet.
	t.Run("a node with an IPv6 address but IPv4-only pods keeps public egress", func(t *testing.T) {
		node := nodeWithPodCIDRs("n1", "10.42.0.0/24")
		node.Status.Addresses = []corev1.NodeAddress{
			{Type: corev1.NodeInternalIP, Address: "203.0.113.10"},
			{Type: corev1.NodeInternalIP, Address: "2001:db8::1"},
		}
		c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
		r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c}
		excepts, publicEgressOK, err := r.egressExceptCIDRs(context.Background())
		require.NoError(t, err)
		assert.True(t, publicEgressOK, "an IPv4-only pod network can express public egress")
		assert.Contains(t, excepts, "203.0.113.10/32")
		for _, e := range excepts {
			assert.NotContains(t, e, ":", "an IPv6 address cannot appear in an IPv4 ipBlock")
		}
	})

	// A dual-stack POD network is the real unsupported case: the pods hold IPv6
	// addresses the single IPv4 ipBlock cannot describe.
	t.Run("a dual-stack pod network cannot express public egress", func(t *testing.T) {
		c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
			nodeWithPodCIDRs("n1", "10.42.0.0/24", "2001:db8:42::/64"),
		).Build()
		r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c}
		_, publicEgressOK, err := r.egressExceptCIDRs(context.Background())
		require.Error(t, err, "an unusable pod network must be reported so the reconcile retries")
		assert.False(t, publicEgressOK)
	})
}

func TestReconcileEgressPolicy_DualStackDeniesAllExternalEgress(t *testing.T) {
	// A dual-stack POD network is unsupported, so the policy must still install
	// and deny ALL external egress (no public ipBlock rule) rather than leave the
	// namespace unrestricted — fail-closed, not fail-open. The error is surfaced
	// after the baseline is in place so the reconcile retries.
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		nodeWithPodCIDRs("n1", "10.42.0.0/24", "2001:db8:42::/64"),
	).Build()
	r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c}

	require.Error(t, r.reconcileEgressPolicy(context.Background(), "project-test"),
		"an unusable pod network must be reported so the reconcile retries")

	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: workloadEgressPolicyName, Namespace: "project-test"}, &np))
	require.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			assert.Nil(t, peer.IPBlock, "a dual-stack cluster must have no public ipBlock egress rule")
		}
	}
}

func TestReconcileEgressPolicy_UpdatesExistingOnNodeChange(t *testing.T) {
	// Drift repair: when a node is added, re-reconciling must update the
	// existing policy's except-list to include the new node IP.
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		nodeWithIP("n1", "InternalIP", "10.0.0.1"),
	).Build()
	r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c}
	require.NoError(t, r.reconcileEgressPolicy(context.Background(), "project-test"))

	// A new node appears; re-reconcile.
	require.NoError(t, c.Create(context.Background(), nodeWithIP("n2", "ExternalIP", "203.0.113.50")))
	require.NoError(t, r.reconcileEgressPolicy(context.Background(), "project-test"))

	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: workloadEgressPolicyName, Namespace: "project-test"}, &np))
	var excepts []string
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.IPBlock != nil {
				excepts = peer.IPBlock.Except
			}
		}
	}
	assert.Contains(t, excepts, "203.0.113.50/32", "the new node IP must be added to the except-list on re-reconcile")
}

func TestProjectReconcile_IsolatesEveryNamespaceEvenOnIPv6Cluster(t *testing.T) {
	// F1 invariant: a full Project reconcile on a cluster whose pod network is
	// dual-stack (unsupported) must still leave every created namespace with an
	// egress policy — never a usable namespace with unrestricted egress. The node
	// carries a dual-stack POD CIDR, which is the unsupported condition; a node
	// with merely an IPv6 address is the ordinary supported case and is covered
	// in TestEgressExceptCIDRs_NodeIPsAndIPv6.
	scheme := testScheme()
	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", Finalizers: []string{projectFinalizer}},
		Spec:       kipperv1.ProjectSpec{Environments: envList("test", "prod")},
	}
	c := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(project, nodeWithPodCIDRs("n1", "10.42.0.0/24", "2001:db8:42::/64")).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()
	r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c}

	// The reconcile surfaces the unusable pod network, and the namespace it had
	// reached is isolated before that error returns.
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
	require.Error(t, err, "an unusable pod network must be reported so the reconcile retries")

	for _, ns := range []string{"demo-test"} {
		var np networkingv1.NetworkPolicy
		require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: workloadEgressPolicyName, Namespace: ns}, &np),
			"namespace %s must carry an egress policy even when the pod network is unusable", ns)
		require.Equal(t, []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}, np.Spec.PolicyTypes)
	}
}

func TestReconcileEgressPolicy_NodeListFailureStillInstallsFailClosedBaseline(t *testing.T) {
	// If the node set can't be read, the namespace must NOT be left without a
	// policy: the deny baseline (DNS + same-namespace, no public egress) is
	// installed and the error is surfaced so the reconcile retries.
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, cl crclient.WithWatch, list crclient.ObjectList, opts ...crclient.ListOption) error {
				if _, isNodes := list.(*corev1.NodeList); isNodes {
					return context.DeadlineExceeded
				}
				return cl.List(ctx, list, opts...)
			},
		}).Build()
	r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c}

	err := r.reconcileEgressPolicy(context.Background(), "project-test")
	require.Error(t, err, "the node-list failure must surface so the reconcile retries")

	var np networkingv1.NetworkPolicy
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: workloadEgressPolicyName, Namespace: "project-test"}, &np),
		"the fail-closed baseline must be installed despite the node-list failure")
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			assert.Nil(t, peer.IPBlock, "no public egress rule may be added when the node set is unreadable")
		}
	}
}

func TestEnqueueProjectsForNode(t *testing.T) {
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "acme"}},
		&kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}},
	).Build()
	r := &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c}

	reqs := r.enqueueProjectsForNode(context.Background(), nodeWithIP("n1", "InternalIP", "10.0.0.5"))
	assert.Len(t, reqs, 2, "a node change must re-reconcile every project's egress policy")
}
