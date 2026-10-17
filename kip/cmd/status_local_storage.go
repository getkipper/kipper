package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

// localStorageLowPercent is the free disk percentage below which kip status warns.
const localStorageLowPercent = 15

// localStorageSummaryTimeout limits how long a slow kubelet can delay kip status.
const localStorageSummaryTimeout = 10 * time.Second

const largestVolumesShown = 3

// localPathStorageDir is where k3s's local-path provisioner keeps volumes.
const localPathStorageDir = "/var/lib/rancher/k3s/storage"

// kubeletSummary is the part of the kubelet's stats summary kip status reads.
// The kubelet reports local-path volume usage for the whole shared filesystem,
// so only node figures are read here. Volumes are measured on the host.
type kubeletSummary struct {
	Node struct {
		Fs struct {
			AvailableBytes *int64 `json:"availableBytes"`
			CapacityBytes  *int64 `json:"capacityBytes"`
		} `json:"fs"`
	} `json:"node"`
}

// volumeUsage holds a local-path claim as namespace/name and its disk usage in bytes.
type volumeUsage struct {
	claim string
	bytes int64
}

// reportLocalStorage shows each node's free disk from the kubelet, the largest
// volumes measured on the server, and a warning when backups are not configured.
func reportLocalStorage(ctx context.Context, out io.Writer, client kubernetes.Interface, nodes []k8s.NodeInfo, backups bool, volumes func() ([]volumeUsage, error)) {
	reportLocalStorageWith(out, nodes, backups, func(node string) ([]byte, error) {
		nodeCtx, cancel := context.WithTimeout(ctx, localStorageSummaryTimeout)
		defer cancel()
		return client.CoreV1().RESTClient().Get().AbsPath("/api/v1/nodes", node, "proxy", "stats", "summary").DoRaw(nodeCtx)
	}, volumes)
}

func reportLocalStorageWith(out io.Writer, nodes []k8s.NodeInfo, backups bool, summary func(node string) ([]byte, error), volumes func() ([]volumeUsage, error)) {
	say(out, "  Local storage:\n")
	if !backups {
		say(out, "    ⚠  %s\n", platform.BackupsNotConfigured)
	}
	for _, n := range nodes {
		raw, err := summary(n.Name)
		var s kubeletSummary
		if err == nil {
			err = json.Unmarshal(raw, &s)
		}
		if err != nil {
			say(out, "    ⚠  %s: not checked (%v)\n", n.Name, err)
			continue
		}
		if s.Node.Fs.AvailableBytes == nil || s.Node.Fs.CapacityBytes == nil || *s.Node.Fs.CapacityBytes <= 0 {
			say(out, "    ⚠  %s: not checked (the kubelet reported no disk figures)\n", n.Name)
			continue
		}
		free, capacity := *s.Node.Fs.AvailableBytes, *s.Node.Fs.CapacityBytes
		mark := "✔"
		if free*100/capacity < localStorageLowPercent {
			mark = "⚠"
		}
		say(out, "    %s  %s: %s free of %s\n", mark, n.Name, humanGiB(free), humanGiB(capacity))
		if mark == "⚠" {
			say(out, "       Local volumes have no size limit of their own and share disk space with images and system data.\n")
			say(out, "       Free up disk space or use Cluster Migration to move to a cluster with more storage.\n")
		}
	}
	usage, err := volumes()
	switch {
	case err != nil:
		say(out, "       Volumes: not measured (%v)\n", err)
	case len(usage) > 0:
		say(out, "       Largest volumes: %s\n", largestVolumes(usage))
	}
	say(out, "\n")
}

// largestVolumes sorts usage in place by descending size and formats up to
// largestVolumesShown claims with their sizes. Empty usage returns an empty string.
func largestVolumes(usage []volumeUsage) string {
	sort.Slice(usage, func(i, j int) bool { return usage[i].bytes > usage[j].bytes })
	var parts []string
	for i, v := range usage {
		if i == largestVolumesShown {
			break
		}
		parts = append(parts, v.claim+" "+humanVolumeSize(v.bytes))
	}
	return strings.Join(parts, ", ")
}

// localVolumeUsage measures local-path volume directories on the server in bytes.
// Directory names use pvc-<uid>_<namespace>_<claim>. Namespace and claim names
// cannot contain underscores, so the split is unambiguous. Other entries are skipped.
// The scan has a 60-second timeout and may return partial results because shell
// errors are suppressed. It returns nil when no valid measurements are available
// and an error if the command runner fails.
func localVolumeUsage(client installer.CommandRunner) ([]volumeUsage, error) {
	out, err := client.Run("timeout 60 du -sk " + localPathStorageDir + "/*/ 2>/dev/null; true")
	if err != nil {
		return nil, fmt.Errorf("measuring local volumes: %w", err)
	}
	var usage []volumeUsage
	for _, line := range strings.Split(out, "\n") {
		size, path, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok {
			continue
		}
		kib, err := strconv.ParseInt(size, 10, 64)
		if err != nil {
			continue
		}
		parts := strings.Split(filepath.Base(strings.TrimRight(path, "/")), "_")
		if len(parts) != 3 || !strings.HasPrefix(parts[0], "pvc-") {
			continue
		}
		usage = append(usage, volumeUsage{claim: parts[1] + "/" + parts[2], bytes: kib * 1024})
	}
	return usage, nil
}

func humanGiB(bytes int64) string {
	return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(int64(1)<<30))
}

// humanVolumeSize formats sizes below 1 GiB as whole MiB, rounded down,
// and sizes of 1 GiB or more as GiB with one decimal place.
func humanVolumeSize(bytes int64) string {
	if bytes < int64(1)<<30 {
		return fmt.Sprintf("%d MiB", bytes>>20)
	}
	return humanGiB(bytes)
}
