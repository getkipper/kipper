// Package stranded finds the Kipper workloads a dead node leaves behind and
// judges where a current copy of their data is. kip and console-api share it
// so the fence, kip status and the NotReady alert apply one rule.
package stranded

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

const (
	// TaintKey is Kubernetes' non-graceful node shutdown taint. PodGC and the
	// attach/detach controller check the key only.
	TaintKey = "node.kubernetes.io/out-of-service"
	// TaintValue is the value KEP-2268 uses for a node that was shut down.
	TaintValue = "nodeshutdown"
	// LonghornDriver is Longhorn's CSI driver name.
	LonghornDriver = "driver.longhorn.io"
	// FenceStopAnnotation marks a StatefulSet stopped by kip node fence.
	// Restarting it requires kip's service start, fence or rejoin checks.
	FenceStopAnnotation = "kipper.run/fence-stop"
	// nodeCheckLabel marks kip node add's disposable validation pods.
	nodeCheckLabel = "kipper.run/node-check"
)

// Taint returns the out-of-service taint kip node fence applies.
func Taint() corev1.Taint {
	return corev1.Taint{Key: TaintKey, Value: TaintValue, Effect: corev1.TaintEffectNoExecute}
}

// OutOfService reports whether node carries the out-of-service taint with any
// value or effect. A nil node returns false.
func OutOfService(node *corev1.Node) bool {
	if node == nil {
		return false
	}
	for _, t := range node.Spec.Taints {
		if t.Key == TaintKey {
			return true
		}
	}
	return false
}

// Ready reports whether a non-nil node has a True Ready condition and no
// out-of-service taint. A fenced host can report Ready after booting, so the
// taint takes precedence when counting available capacity.
func Ready(node *corev1.Node) bool {
	if node == nil || OutOfService(node) {
		return false
	}
	for _, c := range node.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Replica is the part of a Longhorn Replica CR the freshness rule reads. The
// timestamps are Longhorn's RFC 3339 strings, empty when unset.
type Replica struct {
	Name         string
	Volume       string
	Node         string
	HealthyAt    string
	FailedAt     string
	LastFailedAt string
}

// Current reports whether r's data is known to be current: it is not failed,
// it has been healthy, and it has not failed since it last finished a rebuild
// (healthyAt strictly later than lastFailedAt). Longhorn's auto-salvage clears
// failedAt without a rebuild, and lastFailedAt survives it, so a salvaged
// replica is not current. Missing or unparsable timestamps are not current.
func Current(r Replica) bool {
	if r.FailedAt != "" || r.HealthyAt == "" {
		return false
	}
	healthy, err := time.Parse(time.RFC3339, r.HealthyAt)
	if err != nil {
		return false
	}
	if r.LastFailedAt == "" {
		return true
	}
	failed, err := time.Parse(time.RFC3339, r.LastFailedAt)
	if err != nil {
		return false
	}
	return healthy.After(failed)
}

// Elsewhere returns volume's replicas on a node other than node, in input
// order. Replicas not yet placed on a node are left out.
func Elsewhere(volume, node string, replicas []Replica) []Replica {
	var out []Replica
	for _, r := range replicas {
		if r.Volume == volume && r.Node != "" && r.Node != node {
			out = append(out, r)
		}
	}
	return out
}

// CurrentCopies returns the sorted names of the nodes other than node that
// hold a current replica of volume.
func CurrentCopies(volume, node string, replicas []Replica) []string {
	seen := map[string]bool{}
	for _, r := range Elsewhere(volume, node, replicas) {
		if Current(r) {
			seen[r.Node] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

type replicaList struct {
	Items []struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			VolumeName   string `json:"volumeName"`
			NodeID       string `json:"nodeID"`
			HealthyAt    string `json:"healthyAt"`
			FailedAt     string `json:"failedAt"`
			LastFailedAt string `json:"lastFailedAt"`
		} `json:"spec"`
	} `json:"items"`
}

// ReplicasFromJSON reads a list of replicas.longhorn.io objects in kubectl or
// API JSON format. It returns an error for invalid JSON or incompatible field
// types. Missing fields retain their zero values.
func ReplicasFromJSON(data []byte) ([]Replica, error) {
	var list replicaList
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, fmt.Errorf("reading Longhorn replicas: %w", err)
	}
	out := make([]Replica, 0, len(list.Items))
	for _, it := range list.Items {
		out = append(out, Replica{Name: it.Metadata.Name, Volume: it.Spec.VolumeName, Node: it.Spec.NodeID,
			HealthyAt: it.Spec.HealthyAt, FailedAt: it.Spec.FailedAt, LastFailedAt: it.Spec.LastFailedAt})
	}
	return out, nil
}

// Service identifies a Longhorn claim mounted by a pod. Owner is empty
// when the pod has no StatefulSet owner. Volume is the CSI volume handle, which
// can differ from PersistentVolume for statically bound or imported volumes.
type Service struct {
	Namespace        string
	Pod              string
	Owner            string
	Claim            string
	PersistentVolume string
	Volume           string
	ReadWriteOnce    bool
}

// Label names the service by its StatefulSet, or by its pod when no
// StatefulSet owns it, followed by the namespace.
func (s Service) Label() string {
	name := s.Owner
	if name == "" {
		name = s.Pod
	}
	return fmt.Sprintf("%s (%s)", name, s.Namespace)
}

// Find returns the Kipper-managed pods bound to node that mount a claim backed
// by a Longhorn CSI volume, one entry per claim, sorted by namespace, pod and
// claim. Unmanaged pods, kip node add's validation pods, claims that are not
// bound and volumes of other drivers are left out. It returns nil when no claims match.
func Find(node string, pods []corev1.Pod, claims []corev1.PersistentVolumeClaim, volumes []corev1.PersistentVolume) []Service {
	return find(node, true, pods, claims, volumes)
}

// FindUnmanaged returns pods on node without the app.kubernetes.io/managed-by=kipper
// label, such as workloads installed with kubectl or Helm. Like Find, it returns
// one entry per claim backed by a Longhorn CSI volume, sorted by namespace, pod
// and claim. It excludes kip node add's validation pods and unbound claims.
// It returns nil when no claims match.
func FindUnmanaged(node string, pods []corev1.Pod, claims []corev1.PersistentVolumeClaim, volumes []corev1.PersistentVolume) []Service {
	return find(node, false, pods, claims, volumes)
}

func find(node string, managed bool, pods []corev1.Pod, claims []corev1.PersistentVolumeClaim, volumes []corev1.PersistentVolume) []Service {
	claimByKey := map[string]corev1.PersistentVolumeClaim{}
	for _, c := range claims {
		claimByKey[c.Namespace+"/"+c.Name] = c
	}
	pvByName := map[string]corev1.PersistentVolume{}
	for _, pv := range volumes {
		pvByName[pv.Name] = pv
	}
	var out []Service
	for _, p := range pods {
		if p.Spec.NodeName != node || (p.Labels[labels.ManagedBy] == labels.Kipper) != managed {
			continue
		}
		if _, check := p.Labels[nodeCheckLabel]; check {
			continue
		}
		owner := ""
		for _, ref := range p.OwnerReferences {
			if ref.Kind == "StatefulSet" {
				owner = ref.Name
			}
		}
		for _, v := range p.Spec.Volumes {
			if v.PersistentVolumeClaim == nil {
				continue
			}
			c, ok := claimByKey[p.Namespace+"/"+v.PersistentVolumeClaim.ClaimName]
			if !ok || c.Spec.VolumeName == "" {
				continue
			}
			pv, ok := pvByName[c.Spec.VolumeName]
			if !ok || pv.Spec.CSI == nil || pv.Spec.CSI.Driver != LonghornDriver {
				continue
			}
			rwo := false
			for _, m := range c.Spec.AccessModes {
				rwo = rwo || m == corev1.ReadWriteOnce || m == corev1.ReadWriteOncePod
			}
			out = append(out, Service{Namespace: p.Namespace, Pod: p.Name, Owner: owner, Claim: c.Name,
				PersistentVolume: pv.Name, Volume: pv.Spec.CSI.VolumeHandle, ReadWriteOnce: rwo})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		if a.Pod != b.Pod {
			return a.Pod < b.Pod
		}
		return a.Claim < b.Claim
	})
	return out
}

// CopiesOfAll returns the sorted names of the nodes other than node that hold
// a current replica of every one of volumes. No volumes means no nodes.
func CopiesOfAll(volumes []string, node string, replicas []Replica) []string {
	distinct := map[string]bool{}
	for _, v := range volumes {
		distinct[v] = true
	}
	if len(distinct) == 0 {
		return nil
	}
	count := map[string]int{}
	for v := range distinct {
		for _, n := range CurrentCopies(v, node, replicas) {
			count[n]++
		}
	}
	var out []string
	for n, c := range count {
		if c == len(distinct) {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}
