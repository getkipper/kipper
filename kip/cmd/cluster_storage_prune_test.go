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

// pruneServer answers like routedHost, and serves each volume in pvs until
// its reclaim policy turns to Delete, after which the provisioner has removed
// it.
type pruneServer struct {
	routedHost
	pvs      map[string]string
	released map[string]bool
}

func (p *pruneServer) Run(cmd string) (string, error) {
	for name, body := range p.pvs {
		if strings.Contains(cmd, "get pv '"+name+"'") {
			p.ran = append(p.ran, cmd)
			if p.released[name] {
				return `Error from server (NotFound): persistentvolumes "` + name + `" not found`, errors.New("exit 1")
			}
			return body, nil
		}
	}
	out, err := p.routedHost.Run(cmd)
	if strings.Contains(out, "(NotFound)") {
		err = errors.New("exit 1")
	}
	return out, err
}

func (p *pruneServer) RunStdin(cmd string, stdin io.Reader) (string, error) {
	b, _ := io.ReadAll(stdin)
	for name := range p.pvs {
		if strings.Contains(cmd, "patch pv '"+name+"'") && strings.Contains(string(b), `"persistentVolumeReclaimPolicy":"Delete"`) {
			p.released[name] = true
		}
	}
	return p.Run(cmd)
}

// prunePV is a moved copy of service (namespace/name) whose move finished at
// movedAt onto the claim with UID destination.
func prunePV(name, service, movedAt, destination string) string {
	return `{"metadata":{"name":"` + name + `","labels":{"kipper.run/retained-local-copy":"true"},"annotations":{"kipper.run/retained-local-copy-service":"` + service + `","kipper.run/retained-local-copy-state":"moved","kipper.run/retained-local-copy-used-bytes":"3221225472","kipper.run/retained-local-copy-moved-at":"` + movedAt + `","kipper.run/retained-local-copy-destination-uid":"` + destination + `"}},"spec":{}}`
}

// prunePod is service's pod on its data claim with the given Ready condition.
func prunePod(service, ready string) string {
	return `{"metadata":{"name":"` + service + `-0"},"spec":{"volumes":[{"name":"data","persistentVolumeClaim":{"claimName":"data-` + service + `-0"}}]},"status":{"phase":"Running","conditions":[` + ready + `]}}`
}

// pruneGateHost serves the moved copies pvs, keyed by volume name, with routes
// for their services, claims and pods, and no Velero backups.
func pruneGateHost(pvs map[string]string, routes map[string]string) *pruneServer {
	var items []string
	for _, body := range pvs {
		items = append(items, body)
	}
	all := map[string]string{
		"{.spec.storage}":                "longhorn",
		"get pv -l":                      `{"items":[` + strings.Join(items, ",") + `]}`,
		"get backups.velero.io":          `{"items":[]}`,
		"get podvolumebackups.velero.io": `{"items":[]}`,
	}
	for k, v := range routes {
		all[k] = v
	}
	return &pruneServer{routedHost: routedHost{routes: all}, pvs: pvs, released: map[string]bool{}}
}

func pruneHost() *pruneServer {
	moving := `{"metadata":{"name":"pvc-b","labels":{"kipper.run/retained-local-copy":"true"},"annotations":{"kipper.run/retained-local-copy-service":"shop/cache","kipper.run/retained-local-copy-state":"moving","kipper.run/retained-local-copy-used-bytes":"1048576"}}}`
	return pruneGateHost(map[string]string{"pvc-a": prunePV("pvc-a", "shop/db", "2026-10-22T12:00:00Z", "c1"), "pvc-b": moving}, map[string]string{
		"get services.kipper.run 'db'": `{"metadata":{"uid":"s1"},"spec":{}}`,
		"get pvc 'data-db-0'":          `{"metadata":{"name":"data-db-0","uid":"c1"},"spec":{"storageClassName":"longhorn-single"},"status":{"phase":"Bound"}}`,
		"get pod 'db-0'":               prunePod("db", `{"type":"Ready","status":"True","lastTransitionTime":"2026-10-22T13:00:00Z"}`),
	})
}

func TestPruneDeletesOnlyMovedCopies(t *testing.T) {
	host := pruneHost()
	out := &bytes.Buffer{}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	require.NoError(t, pruneRetainedCopies(out, host, nil, false, true, now, pruneOptions{}))
	assert.Contains(t, out.String(), "shop/db (3.0 GiB, moved 2d ago): moved, can be deleted")
	assert.Contains(t, out.String(), "shop/cache (1 MiB): kept: its move is unfinished")
	assert.Contains(t, out.String(), "Deleted the local copy of shop/db")
	assert.True(t, host.released["pvc-a"])
	assert.False(t, host.ranContaining("pvc-b"), "an unfinished move's copy is never touched")
}

func TestPruneAsksFirst(t *testing.T) {
	host := pruneHost()
	err := pruneRetainedCopies(&bytes.Buffer{}, host, nil, false, false, time.Now(), pruneOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--yes")
	assert.False(t, host.ranContaining("patch pv"))

	out := &bytes.Buffer{}
	require.NoError(t, pruneRetainedCopies(out, host, func(string) (bool, error) { return false, nil }, true, false, time.Now(), pruneOptions{}))
	assert.Contains(t, out.String(), "Nothing was deleted")
	assert.False(t, host.ranContaining("patch pv"))
}

func TestPruneRefusesOnLocalStorage(t *testing.T) {
	host := &routedHost{routes: map[string]string{"{.spec.storage}": "local"}}
	err := pruneRetainedCopies(&bytes.Buffer{}, host, nil, false, true, time.Now(), pruneOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "local storage")
}

// gateNow is 3h after the moves in the gate tests' hosts.
var gateNow = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

const (
	readyTwoHours = `{"type":"Ready","status":"True","lastTransitionTime":"2026-10-25T10:00:00Z"}`
	notReady      = `{"type":"Ready","status":"False","lastTransitionTime":"2026-10-25T11:00:00Z"}`
)

// gateHost has shop/db Ready for 2h and shop/pg stopped, both moved 3h before
// gateNow, and blog/gone whose service and claim are both gone.
func gateHost() *pruneServer {
	claim := func(name, uid string) string {
		return `{"metadata":{"name":"` + name + `","uid":"` + uid + `"},"spec":{"storageClassName":"longhorn-single"},"status":{"phase":"Bound"}}`
	}
	return pruneGateHost(map[string]string{
		"pvc-a": prunePV("pvc-a", "shop/db", "2026-10-25T09:00:00Z", "c1"),
		"pvc-p": prunePV("pvc-p", "shop/pg", "2026-10-25T09:00:00Z", "c2"),
		"pvc-g": prunePV("pvc-g", "blog/gone", "2026-10-25T09:00:00Z", "c3"),
	}, map[string]string{
		"get services.kipper.run 'db'":   `{"metadata":{"uid":"s1"},"spec":{}}`,
		"get services.kipper.run 'pg'":   `{"metadata":{"uid":"s2"},"spec":{}}`,
		"get services.kipper.run 'gone'": `Error from server (NotFound): services.kipper.run "gone" not found`,
		"get pvc 'data-db-0'":            claim("data-db-0", "c1"),
		"get pvc 'data-pg-0'":            claim("data-pg-0", "c2"),
		"get pvc 'data-gone-0'":          `Error from server (NotFound): persistentvolumeclaims "data-gone-0" not found`,
		"get pod 'db-0'":                 prunePod("db", readyTwoHours),
		"get pod 'pg-0'":                 prunePod("pg", notReady),
	})
}

func TestPruneKeepsHealthyCopiesUntilABackupOrADay(t *testing.T) {
	host := gateHost()
	out := &bytes.Buffer{}
	require.NoError(t, pruneRetainedCopies(out, host, nil, false, true, gateNow, pruneOptions{}))
	got := out.String()
	assert.Contains(t, got, "shop/db (3.0 GiB, moved 3h ago): kept until a backup of db completes or 21h from now")
	assert.Contains(t, got, "shop/pg (3.0 GiB, moved 3h ago): kept: pg is not Ready on Longhorn")
	assert.Contains(t, got, "A service keeps its copy until its pod has been Ready on Longhorn for an hour, so a stopped service keeps it. To delete one sooner, name it:\n    kip cluster storage prune <service> --project <project> --delete-unproven\n  Add --now as well to skip the wait for a backup or a day.")
	assert.Contains(t, got, "Services and data claims both deleted:\n      blog/gone (3.0 GiB)")
	assert.Contains(t, got, "Deleted the local copy of blog/gone")
	assert.Equal(t, map[string]bool{"pvc-g": true}, host.released, "only the deleted service's copy goes")
}

func TestPruneNowGivesUpTheFallbackOfHealthyServicesOnly(t *testing.T) {
	host := gateHost()
	out := &bytes.Buffer{}
	var prompt string
	confirm := func(p string) (bool, error) { prompt = p; return true, nil }
	require.NoError(t, pruneRetainedCopies(out, host, confirm, true, false, gateNow, pruneOptions{skipSoak: true}))
	got := out.String()
	assert.Contains(t, got, "Given up early with --now:\n      shop/db (3.0 GiB)")
	assert.Contains(t, got, "the local copy is the fallback being given up")
	assert.Contains(t, got, "shop/pg (3.0 GiB, moved 3h ago): kept: pg is not Ready on Longhorn")
	assert.Contains(t, got, "kip cluster storage prune <service> --project <project> --delete-unproven --now\n", "the hint keeps the --now already passed")
	assert.NotContains(t, got, "Add --now as well")
	assert.Contains(t, prompt, "Delete 2 local copies")
	assert.Equal(t, map[string]bool{"pvc-a": true, "pvc-g": true}, host.released, "--now never deletes the copy of a service that is not healthy")
}

func TestPruneNowSaysWhyAConsistentPostgresHadNoBackupEvidence(t *testing.T) {
	host := gateHost()
	host.routes["get services.kipper.run 'db'"] = `{"metadata":{"uid":"s1"},"spec":{"consistentBackups":true}}`
	out := &bytes.Buffer{}
	require.NoError(t, pruneRetainedCopies(out, host, nil, false, true, gateNow, pruneOptions{skipSoak: true}))
	got := out.String()
	assert.Contains(t, got, "Given up early with --now:\n      shop/db (3.0 GiB)")
	assert.Contains(t, got, "db takes consistent backups, which the prune check cannot use as proof that its data was backed up. Its move is less than a day old, so deleting its local copy gives up that fallback.")
	assert.NotContains(t, got, "No backup holds their data", "the copy was kept for another reason")
}

func TestPruneDeleteUnprovenNeedsTheCopyNamed(t *testing.T) {
	host := gateHost()
	err := pruneRetainedCopies(&bytes.Buffer{}, host, nil, false, true, gateNow, pruneOptions{deleteUnproven: true, skipSoak: true})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip cluster storage prune <service> --project <project> --delete-unproven")
	assert.False(t, host.ranContaining("get pv"), "nothing is read before the flags are checked")

	host = gateHost()
	out := &bytes.Buffer{}
	require.NoError(t, pruneRetainedCopies(out, host, nil, false, true, gateNow, pruneOptions{service: "pg", deleteUnproven: true}))
	assert.Contains(t, out.String(), "kept until a backup of pg completes or 21h from now", "--delete-unproven waives only the health gate")
	assert.Empty(t, host.released)

	host = gateHost()
	out = &bytes.Buffer{}
	require.NoError(t, pruneRetainedCopies(out, host, nil, false, true, gateNow, pruneOptions{namespace: "shop", service: "pg", deleteUnproven: true, skipSoak: true}))
	got := out.String()
	assert.Contains(t, got, "Health check waived (--delete-unproven):\n      shop/pg (3.0 GiB)")
	assert.Contains(t, got, "shop/pg (3.0 GiB, moved 3h ago): moved, can be deleted: pg is not Ready on Longhorn, and --delete-unproven waives the health check")
	assert.Contains(t, got, "pg has not passed the health check on Longhorn yet, so its local copy is the fallback if its Longhorn data turns out to be bad.")
	assert.NotContains(t, got, "shop/db", "naming a copy leaves the others out")
	assert.NotContains(t, got, "blog/gone")
	assert.Equal(t, map[string]bool{"pvc-p": true}, host.released)
}

func TestPruneNamedCopyMustExist(t *testing.T) {
	err := pruneRetainedCopies(&bytes.Buffer{}, gateHost(), nil, false, true, gateNow, pruneOptions{namespace: "blog", service: "db"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no local copy of blog/db is kept")

	host := gateHost()
	host.pvs["pvc-x"] = prunePV("pvc-x", "blog/db", "2026-10-25T09:00:00Z", "c9")
	host.routes["get pv -l"] = `{"items":[` + host.pvs["pvc-a"] + `,` + host.pvs["pvc-x"] + `]}`
	err = pruneRetainedCopies(&bytes.Buffer{}, host, nil, false, true, gateNow, pruneOptions{service: "db"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--project")
	assert.Empty(t, host.released)
}
