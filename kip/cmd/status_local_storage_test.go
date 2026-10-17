package cmd

import (
	"bytes"
	"errors"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/k8s"
)

const gib = int64(1) << 30

func summaryJSON(free, capacity int64) []byte {
	return []byte(`{"node":{"fs":{"availableBytes":` + itoa(free) + `,"capacityBytes":` + itoa(capacity) + `}},
"pods":[
 {"podRef":{"namespace":"shop","name":"db-0"},"volume":[{"name":"data","usedBytes":` + itoa(3*gib) + `,"pvcRef":{"namespace":"shop","name":"data-db-0"}}]},
 {"podRef":{"namespace":"shop","name":"web-1"},"volume":[{"name":"tmp","usedBytes":` + itoa(9*gib) + `}]},
 {"podRef":{"namespace":"kipper-system","name":"zot-1"},"volume":[{"name":"data","usedBytes":` + itoa(gib) + `,"pvcRef":{"namespace":"kipper-system","name":"zot-data"}}]}
]}`)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestLocalStorageReportShowsFreeSpaceAndTheLargestVolumes(t *testing.T) {
	var out bytes.Buffer
	reportLocalStorageWith(&out, []k8s.NodeInfo{{Name: "server-1"}}, func(string) ([]byte, error) {
		return summaryJSON(25*gib, 40*gib), nil
	})
	got := out.String()
	assert.Contains(t, got, "server-1: 25.0 GiB free of 40.0 GiB")
	assert.Contains(t, got, "shop/data-db-0 3.0 GiB")
	assert.Contains(t, got, "kipper-system/zot-data 1.0 GiB")
	assert.NotContains(t, got, "web-1", "only claims count as volumes")
	assert.NotContains(t, got, "⚠")
}

func TestLocalStorageReportWarnsWhenTheDiskRunsLow(t *testing.T) {
	var out bytes.Buffer
	reportLocalStorageWith(&out, []k8s.NodeInfo{{Name: "server-1"}}, func(string) ([]byte, error) {
		return summaryJSON(3*gib, 40*gib), nil
	})
	assert.Contains(t, out.String(), "⚠  server-1: 3.0 GiB free of 40.0 GiB")
	assert.Contains(t, out.String(), "no size limit")
}

func TestLocalStorageReportSaysWhenTheSummaryIsUnavailable(t *testing.T) {
	var out bytes.Buffer
	reportLocalStorageWith(&out, []k8s.NodeInfo{{Name: "server-1"}}, func(string) ([]byte, error) {
		return nil, errors.New("forbidden")
	})
	assert.Contains(t, out.String(), "server-1: not checked (forbidden)")
}

func TestLocalStorageReportNamesTheThreeLargestVolumesFirst(t *testing.T) {
	summary := []byte(`{"node":{"fs":{"availableBytes":` + itoa(30*gib) + `,"capacityBytes":` + itoa(40*gib) + `}},"pods":[
 {"volume":[{"usedBytes":` + itoa(1*gib) + `,"pvcRef":{"namespace":"a","name":"one"}}]},
 {"volume":[{"usedBytes":` + itoa(4*gib) + `,"pvcRef":{"namespace":"a","name":"four"}}]},
 {"volume":[{"usedBytes":` + itoa(2*gib) + `,"pvcRef":{"namespace":"a","name":"two"}}]},
 {"volume":[{"usedBytes":` + itoa(3*gib) + `,"pvcRef":{"namespace":"a","name":"three"}}]}]}`)
	var out bytes.Buffer
	reportLocalStorageWith(&out, []k8s.NodeInfo{{Name: "server-1"}}, func(string) ([]byte, error) { return summary, nil })
	assert.Contains(t, out.String(), "Largest volumes: a/four 4.0 GiB, a/three 3.0 GiB, a/two 2.0 GiB\n")
}

func TestLocalStorageReportTreatsMissingFiguresAsUnchecked(t *testing.T) {
	var out bytes.Buffer
	reportLocalStorageWith(&out, []k8s.NodeInfo{{Name: "server-1"}}, func(string) ([]byte, error) { return []byte(`{"node":{}}`), nil })
	assert.Contains(t, out.String(), "server-1: not checked (the kubelet reported no disk figures)")
	assert.NotContains(t, out.String(), "✔")

	out.Reset()
	onlyCapacity := []byte(`{"node":{"fs":{"capacityBytes":` + itoa(40*gib) + `}}}`)
	reportLocalStorageWith(&out, []k8s.NodeInfo{{Name: "server-1"}}, func(string) ([]byte, error) { return onlyCapacity, nil })
	assert.Contains(t, out.String(), "not checked", "a missing free figure is no full disk")
	assert.NotContains(t, out.String(), "0.0 GiB free")
}

func TestLocalStorageReportLeavesOutVolumesWithoutUsage(t *testing.T) {
	summary := []byte(`{"node":{"fs":{"availableBytes":` + itoa(30*gib) + `,"capacityBytes":` + itoa(40*gib) + `}},"pods":[
 {"volume":[{"pvcRef":{"namespace":"a","name":"unmeasured"}}]},
 {"volume":[{"usedBytes":` + itoa(gib) + `,"pvcRef":{"namespace":"a","name":"measured"}}]}]}`)
	var out bytes.Buffer
	reportLocalStorageWith(&out, []k8s.NodeInfo{{Name: "server-1"}}, func(string) ([]byte, error) { return summary, nil })
	assert.Contains(t, out.String(), "Largest volumes: a/measured 1.0 GiB\n")
	assert.NotContains(t, out.String(), "unmeasured")
}
