package controllers

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/longhorncap"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// copiesRebuildAnnotation records on a Longhorn volume when Kipper started a
// copy rebuild there, in RFC 3339. It is set by the patch that makes Longhorn
// build a replica and cleared once the copies are complete.
const copiesRebuildAnnotation = "kipper.run/copies-rebuild-started"

// lhReplica is what a conversion reads from a Longhorn Replica.
type lhReplica struct {
	Name              string
	UID               string
	ResourceVersion   string
	Node              string
	HealthyAt         string
	FailedAt          string
	LastFailedAt      string
	EvictionRequested bool
	Deleting          bool
}

// lhVolume is what a conversion reads from a Longhorn Volume, its replicas and
// its active engine. Modes contains the active engine's replicaModeMap and
// is nil when the map or active engine is absent.
type lhVolume struct {
	Name                   string
	ResourceVersion        string
	Count                  int64
	Selector               []string
	Locality               string
	AutoBalance            string
	OfflineRebuilding      string
	Size                   int64
	Actual                 int64
	State                  string
	Image                  string
	CurrentImage           string
	MigrationNodeID        string
	CurrentMigrationNodeID string
	RebuildStarted         time.Time
	Replicas               []lhReplica
	Modes                  map[string]string
}

// lhCluster is the cluster state a conversion reads besides the volume.
// Servers (tagged kipper-server), Eligible (tagged kipper-replica) and
// Available (Kubernetes node Ready and not fenced) are keyed by node name.
// Disks are the disks taking new replicas, ServerReserve is held back on the
// server's disks for dump claims, and OfflineRebuilding says whether Longhorn
// has per-volume offline rebuilding (1.9 and later). Capacity checks pass
// until CapacityRead is set. Steps that build a copy are planned again after
// reading Disks and ServerReserve.
type lhCluster struct {
	Servers           map[string]bool
	Eligible          map[string]bool
	Available         map[string]bool
	Disks             []longhorncap.Disk
	ServerReserve     int64
	OfflineRebuilding bool
	CapacityRead      bool

	nodes []unstructured.Unstructured
}

// stepKind is what a conversion does in one reconcile.
type stepKind int

const (
	stepDone stepKind = iota
	stepWait
	stepBlocked
	stepPatch
	stepDelete
)

// leaseUse is what a step does with the conversion lease.
type leaseUse int

const (
	leaseRelease leaseUse = iota
	leaseTake
	leaseHold
)

// conversionStep is one reconcile's action. Reason and Message describe it
// for the StorageCopies condition. An empty Reason lets the caller report
// the live copies.
type conversionStep struct {
	Kind    stepKind
	Patch   *volumePatch
	Replica *lhReplica
	Keep    []string
	Lease   leaseUse
	Reason  string
	Message string
}

// volumePatchSpec is the only shape of spec patch a conversion sends. Node
// selector, data locality and auto-balance are always carried, so no patch can
// change the count without also fixing the two settings through which
// Longhorn deletes replicas on its own.
type volumePatchSpec struct {
	NodeSelector       []string `json:"nodeSelector"`
	DataLocality       string   `json:"dataLocality"`
	ReplicaAutoBalance string   `json:"replicaAutoBalance"`
	NumberOfReplicas   *int64   `json:"numberOfReplicas,omitempty"`
	OfflineRebuilding  string   `json:"offlineRebuilding,omitempty"`
}

type volumePatchMeta struct {
	Annotations     map[string]*string `json:"annotations,omitempty"`
	ResourceVersion string             `json:"resourceVersion,omitempty"`
}

// volumePatch is a JSON merge patch for a Longhorn Volume.
type volumePatch struct {
	Metadata *volumePatchMeta `json:"metadata,omitempty"`
	Spec     *volumePatchSpec `json:"spec,omitempty"`
}

// rebuildBudget is how long Kipper waits for a copy it asked Longhorn to build:
// 30 minutes plus one minute per whole GiB of the volume's nominal size.
func rebuildBudget(size int64) time.Duration {
	return 30*time.Minute + time.Duration(size>>30)*time.Minute
}

// targetSelector is the replica node selector for a copies target.
func targetSelector(target int64) []string {
	if target == 1 {
		return []string{platform.ServerStorageTag}
	}
	return []string{platform.ReplicaStorageTag}
}

func sameSelector(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// replicaState classifies one replica for a conversion.
type replicaState struct {
	lhReplica
	present  bool
	building bool
	salvaged bool
	current  bool
	usable   bool
	onServer bool
	eligible bool
}

func classify(v *lhVolume, c *lhCluster) []replicaState {
	attached := v.State == "attached"
	out := make([]replicaState, 0, len(v.Replicas))
	for _, r := range v.Replicas {
		s := replicaState{lhReplica: r, onServer: c.Servers[r.Node], eligible: c.Eligible[r.Node]}
		s.present = r.Node != "" && r.FailedAt == "" && !r.Deleting
		s.building = s.present && (!platform.ReplicaCurrent(r.HealthyAt, r.FailedAt, r.LastFailedAt) || v.Modes[r.Name] == "WO")
		// Auto-salvage clears failedAt and preserves the pre-failure healthyAt,
		// which Longhorn sets only when empty. ReplicaCurrent therefore cannot
		// confirm that the recovered copy is current.
		s.salvaged = s.building && r.HealthyAt != "" && r.LastFailedAt != "" && v.Modes[r.Name] != "WO"
		s.current = s.present && !s.building && c.Available[r.Node]
		s.usable = s.current && !r.EvictionRequested && (!attached || v.Modes[r.Name] == "RW")
		out = append(out, s)
	}
	return out
}

// planConversion decides the one action that moves the Longhorn volume v
// towards target copies. It waits for rebuilds within their time budget,
// ensures a usable server copy before removing replicas, lowers the count
// before deleting surplus replicas one at a time, then settles placement.
// Server replicas are never selected for deletion.
func planConversion(target int64, v *lhVolume, c *lhCluster, now time.Time) conversionStep {
	reps := classify(v, c)
	var present, current, presentOffServer, salvagedCount int64
	building, rebuilding, evicting := false, false, false
	var serverCopy, serverPresent, salvaged *replicaState
	for i := range reps {
		r := &reps[i]
		if r.present {
			present++
			if !r.onServer {
				presentOffServer++
			} else {
				serverPresent = r
			}
			evicting = evicting || r.EvictionRequested
		}
		building = building || r.building
		rebuilding = rebuilding || (r.building && !r.salvaged)
		if r.salvaged {
			salvagedCount++
			if salvaged == nil {
				salvaged = r
			}
		}
		if r.current {
			current++
		}
		if r.usable && r.onServer && serverCopy == nil {
			serverCopy = r
		}
	}
	attached := v.State == "attached"
	annotated := !v.RebuildStarted.IsZero()
	budget := rebuildBudget(v.Size)

	// A rebuild Kipper asked for is outstanding until every counted copy is
	// current, including while Longhorn has not scheduled the new replica yet.
	// Waiting writes nothing, so it comes before the migration and engine
	// checks and keeps the lease through them.
	if building || (annotated && current < v.Count) {
		changing := annotated || target != v.Count || present > target
		switch {
		case salvaged != nil && changing && !rebuilding && (!annotated || current+salvagedCount >= v.Count):
			// All requested copies exist and only salvaged copies remain uncertain,
			// so blocking can release the rebuild lease.
			return blocked(fmt.Sprintf("The copy on %s came back after a failure without being rebuilt, so Kipper cannot tell whether it is current and keeps every copy. Setting the copies back stops the change.", salvaged.Node))
		case !annotated:
			// Longhorn's own replenishment after a failure: report it as it is.
			return conversionStep{Kind: stepWait}
		case now.Before(v.RebuildStarted.Add(budget)):
			return conversionStep{Kind: stepWait, Lease: leaseHold, Reason: kipperv1.StorageCopiesConverting,
				Message: fmt.Sprintf("Longhorn is building a copy of the volume (%d of %d copies healthy).", current, v.Count)}
		case target >= v.Count && present <= target:
			return blocked(fmt.Sprintf("Longhorn has not finished building the copy within %s. It keeps trying. Setting the copies back stops it.", budget))
		}
		// A reduction goes on past a rebuild that ran out of time.
	}

	switch {
	case v.MigrationNodeID != "" || v.CurrentMigrationNodeID != "":
		return blocked("Longhorn is moving this volume to another node. The copy count change continues once the move finishes.")
	case v.Image != "" && v.CurrentImage != "" && v.Image != v.CurrentImage:
		return blocked("Longhorn is upgrading this volume's engine. The copy count change continues once the upgrade finishes.")
	}

	needsServer := target == 1 || present > target
	if needsServer && serverCopy == nil {
		switch {
		case !attached:
			return blocked(detachedDown)
		case serverPresent != nil:
			return blocked("The server's copy of this volume is not healthy or is being evicted, so Kipper keeps every copy. The change continues once it is healthy.")
		case sameSelector(v.Selector, targetSelector(1)) && v.Count > presentOffServer:
			return blocked("Longhorn cannot place a copy of this volume on the server, so Kipper keeps every copy. Check that the server's Longhorn node allows scheduling.")
		case !serverTakes(c, v):
			return blocked(fmt.Sprintf("The server has no room for a copy of this volume (%s), so Kipper keeps every copy. Free space on the server, or set the copies back.", formatSize(v.Size)))
		}
		if step, lowering := lowerFirst(v, present+1, evicting); lowering {
			return step
		}
		return conversionStep{Kind: stepPatch, Lease: leaseTake, Reason: kipperv1.StorageCopiesConverting,
			Message: "Building a copy on the server before removing the extra copies.",
			Patch:   rebuildPatch(targetSelector(1), present+1, false, now)}
	}

	if present > target {
		switch {
		case !attached:
			return blocked(detachedDown)
		case evicting:
			return blocked(evictingCopy)
		case v.Locality != "disabled" || v.AutoBalance != "disabled":
			return patchStep(trimPatch(v, nil), "Turning off data locality and auto-balance before removing copies.")
		case v.Count > target:
			return patchStep(trimPatch(v, &target), fmt.Sprintf("Lowering the copy count to %d. The extra copies are removed one at a time.", target))
		}
		keep := keepSet(reps, serverCopy, target)
		if int64(len(keep)) < target {
			healthy := "No copy"
			if len(keep) == 1 {
				healthy = "Only one copy"
			}
			return blocked(healthy + " among those selected to remain is healthy, so Kipper keeps every copy of this volume. The change continues once another copy is healthy.")
		}
		victim := surplus(reps, keep)
		if victim == nil {
			return blocked("Kipper found no extra copy it may remove.")
		}
		names := make([]string, 0, len(keep))
		for _, k := range keep {
			names = append(names, k.Name)
		}
		r := victim.lhReplica
		return conversionStep{Kind: stepDelete, Replica: &r, Keep: names, Reason: kipperv1.StorageCopiesConverting,
			Message: fmt.Sprintf("Removing an extra copy (%d left, %d wanted).", present-1, target)}
	}

	spec := specMatches(v, target, c)
	if present < target && (v.Count < target || !spec) {
		if !attached && !c.OfflineRebuilding {
			return blocked("The volume is not attached. This Longhorn version builds copies only while the volume is attached, so the change waits for attachment.")
		}
		if have := nodesWithRoom(reps, c, v, target); have < target {
			who := "no node has enough room"
			if have == 1 {
				who = "only one node has enough room"
			}
			return blocked(fmt.Sprintf("%d copies need %d nodes that can hold copies with room for this volume (%s), and %s. Free space, or add a node with 'kip node add'.",
				target, target, formatSize(v.Size), who))
		}
		if step, lowering := lowerFirst(v, target, evicting); lowering {
			return step
		}
		return conversionStep{Kind: stepPatch, Lease: leaseTake, Reason: kipperv1.StorageCopiesConverting,
			Message: fmt.Sprintf("Building a copy: Longhorn copies the volume (%s) to another node.", formatSize(v.Size)),
			Patch:   rebuildPatch(targetSelector(target), target, target == 2 && c.OfflineRebuilding, now)}
	}

	if !spec || v.Count != target {
		count := &target
		if v.Count > target {
			switch {
			case !attached:
				return blocked(detachedDown)
			case evicting:
				return blocked(evictingCopy)
			}
		}
		if v.Count == target {
			// Placement alone changes no copy, so the live copies are reported.
			return conversionStep{Kind: stepPatch, Patch: specPatch(v, target, nil, c)}
		}
		return patchStep(specPatch(v, target, count, c), fmt.Sprintf("Setting the copy count to %d.", target))
	}

	if annotated {
		return conversionStep{Kind: stepPatch, Patch: &volumePatch{Metadata: &volumePatchMeta{
			Annotations: map[string]*string{copiesRebuildAnnotation: nil}}}}
	}
	return conversionStep{Kind: stepDone}
}

const (
	detachedDown = "The volume is not attached. Kipper removes copies only while the volume is attached, so the change waits for attachment."
	evictingCopy = "A copy of this volume is being evicted from its node. Kipper removes copies once the eviction ends."
)

func blocked(message string) conversionStep {
	return conversionStep{Kind: stepBlocked, Reason: kipperv1.StorageCopiesBlocked, Message: message}
}

func patchStep(p *volumePatch, message string) conversionStep {
	return conversionStep{Kind: stepPatch, Patch: p, Reason: kipperv1.StorageCopiesConverting, Message: message}
}

// specMatches checks the target's placement settings, including offline
// rebuilding for two copies when supported. The replica count is checked separately.
func specMatches(v *lhVolume, target int64, c *lhCluster) bool {
	return sameSelector(v.Selector, targetSelector(target)) && v.Locality == "disabled" && v.AutoBalance == "disabled" &&
		(target != 2 || !c.OfflineRebuilding || v.OfflineRebuilding == "enabled")
}

// lowerFirst is the step that brings a spec count above count down to it
// before a rebuild patch asks for count replicas, so a rebuild patch never
// lowers the count. The decrease takes the same guards as any reduction.
func lowerFirst(v *lhVolume, count int64, evicting bool) (conversionStep, bool) {
	switch {
	case v.Count <= count:
		return conversionStep{}, false
	case v.State != "attached":
		return blocked(detachedDown), true
	case evicting:
		return blocked(evictingCopy), true
	case v.Locality != "disabled" || v.AutoBalance != "disabled":
		return patchStep(trimPatch(v, nil), "Turning off data locality and auto-balance before changing the copy count."), true
	}
	return patchStep(trimPatch(v, &count), fmt.Sprintf("Setting the copy count to %d before building a copy.", count)), true
}

// trimPatch turns off data locality and auto-balance and, when count is given
// and both are already off on the live volume, sets the count. It keeps the
// live node selector, so a server-only selector set to build a server copy
// stays until the surplus is gone.
func trimPatch(v *lhVolume, count *int64) *volumePatch {
	if count != nil && (v.Locality != "disabled" || v.AutoBalance != "disabled") {
		count = nil
	}
	selector := append([]string{}, v.Selector...)
	return &volumePatch{Spec: &volumePatchSpec{NodeSelector: selector, DataLocality: "disabled", ReplicaAutoBalance: "disabled", NumberOfReplicas: count}}
}

// specPatch sets the target's placement settings and, when count is given,
// the replica count. A count below the volume's is dropped while the live
// volume still has data locality or auto-balance on, because Longhorn would
// then choose a replica to delete. The patch turns both off first.
func specPatch(v *lhVolume, target int64, count *int64, c *lhCluster) *volumePatch {
	if count != nil && *count < v.Count && (v.Locality != "disabled" || v.AutoBalance != "disabled") {
		count = nil
	}
	spec := &volumePatchSpec{NodeSelector: targetSelector(target), DataLocality: "disabled", ReplicaAutoBalance: "disabled", NumberOfReplicas: count}
	if target == 2 && c.OfflineRebuilding {
		spec.OfflineRebuilding = "enabled"
	}
	return &volumePatch{Spec: spec}
}

// rebuildPatch asks Longhorn for count replicas under selector and records
// when Kipper asked.
func rebuildPatch(selector []string, count int64, offline bool, now time.Time) *volumePatch {
	started := now.UTC().Format(time.RFC3339)
	spec := &volumePatchSpec{NodeSelector: selector, DataLocality: "disabled", ReplicaAutoBalance: "disabled", NumberOfReplicas: &count}
	if offline {
		spec.OfflineRebuilding = "enabled"
	}
	return &volumePatch{Metadata: &volumePatchMeta{Annotations: map[string]*string{copiesRebuildAnnotation: &started}}, Spec: spec}
}

// keepSet is the copies a reduction keeps: the server copy and, for two
// copies, a usable replica on another node. It prefers nodes tagged to hold
// copies, then chooses by replica name.
func keepSet(reps []replicaState, serverCopy *replicaState, target int64) []replicaState {
	if serverCopy == nil {
		return nil
	}
	keep := []replicaState{*serverCopy}
	if target < 2 {
		return keep
	}
	var others []replicaState
	for _, r := range reps {
		if r.usable && !r.onServer {
			others = append(others, r)
		}
	}
	slices.SortStableFunc(others, func(a, b replicaState) int {
		switch {
		case a.eligible != b.eligible && a.eligible:
			return -1
		case a.eligible != b.eligible:
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	if len(others) > 0 {
		keep = append(keep, others[0])
	}
	return keep
}

// surplus picks the replica a reduction deletes: a present replica outside
// keep and never on a server node, preferring one that is not usable, then one
// on a node not tagged to hold copies, then by name. Nil when there is none.
func surplus(reps []replicaState, keep []replicaState) *replicaState {
	var candidates []replicaState
	for _, r := range reps {
		kept := slices.ContainsFunc(keep, func(k replicaState) bool { return k.Name == r.Name })
		if r.present && !r.onServer && !kept {
			candidates = append(candidates, r)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	rank := func(r replicaState) int {
		n := 0
		if r.usable {
			n += 2
		}
		if r.eligible {
			n++
		}
		return n
	}
	slices.SortStableFunc(candidates, func(a, b replicaState) int {
		if d := rank(a) - rank(b); d != 0 {
			return d
		}
		return strings.Compare(a.Name, b.Name)
	})
	return &candidates[0]
}

// serverTakes reports whether a server disk takes a new replica of v, holding
// back the dump reserve from its physical space.
func serverTakes(c *lhCluster, v *lhVolume) bool {
	if !c.CapacityRead {
		return true
	}
	for _, d := range c.Disks {
		if c.Servers[d.Node] && longhorncap.HasAllTags(d.Tags, targetSelector(1)) && d.Takes(v.Size, v.Actual+c.ServerReserve) {
			return true
		}
	}
	return false
}

// nodesWithRoom counts the nodes that hold a current copy of v or have a disk
// under the target's selector that takes a new one.
func nodesWithRoom(reps []replicaState, c *lhCluster, v *lhVolume, target int64) int64 {
	if !c.CapacityRead {
		return target
	}
	nodes := map[string]bool{}
	for _, r := range reps {
		if r.current {
			nodes[r.Node] = true
		}
	}
	for _, d := range c.Disks {
		actual := v.Actual
		if c.Servers[d.Node] {
			actual += c.ServerReserve
		}
		if longhorncap.HasAllTags(d.Tags, targetSelector(target)) && d.Takes(v.Size, actual) {
			nodes[d.Node] = true
		}
	}
	return int64(len(nodes))
}

// formatSize renders bytes in GiB, or MiB below one GiB.
func formatSize(bytes int64) string {
	if bytes >= 1<<30 {
		return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
	}
	return fmt.Sprintf("%d MiB", bytes>>20)
}
