package cmd

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pruneServer answers like routedHost, and serves pvc-a until its reclaim
// policy turns to Delete, after which the provisioner has removed it.
type pruneServer struct {
	routedHost
	released bool
}

func (p *pruneServer) Run(cmd string) (string, error) {
	if strings.Contains(cmd, "get pv 'pvc-a'") {
		p.ran = append(p.ran, cmd)
		if p.released {
			return `Error from server (NotFound): persistentvolumes "pvc-a" not found`, errors.New("exit 1")
		}
		return `{"metadata":{"name":"pvc-a","annotations":{"kipper.run/retained-local-copy-state":"moved","kipper.run/retained-local-copy-service":"shop/db","kipper.run/retained-local-copy-destination-uid":"c1"}},"spec":{}}`, nil
	}
	return p.routedHost.Run(cmd)
}

func (p *pruneServer) RunStdin(cmd string, stdin io.Reader) (string, error) {
	b, _ := io.ReadAll(stdin)
	if strings.Contains(cmd, "patch pv 'pvc-a'") && strings.Contains(string(b), `"persistentVolumeReclaimPolicy":"Delete"`) {
		p.released = true
	}
	return p.Run(cmd)
}

func pruneHost() *pruneServer {
	retained := `{"items":[
{"metadata":{"name":"pvc-a","labels":{"kipper.run/retained-local-copy":"true"},"annotations":{"kipper.run/retained-local-copy-service":"shop/db","kipper.run/retained-local-copy-state":"moved","kipper.run/retained-local-copy-used-bytes":"3221225472","kipper.run/retained-local-copy-moved-at":"2026-10-22T12:00:00Z","kipper.run/retained-local-copy-destination-uid":"c1"}}},
{"metadata":{"name":"pvc-b","labels":{"kipper.run/retained-local-copy":"true"},"annotations":{"kipper.run/retained-local-copy-service":"shop/cache","kipper.run/retained-local-copy-state":"moving","kipper.run/retained-local-copy-used-bytes":"1048576"}}}]}`
	return &pruneServer{routedHost: routedHost{routes: map[string]string{
		"{.spec.storage}":              "longhorn",
		"get pv -l":                    retained,
		"get services.kipper.run 'db'": `{"metadata":{"uid":"s1"},"spec":{}}`,
		"get pvc 'data-db-0'":          `{"metadata":{"name":"data-db-0","uid":"c1"},"spec":{"storageClassName":"longhorn-single"},"status":{"phase":"Bound"}}`,
	}}}
}

func TestPruneDeletesOnlyMovedCopies(t *testing.T) {
	host := pruneHost()
	out := &bytes.Buffer{}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	require.NoError(t, pruneRetainedCopies(out, host, nil, false, true, now))
	assert.Contains(t, out.String(), "shop/db (3.0 GiB, moved 2d ago): moved, can be deleted")
	assert.Contains(t, out.String(), "shop/cache (1 MiB): kept: its move is unfinished")
	assert.Contains(t, out.String(), "Deleted the local copy of shop/db")
	assert.True(t, host.released)
	assert.False(t, host.ranContaining("pvc-b"), "an unfinished move's copy is never touched")
}

func TestPruneAsksFirst(t *testing.T) {
	host := pruneHost()
	err := pruneRetainedCopies(&bytes.Buffer{}, host, nil, false, false, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--yes")
	assert.False(t, host.ranContaining("patch pv"))

	out := &bytes.Buffer{}
	require.NoError(t, pruneRetainedCopies(out, host, func(string) (bool, error) { return false, nil }, true, false, time.Now()))
	assert.Contains(t, out.String(), "Nothing was deleted")
	assert.False(t, host.ranContaining("patch pv"))
}

func TestPruneRefusesOnLocalStorage(t *testing.T) {
	host := &routedHost{routes: map[string]string{"{.spec.storage}": "local"}}
	err := pruneRetainedCopies(&bytes.Buffer{}, host, nil, false, true, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local storage")
}
