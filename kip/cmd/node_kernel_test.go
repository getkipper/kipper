package cmd

import (
	"bytes"
	"fmt"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// cannedHost answers every command with the same output.
type cannedHost string

func (h cannedHost) Run(string) (string, error) { return string(h), nil }

func (h cannedHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

func TestPinKernelOnAMissingKernelListsTheSameNode(t *testing.T) {
	err := pinKernelOn("worker-1", "7.0.0-30-generic", " --host 203.0.113.11", cannedHost("KIPPER-KERNEL-MISSING\n"))

	assert.ErrorContains(t, err, "worker-1: kernel 7.0.0-30-generic is not installed on this host. 'kip node kernel --host 203.0.113.11' lists the installed kernels")
}

// The listing matters most when a worker is down after a kernel update, so one
// unreachable worker must not hide the others.
func TestListKernelsShowsTheNodesAfterAnUnreachableOne(t *testing.T) {
	var out bytes.Buffer
	dial := func(rec installer.NodeRecord) (installer.CommandRunner, func(), error) {
		if rec.Name == "worker-1" {
			return nil, nil, fmt.Errorf("connection refused")
		}
		return statusHost{machineID: statusWorker2ID}, func() {}, nil
	}

	require.NoError(t, listKernels(&out, &config.Cluster{Host: "203.0.113.10"}, statusRecords(), statusHost{machineID: statusServerID}, dial))

	got := out.String()
	assert.Contains(t, got, "  Node worker-1 (203.0.113.11)\n  ⚠  not reached (connection refused)\n")
	assert.Contains(t, got, "  Node worker-2 (203.0.113.12)\n    Running:    7.0.0-38-generic\n")
}

func TestListKernelsSkipsAHostThatIsAnotherMachine(t *testing.T) {
	var out bytes.Buffer
	dial := func(installer.NodeRecord) (installer.CommandRunner, func(), error) {
		return statusHost{machineID: statusWorker2ID}, func() {}, nil
	}

	require.NoError(t, listKernels(&out, &config.Cluster{Host: "203.0.113.10"}, statusRecords(), statusHost{machineID: statusServerID}, dial))

	assert.Contains(t, out.String(), "  Node worker-1 (203.0.113.11)\n  ⚠  not checked (203.0.113.11 no longer answers as worker-1)\n")
}
