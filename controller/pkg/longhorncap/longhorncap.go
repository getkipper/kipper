// Package longhorncap holds Longhorn's disk scheduling rules for new replicas,
// read from Longhorn's node objects, so kip and console-api decide capacity
// the same way. The rules follow longhorn-manager v1.7.3 and v1.11.3.
package longhorncap

import (
	"slices"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Settings are the two Longhorn settings the disk rules read, in percent:
// storage-over-provisioning-percentage and storage-minimal-available-percentage.
type Settings struct {
	OverProvisioning int64
	MinimalAvailable int64
}

// Disk is a disk that takes new replicas, with its node's Longhorn tags and
// the figures Longhorn's scheduler reads, in bytes. Room is the largest new
// replica the disk takes when the replica's actual size is zero.
type Disk struct {
	Node      string
	Tags      []string
	Maximum   int64
	Reserved  int64
	Scheduled int64
	Available int64
	Room      int64

	settings Settings
}

// Takes reports whether Longhorn's IsSchedulableToDisk accepts a new replica
// of a volume with nominal size and actual size in bytes on this disk.
// Available space minus actual size must exceed the minimal share of the maximum, and the
// nominal size plus scheduled space must fit the over-provisioned room.
func (d Disk) Takes(size, actual int64) bool {
	if d.Maximum <= 0 {
		return false
	}
	physical := float64(d.Available-actual) > float64(d.Maximum)*float64(d.settings.MinimalAvailable)/100
	scheduled := float64(size+d.Scheduled) <= float64(d.Maximum-d.Reserved)*float64(d.settings.OverProvisioning)/100
	return physical && scheduled
}

// SchedulingDisks selects filesystem disks on nodes that are Ready,
// Schedulable and allow scheduling, whose disks allow scheduling, are not
// evicting and are Schedulable. Disks under DiskPressure stay so a refusal can
// report their room. This estimates capacity rather than full scheduling
// eligibility. No eligible disks returns nil. Node order follows the input,
// and disk order within a node is unspecified.
func SchedulingDisks(nodes []unstructured.Unstructured, settings Settings) []Disk {
	var disks []Disk
	for _, n := range nodes {
		node := n.Object
		allow, _, _ := unstructured.NestedBool(node, "spec", "allowScheduling")
		tags, _, _ := unstructured.NestedStringSlice(node, "spec", "tags")
		conditions, _, _ := unstructured.NestedSlice(node, "status", "conditions")
		if !allow || !ConditionTrue(conditions, "Ready") || !ConditionTrue(conditions, "Schedulable") {
			continue
		}
		specs, _, _ := unstructured.NestedMap(node, "spec", "disks")
		for name := range specs {
			spec, _, _ := unstructured.NestedMap(specs, name)
			status, found, _ := unstructured.NestedMap(node, "status", "diskStatus", name)
			if !found || !diskTakesReplicas(spec, status) {
				continue
			}
			d := Disk{
				Node: n.GetName(), Tags: tags,
				Maximum: Int64Field(status, "storageMaximum"), Reserved: Int64Field(spec, "storageReserved"),
				Scheduled: Int64Field(status, "storageScheduled"), Available: Int64Field(status, "storageAvailable"),
				settings: settings,
			}
			d.Room = schedulableRoom(d.Maximum, d.Reserved, d.Scheduled, d.Available, settings)
			disks = append(disks, d)
		}
	}
	return disks
}

// NodesThatTake returns unique node names in alphabetical order. Each node
// carries every selector tag and has a disk that accepts a new replica with
// the given nominal and actual sizes in bytes. No matches returns nil.
func NodesThatTake(disks []Disk, selector []string, size, actual int64) []string {
	var nodes []string
	for _, d := range disks {
		if HasAllTags(d.Tags, selector) && d.Takes(size, actual) && !slices.Contains(nodes, d.Node) {
			nodes = append(nodes, d.Node)
		}
	}
	slices.Sort(nodes)
	return nodes
}

func diskTakesReplicas(spec, status map[string]interface{}) bool {
	allow, _, _ := unstructured.NestedBool(spec, "allowScheduling")
	evicting, _, _ := unstructured.NestedBool(spec, "evictionRequested")
	diskType, _, _ := unstructured.NestedString(spec, "diskType")
	if !allow || evicting || (diskType != "" && diskType != "filesystem") {
		return false
	}
	conditions, _, _ := unstructured.NestedSlice(status, "conditions")
	for _, c := range conditions {
		cond, _ := c.(map[string]interface{})
		if cond["type"] == "Schedulable" {
			return cond["status"] == "True" || cond["reason"] == "DiskPressure"
		}
	}
	return false
}

// schedulableRoom returns the largest new replica size allowed by Longhorn's
// IsSchedulableToDisk, assuming zero actual usage for a new replica.
// Available space must exceed the minimal-available percentage of maximum
// capacity. Scheduled space plus the new size must fit within
// (maximum - reserved) times the over-provisioning percentage.
func schedulableRoom(maximum, reserved, scheduled, available int64, s Settings) int64 {
	if maximum <= 0 || available <= 0 || available <= int64(float64(maximum)*float64(s.MinimalAvailable)/100) {
		return 0
	}
	return max(int64(float64(maximum-reserved)*float64(s.OverProvisioning)/100)-scheduled, 0)
}

// ConditionTrue reports whether a Longhorn status condition of conditionType
// has status True. The first matching condition is used. No match returns false.
func ConditionTrue(conditions []interface{}, conditionType string) bool {
	for _, c := range conditions {
		cond, _ := c.(map[string]interface{})
		if cond["type"] == conditionType {
			return cond["status"] == "True"
		}
	}
	return false
}

// HasAllTags reports whether tags contains every selector tag. An empty
// selector matches any tags.
func HasAllTags(tags, selector []string) bool {
	for _, want := range selector {
		if !slices.Contains(tags, want) {
			return false
		}
	}
	return true
}

// Int64Field reads a number that JSON decoding may have left as int64 or
// float64, truncating fractional values. Missing fields and other types return zero.
func Int64Field(obj map[string]interface{}, field string) int64 {
	switch n := obj[field].(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	}
	return 0
}

// Demand describes Copies replicas of Bytes bytes each on distinct nodes.
// Disks contains indexes of eligible disks in the slice passed to Shortfalls.
type Demand struct {
	Bytes  int64
	Copies int
	Disks  []int
}

// Shortfall describes nodes whose available Room is less than the required
// Need, both in bytes. Nodes are in alphabetical order. Copies gives the minimum
// copies required on these nodes for each demand, in input order.
type Shortfall struct {
	Nodes  []string
	Room   int64
	Need   int64
	Copies []int
}

// Shortfalls returns node sets with insufficient room for the combined demands.
// It checks individual nodes in alphabetical order, each demand's eligible nodes
// in input order, then all eligible nodes together. Empty and duplicate sets
// are skipped. Each Demand.Disks entry must be a valid index into disks.
//
// Copies of a volume require distinct nodes. For each demand, the minimum copies
// required in a set is its copy count capped at its eligible node count, minus
// eligible nodes outside the set, with a lower bound of zero. Room includes only
// disks eligible for at least one demand. Inputs are left unchanged.
//
// A nil result means these sets have enough aggregate room. It does not guarantee
// placement, since individual disk limits and other node sets may prevent it.
func Shortfalls(disks []Disk, demands []Demand) []Shortfall {
	usable := make([]bool, len(disks))
	spans := make([][]string, len(demands))
	var sets [][]string
	var all []string
	for j, d := range demands {
		for _, i := range d.Disks {
			usable[i] = true
			if !slices.Contains(spans[j], disks[i].Node) {
				spans[j] = append(spans[j], disks[i].Node)
			}
			if !slices.Contains(all, disks[i].Node) {
				all = append(all, disks[i].Node)
			}
		}
		slices.Sort(spans[j])
	}
	slices.Sort(all)
	for _, n := range all {
		sets = append(sets, []string{n})
	}
	sets = append(append(sets, spans...), all)

	var short []Shortfall
	for k, set := range sets {
		if len(set) == 0 || slices.ContainsFunc(sets[:k], func(seen []string) bool { return slices.Equal(seen, set) }) {
			continue
		}
		s := Shortfall{Nodes: set, Copies: make([]int, len(demands))}
		for i, d := range disks {
			if usable[i] && slices.Contains(set, d.Node) {
				s.Room += d.Room
			}
		}
		for j, d := range demands {
			outside := 0
			for _, n := range spans[j] {
				if !slices.Contains(set, n) {
					outside++
				}
			}
			s.Copies[j] = max(min(d.Copies, len(spans[j]))-outside, 0)
			s.Need += int64(s.Copies[j]) * d.Bytes
		}
		if s.Need > s.Room {
			short = append(short, s)
		}
	}
	return short
}
