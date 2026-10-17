package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"time"

	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/k8s"
)

// localStorageLowPercent is the free disk percentage below which kip status warns.
const localStorageLowPercent = 15

// localStorageSummaryTimeout limits how long a slow kubelet can delay kip status.
const localStorageSummaryTimeout = 10 * time.Second

const largestVolumesShown = 3

// kubeletSummary is the part of the kubelet's stats summary kip status reads.
type kubeletSummary struct {
	Node struct {
		Fs struct {
			AvailableBytes *int64 `json:"availableBytes"`
			CapacityBytes  *int64 `json:"capacityBytes"`
		} `json:"fs"`
	} `json:"node"`
	Pods []struct {
		Volume []struct {
			UsedBytes *int64 `json:"usedBytes"`
			PVCRef    *struct {
				Namespace string `json:"namespace"`
				Name      string `json:"name"`
			} `json:"pvcRef"`
		} `json:"volume"`
	} `json:"pods"`
}

// reportLocalStorage shows node filesystem capacity and PVC usage from kubelet
// summaries, with warnings when free space falls below localStorageLowPercent.
func reportLocalStorage(ctx context.Context, out io.Writer, client kubernetes.Interface, nodes []k8s.NodeInfo) {
	reportLocalStorageWith(out, nodes, func(node string) ([]byte, error) {
		nodeCtx, cancel := context.WithTimeout(ctx, localStorageSummaryTimeout)
		defer cancel()
		return client.CoreV1().RESTClient().Get().AbsPath("/api/v1/nodes", node, "proxy", "stats", "summary").DoRaw(nodeCtx)
	})
}

func reportLocalStorageWith(out io.Writer, nodes []k8s.NodeInfo, summary func(node string) ([]byte, error)) {
	say(out, "  Local storage:\n")
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
		if volumes := largestVolumes(s); volumes != "" {
			say(out, "       Largest volumes: %s\n", volumes)
		}
	}
	say(out, "\n")
}

// largestVolumes formats up to largestVolumesShown PVC usage entries in descending
// size order. Only entries with reported usage are included.
func largestVolumes(s kubeletSummary) string {
	type volume struct {
		name string
		used int64
	}
	var all []volume
	for _, p := range s.Pods {
		for _, v := range p.Volume {
			if v.PVCRef != nil && v.UsedBytes != nil {
				all = append(all, volume{v.PVCRef.Namespace + "/" + v.PVCRef.Name, *v.UsedBytes})
			}
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].used > all[j].used })
	var parts string
	for i, v := range all {
		if i == largestVolumesShown {
			break
		}
		if parts != "" {
			parts += ", "
		}
		parts += v.name + " " + humanGiB(v.used)
	}
	return parts
}

func humanGiB(bytes int64) string {
	return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(int64(1)<<30))
}
