package installer

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/service"
)

// MoveResources is what a move reads to check that the server node and the
// namespace's quotas admit its pods: every node and pod in the cluster, the
// ResourceQuotas of the service's namespace, and the priority class the API
// server gives a pod that names none, or "" when there is no default.
type MoveResources struct {
	Nodes                []corev1.Node
	Pods                 []corev1.Pod
	Quotas               []corev1.ResourceQuota
	DefaultPriorityClass string
}

// moveHelperResources returns copy and verify pod requirements. The pods run
// sequentially, so capacity checks use the larger requirement for each resource.
func moveHelperResources() []corev1.ResourceRequirements {
	return []corev1.ResourceRequirements{copyPodResources(), movePodResources()}
}

// servicePods returns the service's unfinished StatefulSet pods.
func servicePods(pods []corev1.Pod, namespace, svc string) []corev1.Pod {
	var out []corev1.Pod
	for i := range pods {
		p := &pods[i]
		if ownedBy(p, namespace, svc) && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
			out = append(out, *p)
		}
	}
	return out
}

func ownedBy(p *corev1.Pod, namespace, svc string) bool {
	owner := metav1.GetControllerOf(p)
	return p.Namespace == namespace && owner != nil && owner.Kind == "StatefulSet" && owner.Name == svc
}

// moveFitRefusal checks that the node keeps room for the move's largest
// pod, the copy pod, the verify pod or the service itself, once the
// service's own pods stop. Every other pod on the node counts, and so does
// a pending pod that may be placed on the node and fits in the room the
// service frees. CPU and memory are checked separately.
func moveFitRefusal(res MoveResources, node, namespace, svc string) error {
	var target *corev1.Node
	for i := range res.Nodes {
		n := &res.Nodes[i]
		if strings.EqualFold(n.Name, node) || strings.EqualFold(n.Labels[corev1.LabelHostname], node) {
			target = n
		}
	}
	if target == nil {
		return fmt.Errorf("node %s of the volume is not in the cluster", node)
	}
	room := service.Headroom([]corev1.Node{*target}, res.Pods)[0]
	var serviceCPU, serviceMemory int64
	for _, p := range servicePods(res.Pods, namespace, svc) {
		cpu, memory := platform.PodRequests(&p.Spec)
		serviceCPU, serviceMemory = max(serviceCPU, cpu), max(serviceMemory, memory)
		if p.Spec.NodeName == node {
			room.CPUMilli += cpu
			room.MemoryBytes += memory
		}
	}
	pending, cpu, memory := pendingThatFit(res.Pods, target, room, func(p *corev1.Pod) bool { return ownedBy(p, namespace, svc) })
	room.CPUMilli -= cpu
	room.MemoryBytes -= memory
	needCPU, needMemory := serviceCPU, serviceMemory
	for _, r := range moveHelperResources() {
		needCPU = max(needCPU, r.Requests.Cpu().MilliValue())
		needMemory = max(needMemory, r.Requests.Memory().Value())
	}
	if room.CPUMilli >= needCPU && room.MemoryBytes >= needMemory {
		return nil
	}
	after := ""
	if len(pending) > 0 {
		after = " and the pending pods that could start there (" + strings.Join(pending, ", ") + ") take their share"
	}
	return fmt.Errorf("%s would have %dm CPU and %s of memory unrequested once %s stops%s, and the move needs %dm CPU and %s of memory for the larger of its copy pod, its verify pod and %s itself. "+
		"This is a point-in-time check of the requests on the node, not a reservation, so it can refuse a move that would have fit or pass one that later waits for room. "+
		"Lower another service's CPU or memory with 'kip service update', or give the node more, then run the command again",
		node, max(room.CPUMilli, 0), formatBytes(max(room.MemoryBytes, 0)), svc, after, needCPU, formatBytes(needMemory), svc)
}

// pendingThatFit returns sorted namespace/name pairs and summed requests for
// unscheduled pending pods that may run on node and individually fit in room,
// excluding pods matched by skip. It conservatively counts all such pods against
// the same available room, even if they cannot all fit together or may run elsewhere.
func pendingThatFit(pods []corev1.Pod, node *corev1.Node, room service.NodeHeadroom, skip func(*corev1.Pod) bool) (names []string, cpuMilli, memoryBytes int64) {
	for i := range pods {
		p := &pods[i]
		if p.Spec.NodeName != "" || p.Status.Phase != corev1.PodPending || skip(p) {
			continue
		}
		cpu, memory := platform.PodRequests(&p.Spec)
		if cpu > room.CPUMilli || memory > room.MemoryBytes || !mayRunOn(p, node) {
			continue
		}
		cpuMilli += cpu
		memoryBytes += memory
		names = append(names, p.Namespace+"/"+p.Name)
	}
	sort.Strings(names)
	return names, cpuMilli, memoryBytes
}

// mayRunOn reports whether the pod's node selector, required node affinity
// and tolerations allow it on the node.
func mayRunOn(p *corev1.Pod, node *corev1.Node) bool {
	for k, v := range p.Spec.NodeSelector {
		if node.Labels[k] != v {
			return false
		}
	}
	if a := p.Spec.Affinity; a != nil && a.NodeAffinity != nil && a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		matched := false
		for _, term := range a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
			if termMatches(term, node) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	for i := range node.Spec.Taints {
		taint := &node.Spec.Taints[i]
		if taint.Effect != corev1.TaintEffectNoSchedule && taint.Effect != corev1.TaintEffectNoExecute {
			continue
		}
		tolerated := false
		for j := range p.Spec.Tolerations {
			if tolerates(p.Spec.Tolerations[j], taint) {
				tolerated = true
				break
			}
		}
		if !tolerated {
			return false
		}
	}
	return true
}

// tolerates matches a toleration to a taint by key, value and effect. An
// empty key with Exists matches any key, subject to the effect check.
// Unsupported operators never match, so pods using them may be excluded
// from the pending demand estimate.
func tolerates(t corev1.Toleration, taint *corev1.Taint) bool {
	if (t.Effect != "" && t.Effect != taint.Effect) || (t.Key != "" && t.Key != taint.Key) {
		return false
	}
	switch t.Operator {
	case corev1.TolerationOpExists:
		return true
	case "", corev1.TolerationOpEqual:
		return t.Key != "" && t.Value == taint.Value
	}
	return false
}

var nodeSelectorOperators = map[corev1.NodeSelectorOperator]selection.Operator{
	corev1.NodeSelectorOpIn:           selection.In,
	corev1.NodeSelectorOpNotIn:        selection.NotIn,
	corev1.NodeSelectorOpExists:       selection.Exists,
	corev1.NodeSelectorOpDoesNotExist: selection.DoesNotExist,
	corev1.NodeSelectorOpGt:           selection.GreaterThan,
	corev1.NodeSelectorOpLt:           selection.LessThan,
}

// termMatches evaluates one node selector term. A term without
// requirements, or with one that cannot be parsed, matches no node, as the
// scheduler treats it.
func termMatches(term corev1.NodeSelectorTerm, node *corev1.Node) bool {
	if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
		return false
	}
	for _, e := range term.MatchExpressions {
		op, ok := nodeSelectorOperators[e.Operator]
		if !ok {
			return false
		}
		req, err := k8slabels.NewRequirement(e.Key, op, e.Values)
		if err != nil || !req.Matches(k8slabels.Set(node.Labels)) {
			return false
		}
	}
	for _, f := range term.MatchFields {
		in := containsString(f.Values, node.Name)
		switch {
		case f.Key != "metadata.name",
			f.Operator == corev1.NodeSelectorOpIn && !in,
			f.Operator == corev1.NodeSelectorOpNotIn && in,
			f.Operator != corev1.NodeSelectorOpIn && f.Operator != corev1.NodeSelectorOpNotIn:
			return false
		}
	}
	return true
}

// moveQuotaRefusal checks that the namespace's quotas admit the larger of the
// copy pod and the verify pod, which run one at a time, once the service's
// pods are gone. Each quota counts helper pods and frees service pods only
// within its scopes. Limits freed by the service are summed over its
// containers, which never frees more than the
// quota counted.
func moveQuotaRefusal(res MoveResources, namespace, svc string) error {
	pods := servicePods(res.Pods, namespace, svc)
	return quotaRefusal(res.Quotas, "the larger of the move's copy and verify pods", func(q corev1.ResourceQuota) (corev1.ResourceList, corev1.ResourceList, error) {
		add := corev1.ResourceList{}
		for _, r := range moveHelperResources() {
			helper := corev1.PodSpec{PriorityClassName: res.DefaultPriorityClass, Containers: []corev1.Container{{Resources: r}}}
			covered, err := quotaCovers(q, func(r corev1.ScopedResourceSelectorRequirement) (bool, error) { return podInScope(r, &helper) })
			if err != nil {
				return nil, nil, err
			}
			if !covered {
				continue
			}
			raise(add, corev1.ResourceRequestsCPU, *r.Requests.Cpu())
			raise(add, corev1.ResourceCPU, *r.Requests.Cpu())
			raise(add, corev1.ResourceRequestsMemory, *r.Requests.Memory())
			raise(add, corev1.ResourceMemory, *r.Requests.Memory())
			raise(add, corev1.ResourceLimitsCPU, *r.Limits.Cpu())
			raise(add, corev1.ResourceLimitsMemory, *r.Limits.Memory())
		}
		if len(add) == 0 {
			return nil, nil, nil
		}
		add[corev1.ResourcePods] = resource.MustParse("1")
		add["count/pods"] = resource.MustParse("1")

		free := corev1.ResourceList{}
		for i := range pods {
			p := &pods[i]
			covered, err := quotaCovers(q, func(r corev1.ScopedResourceSelectorRequirement) (bool, error) { return podInScope(r, &p.Spec) })
			if err != nil {
				return nil, nil, err
			}
			if !covered {
				continue
			}
			cpu, memory := platform.PodRequests(&p.Spec)
			for _, name := range []corev1.ResourceName{corev1.ResourceRequestsCPU, corev1.ResourceCPU} {
				addTo(free, name, *resource.NewMilliQuantity(cpu, resource.DecimalSI))
			}
			for _, name := range []corev1.ResourceName{corev1.ResourceRequestsMemory, corev1.ResourceMemory} {
				addTo(free, name, *resource.NewQuantity(memory, resource.BinarySI))
			}
			for _, c := range p.Spec.Containers {
				addTo(free, corev1.ResourceLimitsCPU, *c.Resources.Limits.Cpu())
				addTo(free, corev1.ResourceLimitsMemory, *c.Resources.Limits.Memory())
			}
			addTo(free, corev1.ResourcePods, resource.MustParse("1"))
			addTo(free, "count/pods", resource.MustParse("1"))
		}
		return add, free, nil
	})
}

func raise(l corev1.ResourceList, name corev1.ResourceName, q resource.Quantity) {
	if cur, ok := l[name]; !ok || q.Cmp(cur) > 0 {
		l[name] = q.DeepCopy()
	}
}

func addTo(l corev1.ResourceList, name corev1.ResourceName, q resource.Quantity) {
	cur := l[name]
	cur.Add(q)
	l[name] = cur
}

// quotaCharge returns what a quota is charged for an action and what the
// action frees in it first. A nil add means the quota does not cover the
// action's objects.
type quotaCharge func(q corev1.ResourceQuota) (add, free corev1.ResourceList, err error)

// quotaRefusal checks quotas in name order and returns the first evaluation
// error or refusal. It subtracts freed resources from usage before adding the
// new charge, with a minimum starting usage of zero.
func quotaRefusal(quotas []corev1.ResourceQuota, what string, charge quotaCharge) error {
	sorted := append([]corev1.ResourceQuota(nil), quotas...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, q := range sorted {
		add, free, err := charge(q)
		if err != nil {
			return fmt.Errorf("the ResourceQuota %s in %s %w, so kip cannot tell whether it admits %s. Remove that scope from the quota or upgrade kip, then run the command again", q.Name, q.Namespace, err, what)
		}
		names := make([]string, 0, len(q.Status.Hard))
		for name := range q.Status.Hard {
			names = append(names, string(name))
		}
		sort.Strings(names)
		for _, n := range names {
			name := corev1.ResourceName(n)
			more, ok := add[name]
			if !ok {
				continue
			}
			hard := q.Status.Hard[name]
			after := q.Status.Used[name].DeepCopy()
			after.Sub(free[name])
			if after.Sign() < 0 {
				after = resource.Quantity{}
			}
			after.Add(more)
			if after.Cmp(hard) > 0 {
				return fmt.Errorf("the ResourceQuota %s in %s allows %s of %s, and %s would bring it to %s. Raise the quota or lower another workload's %s, then run the command again",
					q.Name, q.Namespace, hard.String(), name, what, after.String(), name)
			}
		}
	}
	return nil
}

// quotaCovers reports whether every scope of the quota and every requirement
// of its scope selector matches, as inScope evaluates them for one object. A
// scope in spec.scopes is a requirement with the Exists operator.
func quotaCovers(q corev1.ResourceQuota, inScope func(corev1.ScopedResourceSelectorRequirement) (bool, error)) (bool, error) {
	reqs := make([]corev1.ScopedResourceSelectorRequirement, 0, len(q.Spec.Scopes))
	for _, s := range q.Spec.Scopes {
		reqs = append(reqs, corev1.ScopedResourceSelectorRequirement{ScopeName: s, Operator: corev1.ScopeSelectorOpExists})
	}
	if q.Spec.ScopeSelector != nil {
		reqs = append(reqs, q.Spec.ScopeSelector.MatchExpressions...)
	}
	for _, r := range reqs {
		if ok, err := inScope(r); err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

// podInScope matches a pod against one quota scope the way the API server's
// pod quota does. A volume attributes class scope covers only claims.
func podInScope(r corev1.ScopedResourceSelectorRequirement, spec *corev1.PodSpec) (bool, error) {
	terminating := spec.ActiveDeadlineSeconds != nil && *spec.ActiveDeadlineSeconds >= 0
	switch r.ScopeName {
	case corev1.ResourceQuotaScopeTerminating:
		return terminating, nil
	case corev1.ResourceQuotaScopeNotTerminating:
		return !terminating, nil
	case corev1.ResourceQuotaScopeBestEffort:
		return bestEffort(spec), nil
	case corev1.ResourceQuotaScopeNotBestEffort:
		return !bestEffort(spec), nil
	case corev1.ResourceQuotaScopePriorityClass:
		return valueInScope(r, spec.PriorityClassName)
	case corev1.ResourceQuotaScopeCrossNamespacePodAffinity:
		return crossNamespaceAffinity(spec), nil
	case corev1.ResourceQuotaScopeVolumeAttributesClass:
		return false, nil
	}
	return false, fmt.Errorf("has the scope %s, which kip cannot evaluate", r.ScopeName)
}

// claimInScope matches a claim with the given volume attributes class, ""
// for none, against one quota scope the way the API server's claim quota
// does. Pod scopes never cover a claim.
func claimInScope(r corev1.ScopedResourceSelectorRequirement, class string) (bool, error) {
	switch r.ScopeName {
	case corev1.ResourceQuotaScopeVolumeAttributesClass:
		return valueInScope(r, class)
	case corev1.ResourceQuotaScopeTerminating, corev1.ResourceQuotaScopeNotTerminating, corev1.ResourceQuotaScopeBestEffort,
		corev1.ResourceQuotaScopeNotBestEffort, corev1.ResourceQuotaScopePriorityClass, corev1.ResourceQuotaScopeCrossNamespacePodAffinity:
		return false, nil
	}
	return false, fmt.Errorf("has the scope %s, which kip cannot evaluate", r.ScopeName)
}

// valueInScope evaluates a selector requirement on a class name, "" for
// none, as a label that is absent when the name is empty.
func valueInScope(r corev1.ScopedResourceSelectorRequirement, value string) (bool, error) {
	ops := map[corev1.ScopeSelectorOperator]selection.Operator{
		corev1.ScopeSelectorOpIn: selection.In, corev1.ScopeSelectorOpNotIn: selection.NotIn,
		corev1.ScopeSelectorOpExists: selection.Exists, corev1.ScopeSelectorOpDoesNotExist: selection.DoesNotExist,
	}
	op, ok := ops[r.Operator]
	if !ok {
		return false, fmt.Errorf("has the operator %s on the scope %s, which kip cannot evaluate", r.Operator, r.ScopeName)
	}
	req, err := k8slabels.NewRequirement(string(r.ScopeName), op, r.Values)
	if err != nil {
		return false, fmt.Errorf("has a %s selector kip cannot evaluate: %w", r.ScopeName, err)
	}
	set := k8slabels.Set{}
	if value != "" {
		set[string(r.ScopeName)] = value
	}
	return req.Matches(set), nil
}

// bestEffort reports whether no container sets a CPU or memory request or
// limit, which makes the pod BestEffort.
func bestEffort(spec *corev1.PodSpec) bool {
	containers := append(append([]corev1.Container(nil), spec.InitContainers...), spec.Containers...)
	for _, c := range containers {
		for _, l := range []corev1.ResourceList{c.Resources.Requests, c.Resources.Limits} {
			if _, ok := l[corev1.ResourceCPU]; ok {
				return false
			}
			if _, ok := l[corev1.ResourceMemory]; ok {
				return false
			}
		}
	}
	return true
}

// crossNamespaceAffinity reports whether any pod affinity or anti-affinity
// term explicitly selects namespaces, including an empty namespace selector.
func crossNamespaceAffinity(spec *corev1.PodSpec) bool {
	a := spec.Affinity
	if a == nil {
		return false
	}
	var terms []corev1.PodAffinityTerm
	if a.PodAffinity != nil {
		terms = append(terms, a.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution...)
		for _, w := range a.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			terms = append(terms, w.PodAffinityTerm)
		}
	}
	if a.PodAntiAffinity != nil {
		terms = append(terms, a.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution...)
		for _, w := range a.PodAntiAffinity.PreferredDuringSchedulingIgnoredDuringExecution {
			terms = append(terms, w.PodAffinityTerm)
		}
	}
	for _, t := range terms {
		if len(t.Namespaces) != 0 || t.NamespaceSelector != nil {
			return true
		}
	}
	return false
}
