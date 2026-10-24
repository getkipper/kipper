package installer

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// Longhorn's defaults for the storage settings that decide whether a disk can
// take a replica.
const (
	defaultMinimalAvailablePercent = 25
	defaultOverProvisioningPercent = 100
)

type longhornCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

type longhornDisk struct {
	AllowScheduling   bool     `json:"allowScheduling"`
	EvictionRequested bool     `json:"evictionRequested"`
	StorageReserved   int64    `json:"storageReserved"`
	Tags              []string `json:"tags"`
}

type longhornDiskStatus struct {
	Conditions       []longhornCondition `json:"conditions"`
	StorageAvailable int64               `json:"storageAvailable"`
	StorageMaximum   int64               `json:"storageMaximum"`
	StorageScheduled int64               `json:"storageScheduled"`
}

type longhornNode struct {
	Metadata struct {
		Name              string  `json:"name"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Spec struct {
		AllowScheduling   bool                    `json:"allowScheduling"`
		EvictionRequested bool                    `json:"evictionRequested"`
		Tags              []string                `json:"tags"`
		Disks             map[string]longhornDisk `json:"disks"`
	} `json:"spec"`
	Status struct {
		Conditions []longhornCondition           `json:"conditions"`
		DiskStatus map[string]longhornDiskStatus `json:"diskStatus"`
	} `json:"status"`
}

type longhornVolume struct {
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Size         string   `json:"size"`
		NodeSelector []string `json:"nodeSelector"`
		DiskSelector []string `json:"diskSelector"`
	} `json:"spec"`
	Status struct {
		ActualSize int64 `json:"actualSize"`
	} `json:"status"`
}

// scheduling holds Longhorn's storage settings in percent and whether volumes
// without a node or disk selector may use tagged nodes or disks.
type scheduling struct {
	minimalAvailable  int64
	overProvisioning  int64
	emptyNodeSelector bool
	emptyDiskSelector bool
}

// LastCopiesWithoutRoom checks destinations for volumes whose healthy copies are
// all on node. The caller supplies those volumes in last. It reads Longhorn nodes,
// volumes and settings through master and returns an error if a command fails or
// node or volume JSON is invalid. An empty last returns nil without running commands.
//
// Each result names a blocked volume by its claim, or its name if it has no claim,
// and gives the first rejection reason for each other node, listing disk reasons
// when applicable. Results follow input order, with nodes and disks sorted by name.
// Volumes missing from the current Longhorn listing are skipped.
//
// Checks cover deletion, scheduling and eviction flags, node readiness, node and
// disk tags, empty-selector settings, disk schedulability, minimum free space and
// over-provisioning. Each volume is checked independently against current capacity.
// An empty result means these checks found no obstacle. Longhorn still decides
// placement during the drain, including rules for the v2 data engine, block disks
// and backing images that this check omits.
func LastCopiesWithoutRoom(master CommandRunner, node string, last []NodeVolume) ([]string, error) {
	if len(last) == 0 {
		return nil, nil
	}
	return withoutRoom(master, node, last, nil)
}

// CopiesWithoutRebuildRoom returns volumes whose copy on node has no rebuild
// destination after node is removed. It checks volumes supplied by VolumesOnNode
// with a current copy on node and at least one available copy elsewhere.
// Destinations already holding a current copy are excluded using CurrentReplicaNodes.
// LastCopiesWithoutRoom checks last copies.
// If no volumes qualify, it returns nil without running commands. Replica read
// errors are returned. Results, other errors and capacity checks follow
// LastCopiesWithoutRoom.
func CopiesWithoutRebuildRoom(master CommandRunner, node string, volumes []NodeVolume) ([]string, error) {
	var shared []NodeVolume
	for _, v := range volumes {
		if v.Here > 0 && v.Elsewhere > 0 {
			shared = append(shared, v)
		}
	}
	if len(shared) == 0 {
		return nil, nil
	}
	holders, err := CurrentReplicaNodes(master)
	if err != nil {
		return nil, err
	}
	return withoutRoom(master, node, shared, holders)
}

// withoutRoom returns volumes with no eligible destination outside node.
// holders maps each volume to existing copy holders, which are also excluded.
func withoutRoom(master CommandRunner, node string, check []NodeVolume, holders map[string]map[string]bool) ([]string, error) {
	out, err := master.Run(`kubectl -n longhorn-system get nodes.longhorn.io --request-timeout=30s -o json`)
	if err != nil {
		return nil, fmt.Errorf("listing Longhorn nodes: %w", err)
	}
	var nodes struct {
		Items []longhornNode `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &nodes); err != nil {
		return nil, fmt.Errorf("reading Longhorn nodes: %w", err)
	}
	out, err = master.Run(`kubectl -n longhorn-system get volumes.longhorn.io --request-timeout=30s -o json`)
	if err != nil {
		return nil, fmt.Errorf("listing Longhorn volumes: %w", err)
	}
	var volumes struct {
		Items []longhornVolume `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &volumes); err != nil {
		return nil, fmt.Errorf("reading Longhorn volumes: %w", err)
	}
	settings, err := schedulingSettings(master)
	if err != nil {
		return nil, err
	}
	byName := map[string]longhornVolume{}
	for _, v := range volumes.Items {
		byName[v.Metadata.Name] = v
	}
	sort.Slice(nodes.Items, func(i, j int) bool { return nodes.Items[i].Metadata.Name < nodes.Items[j].Metadata.Name })

	var blocked []string
	for _, v := range check {
		vol, ok := byName[v.Name]
		if !ok {
			continue
		}
		var reasons []string
		fits := false
		for _, n := range nodes.Items {
			if n.Metadata.Name == node {
				continue
			}
			if holders[v.Name][n.Metadata.Name] {
				reasons = append(reasons, n.Metadata.Name+": already holds a copy")
				continue
			}
			reason := nodeRefuses(n, vol, settings)
			if reason == "" {
				fits = true
				break
			}
			reasons = append(reasons, n.Metadata.Name+": "+reason)
		}
		if fits {
			continue
		}
		if len(reasons) == 0 {
			reasons = []string{"there is no other Longhorn node"}
		}
		blocked = append(blocked, fmt.Sprintf("%s (%s)", v.Label(), strings.Join(reasons, "; ")))
	}
	return blocked, nil
}

// schedulingSettings reads Longhorn's storage and empty-selector settings. Missing
// values and percentages that cannot be parsed as integers retain the defaults. Only the literal false
// disables either empty-selector setting.
func schedulingSettings(master CommandRunner) (scheduling, error) {
	s := scheduling{minimalAvailable: defaultMinimalAvailablePercent, overProvisioning: defaultOverProvisioningPercent, emptyNodeSelector: true, emptyDiskSelector: true}
	out, err := master.Run(`kubectl -n longhorn-system get settings.longhorn.io storage-minimal-available-percentage storage-over-provisioning-percentage allow-empty-node-selector-volume allow-empty-disk-selector-volume --request-timeout=30s -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.value}{"\n"}{end}'`)
	if err != nil {
		return s, fmt.Errorf("reading Longhorn's storage settings: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		switch name {
		case "allow-empty-node-selector-volume":
			s.emptyNodeSelector = value != "false"
			continue
		case "allow-empty-disk-selector-volume":
			s.emptyDiskSelector = value != "false"
			continue
		}
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			continue
		}
		switch name {
		case "storage-minimal-available-percentage":
			s.minimalAvailable = n
		case "storage-over-provisioning-percentage":
			s.overProvisioning = n
		}
	}
	return s, nil
}

// nodeRefuses returns why node n cannot take a copy of vol, or "" when one of
// its disks can.
func nodeRefuses(n longhornNode, vol longhornVolume, s scheduling) string {
	switch {
	case n.Metadata.DeletionTimestamp != nil:
		return "is being deleted"
	case !n.Spec.AllowScheduling:
		return "scheduling is turned off"
	case n.Spec.EvictionRequested:
		return "eviction is requested"
	}
	for _, kind := range []string{"Ready", "Schedulable"} {
		if c, ok := condition(n.Status.Conditions, kind); !ok || c.Status != "True" {
			return withMessage("not "+strings.ToLower(kind), c.Message)
		}
	}
	if missing := missingTags(vol.Spec.NodeSelector, n.Spec.Tags); len(missing) > 0 {
		return "lacks the node tag " + strings.Join(missing, ", ")
	}
	if len(vol.Spec.NodeSelector) == 0 && len(n.Spec.Tags) > 0 && !s.emptyNodeSelector {
		return "is tagged, and Longhorn keeps volumes without a node selector off tagged nodes"
	}
	size, _ := strconv.ParseInt(vol.Spec.Size, 10, 64)
	required := vol.Status.ActualSize
	var reasons []string
	names := make([]string, 0, len(n.Spec.Disks))
	for name := range n.Spec.Disks {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		reason := diskRefuses(n.Spec.Disks[name], n.Status.DiskStatus[name], vol, size, required, s)
		if reason == "" {
			return ""
		}
		reasons = append(reasons, "disk "+name+" "+reason)
	}
	if len(reasons) == 0 {
		return "has no disk"
	}
	return strings.Join(reasons, ", ")
}

func diskRefuses(d longhornDisk, st longhornDiskStatus, vol longhornVolume, size, required int64, s scheduling) string {
	switch {
	case !d.AllowScheduling:
		return "has scheduling turned off"
	case d.EvictionRequested:
		return "is being evicted"
	}
	if c, ok := condition(st.Conditions, "Schedulable"); !ok || c.Status != "True" {
		return withMessage("is not schedulable", c.Message)
	}
	if missing := missingTags(vol.Spec.DiskSelector, d.Tags); len(missing) > 0 {
		return "lacks the disk tag " + strings.Join(missing, ", ")
	}
	if len(vol.Spec.DiskSelector) == 0 && len(d.Tags) > 0 && !s.emptyDiskSelector {
		return "is tagged, and Longhorn keeps volumes without a disk selector off tagged disks"
	}
	if st.StorageAvailable-required <= st.StorageMaximum*s.minimalAvailable/100 {
		return fmt.Sprintf("has %s free, and Longhorn keeps %d%% of its %s free", gib(st.StorageAvailable), s.minimalAvailable, gib(st.StorageMaximum))
	}
	if size+st.StorageScheduled > (st.StorageMaximum-d.StorageReserved)*s.overProvisioning/100 {
		return fmt.Sprintf("has %s scheduled of the %s it may hold, too little for %s", gib(st.StorageScheduled), gib((st.StorageMaximum-d.StorageReserved)*s.overProvisioning/100), gib(size))
	}
	return ""
}

func condition(conditions []longhornCondition, kind string) (longhornCondition, bool) {
	for _, c := range conditions {
		if c.Type == kind {
			return c, true
		}
	}
	return longhornCondition{}, false
}

func missingTags(want, have []string) []string {
	var missing []string
	for _, t := range want {
		if !slices.Contains(have, t) {
			missing = append(missing, t)
		}
	}
	return missing
}

func gib(bytes int64) string {
	return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
}

func withMessage(reason, message string) string {
	if message = strings.TrimSpace(message); message != "" {
		return reason + ": " + message
	}
	return reason
}
