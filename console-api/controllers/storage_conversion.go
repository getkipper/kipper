package controllers

import (
	"fmt"
	"maps"
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
// CounterDisabled is the replica's spec.revisionCounterDisabled. HomeEvicted
// reports that the Longhorn node or disk holding the replica asks for
// eviction, which Longhorn copies to the replica's EvictionRequested later.
type lhReplica struct {
	Name              string
	UID               string
	ResourceVersion   string
	Node              string
	DiskID            string
	HealthyAt         string
	FailedAt          string
	LastFailedAt      string
	LastHealthyAt     string
	CurrentState      string
	EvictionRequested bool
	HomeEvicted       bool
	Deleting          bool
	CounterDisabled   bool
}

// present reports a replica that is scheduled, not failed and not being
// deleted.
func (r lhReplica) present() bool {
	return r.Node != "" && r.FailedAt == "" && !r.Deleting
}

// counted reports a present replica that Longhorn counts against the volume's
// copy count, which leaves out a copy it is evicting, as its replenishment does.
func (r lhReplica) counted() bool {
	return r.present() && !r.EvictionRequested
}

// lhEngineRef is what a conversion reads from one of a volume's Longhorn Engines.
type lhEngineRef struct {
	Name            string
	ResourceVersion string
	CounterDisabled bool
}

// lhAttachment is what a conversion reads from a volume's Longhorn
// VolumeAttachment. TicketNode is the node of Kipper's attachment ticket, or
// empty without one. TicketSatisfied reports that Longhorn satisfied the
// ticket's current generation.
type lhAttachment struct {
	ResourceVersion string
	TicketNode      string
	TicketSatisfied bool
}

// lhVolume is what a conversion reads from a Longhorn Volume, its replicas,
// engines and attachment. Modes contains the active engine's replicaModeMap
// and is nil when the map or active engine is absent or the status fails the
// checks in storageConverter.read. EngineUntrusted gives the reason for a
// failed check on a non-detached volume, including a missing active engine
// on an attached volume. EngineNode and EngineOwner are the engine's node and
// the node of the manager writing its status, set only when the status is
// trusted. AttachedNode is the node Longhorn attached the volume to.
// Addresses is the engine's currentReplicaAddressMap. CounterOn reports that
// the volume, every engine and every replica not being deleted have Longhorn's
// revision counter on, and CounterOff that none has. Held reports that the
// volume runs on the node of Kipper's attachment ticket with its frontend
// disabled, an engine trusted there and the counter on, so nothing can write
// to it and its engine compared the copies' revision counters when it started.
// Records contains the claim's conversion records. Seeded indicates a backup,
// clone or backing-image source, as in platform.CopyVolume.
type lhVolume struct {
	Name                   string
	Seeded                 bool
	UID                    string
	ResourceVersion        string
	Robustness             string
	EngineFound            bool
	EngineUntrusted        string
	EngineNode             string
	EngineOwner            string
	AttachedNode           string
	Addresses              map[string]string
	Records                claimRecords
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
	Engines                []lhEngineRef
	Modes                  map[string]string
	CounterDisabled        bool
	CounterOn              bool
	CounterOff             bool
	FrontendDisabled       bool
	Attachment             *lhAttachment
	Held                   bool
}

// claimRecords are the conversion records on a volume's claim. Stop is the
// claim's copy-change stop, or nil without one.
type claimRecords struct {
	Stop *platform.CopyChangeStop
}

// lhCluster is the cluster state a conversion reads besides the volume.
// Servers (tagged kipper-server), Eligible (tagged kipper-replica) and
// Available (Kubernetes node Ready and not fenced) are keyed by node name.
// Available holds the nodes of the volume's replicas and of its attachment
// ticket. Disks are the disks taking new replicas, ServerReserve is held back
// on the server's disks for dump claims, and OfflineRebuilding says whether
// Longhorn has per-volume offline rebuilding (1.9 and later). Capacity checks
// pass until CapacityRead is set. Steps that build a copy are planned again
// after reading Disks and ServerReserve.
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

// stepKind is what a conversion does in one reconcile. stepHold asks for the
// volume to be held, as lhVolume.Held describes, before the reduction goes on.
type stepKind int

const (
	stepDone stepKind = iota
	stepWait
	stepBlocked
	stepPatch
	stepHold
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
// the live copies. Hold marks a step of a copy reduction, which runs only
// while the volume is held. NeedsAttach marks a block that only an attached
// volume ends.
type conversionStep struct {
	Kind        stepKind
	Patch       *volumePatch
	Lease       leaseUse
	Reason      string
	Message     string
	Hold        bool
	NeedsAttach bool
}

// volumePatchSpec is the only shape of spec patch a conversion sends. Node
// selector, data locality and auto-balance are always carried, so no patch can
// change the count without also setting the two settings through which
// Longhorn removes healthy copies on its own.
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

// replicaState classifies one replica for a conversion. inSync means the
// replica runs on an available node and is RW in the active engine.
type replicaState struct {
	lhReplica
	healthy  bool
	present  bool
	building bool
	inSync   bool
	current  bool
	usable   bool
	onServer bool
	eligible bool
}

// classify evaluates v's replicas in their existing order for a conversion.
func classify(v *lhVolume, c *lhCluster) []replicaState {
	markers := make([]platform.CopyMarkers, len(v.Replicas))
	for i, r := range v.Replicas {
		markers[i] = platform.CopyMarkers{HealthyAt: r.HealthyAt, FailedAt: r.FailedAt, LastFailedAt: r.LastFailedAt, LastHealthyAt: r.LastHealthyAt}
	}
	healthy := platform.HealthyCopies(platform.CopyVolume{State: v.State, Seeded: v.Seeded}, markers)
	out := make([]replicaState, 0, len(v.Replicas))
	for i, r := range v.Replicas {
		s := replicaState{lhReplica: r, healthy: healthy[i], onServer: c.Servers[r.Node], eligible: c.Eligible[r.Node]}
		s.present = r.present()
		s.inSync = s.present && r.CurrentState == "running" && c.Available[r.Node] && v.Modes[r.Name] == "RW"
		s.building = s.present && (!healthy[i] || v.Modes[r.Name] == "WO")
		s.current = s.present && !s.building && c.Available[r.Node]
		s.usable = s.current && !r.EvictionRequested && s.inSync
		out = append(out, s)
	}
	return out
}

// planConversion decides the one action that moves the Longhorn volume v
// toward target copies. It waits for rebuilds within their time budget.
// A reduction, which removes a copy or lowers the count, asks for the volume
// to be held first. Held, it ensures a server copy, waits until every copy is
// RW and then lowers the count with data locality on, so Longhorn removes the
// extra copies away from the held engine's node. Kipper deletes no replica.
// Builds and placement changes run live.
func planConversion(target int64, v *lhVolume, c *lhCluster, now time.Time) conversionStep {
	reps := classify(v, c)
	var present, current, presentOffServer int64
	building, evictingNode := false, ""
	var serverCopy, serverPresent *replicaState
	for i := range reps {
		r := &reps[i]
		if r.present {
			present++
			if !r.onServer {
				presentOffServer++
			} else if serverPresent == nil {
				serverPresent = r
			}
			if (r.EvictionRequested || r.HomeEvicted) && evictingNode == "" {
				evictingNode = r.Node
			}
		}
		building = building || r.building
		if r.current {
			current++
		}
		if r.usable && r.onServer && serverCopy == nil {
			serverCopy = r
		}
	}
	annotated := !v.RebuildStarted.IsZero()
	budget := rebuildBudget(v.Size)
	needsServer := present > target || (target == 1 && serverPresent == nil)
	reduces := present > 0 && (needsServer || v.Count > target || lowered(v, target))

	// A rebuild Kipper asked for is outstanding until every counted copy is
	// current, including while Longhorn has not scheduled the new replica yet.
	if building || (annotated && current < v.Count) {
		switch {
		case !annotated && !reduces:
			// Longhorn's own replenishment after a failure: report it as it is.
			return conversionStep{Kind: stepWait}
		case annotated && (!reduces || needsServer) && now.Before(v.RebuildStarted.Add(budget)):
			reason := kipperv1.StorageCopiesConverting
			if v.Held {
				reason = kipperv1.StorageCopiesConversionPending
			}
			return conversionStep{Kind: stepWait, Lease: leaseHold, Reason: reason, Hold: reduces,
				Message: fmt.Sprintf("Longhorn is building a copy of the volume (%d of %d copies healthy).", healthyNodes(reps, c), v.Count)}
		case annotated && !reduces:
			return blocked(fmt.Sprintf("Longhorn has not finished building the copy within %s. It keeps trying. Setting the copies back stops it.", budget))
		}
		// A reduction goes on, since the held engine rebuilds what it needs.
		// One that needs no further copy, as when a reduction is set back
		// before its server copy is placed, does not wait for the build.
	}

	var busy string
	switch {
	case v.MigrationNodeID != "" || v.CurrentMigrationNodeID != "":
		busy = "Longhorn is moving this volume to another node. The copy count change continues once the move finishes."
	case v.Image != "" && v.CurrentImage != "" && v.Image != v.CurrentImage:
		busy = "Longhorn is upgrading this volume's engine. The copy count change continues once the upgrade finishes."
	}
	if busy != "" {
		// Once the count is lowered, the volume stays held through the wait
		// like every other wait for Longhorn.
		if reduces && lowered(v, target) {
			step := pendingStep(busy)
			step.Hold = true
			return step
		}
		return blocked(busy)
	}

	if present == 0 && v.Count > target {
		return blocked("No copy of this volume is present, so Kipper keeps every copy and leaves the copy count unchanged. The change continues once Longhorn has a healthy copy.")
	}
	if reduces {
		return planReduction(target, v, c, now, reps, reductionCounts{present: present, presentOffServer: presentOffServer,
			needsServer: needsServer, evictingNode: evictingNode, serverCopy: serverCopy, serverPresent: serverPresent})
	}

	spec := specMatches(v, target, c)
	if present < target && (v.Count < target || !spec) {
		// The counter switch a build needs first runs only while detached,
		// so it goes ahead and the build follows once the volume is attached.
		if v.State != "attached" && !c.OfflineRebuilding && v.CounterOn {
			step := blocked("The volume is not attached. This Longhorn version builds copies only while the volume is attached, so the change waits for attachment.")
			step.NeedsAttach = true
			return step
		}
		if have := nodesWithRoom(reps, c, v, target); have < target {
			who := "no node has enough room"
			if have == 1 {
				who = "only one node has enough room"
			}
			return blocked(fmt.Sprintf("%d copies need %d nodes that can hold copies with room for this volume (%s), and %s. Free space, or add a node with 'kip node add'.",
				target, target, formatSize(v.Size), who))
		}
		return conversionStep{Kind: stepPatch, Lease: leaseTake, Reason: kipperv1.StorageCopiesConverting,
			Message: fmt.Sprintf("Building a copy: Longhorn copies the volume (%s) to another node.", formatSize(v.Size)),
			Patch:   rebuildPatch(targetSelector(target), target, target == 2 && c.OfflineRebuilding, now)}
	}

	if !spec || v.Count != target {
		if v.Count == target {
			// Placement alone changes no copy, so the live copies are reported.
			return conversionStep{Kind: stepPatch, Patch: specPatch(target, nil, c)}
		}
		return patchStep(specPatch(target, &target, c), fmt.Sprintf("Setting the copy count to %d.", target))
	}

	if annotated {
		return conversionStep{Kind: stepPatch, Patch: &volumePatch{Metadata: &volumePatchMeta{
			Annotations: map[string]*string{copiesRebuildAnnotation: nil}}}}
	}
	return conversionStep{Kind: stepDone}
}

// reductionCounts is what planConversion learned about the copies of a volume
// it reduces.
type reductionCounts struct {
	present, presentOffServer int64
	needsServer               bool
	evictingNode              string
	serverCopy, serverPresent *replicaState
}

// planReduction plans a step of a reduction to target copies. An unheld
// volume gets stepHold unless the server cannot take the copy the reduction
// keeps. A held volume needs every copy's node available, no eviction and a
// usable server copy, which it builds if missing. Once every copy is RW in the
// held engine, one patch lowers the count with data locality on, and the
// reduction waits for Longhorn to remove the extra copies, then turns data
// locality off again. Every step is marked Hold.
func planReduction(target int64, v *lhVolume, c *lhCluster, now time.Time, reps []replicaState, n reductionCounts) conversionStep {
	step := planReductionStep(target, v, c, now, reps, n)
	step.Hold = true
	return step
}

func planReductionStep(target int64, v *lhVolume, c *lhCluster, now time.Time, reps []replicaState, n reductionCounts) conversionStep {
	var inSync int64
	for _, r := range reps {
		if r.inSync {
			inSync++
		}
	}
	if lowered(v, target) {
		// While healthy copies exceed the count, Longhorn removes one per pass:
		// a copy it is evicting first, otherwise one away from the engine's
		// node while a copy is on that node, and never its last healthy copy.
		// Kipper reports what it sees and leaves the outcome to Longhorn.
		switch {
		case n.present > target:
			extra := fmt.Sprintf("the volume has %d and keeps %d", n.present, target)
			if !slices.ContainsFunc(v.Replicas, func(r lhReplica) bool { return r.Node == v.EngineNode }) {
				return pendingStep(fmt.Sprintf("Waiting for Longhorn to remove the extra copies: %s. Longhorn's data locality cleanup waits for a copy on the server that holds the volume, and the volume has none.", extra))
			}
			stuck, where := longhornKeepsAll(v)
			if len(stuck) == 0 {
				return pendingStep(fmt.Sprintf("Longhorn is removing the extra copies: %s.", extra))
			}
			names := make([]string, len(stuck))
			for i, r := range stuck {
				names[i] = r.Name
			}
			why := fmt.Sprintf("Longhorn picks the copy to remove among %s, which %s, and it keeps the copies there", joinNames(names), where)
			if i := slices.IndexFunc(stuck, func(r lhReplica) bool { return r.FailedAt != "" && !r.Deleting }); i >= 0 {
				return pendingStep(fmt.Sprintf("Waiting for Longhorn to remove the extra copies: %s. %s, so it removes the extra copy only after the failed copy %s is gone, once the volume's stale replica timeout has passed.",
					extra, why, stuck[i].Name))
			}
			return pendingStep(fmt.Sprintf("Waiting for Longhorn to remove the extra copies: %s. %s until it can remove one of them.", extra, why))
		case inSync < n.present:
			return pendingStep(fmt.Sprintf("Longhorn removed the extra copies and is bringing the rest up to date (%d of %d copies in sync).", inSync, n.present))
		}
		step := pendingStep("Longhorn removed the extra copies, so Kipper turns data locality off again.")
		step.Kind, step.Patch = stepPatch, trimPatch(v, nil)
		return step
	}
	buildServer := n.needsServer && n.serverPresent == nil
	if buildServer {
		switch {
		case sameSelector(v.Selector, targetSelector(1)) && v.Count > n.presentOffServer:
			return blocked("Longhorn cannot place a copy of this volume on the server, so Kipper keeps every copy. Check that the server's Longhorn node allows scheduling.")
		case !serverTakes(c, v):
			if refusal := serverRefusal(c); refusal != "" {
				return blocked(fmt.Sprintf("Longhorn takes no new copy on the server now, because %s, so Kipper keeps every copy. The change continues once the server takes copies again.", refusal))
			}
			return blocked(fmt.Sprintf("The server has no room for a copy of this volume (%s), so Kipper keeps every copy. Free space on the server.", formatSize(v.Size)))
		}
	}
	if node := unavailableCopyNode(v.Replicas, c); node != "" {
		return blocked(copyNodeDown(node))
	}
	if n.evictingNode != "" {
		return blocked(evictingCopy(n.evictingNode))
	}
	if !v.Held {
		return conversionStep{Kind: stepHold}
	}
	if buildServer {
		step := pendingStep("Building a copy on the server before Longhorn removes the extra copies.")
		step.Kind, step.Lease, step.Patch = stepPatch, leaseTake, rebuildPatch(targetSelector(1), max(v.Count, n.present+1), false, now)
		return step
	}
	if inSync < n.present || (n.needsServer && n.serverCopy == nil) {
		// The engine compared the copies' revision counters when it started on
		// the server and marks a copy that lags behind ERR, which Longhorn
		// then rebuilds. The count is lowered only once every copy serves writes.
		return pendingStep(fmt.Sprintf("Longhorn is bringing every copy up to date before it removes the extra copies (%d of %d copies in sync).", inSync, n.present))
	}
	step := pendingStep(fmt.Sprintf("Lowering the copy count to %d. Longhorn removes the extra copies one at a time and keeps the copy on %s.", target, v.EngineNode))
	step.Kind, step.Patch = stepPatch, lowerPatch(v, target)
	return step
}

// lowered reports a held volume whose count the reduction to target lowered,
// which data locality marks until the extra copies are gone. Kipper turns data
// locality on only for that.
func lowered(v *lhVolume, target int64) bool {
	return v.Held && v.Count == target && v.Locality == "best-effort"
}

// longhornKeepsAll returns the replicas of v, sorted by name, among which
// Longhorn's locality cleanup picks the copy to remove when every one of them
// is on the engine's node, so that it removes none while they stay, and where
// they are. Status messages use it to name these copies. It follows
// getPreferredReplicaCandidatesForDeletion in
// longhorn-manager 1.11.3: the largest group of replicas on one disk if it has
// two or more, else the largest on one node. Replicas of every state count,
// as there. It returns nil when Longhorn may pick a copy elsewhere.
func longhornKeepsAll(v *lhVolume) (stuck []lhReplica, where string) {
	byDisk, byNode := map[string][]lhReplica{}, map[string][]lhReplica{}
	for _, r := range v.Replicas {
		byDisk[r.Node+"/"+r.DiskID] = append(byDisk[r.Node+"/"+r.DiskID], r)
		byNode[r.Node] = append(byNode[r.Node], r)
	}
	for _, level := range []struct {
		groups map[string][]lhReplica
		where  string
	}{{byDisk, "share a disk on " + v.EngineNode}, {byNode, "are all on " + v.EngineNode}} {
		largest := 0
		for _, g := range level.groups {
			largest = max(largest, len(g))
		}
		if largest < 2 {
			continue
		}
		// Longhorn takes one of the largest groups in map order, so it is
		// stuck only when every one of them is on the engine's node.
		for _, key := range slices.Sorted(maps.Keys(level.groups)) {
			g := level.groups[key]
			if len(g) != largest {
				continue
			}
			if slices.ContainsFunc(g, func(r lhReplica) bool { return r.Node != v.EngineNode }) {
				return nil, ""
			}
			if stuck == nil {
				stuck = slices.SortedFunc(slices.Values(g), func(a, b lhReplica) int { return strings.Compare(a.Name, b.Name) })
			}
		}
		return stuck, level.where
	}
	return nil, ""
}

// joinNames lists names as "a", "a and b" or "a, b and c".
func joinNames(names []string) string {
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// serverRefusal returns why no disk of the server's Longhorn node takes a new
// copy of any size, or "" when one does and only its room can fall short.
// A Ready or Schedulable condition of the node that is not True is named with
// Longhorn's reason before the node's settings, and its disks come last.
func serverRefusal(c *lhCluster) string {
	if !c.CapacityRead {
		return ""
	}
	for _, d := range c.Disks {
		if c.Servers[d.Node] && longhorncap.HasAllTags(d.Tags, targetSelector(1)) {
			return ""
		}
	}
	for i := range c.nodes {
		n := &c.nodes[i]
		if !c.Servers[n.GetName()] {
			continue
		}
		for _, kind := range []string{"Ready", "Schedulable"} {
			cond := longhornCondition(*n, kind)
			switch {
			case cond.ok:
			case cond.reason == "KubernetesNodeCordoned":
				return "the server " + n.GetName() + " is cordoned"
			case cond.reason != "":
				return fmt.Sprintf("Longhorn reports the server's node not %s (%s)", kind, cond.reason)
			default:
				return "Longhorn reports the server's node not " + kind
			}
		}
		switch {
		case boolField(n, "spec", "evictionRequested"):
			return "eviction is requested for the server's Longhorn node"
		case !boolField(n, "spec", "allowScheduling"):
			return "the server's Longhorn node does not allow scheduling"
		}
	}
	return "no disk of the server's Longhorn node is ready and allows scheduling without an eviction request"
}

// healthyNodes counts the available nodes holding a healthy copy, as the
// StorageCopies status counts them.
func healthyNodes(reps []replicaState, c *lhCluster) int {
	nodes := map[string]bool{}
	for _, r := range reps {
		if r.healthy && r.Node != "" && c.Available[r.Node] {
			nodes[r.Node] = true
		}
	}
	return len(nodes)
}

// copyNodeDown explains a block caused by an unavailable node and how to
// proceed if the node is permanently lost.
func copyNodeDown(node string) string {
	return fmt.Sprintf("A copy of this volume is on %s, which is not Ready or is fenced, so Kipper keeps every copy. "+
		"The change continues once the node is back. If %s is gone for good, run 'kip node fence %s' and then 'kip node remove %s --force'.",
		node, node, node, node)
}

// unavailableCopyNode returns the node of the first replica not being deleted
// whose node is not available, or "" when every such replica's node is. A
// failed replica counts, since the held engine could not compare it.
func unavailableCopyNode(replicas []lhReplica, c *lhCluster) string {
	for _, r := range replicas {
		if r.Node != "" && !r.Deleting && !c.Available[r.Node] {
			return r.Node
		}
	}
	return ""
}

// evictingCopy explains a block caused by Longhorn evicting the copy on node.
func evictingCopy(node string) string {
	return fmt.Sprintf("Longhorn is evicting the copy on %s, because the node is cordoned or drained, or an eviction was requested for its Longhorn node or disk. "+
		"The change continues once the eviction ends: uncordon %s, or cancel the eviction request in Longhorn.", node, node)
}

func blocked(message string) conversionStep {
	return conversionStep{Kind: stepBlocked, Reason: kipperv1.StorageCopiesBlocked, Message: message}
}

func pendingStep(message string) conversionStep {
	return conversionStep{Kind: stepWait, Reason: kipperv1.StorageCopiesConversionPending, Message: message}
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

// trimPatch turns off data locality and auto-balance and, when count is given,
// sets the count. It keeps the live node selector, so a server-only selector
// set to build a server copy stays until the extra copies are gone.
func trimPatch(v *lhVolume, count *int64) *volumePatch {
	selector := append([]string{}, v.Selector...)
	return &volumePatch{Spec: &volumePatchSpec{NodeSelector: selector, DataLocality: "disabled", ReplicaAutoBalance: "disabled", NumberOfReplicas: count}}
}

// lowerPatch lowers v's count to target with data locality on and
// auto-balance off, keeping the live node selector, so Longhorn removes the
// extra copies away from the engine's node.
func lowerPatch(v *lhVolume, target int64) *volumePatch {
	p := trimPatch(v, &target)
	p.Spec.DataLocality = "best-effort"
	return p
}

// keepsExtraCopies reports that Longhorn removes no healthy copy of v on its
// own except one it evicts, since data locality and auto-balance are off.
func keepsExtraCopies(v *lhVolume) bool {
	return (v.Locality == "" || v.Locality == "disabled") && v.AutoBalance == "disabled"
}

// specPatch sets the target's placement settings and, when count is given,
// the replica count, which is never below the volume's here.
func specPatch(target int64, count *int64, c *lhCluster) *volumePatch {
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
