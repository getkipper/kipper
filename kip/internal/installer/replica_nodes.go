package installer

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

var replicaTagWait = joinTimeouts{ready: 2 * time.Minute, interval: 5 * time.Second}

// SetReplicaTag adds platform.ReplicaStorageTag when on is true and removes it
// otherwise, preserving the node's other Longhorn tags. Reads and patches are
// retried with a two minute deadline for each operation. Invalid node names
// and exhausted retries return an error.
func SetReplicaTag(client CommandRunner, node string, on bool) error {
	return setReplicaTag(client, node, on, replicaTagWait)
}

func setReplicaTag(client CommandRunner, node string, on bool, wait joinTimeouts) error {
	if on {
		return setLonghornNodeTags(client, node, []string{platform.ReplicaStorageTag}, nil, wait)
	}
	return setLonghornNodeTags(client, node, nil, []string{platform.ReplicaStorageTag}, wait)
}

// setLonghornNodeTags adds and removes tags on node's Longhorn node, writing
// only when something changes.
func setLonghornNodeTags(client CommandRunner, node string, add, remove []string, wait joinTimeouts) error {
	tags, err := longhornNodeTags(client, node, wait)
	if err != nil {
		return err
	}
	changed := false
	for _, tag := range add {
		if !slices.Contains(tags, tag) {
			tags = append(tags, tag)
			changed = true
		}
	}
	for _, tag := range remove {
		if i := slices.Index(tags, tag); i >= 0 {
			tags = slices.Delete(tags, i, i+1)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	patch := fmt.Sprintf(`{"spec":{"tags":%s}}`, jsonList(tags))
	// Longhorn's webhook refuses node changes while a new node's disks sync.
	err = pollUntil(wait.ready, wait.interval, func() (bool, error) {
		_, err := client.Run(fmt.Sprintf("kubectl -n longhorn-system patch nodes.longhorn.io %s --request-timeout=30s --type merge -p %s >/dev/null", node, shellQuote(patch)))
		return err == nil, err
	})
	if err != nil {
		return fmt.Errorf("tagging the Longhorn node %s: %w", node, err)
	}
	return nil
}

// ReplicaTagged reports whether node's Longhorn node carries
// platform.ReplicaStorageTag, so Longhorn may place new copies on it.
func ReplicaTagged(client CommandRunner, node string) (bool, error) {
	tags, err := longhornNodeTags(client, node, joinTimeouts{ready: 0, interval: time.Second})
	if err != nil {
		return false, err
	}
	return slices.Contains(tags, platform.ReplicaStorageTag), nil
}

func longhornNodeTags(client CommandRunner, node string, wait joinTimeouts) ([]string, error) {
	if !nodeNamePattern.MatchString(node) {
		return nil, fmt.Errorf("%q is not a node name", node)
	}
	var tags []string
	err := pollUntil(wait.ready, wait.interval, func() (bool, error) {
		out, err := client.Run(fmt.Sprintf("kubectl -n longhorn-system get nodes.longhorn.io %s --request-timeout=30s -o jsonpath='{.spec.tags}'", node))
		if err != nil {
			return false, err
		}
		tags, err = parseJSONList(out)
		return err == nil, err
	})
	if err != nil {
		return nil, fmt.Errorf("reading the Longhorn tags of %s: %w", node, err)
	}
	return tags, nil
}

// ReplicasOnNode counts the Longhorn replicas on node, healthy or not. A
// failed read is an error, never a count of zero.
func ReplicasOnNode(client CommandRunner, node string) (int, error) {
	if !nodeNamePattern.MatchString(node) {
		return 0, fmt.Errorf("%q is not a node name", node)
	}
	out, err := client.Run(`kubectl -n longhorn-system get replicas.longhorn.io --request-timeout=30s -o jsonpath='{range .items[*]}{.spec.nodeID}{"\n"}{end}'`)
	if err != nil {
		return 0, fmt.Errorf("listing Longhorn replicas: %w", err)
	}
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == node {
			count++
		}
	}
	return count, nil
}

// EngineReplicaTimeout reads Longhorn's live engine-replica-timeout setting
// in seconds, selecting the v1 value when values are stored per data engine.
// It returns DefaultEngineReplicaTimeout if the read fails or the selected
// value is missing, is not an integer, or is nonpositive.
func EngineReplicaTimeout(client CommandRunner) time.Duration {
	out, err := client.Run("kubectl -n longhorn-system get settings.longhorn.io engine-replica-timeout --request-timeout=30s -o jsonpath='{.value}'")
	if err != nil {
		return DefaultEngineReplicaTimeout
	}
	seconds, err := strconv.Atoi(v1SettingValue(out))
	if err != nil || seconds <= 0 {
		return DefaultEngineReplicaTimeout
	}
	return time.Duration(seconds) * time.Second
}

// EnsureStoragePackages installs Longhorn's host packages, fio among them, on
// a node that lacks any of them.
func EnsureStoragePackages(client CommandRunner, out io.Writer) error {
	host, err := DetectHostOS(client)
	if err != nil {
		return err
	}
	return installStoragePackages(client, host, out)
}

// CopiesInventory reports existing storage for an upgrade. UntaggedHolders
// contains sorted names of Longhorn nodes with healthy replicas and without
// platform.ReplicaStorageTag. Unmanaged counts Services and Volumes with copies unset.
type CopiesInventory struct {
	UntaggedHolders []string
	Unmanaged       int
}

// ReadCopiesInventory reads replicas, Longhorn node tags and the copies of
// every Service and Volume with kubectl. Any failed read is an error.
func ReadCopiesInventory(client CommandRunner) (CopiesInventory, error) {
	var inv CopiesInventory
	replicas, err := client.Run(`kubectl -n longhorn-system get replicas.longhorn.io --request-timeout=30s -o jsonpath='{range .items[*]}{.spec.nodeID}{"\t"}{.spec.healthyAt}{"\t"}{.spec.failedAt}{"\n"}{end}'`)
	if err != nil {
		return inv, fmt.Errorf("listing Longhorn replicas: %w", err)
	}
	nodes, err := client.Run(`kubectl -n longhorn-system get nodes.longhorn.io --request-timeout=30s -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.tags}{"\n"}{end}'`)
	if err != nil {
		return inv, fmt.Errorf("listing Longhorn nodes: %w", err)
	}
	objects, err := client.Run(`kubectl get services.kipper.run,volumes.kipper.run -A --request-timeout=30s -o jsonpath='{range .items[*]}{.kind}{"\t"}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.spec.copies}{"\n"}{end}'`)
	if err != nil {
		return inv, fmt.Errorf("listing services and volumes: %w", err)
	}

	tagged := map[string]bool{}
	for _, line := range strings.Split(nodes, "\n") {
		name, tags, _ := strings.Cut(line, "\t")
		if name == "" {
			continue
		}
		list, _ := parseJSONList(tags)
		tagged[name] = slices.Contains(list, platform.ReplicaStorageTag)
	}
	holders := map[string]bool{}
	// Lines are split without trimming, because an empty last field leaves a
	// trailing tab that trimming would remove.
	for _, line := range strings.Split(replicas, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) != 3 || fields[0] == "" || fields[1] == "" || fields[2] != "" {
			continue
		}
		if known, ok := tagged[fields[0]]; ok && !known {
			holders[fields[0]] = true
		}
	}
	for name := range holders {
		inv.UntaggedHolders = append(inv.UntaggedHolders, name)
	}
	slices.Sort(inv.UntaggedHolders)
	for _, line := range strings.Split(objects, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) == 4 && fields[3] == "" {
			inv.Unmanaged++
		}
	}
	return inv, nil
}
