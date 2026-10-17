package cmd

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

const gib = int64(1) << 30

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// nodeSummary returns a kubelet summary reader for a 40 GiB disk with free bytes available.
func nodeSummary(free int64) func(string) ([]byte, error) {
	return func(string) ([]byte, error) {
		return []byte(`{"node":{"fs":{"availableBytes":` + itoa(free) + `,"capacityBytes":` + itoa(40*gib) + `}}}`), nil
	}
}

func volumesOf(usage ...volumeUsage) func() ([]volumeUsage, error) {
	return func() ([]volumeUsage, error) { return usage, nil }
}

func report(backups bool, summary func(string) ([]byte, error), volumes func() ([]volumeUsage, error)) string {
	var out bytes.Buffer
	reportLocalStorageWith(&out, []k8s.NodeInfo{{Name: "server-1"}}, backups, summary, volumes)
	return out.String()
}

func TestLocalStorageReportShowsFreeSpaceAndTheLargestVolumes(t *testing.T) {
	got := report(true, nodeSummary(25*gib), volumesOf(
		volumeUsage{"shop/data-db-0", 3 * gib}, volumeUsage{"kipper-system/zot-data", gib}))
	assert.Contains(t, got, "server-1: 25.0 GiB free of 40.0 GiB")
	assert.Contains(t, got, "Largest volumes: shop/data-db-0 3.0 GiB, kipper-system/zot-data 1.0 GiB\n")

	small := report(true, nodeSummary(25*gib), volumesOf(volumeUsage{"kipper-system/zot-data", 60 * 1024}))
	assert.Contains(t, small, "kipper-system/zot-data 0 MiB")
	assert.NotContains(t, got, "⚠")
}

func TestLocalStorageReportWarnsWhenTheDiskRunsLow(t *testing.T) {
	got := report(true, nodeSummary(3*gib), volumesOf())
	assert.Contains(t, got, "⚠  server-1: 3.0 GiB free of 40.0 GiB")
	assert.Contains(t, got, "no size limit")
}

func TestLocalStorageReportSaysWhenTheSummaryIsUnavailable(t *testing.T) {
	got := report(true, func(string) ([]byte, error) { return nil, errors.New("forbidden") }, volumesOf())
	assert.Contains(t, got, "server-1: not checked (forbidden)")
}

func TestLocalStorageReportNamesTheThreeLargestVolumesFirst(t *testing.T) {
	got := report(true, nodeSummary(30*gib), volumesOf(
		volumeUsage{"a/one", gib}, volumeUsage{"a/four", 4 * gib}, volumeUsage{"a/two", 2 * gib}, volumeUsage{"a/three", 3 * gib}))
	assert.Contains(t, got, "Largest volumes: a/four 4.0 GiB, a/three 3.0 GiB, a/two 2.0 GiB\n")
}

func TestLocalStorageReportTreatsMissingFiguresAsUnchecked(t *testing.T) {
	got := report(true, func(string) ([]byte, error) { return []byte(`{"node":{}}`), nil }, volumesOf())
	assert.Contains(t, got, "server-1: not checked (the kubelet reported no disk figures)")
	assert.NotContains(t, got, "✔")

	onlyCapacity := func(string) ([]byte, error) {
		return []byte(`{"node":{"fs":{"capacityBytes":` + itoa(40*gib) + `}}}`), nil
	}
	got = report(true, onlyCapacity, volumesOf())
	assert.Contains(t, got, "not checked", "a missing free figure is no full disk")
	assert.NotContains(t, got, "0.0 GiB free")
}

func TestLocalStorageReportSaysWhenVolumesCannotBeMeasured(t *testing.T) {
	got := report(true, nodeSummary(30*gib), func() ([]volumeUsage, error) { return nil, errors.New("no SSH") })
	assert.Contains(t, got, "Volumes: not measured (no SSH)")
}

func TestLocalStorageReportSaysWhenTheClusterHasNoBackups(t *testing.T) {
	assert.Contains(t, report(false, nodeSummary(25*gib), volumesOf()), "⚠  "+platform.BackupsNotConfigured)
}

// The kubelet reports usage for the whole shared filesystem, so per-volume
// measurements use du on the server's pvc-<uid>_<namespace>_<claim> directories.
func TestLocalVolumeUsageReadsTheLocalPathDirectories(t *testing.T) {
	host := &duHost{out: "60\t/var/lib/rancher/k3s/storage/pvc-48b2_kipper-system_zot-data/\n" +
		"3145728\t/var/lib/rancher/k3s/storage/pvc-9f1c_shop_data-db-0/\n" +
		"12\t/var/lib/rancher/k3s/storage/lost+found/\n"}
	usage, err := localVolumeUsage(host)
	require.NoError(t, err)
	assert.Equal(t, []volumeUsage{{"kipper-system/zot-data", 60 * 1024}, {"shop/data-db-0", 3 * gib}}, usage)
	assert.Contains(t, host.commands[0], "du -sk /var/lib/rancher/k3s/storage/*/")
}

// duHost answers every command with out and records what it was asked.
type duHost struct {
	out      string
	commands []string
}

func (h *duHost) Run(cmd string) (string, error) {
	h.commands = append(h.commands, cmd)
	return h.out, nil
}

func (h *duHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }
