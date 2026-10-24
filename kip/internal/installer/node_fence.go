package installer

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"

	"github.com/getkipper/kipper/controller/pkg/stranded"
	"github.com/getkipper/kipper/kip/internal/config"
)

// FenceStopAnnotation marks a StatefulSet the fence stopped. Its value is a
// FenceStopMark in JSON. It is written in the same patch that sets replicas
// to 0, so a stopped service never loses the count to restore.
const FenceStopAnnotation = stranded.FenceStopAnnotation

// FenceStopMark is the value of FenceStopAnnotation.
type FenceStopMark struct {
	Node     string   `json:"node"`
	Replicas int32    `json:"replicas"`
	Reason   string   `json:"reason"`
	Volumes  []string `json:"volumes"`
}

// FenceNode captures a Node's readiness, taint and boot ID. NotReadyFor uses
// the server's clock and is zero for a Ready node or an unknown transition time.
type FenceNode struct {
	Exists       bool
	Ready        bool
	OutOfService bool
	NotReadyFor  time.Duration
	BootID       string
}

// ReadFenceNode reads node's readiness, taint and boot ID through the server.
// A missing Node returns Exists false and no error.
func ReadFenceNode(master CommandRunner, node string) (FenceNode, error) {
	out, err := master.Run(fmt.Sprintf("kubectl get node %s --ignore-not-found -o json && echo kip-server-now=$(date -u +%%s)", shellQuote(node)))
	if err != nil {
		return FenceNode{}, fmt.Errorf("reading node %s: %w", node, err)
	}
	body, nowLine, ok := strings.Cut(out, "kip-server-now=")
	if !ok {
		return FenceNode{}, fmt.Errorf("reading node %s: the server's clock is missing from the answer", node)
	}
	secs, err := strconv.ParseInt(strings.TrimSpace(nowLine), 10, 64)
	if err != nil {
		return FenceNode{}, fmt.Errorf("reading the server's clock: %w", err)
	}
	body = kubectlPayload(body)
	if strings.TrimSpace(body) == "" {
		return FenceNode{}, nil
	}
	var n corev1.Node
	if err := json.Unmarshal([]byte(body), &n); err != nil {
		return FenceNode{}, fmt.Errorf("reading node %s: %w", node, err)
	}
	f := FenceNode{Exists: true, OutOfService: stranded.OutOfService(&n), BootID: n.Status.NodeInfo.BootID}
	for _, c := range n.Status.Conditions {
		if c.Type != corev1.NodeReady {
			continue
		}
		f.Ready = c.Status == corev1.ConditionTrue
		if !f.Ready && !c.LastTransitionTime.IsZero() {
			f.NotReadyFor = max(time.Unix(secs, 0).Sub(c.LastTransitionTime.Time), 0)
		}
	}
	return f, nil
}

// TaintOutOfService applies the out-of-service NoExecute taint to node.
func TaintOutOfService(master CommandRunner, node string) error {
	t := stranded.Taint()
	if _, err := master.Run(fmt.Sprintf("kubectl taint node %s %s=%s:%s --overwrite >/dev/null", shellQuote(node), t.Key, t.Value, t.Effect)); err != nil {
		return fmt.Errorf("tainting %s out of service: %w", node, err)
	}
	return nil
}

// RemoveOutOfServiceTaint removes the out-of-service taint and confirms that
// it is gone.
func RemoveOutOfServiceTaint(master CommandRunner, node string) error {
	if _, err := master.Run(removeTaintCommand(shellQuote(node), stranded.TaintKey)); err != nil {
		return fmt.Errorf("removing the out-of-service taint from %s: %w", node, err)
	}
	return nil
}

// KubeletAnswers probes address's kubelet TCP port from the server, which has
// access through the managed firewall and resolves hostnames to IPv4.
// It first checks the probe against the server's local API port. A refused
// or timed-out kubelet connection returns false with no error. Address
// resolution or probe failures return an error to distinguish an unavailable
// check from an unresponsive kubelet.
func KubeletAnswers(master CommandRunner, address string) (bool, error) {
	addr, err := netip.ParseAddr(address)
	if err == nil && !addr.Is4() {
		return false, fmt.Errorf("%s is not an IPv4 address", address)
	}
	if err != nil {
		out, rerr := master.Run(fmt.Sprintf("getent ahostsv4 %s", shellQuote(address)))
		fields := strings.Fields(out)
		if rerr == nil && len(fields) > 0 {
			addr, err = netip.ParseAddr(fields[0])
		}
		if rerr != nil || len(fields) == 0 || err != nil || !addr.Is4() {
			return false, fmt.Errorf("%s does not resolve to an IPv4 address on the server", address)
		}
	}
	out, err := master.Run(kubeletProbe(addr))
	if err != nil {
		return false, fmt.Errorf("probing the kubelet of %s from the server: %w", address, err)
	}
	switch strings.TrimSpace(out) {
	case kubeletOpen:
		return true, nil
	case kubeletClosed:
		return false, nil
	}
	return false, fmt.Errorf("probing the kubelet of %s from the server gave no result: %s", address, strings.TrimSpace(out))
}

// The kubelet probe prints one of these, so an answer without either shows
// that the probe did not run.
const (
	kubeletOpen   = "kip-kubelet=open"
	kubeletClosed = "kip-kubelet=closed"
)

// kubeletProbe connects to addr's kubelet port with Bash's /dev/tcp. Bash
// exits 1 when the connection fails and timeout exits 124 when it hangs.
// Any other status, or a failed connection to the local API port, means the
// probe could not run, so it prints neither marker.
func kubeletProbe(addr netip.Addr) string {
	return fmt.Sprintf(`if timeout 5 bash -c '</dev/tcp/127.0.0.1/6443' 2>/dev/null; then timeout 5 bash -c '</dev/tcp/%s/%s' 2>/dev/null; case $? in 0) echo %s;; 1|124) echo %s;; *) echo kip-kubelet-probe-failed;; esac; else echo kip-kubelet-probe-failed; fi`, addr, kubeletPort, kubeletOpen, kubeletClosed)
}

// NodeExternalIP reads the node's published ExternalIP addresses through master.
// Multiple addresses are space-separated. A missing node or a node with no
// ExternalIP returns an empty string with no error. Command failures return an error.
func NodeExternalIP(master CommandRunner, node string) (string, error) {
	out, err := master.Run(fmt.Sprintf(`kubectl get node %s --ignore-not-found -o jsonpath='{.status.addresses[?(@.type=="ExternalIP")].address}'`, shellQuote(node)))
	if err != nil {
		return "", fmt.Errorf("reading the external address of node %s: %w", node, err)
	}
	return kubectlValue(out), nil
}

// NodeDownPolicy reads Longhorn's node-down-pod-deletion-policy setting.
func NodeDownPolicy(master CommandRunner) (string, error) {
	out, err := master.Run(`kubectl -n longhorn-system get settings.longhorn.io node-down-pod-deletion-policy -o jsonpath='{.value}'`)
	if err != nil {
		return "", fmt.Errorf("reading Longhorn's node-down pod deletion policy: %w", err)
	}
	return kubectlValue(out), nil
}

// FenceService is a StatefulSet with RWO Longhorn volumes stranded on a node,
// or one an earlier fence of that node stopped. Replicas is the count to
// restore for a StatefulSet that a migration had already scaled to zero, or 0
// when it is unknown.
type FenceService struct {
	Namespace   string
	StatefulSet string
	Pods        []string
	Volumes     []string
	Stop        *FenceStopMark
	Replicas    int32
}

// Label names the service and its namespace.
func (s FenceService) Label() string { return fmt.Sprintf("%s (%s)", s.StatefulSet, s.Namespace) }

// FenceView is the cluster state used for fencing decisions. Holders lists the
// Kipper pods with single-node Longhorn claims that have no StatefulSet owner.
// Unmanaged lists pods with such claims that lack the
// app.kubernetes.io/managed-by=kipper label, using namespace/name entries.
// These pods are listed for warnings because fencing checks only Kipper workloads.
type FenceView struct {
	Services  []FenceService
	Holders   []PodHolder
	Unmanaged []string
	Replicas  []stranded.Replica
}

// Kinds of PodHolder that kip knows how to stop.
const (
	HolderDataTransfer = "DataTransfer"
	HolderJob          = "Job"
)

// PodHolder identifies a pod outside a StatefulSet that mounts a single-node
// volume and the object that can restart it. Kind names a migration mover's
// DataTransfer or the controlling owner, falling back to the first owner
// reference. An empty Kind means the pod has no owner.
type PodHolder struct {
	Namespace  string
	Pod        string
	Kind       string
	Name       string
	APIVersion string
}

// Label names the pod.
func (h PodHolder) Label() string { return h.Namespace + "/" + h.Pod }

// moverPrefix and moverSuffix frame the DataTransfer name in its mover pod's
// name.
const (
	moverPrefix = "dt-"
	moverSuffix = "-export"
)

func holderOf(p corev1.Pod) PodHolder {
	h := PodHolder{Namespace: p.Namespace, Pod: p.Name}
	if p.Labels["kipper.run/resource-type"] == "datatransfer-mover" && strings.HasPrefix(p.Name, moverPrefix) && strings.HasSuffix(p.Name, moverSuffix) {
		h.Kind, h.Name = HolderDataTransfer, strings.TrimSuffix(strings.TrimPrefix(p.Name, moverPrefix), moverSuffix)
		return h
	}
	if len(p.OwnerReferences) == 0 {
		return h
	}
	ref := p.OwnerReferences[0]
	for _, r := range p.OwnerReferences {
		if r.Controller != nil && *r.Controller {
			ref = r
		}
	}
	h.Kind, h.Name, h.APIVersion = ref.Kind, ref.Name, ref.APIVersion
	return h
}

// kubectlPayload returns kubectl's JSON from out without the lines printed
// before it, such as Warning: lines that the SSH runner merges into stdout.
// The opening brace must begin a line. Otherwise it returns an empty string.
func kubectlPayload(out string) string {
	if strings.HasPrefix(out, "{") {
		return out
	}
	if i := strings.Index(out, "\n{"); i >= 0 {
		return out[i+1:]
	}
	return ""
}

// kubectlValue strips Warning: lines merged into stdout by the SSH runner
// and trims surrounding whitespace from kubectl's text output.
func kubectlValue(out string) string {
	var kept []string
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "Warning: ") {
			kept = append(kept, line)
		}
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

func kubectlJSON(master CommandRunner, what, cmd string, into any) error {
	out, err := master.Run(cmd)
	if err != nil {
		return fmt.Errorf("listing %s: %w", what, err)
	}
	if err := json.Unmarshal([]byte(kubectlPayload(out)), into); err != nil {
		return fmt.Errorf("reading %s: %w", what, err)
	}
	return nil
}

// ReadFenceView lists stranded services and StatefulSets stopped by an earlier
// fence of node, sorted by namespace and name, with every Longhorn replica.
// It includes Kipper pods outside StatefulSets and services identified by their
// claim templates, with replica counts saved by migrations whose movers are present.
// Unmanaged contains distinct namespace/name entries for other pods on node with
// single-node Longhorn claims, sorted by namespace and pod name.
// It fails on read errors or unreadable fence stop annotations.
func ReadFenceView(master CommandRunner, node string) (FenceView, error) {
	var pods corev1.PodList
	if err := kubectlJSON(master, "the pods on "+node, fmt.Sprintf("kubectl get pods -A --field-selector spec.nodeName=%s -o json", shellQuote(node)), &pods); err != nil {
		return FenceView{}, err
	}
	var claims corev1.PersistentVolumeClaimList
	if err := kubectlJSON(master, "volume claims", "kubectl get pvc -A -o json", &claims); err != nil {
		return FenceView{}, err
	}
	var pvs corev1.PersistentVolumeList
	if err := kubectlJSON(master, "persistent volumes", "kubectl get pv -o json", &pvs); err != nil {
		return FenceView{}, err
	}
	var sets appsv1.StatefulSetList
	if err := kubectlJSON(master, "StatefulSets", "kubectl get statefulsets -A -o json", &sets); err != nil {
		return FenceView{}, err
	}
	replicas, err := ReadReplicas(master)
	if err != nil {
		return FenceView{}, err
	}
	view, err := buildFenceView(node, stranded.Find(node, pods.Items, claims.Items, pvs.Items), pods.Items, sets.Items, replicas)
	if err != nil {
		return FenceView{}, err
	}
	for _, s := range stranded.FindUnmanaged(node, pods.Items, claims.Items, pvs.Items) {
		if s.ReadWriteOnce {
			view.Unmanaged = appendUnique(view.Unmanaged, s.Namespace+"/"+s.Pod)
		}
	}
	if err := addPausedReplicas(master, &view); err != nil {
		return FenceView{}, err
	}
	return view, nil
}

type transferHead struct {
	Metadata struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Source struct {
			Service string `json:"service"`
		} `json:"source"`
		SourceReplicas int32 `json:"sourceReplicas"`
	} `json:"spec"`
}

// addPausedReplicas sets the replica count a migration recorded before it
// paused its source service, for each service whose export mover is a holder.
func addPausedReplicas(master CommandRunner, view *FenceView) error {
	movers := map[string]bool{}
	for _, h := range view.Holders {
		if h.Kind == HolderDataTransfer {
			movers[h.Namespace+"/"+h.Name] = true
		}
	}
	if len(movers) == 0 {
		return nil
	}
	var transfers struct {
		Items []transferHead `json:"items"`
	}
	if err := kubectlJSON(master, "data transfers", "kubectl get datatransfers.kipper.run -A -o json", &transfers); err != nil {
		return err
	}
	for _, t := range transfers.Items {
		if !movers[t.Metadata.Namespace+"/"+t.Metadata.Name] {
			continue
		}
		for i := range view.Services {
			svc := &view.Services[i]
			if svc.Namespace == t.Metadata.Namespace && svc.StatefulSet == t.Spec.Source.Service {
				svc.Replicas = t.Spec.SourceReplicas
			}
		}
	}
	return nil
}

// ClaimOwner returns the name of the first StatefulSet in ns whose claim
// template matches claim using the <template>-<set>-<ordinal> naming convention.
// The ordinal must contain one or more decimal digits and may exceed the set's
// current replica count, so claims of stopped services still match. Matching
// uses names only, without checking ownership metadata. No match returns "", false.
func ClaimOwner(sets []appsv1.StatefulSet, ns, claim string) (string, bool) {
	for _, set := range sets {
		if set.Namespace != ns {
			continue
		}
		for _, t := range set.Spec.VolumeClaimTemplates {
			ordinal, ok := strings.CutPrefix(claim, t.Name+"-"+set.Name+"-")
			if ok && ordinal != "" && strings.Trim(ordinal, "0123456789") == "" {
				return set.Name, true
			}
		}
	}
	return "", false
}

// ReadReplicas lists every Longhorn replica.
func ReadReplicas(master CommandRunner) ([]stranded.Replica, error) {
	out, err := master.Run("kubectl -n longhorn-system get replicas.longhorn.io --request-timeout=30s -o json")
	if err != nil {
		return nil, fmt.Errorf("listing Longhorn replicas: %w", err)
	}
	return stranded.ReplicasFromJSON([]byte(kubectlPayload(out)))
}

func buildFenceView(node string, found []stranded.Service, pods []corev1.Pod, sets []appsv1.StatefulSet, replicas []stranded.Replica) (FenceView, error) {
	podByKey := map[string]corev1.Pod{}
	for _, p := range pods {
		podByKey[p.Namespace+"/"+p.Name] = p
	}
	held := map[string]bool{}
	byKey := map[string]*FenceService{}
	var order []string
	get := func(ns, name string) *FenceService {
		key := ns + "/" + name
		if s, ok := byKey[key]; ok {
			return s
		}
		byKey[key] = &FenceService{Namespace: ns, StatefulSet: name}
		order = append(order, key)
		return byKey[key]
	}
	view := FenceView{Replicas: replicas}
	for _, s := range found {
		if !s.ReadWriteOnce {
			continue
		}
		if s.Owner == "" {
			if key := s.Namespace + "/" + s.Pod; !held[key] {
				held[key] = true
				view.Holders = append(view.Holders, holderOf(podByKey[key]))
			}
			// A migration mover mounts its service's claim while the service
			// is paused, and the migration starts the service again when the
			// mover ends.
			if set, ok := ClaimOwner(sets, s.Namespace, s.Claim); ok {
				svc := get(s.Namespace, set)
				svc.Volumes = appendUnique(svc.Volumes, s.Volume)
			}
			continue
		}
		svc := get(s.Namespace, s.Owner)
		svc.Pods = appendUnique(svc.Pods, s.Pod)
		svc.Volumes = appendUnique(svc.Volumes, s.Volume)
	}
	for _, set := range sets {
		raw, ok := set.Annotations[FenceStopAnnotation]
		if !ok {
			continue
		}
		var mark FenceStopMark
		if err := json.Unmarshal([]byte(raw), &mark); err != nil {
			return FenceView{}, fmt.Errorf("StatefulSet %s/%s has an unreadable %s annotation: %w", set.Namespace, set.Name, FenceStopAnnotation, err)
		}
		if mark.Node != node {
			continue
		}
		svc := get(set.Namespace, set.Name)
		svc.Stop = &mark
		for _, v := range mark.Volumes {
			svc.Volumes = appendUnique(svc.Volumes, v)
		}
	}
	sort.Strings(order)
	for _, k := range order {
		view.Services = append(view.Services, *byKey[k])
	}
	sort.Slice(view.Holders, func(i, j int) bool { return view.Holders[i].Label() < view.Holders[j].Label() })
	return view, nil
}

func appendUnique(list []string, v string) []string {
	for _, x := range list {
		if x == v {
			return list
		}
	}
	return append(list, v)
}

// Freshness describes whether every volume has a current replica on another
// node. Reason is empty when Current is true, only-copy if any volume has no
// replica elsewhere, or stale otherwise. Stamps identify the replicas and
// timestamps to which consent applies.
type Freshness struct {
	Current bool
	Reason  string
	Stamps  []config.ReplicaStamp
}

// AvailableReplicas returns replicas in input order from nodes recorded as
// ready with no fence, plus keep regardless of its record. The keep exception
// allows a rejoin to count the worker's copies after releasing its fence.
// Other joining, removing, removed or fenced workers are excluded.
func AvailableReplicas(replicas []stranded.Replica, records []NodeRecord, keep string) []stranded.Replica {
	available := map[string]bool{}
	if keep != "" {
		available[keep] = true
	}
	for _, r := range records {
		if r.State == NodeStateReady && r.Fence == nil {
			available[r.Name] = true
		}
	}
	var out []stranded.Replica
	for _, r := range replicas {
		if available[r.Node] {
			out = append(out, r)
		}
	}
	return out
}

// JudgeFreshness checks each volume for a current replica on a node other
// than node. Pass an empty node to consider all placed replicas. An empty
// volume list is current. Stamps are sorted by volume and replica name.
func JudgeFreshness(node string, volumes []string, replicas []stranded.Replica) Freshness {
	f := Freshness{Current: true}
	for _, v := range volumes {
		elsewhere := stranded.Elsewhere(v, node, replicas)
		current := false
		for _, r := range elsewhere {
			current = current || stranded.Current(r)
			f.Stamps = append(f.Stamps, config.ReplicaStamp{Volume: v, Name: r.Name, Node: r.Node,
				HealthyAt: r.HealthyAt, FailedAt: r.FailedAt, LastFailedAt: r.LastFailedAt})
		}
		switch {
		case len(elsewhere) == 0:
			f.Current, f.Reason = false, FenceStopOnlyCopy
		case !current:
			f.Current = false
			if f.Reason == "" {
				f.Reason = FenceStopStale
			}
		}
	}
	if len(volumes) == 0 {
		f.Current = true
	}
	if f.Current {
		f.Reason = ""
	}
	sort.Slice(f.Stamps, func(i, j int) bool {
		if f.Stamps[i].Volume != f.Stamps[j].Volume {
			return f.Stamps[i].Volume < f.Stamps[j].Volume
		}
		return f.Stamps[i].Name < f.Stamps[j].Name
	})
	return f
}

// SameStamps reports whether consent given for a still applies to b. Both
// slices must be non-empty and contain identical stamps in the same order.
func SameStamps(a, b []config.ReplicaStamp) bool {
	if len(a) != len(b) || len(a) == 0 {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

type statefulSetHead struct {
	Metadata struct {
		ResourceVersion string            `json:"resourceVersion"`
		Annotations     map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Replicas *int32 `json:"replicas"`
	} `json:"spec"`
}

func readStatefulSet(master CommandRunner, ns, name string) (statefulSetHead, error) {
	var head statefulSetHead
	err := kubectlJSON(master, "StatefulSet "+ns+"/"+name, fmt.Sprintf("kubectl -n %s get statefulset %s -o json", shellQuote(ns), shellQuote(name)), &head)
	return head, err
}

func patchStatefulSet(master CommandRunner, ns, name string, patch map[string]any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	_, err = master.RunStdin(fmt.Sprintf("kubectl -n %s patch statefulset %s --type merge --patch-file /dev/stdin >/dev/null", shellQuote(ns), shellQuote(name)), strings.NewReader(string(body)))
	return err
}

// StopForFence scales a StatefulSet to 0 and records mark with the replica
// count it had, in one patch guarded by the resource version. A StatefulSet
// already at 0 uses mark.Replicas, with a minimum of 1, the initial replica
// count for Kipper services. An existing annotation makes this a no-op,
// preserving the count to restore across retries.
func StopForFence(master CommandRunner, ns, name string, mark FenceStopMark) error {
	head, err := readStatefulSet(master, ns, name)
	if err != nil {
		return err
	}
	if _, stopped := head.Metadata.Annotations[FenceStopAnnotation]; stopped {
		return nil
	}
	if head.Spec.Replicas == nil || *head.Spec.Replicas > 0 {
		mark.Replicas = 1
		if head.Spec.Replicas != nil {
			mark.Replicas = *head.Spec.Replicas
		}
	}
	if mark.Replicas < 1 {
		mark.Replicas = 1
	}
	value, err := json.Marshal(mark)
	if err != nil {
		return err
	}
	if err := patchStatefulSet(master, ns, name, map[string]any{
		"metadata": map[string]any{"resourceVersion": head.Metadata.ResourceVersion, "annotations": map[string]string{FenceStopAnnotation: string(value)}},
		"spec":     map[string]any{"replicas": 0},
	}); err != nil {
		return fmt.Errorf("stopping %s/%s: %w", ns, name, err)
	}
	return nil
}

// StartAfterFence restores the replica count recorded in the fence stop
// annotation and removes the annotation, in one patch guarded by the
// resource version. A StatefulSet without the annotation is left alone.
func StartAfterFence(master CommandRunner, ns, name string) error {
	head, err := readStatefulSet(master, ns, name)
	if err != nil {
		return err
	}
	raw, stopped := head.Metadata.Annotations[FenceStopAnnotation]
	if !stopped {
		return nil
	}
	var mark FenceStopMark
	if err := json.Unmarshal([]byte(raw), &mark); err != nil {
		return fmt.Errorf("reading the %s annotation of %s/%s: %w", FenceStopAnnotation, ns, name, err)
	}
	if err := patchStatefulSet(master, ns, name, map[string]any{
		"metadata": map[string]any{"resourceVersion": head.Metadata.ResourceVersion, "annotations": map[string]any{FenceStopAnnotation: nil}},
		"spec":     map[string]any{"replicas": mark.Replicas},
	}); err != nil {
		return fmt.Errorf("starting %s/%s: %w", ns, name, err)
	}
	return nil
}

// ClearFenceStop removes the fence stop annotation, preserving the replica
// count so a stopped service stays stopped. An absent annotation is a no-op.
// The patch is guarded by the resource version.
func ClearFenceStop(master CommandRunner, ns, name string) error {
	head, err := readStatefulSet(master, ns, name)
	if err != nil {
		return err
	}
	if _, stopped := head.Metadata.Annotations[FenceStopAnnotation]; !stopped {
		return nil
	}
	if err := patchStatefulSet(master, ns, name, map[string]any{
		"metadata": map[string]any{"resourceVersion": head.Metadata.ResourceVersion, "annotations": map[string]any{FenceStopAnnotation: nil}},
	}); err != nil {
		return fmt.Errorf("clearing the fence stop mark of %s/%s: %w", ns, name, err)
	}
	return nil
}

// ReadLonghornVolumes lists existing volume names from longhorn-system in API
// response order. It returns nil when there are no volumes and an error if
// the query or JSON decoding fails.
func ReadLonghornVolumes(master CommandRunner) ([]string, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		} `json:"items"`
	}
	if err := kubectlJSON(master, "Longhorn volumes", "kubectl -n longhorn-system get volumes.longhorn.io --request-timeout=30s -o json", &list); err != nil {
		return nil, err
	}
	var names []string
	for _, v := range list.Items {
		names = append(names, v.Metadata.Name)
	}
	return names, nil
}

// FailDataTransfer marks an unfinished DataTransfer Failed with reason as its
// last error to prevent the controller from restarting its mover pod. Completed,
// failed or deleted transfers are left alone. The patch is guarded by the
// resource version.
func FailDataTransfer(master CommandRunner, ns, name, reason string) error {
	out, err := master.Run(fmt.Sprintf("kubectl -n %s get datatransfers.kipper.run %s --ignore-not-found -o json", shellQuote(ns), shellQuote(name)))
	if err != nil {
		return fmt.Errorf("reading data transfer %s/%s: %w", ns, name, err)
	}
	out = kubectlPayload(out)
	if strings.TrimSpace(out) == "" {
		return nil
	}
	var dt struct {
		Metadata struct {
			ResourceVersion string `json:"resourceVersion"`
		} `json:"metadata"`
		Status struct {
			Phase string `json:"phase"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(out), &dt); err != nil {
		return fmt.Errorf("reading data transfer %s/%s: %w", ns, name, err)
	}
	if dt.Status.Phase == "Completed" || dt.Status.Phase == "Failed" {
		return nil
	}
	body, err := json.Marshal(map[string]any{
		"metadata": map[string]any{"resourceVersion": dt.Metadata.ResourceVersion},
		"status":   map[string]any{"phase": "Failed", "lastError": reason},
	})
	if err != nil {
		return err
	}
	if _, err := master.RunStdin(fmt.Sprintf("kubectl -n %s patch datatransfers.kipper.run %s --subresource=status --type merge --patch-file /dev/stdin >/dev/null", shellQuote(ns), shellQuote(name)), bytes.NewReader(body)); err != nil {
		return fmt.Errorf("marking data transfer %s/%s failed: %w", ns, name, err)
	}
	return nil
}

// SuspendJob sets a Job's suspend flag to prevent new pods until it is resumed.
// It returns an error if the Job is missing or the patch fails.
func SuspendJob(master CommandRunner, ns, name string) error {
	if _, err := master.RunStdin(fmt.Sprintf("kubectl -n %s patch job %s --type merge --patch-file /dev/stdin >/dev/null", shellQuote(ns), shellQuote(name)), strings.NewReader(`{"spec":{"suspend":true}}`)); err != nil {
		return fmt.Errorf("suspending Job %s/%s: %w", ns, name, err)
	}
	return nil
}

// OwnerExists reports whether the object an owner reference names still
// exists in ns. A missing object returns false with no error. Lookup failures
// return an error.
func OwnerExists(master CommandRunner, ns, apiVersion, kind, name string) (bool, error) {
	resource := strings.ToLower(kind)
	if group, version, ok := strings.Cut(apiVersion, "/"); ok {
		resource += "." + version + "." + group
	}
	out, err := master.Run(fmt.Sprintf("kubectl -n %s get %s %s --ignore-not-found -o name", shellQuote(ns), shellQuote(resource), shellQuote(name)))
	if err != nil {
		return false, fmt.Errorf("looking for %s %s/%s: %w", kind, ns, name, err)
	}
	return kubectlValue(out) != "", nil
}

// FencedPod is a Kipper pod with a Longhorn claim still bound to a fenced node.
type FencedPod struct {
	Namespace         string
	Name              string
	UID               string
	PersistentVolumes []string
}

// PodsWithVolumesOn lists the Kipper pods on node that mount a Longhorn
// volume, RWO or RWX.
func PodsWithVolumesOn(master CommandRunner, node string) ([]FencedPod, error) {
	var pods corev1.PodList
	if err := kubectlJSON(master, "the pods on "+node, fmt.Sprintf("kubectl get pods -A --field-selector spec.nodeName=%s -o json", shellQuote(node)), &pods); err != nil {
		return nil, err
	}
	var claims corev1.PersistentVolumeClaimList
	if err := kubectlJSON(master, "volume claims", "kubectl get pvc -A -o json", &claims); err != nil {
		return nil, err
	}
	var pvs corev1.PersistentVolumeList
	if err := kubectlJSON(master, "persistent volumes", "kubectl get pv -o json", &pvs); err != nil {
		return nil, err
	}
	uids := map[string]string{}
	for _, p := range pods.Items {
		uids[p.Namespace+"/"+p.Name] = string(p.UID)
	}
	var out []FencedPod
	index := map[string]int{}
	for _, s := range stranded.Find(node, pods.Items, claims.Items, pvs.Items) {
		key := s.Namespace + "/" + s.Pod
		i, ok := index[key]
		if !ok {
			i = len(out)
			index[key] = i
			out = append(out, FencedPod{Namespace: s.Namespace, Name: s.Pod, UID: uids[key]})
		}
		out[i].PersistentVolumes = appendUnique(out[i].PersistentVolumes, s.PersistentVolume)
	}
	return out, nil
}

// Attachment is a VolumeAttachment of a Longhorn volume to a node.
type Attachment struct {
	Name             string
	PersistentVolume string
}

// AttachmentsOn lists the Longhorn VolumeAttachments that name node.
func AttachmentsOn(master CommandRunner, node string) ([]Attachment, error) {
	var list storagev1.VolumeAttachmentList
	if err := kubectlJSON(master, "volume attachments", "kubectl get volumeattachments -o json", &list); err != nil {
		return nil, err
	}
	var out []Attachment
	for _, va := range list.Items {
		if va.Spec.NodeName != node || va.Spec.Attacher != stranded.LonghornDriver || va.Spec.Source.PersistentVolumeName == nil {
			continue
		}
		out = append(out, Attachment{Name: va.Name, PersistentVolume: *va.Spec.Source.PersistentVolumeName})
	}
	return out, nil
}

// DeleteAttachment asks Kubernetes to delete a VolumeAttachment and returns
// without waiting. Finalizers are left to their owners.
func DeleteAttachment(master CommandRunner, name string) error {
	if _, err := master.Run(fmt.Sprintf("kubectl delete volumeattachment %s --ignore-not-found --wait=false >/dev/null", shellQuote(name))); err != nil {
		return fmt.Errorf("deleting VolumeAttachment %s: %w", name, err)
	}
	return nil
}

// ForceDeletePod deletes the pod with the given UID with a grace period of 0.
// The UID is a precondition of the delete, so a pod that replaced it under the
// same name is never touched. It returns nil when the pod is already gone or
// replaced. Only the fence's guarded nudge calls it, on a NotReady node that
// carries the out-of-service taint.
func ForceDeletePod(master CommandRunner, ns, name, uid string) error {
	if uid == "" {
		return fmt.Errorf("deleting pod %s/%s: its UID is unknown", ns, name)
	}
	body, err := json.Marshal(map[string]any{
		"kind": "DeleteOptions", "apiVersion": "v1", "gracePeriodSeconds": 0,
		"preconditions": map[string]string{"uid": uid},
	})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s", ns, name)
	_, delErr := master.RunStdin(fmt.Sprintf("kubectl delete --raw %s -f - >/dev/null", shellQuote(path)), bytes.NewReader(body))
	if delErr == nil {
		return nil
	}
	// The raw delete fails on a missing pod and on a UID mismatch alike, so
	// the pod is read back to tell those apart from a real failure.
	out, err := master.Run(fmt.Sprintf("kubectl -n %s get pod %s --ignore-not-found -o jsonpath='{.metadata.uid}'", shellQuote(ns), shellQuote(name)))
	if err != nil {
		return fmt.Errorf("deleting pod %s/%s: %w", ns, name, delErr)
	}
	if kubectlValue(out) != uid {
		return nil
	}
	return fmt.Errorf("deleting pod %s/%s: %w", ns, name, delErr)
}

// PodPlacement reports the node a pod is bound to and whether it is Ready.
// A missing pod returns an empty node, false and no error. A deleting pod
// always reports false, regardless of its Ready condition.
func PodPlacement(master CommandRunner, ns, name string) (string, bool, error) {
	out, err := master.Run(fmt.Sprintf("kubectl -n %s get pod %s --ignore-not-found -o json", shellQuote(ns), shellQuote(name)))
	if err != nil {
		return "", false, fmt.Errorf("reading pod %s/%s: %w", ns, name, err)
	}
	out = kubectlPayload(out)
	if strings.TrimSpace(out) == "" {
		return "", false, nil
	}
	var pod corev1.Pod
	if err := json.Unmarshal([]byte(out), &pod); err != nil {
		return "", false, fmt.Errorf("reading pod %s/%s: %w", ns, name, err)
	}
	if pod.DeletionTimestamp != nil {
		return pod.Spec.NodeName, false, nil
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return pod.Spec.NodeName, c.Status == corev1.ConditionTrue, nil
		}
	}
	return pod.Spec.NodeName, false, nil
}

// ReadFenceStop returns the fence stop mark of a StatefulSet, or nil when it
// carries none.
func ReadFenceStop(master CommandRunner, ns, name string) (*FenceStopMark, error) {
	head, err := readStatefulSet(master, ns, name)
	if err != nil {
		return nil, err
	}
	raw, ok := head.Metadata.Annotations[FenceStopAnnotation]
	if !ok {
		return nil, nil
	}
	var mark FenceStopMark
	if err := json.Unmarshal([]byte(raw), &mark); err != nil {
		return nil, fmt.Errorf("reading the %s annotation of %s/%s: %w", FenceStopAnnotation, ns, name, err)
	}
	return &mark, nil
}

// JoinForChecks reads what ValidateWorker needs about the cluster without
// preparing the worker again, for a worker that rejoins after a fence.
func JoinForChecks(server CommandRunner) (WorkerJoin, error) {
	m, err := ReadJoinMaterial(server)
	if err != nil {
		return WorkerJoin{}, err
	}
	if m, err = m.checked(); err != nil {
		return WorkerJoin{}, err
	}
	return WorkerJoin{serverVersion: m.K3sVersion, zotIP: m.ZotIP, pullPassword: m.PullPassword}, nil
}

// StartAgent starts the worker's k3s agent. Remove the fence interlock first,
// or the start fails on its assert. The unit's failed state from starts the
// interlock refused is cleared first, so a start limit cannot block it.
func StartAgent(host CommandRunner) error {
	unit := RoleWorker.k3sUnit()
	if _, err := host.Run("systemctl reset-failed " + unit + " 2>/dev/null; systemctl start " + unit); err != nil {
		return fmt.Errorf("starting the k3s agent: %w", err)
	}
	return nil
}
