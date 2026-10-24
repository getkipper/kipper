package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

func moveNode(name, cpu, memory string) corev1.Node {
	n := cpuNode(name, cpu)
	n.Labels = map[string]string{corev1.LabelHostname: name}
	n.Status.Allocatable[corev1.ResourceMemory] = resource.MustParse(memory)
	return n
}

func requestPod(namespace, name, node, cpu, memory string) corev1.Pod {
	p := cpuPod(namespace, name, node, cpu)
	p.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory] = resource.MustParse(memory)
	p.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu), corev1.ResourceMemory: resource.MustParse(memory)}
	return p
}

// servicePod is the ordinal-zero pod of a Kipper service's StatefulSet.
func servicePod(namespace, service, node, cpu, memory string) corev1.Pod {
	p := requestPod(namespace, service+"-0", node, cpu, memory)
	p.Labels = map[string]string{"app": service, labels.ServiceType: "postgres"}
	p.OwnerReferences = []metav1.OwnerReference{{Kind: "StatefulSet", Name: service, Controller: ptr.To(true)}}
	p.Spec.Containers[0].Name = service
	return p
}

func pendingPod(name, cpu string) corev1.Pod {
	p := requestPod("web", name, "", cpu, "128Mi")
	p.Status.Phase = corev1.PodPending
	return p
}

func TestMoveFitCountsTheNodeAsItWillBe(t *testing.T) {
	server := moveNode("server-1", "2", "4Gi")
	db := servicePod("shop", "db", "server-1", "500m", "1Gi")
	full := func(pods ...corev1.Pod) MoveResources {
		return MoveResources{Nodes: []corev1.Node{server, moveNode("worker-1", "4", "8Gi")}, Pods: append([]corev1.Pod{db}, pods...)}
	}
	for name, tc := range map[string]struct {
		res  MoveResources
		want string
	}{
		"room once the service stops": {full(requestPod("kube-system", "platform", "server-1", "1000m", "2Gi")), ""},
		"too little CPU for the service again": {full(requestPod("kube-system", "platform", "server-1", "1600m", "2Gi")),
			"server-1 would have 400m CPU and 2.0 GiB of memory unrequested once db stops"},
		"too little memory, counted on its own": {full(requestPod("kube-system", "platform", "server-1", "100m", "3584Mi")), "512 MiB of memory"},
		"a pending pod that could take the room": {full(requestPod("kube-system", "platform", "server-1", "1400m", "2Gi"), pendingPod("web-1", "200m")),
			"pending pods that could start there (web/web-1)"},
		"a pending pod pinned to another node": {full(requestPod("kube-system", "platform", "server-1", "1400m", "2Gi"), func() corev1.Pod {
			p := pendingPod("web-1", "200m")
			p.Spec.NodeSelector = map[string]string{corev1.LabelHostname: "worker-1"}
			return p
		}()), ""},
		"a pending pod with affinity for another node": {full(requestPod("kube-system", "platform", "server-1", "1400m", "2Gi"), func() corev1.Pod {
			p := pendingPod("web-1", "200m")
			p.Spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpNotIn, Values: []string{"server-1"}},
				}}},
			}}}
			return p
		}()), ""},
		"a pending pod too large for the freed room": {full(requestPod("kube-system", "platform", "server-1", "1400m", "2Gi"), pendingPod("big", "3")), ""},
		"a pending pod that does not tolerate the node's taint": {func() MoveResources {
			r := full(requestPod("kube-system", "platform", "server-1", "1400m", "2Gi"), pendingPod("web-1", "200m"))
			r.Nodes[0].Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}}
			return r
		}(), ""},
		"a pending pod that tolerates the node's taint": {func() MoveResources {
			r := full(requestPod("kube-system", "platform", "server-1", "1400m", "2Gi"), func() corev1.Pod {
				p := pendingPod("web-1", "200m")
				p.Spec.Tolerations = []corev1.Toleration{{Key: "dedicated", Operator: corev1.TolerationOpExists}}
				return p
			}())
			r.Nodes[0].Spec.Taints = []corev1.Taint{{Key: "dedicated", Value: "db", Effect: corev1.TaintEffectNoSchedule}}
			return r
		}(), "pending pods that could start there (web/web-1)"},
	} {
		t.Run(name, func(t *testing.T) {
			err := moveFitRefusal(tc.res, "server-1", "shop", "db")
			if tc.want == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
			assert.Contains(t, err.Error(), "point-in-time check")
		})
	}
}

// A service smaller than the copy pod still leaves room for the copy pod.
func TestMoveFitNeedsTheLargestOfTheMovesPods(t *testing.T) {
	res := MoveResources{
		Nodes: []corev1.Node{moveNode("server-1", "1", "1Gi")},
		Pods:  []corev1.Pod{servicePod("shop", "db", "server-1", "10m", "16Mi"), requestPod("kube-system", "platform", "server-1", "980m", "960Mi")},
	}
	err := moveFitRefusal(res, "server-1", "shop", "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the move needs 25m CPU and 64 MiB")
}

func moveQuota(name string, hard, used corev1.ResourceList) corev1.ResourceQuota {
	return corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Namespace: "shop", Name: name},
		Spec:       corev1.ResourceQuotaSpec{Hard: hard},
		Status:     corev1.ResourceQuotaStatus{Hard: hard, Used: used},
	}
}

func list(pairs ...string) corev1.ResourceList {
	out := corev1.ResourceList{}
	for i := 0; i+1 < len(pairs); i += 2 {
		out[corev1.ResourceName(pairs[i])] = resource.MustParse(pairs[i+1])
	}
	return out
}

func TestMoveQuotaAdmitsTheHelperPodsOnceTheServiceStops(t *testing.T) {
	db := servicePod("shop", "db", "server-1", "500m", "1Gi")
	check := func(quotas ...corev1.ResourceQuota) error {
		return moveQuotaRefusal(MoveResources{Pods: []corev1.Pod{db, servicePod("other", "db", "server-1", "4", "8Gi")}, Quotas: quotas}, "shop", "db")
	}
	assert.NoError(t, check(moveQuota("kipper-quota", list("limits.memory", "1Gi", "limits.cpu", "1", "pods", "5"), list("limits.memory", "1Gi", "limits.cpu", "500m", "pods", "1"))),
		"the copy pod's 1Gi limit takes the room db frees")

	err := check(moveQuota("kipper-quota", list("limits.memory", "1Gi"), list("limits.memory", "1536Mi")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kipper-quota")
	assert.Contains(t, err.Error(), "limits.memory")

	err = check(moveQuota("pods-only", list("pods", "1"), list("pods", "2")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pods-only")

	scoped := moveQuota("best-effort", list("pods", "0"), list("pods", "0"))
	scoped.Spec.Scopes = []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeBestEffort}
	assert.NoError(t, check(scoped), "a quota for best-effort pods never counts the move's pods")

	notTerminating := moveQuota("long-running", list("requests.cpu", "500m"), list("requests.cpu", "500m"))
	notTerminating.Spec.Scopes = []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeNotTerminating}
	assert.NoError(t, check(notTerminating))
	notTerminating.Status.Used = list("requests.cpu", "490m")
	notTerminating.Status.Hard = list("requests.cpu", "10m")
	assert.Error(t, check(notTerminating))
}

// The copy pod and the verify pod run one after the other, so the quota is
// charged for the larger of them, and the refusal says so.
func TestMoveQuotaRefusalNamesTheLargerHelperPod(t *testing.T) {
	db := servicePod("shop", "db", "server-1", "200m", "1Gi")
	err := moveQuotaRefusal(MoveResources{Pods: []corev1.Pod{db}, Quotas: []corev1.ResourceQuota{
		moveQuota("tight", list("requests.memory", "2Gi"), list("requests.memory", "3Gi")),
	}}, "shop", "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the ResourceQuota tight in shop allows 2Gi of requests.memory, and the larger of the move's copy and verify pods would bring it to 2112Mi")
}

// A quota's scopes and scope selector are evaluated against the helper pods
// as the API server admits them, and against each service pod for the room it
// frees.
func TestMoveQuotaEvaluatesScopesAgainstTheHelperPods(t *testing.T) {
	db := servicePod("shop", "db", "server-1", "500m", "1Gi")
	check := func(defaultClass string, q corev1.ResourceQuota) error {
		return moveQuotaRefusal(MoveResources{Pods: []corev1.Pod{db}, Quotas: []corev1.ResourceQuota{q}, DefaultPriorityClass: defaultClass}, "shop", "db")
	}
	full := func(name string, scope corev1.ResourceQuotaScope, op corev1.ScopeSelectorOperator, values ...string) corev1.ResourceQuota {
		q := moveQuota(name, list("limits.memory", "1Gi"), list("limits.memory", "1536Mi"))
		q.Spec.ScopeSelector = &corev1.ScopeSelector{MatchExpressions: []corev1.ScopedResourceSelectorRequirement{{ScopeName: scope, Operator: op, Values: values}}}
		return q
	}

	for name, tc := range map[string]struct {
		defaultClass string
		quota        corev1.ResourceQuota
		counts       bool
	}{
		"not best effort":                {"", full("q", corev1.ResourceQuotaScopeNotBestEffort, corev1.ScopeSelectorOpExists), true},
		"best effort":                    {"", full("q", corev1.ResourceQuotaScopeBestEffort, corev1.ScopeSelectorOpExists), false},
		"terminating":                    {"", full("q", corev1.ResourceQuotaScopeTerminating, corev1.ScopeSelectorOpExists), false},
		"not terminating":                {"", full("q", corev1.ResourceQuotaScopeNotTerminating, corev1.ScopeSelectorOpExists), true},
		"cross-namespace affinity":       {"", full("q", corev1.ResourceQuotaScopeCrossNamespacePodAffinity, corev1.ScopeSelectorOpExists), false},
		"a priority class":               {"", full("q", corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpExists), false},
		"a default priority class":       {"standard", full("q", corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpExists), true},
		"no priority class":              {"", full("q", corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpDoesNotExist), true},
		"priority class high":            {"", full("q", corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpIn, "high"), false},
		"default priority class in high": {"high", full("q", corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpIn, "high"), true},
		"priority class not high":        {"", full("q", corev1.ResourceQuotaScopePriorityClass, corev1.ScopeSelectorOpNotIn, "high"), true},
		"volume attributes class":        {"", full("q", corev1.ResourceQuotaScopeVolumeAttributesClass, corev1.ScopeSelectorOpExists), false},
	} {
		t.Run(name, func(t *testing.T) {
			err := check(tc.defaultClass, tc.quota)
			if !tc.counts {
				assert.NoError(t, err, "a quota that does not cover the helper pods never refuses them")
				return
			}
			require.Error(t, err, "a full quota that covers the helper pods refuses the move")
			assert.Contains(t, err.Error(), "ResourceQuota q")
		})
	}

	both := full("q", corev1.ResourceQuotaScopeNotBestEffort, corev1.ScopeSelectorOpExists)
	both.Spec.Scopes = []corev1.ResourceQuotaScope{corev1.ResourceQuotaScopeTerminating}
	assert.NoError(t, check("", both), "scopes and the selector must all match")

	unknown := full("future", "SomeNewScope", corev1.ScopeSelectorOpExists)
	unknown.Status.Used = list("limits.memory", "0")
	err := check("", unknown)
	require.Error(t, err, "a scope kip cannot evaluate refuses the move")
	assert.Contains(t, err.Error(), "ResourceQuota future")
	assert.Contains(t, err.Error(), "SomeNewScope")

	high := moveQuota("high", list("limits.memory", "1Gi"), list("limits.memory", "1Gi"))
	high.Spec.ScopeSelector = &corev1.ScopeSelector{MatchExpressions: []corev1.ScopedResourceSelectorRequirement{
		{ScopeName: corev1.ResourceQuotaScopePriorityClass, Operator: corev1.ScopeSelectorOpIn, Values: []string{"high"}}}}
	withClass := func(class string) error {
		p := servicePod("shop", "db", "server-1", "500m", "1Gi")
		p.Spec.PriorityClassName = class
		return moveQuotaRefusal(MoveResources{Pods: []corev1.Pod{p}, Quotas: []corev1.ResourceQuota{high}, DefaultPriorityClass: "high"}, "shop", "db")
	}
	assert.NoError(t, withClass("high"), "the helpers take the room db frees")
	err = withClass("low")
	require.Error(t, err, "a service pod the quota does not cover frees nothing in it")
	assert.Contains(t, err.Error(), "ResourceQuota high")
}
