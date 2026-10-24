package cmd

import (
	"fmt"
	"io"
	"strings"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/service"
)

const joinDataSummary = `     Kipper configures database, registry and in-cluster backup volumes to keep
     one copy on the server. Any copies still on workers are listed above.
     Workers add CPU and memory. With volume data on the server, worker loss
     preserves that data, but services on the worker may stay unavailable
     until it returns. Server loss stops the cluster. Permanent server disk
     loss requires recovery from a surviving backup. Services created with two
     copies keep the second on a worker that passed the disk check, so either
     disk can be lost. 'kip service info <name>' shows where each service runs
     and where its data is.
`

const offServerJoinWarning = "The volumes listed above still have data on workers. Permanent loss of a worker can lose that data."

const inClusterBackupWarning = "Backups use the cluster's own MinIO. When its data is on the server, a lost server disk takes the backups with it. External backup storage is chosen at install time. See the Backups page in the Kipper docs for options for a running cluster."

func backupsInCluster(ref *config.BackupStorageRef) bool {
	return ref == nil || ref.Mode == "" || ref.Mode == "in-cluster"
}

func reportSingleCopyPlacement(out io.Writer, p installer.SingleCopyPlacement) {
	if n := len(p.Pinned); n > 0 {
		say(out, "  ✔  %d single-copy %s pinned to the server\n", n, plural(n, "volume", "volumes"))
	}
	for _, u := range p.Unpinned {
		say(out, "  ⚠  Could not set server-only scheduling for %s\n", u)
	}
	if notice := installer.OffServerNotice(p.OffServer); notice != "" {
		say(out, "  ⚠  %s\n", notice)
	}
}

func reportDataPlacement(out io.Writer, client installer.CommandRunner, cluster *config.Cluster, nodes []k8s.NodeInfo) {
	if len(nodes) < 2 {
		return
	}
	var lines []string
	if off, err := installer.SingleCopiesOffServer(client); err != nil {
		lines = append(lines, fmt.Sprintf("⚠  single-copy volume placement not checked (%v)", err))
	} else if notice := installer.OffServerNotice(off); notice != "" {
		lines = append(lines, "⚠  "+notice)
	}
	if backupsInCluster(cluster.BackupStorage) {
		lines = append(lines, "⚠  "+inClusterBackupWarning)
	}
	if len(lines) == 0 {
		return
	}
	say(out, "  Data placement:\n")
	for _, l := range lines {
		say(out, "    %s\n", l)
	}
	say(out, "\n")
}

func writePlacement(out io.Writer, p service.Placement, err error) {
	pod := p.PodNode
	if pod == "" {
		pod = "not scheduled"
		if p.Unscheduled != "" {
			pod += " (" + p.Unscheduled + ")"
		}
	}
	say(out, "  Pod node:  %s\n", pod)
	if err != nil {
		say(out, "  Data node: not checked (%v)\n", err)
		return
	}
	data := "no healthy copy found"
	if len(p.DataNodes) > 0 {
		data = fmt.Sprintf("%s (%d %s)", strings.Join(p.DataNodes, ", "), len(p.DataNodes), plural(len(p.DataNodes), "copy", "copies"))
	}
	if len(p.UncheckedNodes) > 0 {
		n := len(p.UncheckedNodes)
		data += fmt.Sprintf(". Could not read %s %s, so %s %s not checked", plural(n, "node", "nodes"), strings.Join(p.UncheckedNodes, ", "), plural(n, "its copy", "their copies"), plural(n, "was", "were"))
	}
	say(out, "  Data node: %s\n", data)
}
