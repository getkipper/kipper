package cmd

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/domain"
)

// recorder captures the order collaborators were called in, which is the whole
// point: three review rounds turned on when the name is released relative to the
// wipe, and nothing could assert it.
type recorder struct {
	events     []string
	workerErrs map[string]error
}

func (r *recorder) note(event string) { r.events = append(r.events, event) }

func (r *recorder) sawBefore(first, second string) bool {
	fi, si := -1, -1
	for i, e := range r.events {
		if e == first && fi < 0 {
			fi = i
		}
		if e == second && si < 0 {
			si = i
		}
	}
	return fi >= 0 && si >= 0 && fi < si
}

func (r *recorder) saw(event string) bool {
	for _, e := range r.events {
		if e == event {
			return true
		}
	}
	return false
}

type fakeHost struct {
	out string
	err error
	// duringRun runs while the host is being read, which is where a concurrent
	// command's write actually lands: reading the cluster's Secret is a network
	// round trip, and that is the gap the mirror write has to survive. Keying it
	// to a mirror read instead would move the trigger along with the code under
	// test, so a version that read the mirror at the wrong moment would still
	// look correct.
	duringRun func()
}

func (f *fakeHost) Run(string) (string, error) {
	if f.duringRun != nil {
		f.duringRun()
	}
	return f.out, f.err
}
func (f *fakeHost) RunStdin(cmd string, _ io.Reader) (string, error) {
	return f.Run(cmd)
}

// clusterToken is what a cluster's gateway-credentials Secret looks like coming
// back from kubectl: base64, quoted, newline-terminated.
const clusterTokenValue = "tok-1"

func encodedToken() string {
	return "'" + base64.StdEncoding.EncodeToString([]byte(clusterTokenValue)) + "'\n"
}

type fakeMirror struct {
	stored   string
	writeErr error
	// failWrites bounds writeErr to the first N writes, so a test can model a
	// transient failure that has cleared by the time the retry comes round.
	// Zero means every write fails.
	failWrites int
	writes     int
}

func (m *fakeMirror) read(string) string { return m.stored }

// write mirrors the real writer: it replaces only what the caller expected to
// find, so a credential recorded by something else is never written over.
func (m *fakeMirror) write(name, expected, token string) error {
	if m.stored == token {
		// Already recorded. The real writer reports ErrNoChange and does not
		// touch the file, so no write error can arise here.
		return nil
	}
	if m.stored != expected {
		return fmt.Errorf("%w: %s", ErrMirrorHolds, name)
	}
	m.writes++
	if m.writeErr != nil && (m.failWrites == 0 || m.writes <= m.failWrites) {
		return m.writeErr
	}
	m.stored = token
	return nil
}

func testCluster() *config.Cluster {
	return &config.Cluster{
		Name: "203-0-113-10.kipper.run", Host: "203.0.113.10",
		Domain: "203-0-113-10.kipper.run",
	}
}

func deps(rec *recorder, host gatewayTokenReader, mirror *fakeMirror, wipeErr, relErr error, prompt string, out io.Writer) uninstallDeps {
	return uninstallDeps{
		Host: host,
		Wipe: func() error { rec.note("wipe"); return wipeErr },
		WipeWorker: func(n config.NodeRecord) error {
			rec.note("wipe " + n.Host)
			return rec.workerErrs[n.Host]
		},
		Release: func(string) error {
			rec.note("release")
			return relErr
		},
		ReadMirror:  mirror.read,
		WriteMirror: mirror.write,
		Prompt:      strings.NewReader(prompt),
		Out:         out,
	}
}

// The ordering that took two rounds to get right: releasing before the wipe took
// a live cluster off the air when the wipe then failed.
func TestUninstallReleasesTheNameOnlyAfterTheHostIsWiped(t *testing.T) {
	rec := &recorder{}
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil, nil, "", io.Discard)

	if _, err := uninstallCluster(testCluster(), true, d); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !rec.saw("release") {
		t.Fatal("the name must be released on a successful wipe")
	}
	if !rec.sawBefore("wipe", "release") {
		t.Errorf("the wipe must complete before the name is released, got %v", rec.events)
	}
}

// A failed wipe may have left a live cluster. Releasing then frees a name that
// is still serving, which is the outage the ordering exists to prevent.
func TestUninstallReleasesNothingWhenTheWipeFails(t *testing.T) {
	rec := &recorder{}
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, errors.New("ssh: connection lost"), nil, "", io.Discard)

	if _, err := uninstallCluster(testCluster(), true, d); err == nil {
		t.Fatal("a failed wipe must surface as an error")
	}
	if rec.saw("release") {
		t.Error("a failed wipe must not release the name — the cluster may still be live")
	}
}

func twoWorkerCluster() *config.Cluster {
	cluster := testCluster()
	cluster.Nodes = []config.NodeRecord{
		{Name: "server", Host: cluster.Host, Role: "server"},
		{Name: "worker-a", Host: "203.0.113.11", Role: "worker", MachineID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		{Name: "worker-b", Host: "203.0.113.12", Role: "worker", MachineID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	}
	return cluster
}

func TestUninstallWipesTheWorkersBeforeTheServer(t *testing.T) {
	rec := &recorder{}
	var out bytes.Buffer
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil, nil, "", &out)

	if _, err := uninstallCluster(twoWorkerCluster(), true, d); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	for _, w := range []string{"wipe 203.0.113.11", "wipe 203.0.113.12"} {
		if !rec.sawBefore(w, "wipe") {
			t.Errorf("%s must happen before the server wipe, got %v", w, rec.events)
		}
	}
	if strings.Contains(out.String(), "k3s-agent-uninstall.sh") {
		t.Errorf("wiped workers need no manual cleanup, got %q", out.String())
	}
}

func TestUninstallNamesTheWorkersItLeavesBehind(t *testing.T) {
	rec := &recorder{workerErrs: map[string]error{"203.0.113.12": errors.New("ssh: no route to host")}}
	var out bytes.Buffer
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil, nil, "", &out)
	cluster := twoWorkerCluster()

	if _, err := uninstallCluster(cluster, true, d); err != nil {
		t.Fatalf("an unreachable worker must not stop the server wipe: %v", err)
	}
	if !rec.saw("wipe") {
		t.Fatal("the server is still wiped")
	}
	for _, want := range []string{"203.0.113.12", "no route to host", "k3s-agent-uninstall.sh"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the operator must learn that %s is left to clean up, got %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "203.0.113.11 ") && strings.Contains(out.String(), "left") && strings.Contains(out.String(), "203.0.113.11,") {
		t.Errorf("the wiped worker is not left behind, got %q", out.String())
	}
	if strings.Contains(out.String(), cluster.Host+" (server)") {
		t.Errorf("the server is wiped, so it is not listed as left behind, got %q", out.String())
	}

	var single bytes.Buffer
	d = deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil, nil, "", &single)
	if _, err := uninstallCluster(testCluster(), true, d); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if strings.Contains(single.String(), "worker") {
		t.Errorf("a single-node cluster has no workers to mention, got %q", single.String())
	}
}

// The cluster is unreadable but an earlier run recorded the token. That copy is
// the only thing that can release the name.
func TestUninstallFallsBackToTheLocallyMirroredToken(t *testing.T) {
	rec := &recorder{}
	mirror := &fakeMirror{stored: "tok-mirrored"}
	var released string
	d := deps(rec, &fakeHost{err: errors.New("kubectl: no such host")}, mirror, nil, nil, "", io.Discard)
	d.Release = func(token string) error { rec.note("release"); released = token; return nil }

	if _, err := uninstallCluster(testCluster(), true, d); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if released != "tok-mirrored" {
		t.Errorf("released with %q, want the mirrored token", released)
	}
}

// The mirror is what makes a failed wipe recoverable. Wiping without it carries
// the risk the mirror was added to remove, so the operator decides.
func TestUninstallStopsWhenTheMirrorCannotBeWritten(t *testing.T) {
	rec := &recorder{}
	mirror := &fakeMirror{writeErr: errors.New("read-only file system")}
	var out bytes.Buffer
	d := deps(rec, &fakeHost{out: encodedToken()}, mirror, nil, nil, "n\n", &out)

	if _, err := uninstallCluster(testCluster(), false, d); err != nil {
		t.Fatalf("declining is a decision, not a failure: %v", err)
	}
	if rec.saw("wipe") {
		t.Error("declining must stop before the host is touched")
	}
	if !strings.Contains(out.String(), "cannot be released") {
		t.Errorf("the operator must be told what the risk is, got %q", out.String())
	}
}

// A person saying no exits cleanly; a prompt nobody can answer must not report
// success, or a pipeline reads "wiped" from an untouched host.
func TestUninstallDistinguishesADeclineFromAnUnanswerablePrompt(t *testing.T) {
	rec := &recorder{}
	d := deps(rec, &fakeHost{out: "\n"}, &fakeMirror{}, nil, nil, "n\n", io.Discard)
	if _, err := uninstallCluster(testCluster(), false, d); err != nil {
		t.Errorf("an explicit no is a decision: %v", err)
	}

	rec2 := &recorder{}
	d2 := deps(rec2, &fakeHost{out: "\n"}, &fakeMirror{}, nil, nil, "", io.Discard)
	if _, err := uninstallCluster(testCluster(), false, d2); err == nil {
		t.Error("an unanswerable prompt must not report success")
	}
	if rec2.saw("wipe") {
		t.Error("and must not wipe")
	}
}

// A custom-domain cluster registered no name, so nothing is read, asked or
// released.
func TestUninstallLeavesACustomDomainClusterAlone(t *testing.T) {
	rec := &recorder{}
	cluster := &config.Cluster{Name: "shop", Host: "203.0.113.10", Domain: "shop.example.com"}
	d := deps(rec, &fakeHost{out: "\n"}, &fakeMirror{}, nil, nil, "", io.Discard)

	if _, err := uninstallCluster(cluster, false, d); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if rec.saw("release") {
		t.Error("a cluster that registered no name has none to release")
	}
	if !rec.saw("wipe") {
		t.Error("and must still be wiped")
	}
}

// Automation cannot answer a prompt, so --yes warns and continues rather than
// hanging a scripted teardown on a name it could not release.
func TestUninstallDoesNotPromptUnderYes(t *testing.T) {
	rec := &recorder{}
	var out bytes.Buffer
	// No token on the cluster and none mirrored: the case that prompts.
	d := deps(rec, &fakeHost{out: "\n"}, &fakeMirror{}, nil, nil, "", &out)

	if _, err := uninstallCluster(testCluster(), true, d); err != nil {
		t.Fatalf("--yes must not stop on a name it cannot release: %v", err)
	}
	if !rec.saw("wipe") {
		t.Error("--yes must proceed to the wipe")
	}
	if strings.Contains(out.String(), "Wipe anyway?") {
		t.Error("--yes must not ask a question nothing can answer")
	}
}

// An operator who accepts the risk gets the wipe they asked for.
func TestUninstallProceedsWhenTheOperatorAccepts(t *testing.T) {
	rec := &recorder{}
	d := deps(rec, &fakeHost{out: "\n"}, &fakeMirror{}, nil, nil, "y\n", io.Discard)

	if _, err := uninstallCluster(testCluster(), false, d); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !rec.saw("wipe") {
		t.Error("an explicit yes must proceed to the wipe")
	}
}

// The host is gone by the time the name is released, so a gateway that cannot be
// reached must not turn a completed uninstall into a failure. It must say so
// though, and say what the operator can do about it — which is now re-running
// the command, because the credential that releases the name is kept rather
// than deleted with the entry.
func TestUninstallReportsButDoesNotFailWhenTheReleaseIsRefused(t *testing.T) {
	rec := &recorder{}
	var out bytes.Buffer
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil,
		errors.New("gateway: 503"), "", &out)

	if _, err := uninstallCluster(testCluster(), true, d); err != nil {
		t.Fatalf("the host was wiped, so the uninstall succeeded: %v", err)
	}
	if !strings.Contains(out.String(), "re-running") {
		t.Errorf("the operator must be told the name can still be released, got %q", out.String())
	}
}

// A gateway that has already forgotten the registration is success, not failure.
func TestUninstallTreatsAnAlreadyReleasedNameAsDone(t *testing.T) {
	rec := &recorder{}
	var out bytes.Buffer
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil,
		domain.ErrNotRegistered, "", &out)

	if _, err := uninstallCluster(testCluster(), true, d); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !strings.Contains(out.String(), "already released") {
		t.Errorf("an unknown token means the name is already free, got %q", out.String())
	}
}

// Declining must be visible to the caller, which removes the local entry and
// kubeconfig on the strength of a wipe having happened. Reporting an abort as
// ordinary success took the operator's only credential for a cluster that was
// still running — a regression the extraction introduced, and one its own
// function-level tests could not see.
func TestUninstallReportsWhetherTheHostWasActuallyWiped(t *testing.T) {
	rec := &recorder{}
	d := deps(rec, &fakeHost{out: "\n"}, &fakeMirror{}, nil, nil, "n\n", io.Discard)

	outcome, err := uninstallCluster(testCluster(), false, d)
	if err != nil {
		t.Fatalf("declining is a decision: %v", err)
	}
	if outcome.Wiped {
		t.Error("nothing was wiped, and saying otherwise deletes local state for a live cluster")
	}

	rec2 := &recorder{}
	d2 := deps(rec2, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil, nil, "", io.Discard)
	outcome2, err2 := uninstallCluster(testCluster(), true, d2)
	if err2 != nil || !outcome2.Wiped {
		t.Errorf("a completed wipe must report true, got wiped=%v err=%v", outcome2.Wiped, err2)
	}
}

// The host is gone, so the local entry holds the only token that can release the
// name. Reporting a refused release as an ordinary success let the caller delete
// it, stranding the name for thirty days — the one case where this feature could
// still lose the thing it exists to protect.
func TestUninstallReportsThatTheNameIsStillRegistered(t *testing.T) {
	rec := &recorder{}
	var out bytes.Buffer
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil,
		errors.New("gateway: 503"), "", &out)

	outcome, err := uninstallCluster(testCluster(), true, d)
	if err != nil {
		t.Fatalf("the host was wiped, so the uninstall succeeded: %v", err)
	}
	if !outcome.Wiped {
		t.Error("the host was wiped")
	}
	if !outcome.NameStillRegistered {
		t.Error("a refused release must be reported, or the caller deletes the only token that can retry it")
	}
	if !strings.Contains(out.String(), "re-running") {
		t.Errorf("the operator must be told the retry is possible, got %q", out.String())
	}
}

// A released name leaves nothing to keep.
func TestUninstallReportsTheNameFreedWhenTheReleaseSucceeds(t *testing.T) {
	rec := &recorder{}
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil, nil, "", io.Discard)

	outcome, err := uninstallCluster(testCluster(), true, d)
	if err != nil || !outcome.Wiped {
		t.Fatalf("uninstall: %v", err)
	}
	if outcome.NameStillRegistered {
		t.Error("a released name is not still registered")
	}
}

// A gateway that has already forgotten the registration is not still holding it.
func TestUninstallTreatsAnAlreadyFreeNameAsReleased(t *testing.T) {
	rec := &recorder{}
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil,
		domain.ErrNotRegistered, "", io.Discard)

	outcome, err := uninstallCluster(testCluster(), true, d)
	if err != nil || outcome.NameStillRegistered {
		t.Errorf("an unknown token means the name is already free, got %+v %v", outcome, err)
	}
}

// The caller-side half of the fix. uninstallCluster reporting the name is still
// registered is worth nothing if runClusterUninstall deletes the entry anyway,
// which is exactly what the original defect did.

type localState struct {
	// ownedBy, when set, is the credential the entry holds — the removal is
	// refused for anything else, as the real writer does under its lock.
	ownedBy string
	removed []string
	wiped   map[string]bool
	err     error
}

func newLocalState() *localState {
	return &localState{wiped: map[string]bool{}}
}

func (l *localState) remove(name, ownedToken string) error {
	if l.ownedBy != "" && l.ownedBy != ownedToken {
		return errors.New("the local entry changed while this command was running")
	}
	l.removed = append(l.removed, name)
	return nil
}

func (l *localState) setWiped(name string, wiped bool) error {
	if l.err != nil {
		return l.err
	}
	l.wiped[name] = wiped
	return nil
}

func TestFinishUninstallKeepsTheEntryWhileTheNameIsStillRegistered(t *testing.T) {
	local := newLocalState()
	var out bytes.Buffer

	outcome := uninstallOutcome{Wiped: true, NameStillRegistered: true}
	if err := finishUninstall(outcome, testCluster(), false, local.remove, local.setWiped, &out); err != nil {
		t.Fatalf("finishUninstall: %v", err)
	}

	if len(local.removed) != 0 {
		t.Errorf("removed the entry holding the only release token: %v", local.removed)
	}
	if !local.wiped[testCluster().Name] {
		t.Error("did not record that the host was wiped, so the retry would try to reach a destroyed server")
	}
	if !strings.Contains(out.String(), "Re-run") {
		t.Errorf("did not tell the operator how to finish: %q", out.String())
	}
}

func TestFinishUninstallRemovesTheEntryOnceTheNameIsFree(t *testing.T) {
	local := newLocalState()
	var out bytes.Buffer

	outcome := uninstallOutcome{Wiped: true}
	if err := finishUninstall(outcome, testCluster(), false, local.remove, local.setWiped, &out); err != nil {
		t.Fatalf("finishUninstall: %v", err)
	}

	if len(local.removed) != 1 || local.removed[0] != testCluster().Name {
		t.Errorf("removed = %v, want the cluster removed exactly once", local.removed)
	}
}

func TestFinishUninstallKeepsTheEntryWhenNothingWasWiped(t *testing.T) {
	local := newLocalState()
	var out bytes.Buffer

	if err := finishUninstall(uninstallOutcome{}, testCluster(), false, local.remove, local.setWiped, &out); err != nil {
		t.Fatalf("finishUninstall: %v", err)
	}

	if len(local.removed) != 0 {
		t.Errorf("removed the entry for a cluster that is still running: %v", local.removed)
	}
}

func TestFinishUninstallClearsTheWipedMarkerWhenTheEntryIsKept(t *testing.T) {
	local := newLocalState()
	local.wiped[testCluster().Name] = true
	var out bytes.Buffer

	outcome := uninstallOutcome{Wiped: true}
	if err := finishUninstall(outcome, testCluster(), true, local.remove, local.setWiped, &out); err != nil {
		t.Fatalf("finishUninstall: %v", err)
	}

	if local.wiped[testCluster().Name] {
		t.Error("left the wiped marker set, so a later uninstall would take the release-only path forever")
	}
	if len(local.removed) != 0 {
		t.Errorf("--keep-local-config removed the entry anyway: %v", local.removed)
	}
}

// Nothing readable in the mirror. That means either the entry holds no
// credential or the config could not be opened, and the two answer identically,
// so the entry has to survive: deleting it would throw away a token that may
// still be in it.
func TestPendingReleaseKeepsTheEntryWhenNoCredentialCanBeRead(t *testing.T) {
	rec := &recorder{}
	mirror := &fakeMirror{}
	var out bytes.Buffer

	outcome := finishPendingRelease("", testCluster(), uninstallDeps{
		Release:     func(string) error { rec.note("release"); return nil },
		ReadMirror:  mirror.read,
		WriteMirror: mirror.write,
		Out:         &out,
	})

	if rec.saw("release") {
		t.Error("called the gateway with no token")
	}
	if outcome.Wiped {
		t.Error("reported work done, so the caller deletes an entry that may still hold the token")
	}
	if !strings.Contains(out.String(), "kip cluster remove") {
		t.Errorf("left the operator with no way to forget the cluster: %q", out.String())
	}
}

// A mirror write that failed before the wipe and a release the gateway then
// refuses is the one corner where the retry is promised and cannot happen. The
// token is still in hand at the refusal, so it gets one more chance.
func TestRefusedReleaseRecordsTheCredentialItStillHolds(t *testing.T) {
	rec := &recorder{}
	mirror := &fakeMirror{writeErr: errors.New("disk full"), failWrites: 1}
	var out bytes.Buffer

	d := deps(rec, &fakeHost{out: encodedToken()}, mirror, nil, errors.New("gateway unreachable"), "y\n", &out)
	outcome, err := uninstallCluster(testCluster(), true, d)
	if err != nil {
		t.Fatalf("uninstallCluster: %v", err)
	}

	if !outcome.NameStillRegistered {
		t.Fatal("reported the name freed after a refused release")
	}
	if mirror.stored != clusterTokenValue {
		t.Errorf("mirror holds %q, want the token the retry needs", mirror.stored)
	}
	if !strings.Contains(out.String(), "recorded locally") {
		t.Errorf("promised nothing about the retry: %q", out.String())
	}
}

// The dispatch itself, which is what round 9 got wrong at the other end: an
// entry whose host is already wiped must reach the gateway without a
// confirmation and without an SSH connection, because the server it names may
// no longer exist.
func TestUninstallSkipsTheHostEntirelyForAWipedCluster(t *testing.T) {
	rec := &recorder{}
	local := newLocalState()
	mirror := &fakeMirror{stored: "tok-1"}
	var out bytes.Buffer

	cluster := testCluster()
	cluster.HostWiped = true

	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			rec.note("connect")
			return uninstallDeps{}, func() {}, errors.New("ssh: no route to host")
		},
		ReleaseOnly: func(*config.Cluster) uninstallDeps {
			return uninstallDeps{
				Release:     func(string) error { rec.note("release"); return nil },
				ReadMirror:  mirror.read,
				WriteMirror: mirror.write,
				Out:         &out,
			}
		},
		Confirm:     func(*config.Cluster) bool { rec.note("confirm"); return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	if err := cmd.run(cluster, false, false); err != nil {
		t.Fatalf("run: %v", err)
	}

	if rec.saw("connect") {
		t.Error("reached for SSH on a host that was already wiped and may no longer exist")
	}
	if rec.saw("confirm") {
		t.Error("asked the operator to confirm destroying a host with nothing left on it")
	}
	if !rec.saw("release") {
		t.Error("did not release the name, which is the only reason this entry survived")
	}
	if len(local.removed) != 1 {
		t.Errorf("removed = %v, want the entry gone once its name is free", local.removed)
	}
}

// And an ordinary cluster still goes through the confirmation and the host.
func TestUninstallStillConfirmsAndConnectsForALiveCluster(t *testing.T) {
	rec := &recorder{}
	local := newLocalState()
	mirror := &fakeMirror{}
	var out bytes.Buffer

	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			rec.note("connect")
			return uninstallDeps{}, func() {}, errors.New("ssh: no route to host")
		},
		ReleaseOnly: releaseOnlyDeps(rec, mirror, "", &out),
		Confirm:     func(*config.Cluster) bool { rec.note("confirm"); return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	if err := cmd.run(testCluster(), false, false); err == nil {
		t.Fatal("a host that cannot be reached and holds no local credential must surface as an error")
	}
	if !rec.sawBefore("confirm", "connect") {
		t.Errorf("want the typed-name confirmation before the connection, got %v", rec.events)
	}
	if rec.saw("release") {
		t.Error("released a name with no credential recorded for it")
	}
}

// A server that does not answer is exactly when releasing the name without it
// matters, so the operator is offered that rather than being left with a
// connection error and a name held for thirty days.
func TestUninstallOffersToReleaseTheNameWhenTheHostIsGone(t *testing.T) {
	rec := &recorder{}
	local := newLocalState()
	mirror := &fakeMirror{stored: "tok-1"}
	var out bytes.Buffer

	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			return uninstallDeps{}, func() {}, errors.New("ssh: no route to host")
		},
		ReleaseOnly: releaseOnlyDeps(rec, mirror, "y\n", &out),
		Confirm:     func(*config.Cluster) bool { return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	if err := cmd.run(testCluster(), false, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !rec.saw("release") {
		t.Error("did not release the name of a host that is gone")
	}
	if len(local.removed) != 1 {
		t.Errorf("removed = %v, want the entry gone once its name is free", local.removed)
	}
}

// Declining leaves everything alone and still reports the connection failure,
// because nothing was uninstalled.
func TestUninstallKeepsTheNameWhenTheOperatorWillNotVouchForTheHost(t *testing.T) {
	rec := &recorder{}
	local := newLocalState()
	mirror := &fakeMirror{stored: "tok-1"}
	var out bytes.Buffer

	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			return uninstallDeps{}, func() {}, errors.New("ssh: connection refused")
		},
		ReleaseOnly: releaseOnlyDeps(rec, mirror, "n\n", &out),
		Confirm:     func(*config.Cluster) bool { return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	if err := cmd.run(testCluster(), false, false); err == nil {
		t.Fatal("declining must still report that the host could not be reached")
	}
	if !strings.Contains(out.String(), "Release the name without the host?") {
		t.Errorf("never put the question, so the decline proves nothing: %q", out.String())
	}
	if rec.saw("release") {
		t.Error("released the name of a cluster the operator would not vouch for")
	}
	if len(local.removed) != 0 {
		t.Errorf("removed = %v, want the entry kept", local.removed)
	}
}

// --yes means do not ask, and the unasked answer here has to be no: a script
// cannot tell a destroyed server from one that is briefly down.
func TestUninstallNeverReleasesWithoutTheHostUnderYes(t *testing.T) {
	rec := &recorder{}
	local := newLocalState()
	mirror := &fakeMirror{stored: "tok-1"}
	var out bytes.Buffer

	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			return uninstallDeps{}, func() {}, errors.New("ssh: no route to host")
		},
		ReleaseOnly: releaseOnlyDeps(rec, mirror, "y\n", &out),
		Confirm:     func(*config.Cluster) bool { return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	if err := cmd.run(testCluster(), true, false); err == nil {
		t.Fatal("--yes must surface the connection failure")
	}
	if rec.saw("release") {
		t.Error("--yes released a name nobody vouched for")
	}
}

func releaseOnlyDeps(rec *recorder, mirror *fakeMirror, prompt string, out io.Writer) func(*config.Cluster) uninstallDeps {
	return func(*config.Cluster) uninstallDeps {
		return uninstallDeps{
			Release:     func(string) error { rec.note("release"); return nil },
			ReadMirror:  mirror.read,
			WriteMirror: mirror.write,
			Prompt:      strings.NewReader(prompt),
			Out:         out,
		}
	}
}

// The wipe succeeded, the release did not, and the record of that could not be
// written. Reporting success then lets automation decommission the server, and
// the re-run reaches for a host that no longer answers before it looks at the
// token sitting locally.
func TestFinishUninstallFailsWhenTheWipeCannotBeRecorded(t *testing.T) {
	local := newLocalState()
	local.err = errors.New("read-only file system")
	var out bytes.Buffer

	outcome := uninstallOutcome{Wiped: true, NameStillRegistered: true}
	err := finishUninstall(outcome, testCluster(), false, local.remove, local.setWiped, &out)
	if err == nil {
		t.Fatal("an unrecorded pending release must not report success")
	}
	if len(local.removed) != 0 {
		t.Errorf("removed the entry holding the only release token: %v", local.removed)
	}
	if !strings.Contains(out.String(), "before decommissioning") {
		t.Errorf("did not warn against decommissioning the host: %q", out.String())
	}
}

// The retry path, where the token's own source is the local mirror. A failed
// re-write of the value the mirror already holds removes nothing from disk, and
// the operator must not be told to wait thirty days for a name their own config
// can still release.
func TestPendingReleaseKeepsACredentialTheMirrorStillHolds(t *testing.T) {
	mirror := &fakeMirror{stored: "tok-1", writeErr: errors.New("read-only file system")}
	var out bytes.Buffer

	outcome := finishPendingRelease("tok-1", testCluster(), uninstallDeps{
		Release:     func(string) error { return errors.New("gateway unreachable") },
		ReadMirror:  mirror.read,
		WriteMirror: mirror.write,
		Out:         &out,
	})

	if !outcome.NameStillRegistered {
		t.Fatal("reported the name freed, so the caller deletes the entry holding the last copy")
	}
	if strings.Contains(out.String(), "30-day") {
		t.Errorf("told the operator to wait for the sweep with the token still on disk: %q", out.String())
	}
}

// The same refusal with nothing readable in the mirror. The entry is still kept
// — an empty read means either nothing was recorded or the config could not be
// opened, and only one of those is safe to act on — but the message stops
// claiming the retry will certainly work.
func TestRefusedReleaseHedgesWhenTheMirrorShowsNothing(t *testing.T) {
	rec := &recorder{}
	mirror := &fakeMirror{writeErr: errors.New("read-only file system")}
	var out bytes.Buffer

	d := deps(rec, &fakeHost{out: encodedToken()}, mirror, nil, errors.New("gateway unreachable"), "", &out)
	outcome, err := uninstallCluster(testCluster(), true, d)
	if err != nil {
		t.Fatalf("uninstallCluster: %v", err)
	}

	if !outcome.NameStillRegistered {
		t.Error("deleted the entry on a mirror read that cannot report its own failure")
	}
	if !strings.Contains(out.String(), "may not have been recorded") {
		t.Errorf("claimed more than it knows: %q", out.String())
	}
}

// The operator vouched for a host once and the gateway refused anyway — most
// likely because the same local outage hid both. That is one answer, not
// standing authority: recording it as the wiped marker would let the next run,
// a scripted one included, release the name without asking and without touching
// a server that may have been alive throughout.
func TestUninstallDoesNotRecordAWipeNobodyPerformed(t *testing.T) {
	rec := &recorder{}
	local := newLocalState()
	mirror := &fakeMirror{stored: "tok-1"}
	var out bytes.Buffer

	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			return uninstallDeps{}, func() {}, errors.New("connecting to 203.0.113.10: no route to host")
		},
		ReleaseOnly: func(*config.Cluster) uninstallDeps {
			return uninstallDeps{
				Release:     func(string) error { rec.note("release"); return errors.New("gateway unreachable") },
				ReadMirror:  mirror.read,
				WriteMirror: mirror.write,
				Prompt:      strings.NewReader("y\n"),
				Out:         &out,
			}
		},
		Confirm:     func(*config.Cluster) bool { return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	if err := cmd.run(testCluster(), false, false); err == nil {
		t.Fatal("nothing was uninstalled, so the connection failure must still surface")
	}
	if local.wiped[testCluster().Name] {
		t.Error("recorded a wipe nobody performed, so the next run would skip the host entirely")
	}
	if len(local.removed) != 0 {
		t.Errorf("removed = %v, want the entry kept for another attempt", local.removed)
	}
}

// And with nothing recorded locally there is nothing to offer, so an unreachable
// host is an ordinary failure and the entry stays put.
func TestUninstallMakesNoOfferWithoutACredential(t *testing.T) {
	rec := &recorder{}
	local := newLocalState()
	mirror := &fakeMirror{}
	var out bytes.Buffer

	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			return uninstallDeps{}, func() {}, errors.New("connecting to 203.0.113.10: no route to host")
		},
		ReleaseOnly: releaseOnlyDeps(rec, mirror, "y\n", &out),
		Confirm:     func(*config.Cluster) bool { return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	if err := cmd.run(testCluster(), false, false); err == nil {
		t.Fatal("an unreachable host with no credential is a plain failure")
	}
	if rec.saw("release") {
		t.Error("released a name with no credential recorded for it")
	}
	if strings.Contains(out.String(), "Release the name without the host?") {
		t.Errorf("offered something it cannot do: %q", out.String())
	}
	if len(local.removed) != 0 {
		t.Errorf("removed = %v, want the entry kept", local.removed)
	}
}

// The operator approves releasing the credential they were shown, and the wait
// for their answer is as long as they are slow. Another kip run moving a domain
// in that window leaves a different, live registration under the same name:
// spending it would give up a name nobody agreed to, and deleting its entry
// would discard the only local copy of its credential.
func TestUninstallLeavesAnEntryThatChangedWhileThePromptWaited(t *testing.T) {
	local := newLocalState()
	// The entry holds a replacement by the time the removal is attempted.
	local.ownedBy = "tok-someone-elses"
	mirror := &fakeMirror{stored: "tok-approved"}
	var out bytes.Buffer

	var released string
	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			return uninstallDeps{}, func() {}, errors.New("connecting to 203.0.113.10: no route to host")
		},
		ReleaseOnly: func(*config.Cluster) uninstallDeps {
			return uninstallDeps{
				Release:     func(token string) error { released = token; return nil },
				ReadMirror:  mirror.read,
				WriteMirror: mirror.write,
				Prompt:      strings.NewReader("y\n"),
				Out:         &out,
			}
		},
		Confirm:     func(*config.Cluster) bool { return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	err := cmd.run(testCluster(), false, false)
	if released != "tok-approved" {
		t.Errorf("released %q, want the credential the operator was asked about", released)
	}
	if len(local.removed) != 0 {
		t.Errorf("removed = %v, want the replacement entry left alone", local.removed)
	}
	if err == nil {
		t.Error("said nothing about leaving the entry behind")
	}
}

// With nothing moving underneath it, the same path finishes: the approved name
// is released and its entry goes.
func TestUninstallRemovesTheEntryOnceItsNameIsFree(t *testing.T) {
	local := newLocalState()
	mirror := &fakeMirror{stored: "tok-1"}
	var out bytes.Buffer

	var released string
	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			return uninstallDeps{}, func() {}, errors.New("connecting to 203.0.113.10: no route to host")
		},
		ReleaseOnly: func(*config.Cluster) uninstallDeps {
			return uninstallDeps{
				Release:     func(token string) error { released = token; return nil },
				ReadMirror:  mirror.read,
				WriteMirror: mirror.write,
				Prompt:      strings.NewReader("y\n"),
				Out:         &out,
			}
		},
		Confirm:     func(*config.Cluster) bool { return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	if err := cmd.run(testCluster(), false, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	if released != "tok-1" {
		t.Errorf("released %q, want the recorded credential", released)
	}
	if len(local.removed) != 1 {
		t.Errorf("removed = %v, want the entry gone", local.removed)
	}
}

// A refused release must not write its own token over a credential some other
// command recorded in the meantime: that one is live, and this one has just
// been turned down.
func TestRefusedReleaseDoesNotOverwriteACredentialItDoesNotOwn(t *testing.T) {
	mirror := &fakeMirror{stored: "tok-someone-elses"}
	var out bytes.Buffer

	outcome := finishPendingRelease("tok-approved", testCluster(), uninstallDeps{
		Release:     func(string) error { return errors.New("gateway unreachable") },
		ReadMirror:  mirror.read,
		WriteMirror: mirror.write,
		Out:         &out,
	})

	if mirror.stored != "tok-someone-elses" {
		t.Errorf("mirror holds %q — overwrote the only local copy of a live credential", mirror.stored)
	}
	if !outcome.NameStillRegistered {
		t.Error("reported the name freed after a refused release")
	}
	if !strings.Contains(out.String(), "different credential") {
		t.Errorf("did not say why nothing was written: %q", out.String())
	}
}

// The marker says this host was wiped, so the only work left is the gateway —
// and nothing readable means none of it happened. Exiting zero would tell a
// scripted retry the teardown had finished, and it would keep finding the same
// entry and keep succeeding.
func TestUninstallFailsWhenAWipedClusterHasNoReadableCredential(t *testing.T) {
	rec := &recorder{}
	local := newLocalState()
	mirror := &fakeMirror{}
	var out bytes.Buffer

	cluster := testCluster()
	cluster.HostWiped = true

	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			rec.note("connect")
			return uninstallDeps{}, func() {}, errors.New("ssh: no route to host")
		},
		ReleaseOnly: releaseOnlyDeps(rec, mirror, "", &out),
		Confirm:     func(*config.Cluster) bool { return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	if err := cmd.run(cluster, false, false); err == nil {
		t.Fatal("nothing was released, so this must not report success")
	}
	if len(local.removed) != 0 {
		t.Errorf("removed = %v, want the entry kept for another attempt", local.removed)
	}
	if rec.saw("connect") {
		t.Error("reached for a host an earlier run already wiped")
	}
}

// The release is this path's entire job, so a refusal is a failure however
// politely it is reported. Exiting zero would stop a retry loop on a name that
// is still registered — the same deception the empty-credential case on this
// path was changed to avoid.
func TestUninstallFailsWhenAWipedClustersNameIsRefusedAgain(t *testing.T) {
	local := newLocalState()
	mirror := &fakeMirror{stored: "tok-1"}
	var out bytes.Buffer

	cluster := testCluster()
	cluster.HostWiped = true

	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			return uninstallDeps{}, func() {}, errors.New("ssh: no route to host")
		},
		ReleaseOnly: func(*config.Cluster) uninstallDeps {
			return uninstallDeps{
				Release:     func(string) error { return errors.New("gateway unreachable") },
				ReadMirror:  mirror.read,
				WriteMirror: mirror.write,
				Out:         &out,
			}
		},
		Confirm:     func(*config.Cluster) bool { return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	err := cmd.run(cluster, false, false)
	if err == nil {
		t.Fatal("a refused release was this run's whole job, so it must not report success")
	}
	if !strings.Contains(err.Error(), "gateway unreachable") {
		t.Errorf("error = %v, want the refusal that decided it", err)
	}
	if len(local.removed) != 0 {
		t.Errorf("removed = %v, want the entry kept", local.removed)
	}
}

// A run that wiped the server did the irreversible part. The name outstanding is
// worth saying and is not worth failing over, or every scripted teardown of a
// cluster whose gateway is briefly down reports disaster.
func TestUninstallStillSucceedsWhenAWipeOutlivesARefusedRelease(t *testing.T) {
	rec := &recorder{}
	local := newLocalState()
	mirror := &fakeMirror{}
	var out bytes.Buffer

	deps := deps(rec, &fakeHost{out: encodedToken()}, mirror, nil, errors.New("gateway unreachable"), "", &out)
	cmd := uninstallCommand{
		Connect:     func(*config.Cluster) (uninstallDeps, func(), error) { return deps, func() {}, nil },
		ReleaseOnly: releaseOnlyDeps(rec, mirror, "", &out),
		Confirm:     func(*config.Cluster) bool { return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	if err := cmd.run(testCluster(), true, false); err != nil {
		t.Fatalf("the wipe succeeded, so this must not report failure: %v", err)
	}
	if !local.wiped[testCluster().Name] {
		t.Error("did not record the wipe, so the retry would reach for a destroyed host")
	}
}

// The marker path's guard against a replaced entry, which the vouched path's
// test cannot reach.
func TestUninstallLeavesAReplacedEntryAloneOnTheMarkerPath(t *testing.T) {
	local := newLocalState()
	local.ownedBy = "tok-someone-elses"
	mirror := &fakeMirror{stored: "tok-1"}
	var out bytes.Buffer

	cluster := testCluster()
	cluster.HostWiped = true

	var released string
	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			return uninstallDeps{}, func() {}, errors.New("ssh: no route to host")
		},
		ReleaseOnly: func(*config.Cluster) uninstallDeps {
			return uninstallDeps{
				Release:     func(token string) error { released = token; return nil },
				ReadMirror:  mirror.read,
				WriteMirror: mirror.write,
				Out:         &out,
			}
		},
		Confirm:     func(*config.Cluster) bool { return true },
		RemoveLocal: local.remove,
		SetWiped:    local.setWiped,
		Out:         &out,
	}

	err := cmd.run(cluster, false, false)
	if released != "tok-1" {
		t.Errorf("released %q, want the credential this run read", released)
	}
	if len(local.removed) != 0 {
		t.Errorf("removed = %v, want the replacement entry left alone", local.removed)
	}
	if err == nil {
		t.Error("said nothing about leaving the entry behind")
	}
}

// Reading the cluster's credential is a network round trip, and a domain move
// finishing inside it leaves a newer one recorded locally. Writing the token
// this run just read over that would discard the only local copy of a live
// credential — the cluster being authoritative is a statement about the
// registration that was read, not a licence to overwrite whatever is here now.
func TestUninstallDoesNotMirrorOverACredentialThatArrivedMeanwhile(t *testing.T) {
	rec := &recorder{}
	mirror := &fakeMirror{}
	var out bytes.Buffer

	host := &fakeHost{out: encodedToken()}
	host.duringRun = func() { mirror.stored = "tok-arrived-meanwhile" }
	d := deps(rec, host, mirror, nil, nil, "n\n", &out)
	if _, err := uninstallCluster(testCluster(), false, d); err != nil {
		t.Fatalf("declining is a decision, not a failure: %v", err)
	}

	if mirror.stored != "tok-arrived-meanwhile" {
		t.Errorf("mirror holds %q — overwrote a credential this run never read", mirror.stored)
	}
	if rec.saw("wipe") {
		t.Error("wiped the host after declining")
	}
	if !strings.Contains(out.String(), "Could not record") {
		t.Errorf("did not tell the operator the mirror was refused: %q", out.String())
	}
}

// TestWipeRecordedWorkerChecksTheMachineID verifies that a wipe checks the host's
// recorded identity because a former worker's address may have been reassigned.
func TestWipeRecordedWorkerChecksTheMachineID(t *testing.T) {
	other := &routedHost{routes: map[string]string{"cat /etc/machine-id": "ffffffffffffffffffffffffffffffff\n"}}
	err := wipeRecordedWorker(other, config.NodeRecord{Host: "203.0.113.11", MachineID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, elsewhere)
	if err == nil || !strings.Contains(err.Error(), "left alone") {
		t.Fatalf("a different machine must be left alone, got %v", err)
	}
	for _, c := range other.ran {
		if strings.Contains(c, "uninstall") {
			t.Fatalf("nothing is uninstalled on a different machine, ran %v", other.ran)
		}
	}

	same := &routedHost{routes: map[string]string{"cat /etc/machine-id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"}}
	if err := wipeRecordedWorker(same, config.NodeRecord{Host: "203.0.113.11", MachineID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, elsewhere); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if !same.ranContaining("k3s-agent-uninstall.sh") {
		t.Errorf("the recorded worker is uninstalled, ran %v", same.ran)
	}

	unknown := &routedHost{routes: map[string]string{"cat /etc/machine-id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"}}
	if err := wipeRecordedWorker(unknown, config.NodeRecord{Host: "203.0.113.11"}, elsewhere); err == nil {
		t.Error("a record without a machine id cannot prove the host, so it is left alone")
	}
}

func elsewhere() (string, error) { return "cccccccccccccccccccccccccccccccc", nil }

func TestWipeRecordedWorkerLeavesTheMachineRunningKipAlone(t *testing.T) {
	self := &routedHost{routes: map[string]string{"cat /etc/machine-id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"}}
	here := func() (string, error) { return "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", nil }
	err := wipeRecordedWorker(self, config.NodeRecord{Host: "203.0.113.11", MachineID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, here)
	if err == nil || !strings.Contains(err.Error(), "machine running kip") {
		t.Fatalf("kip must not uninstall the machine it runs on, got %v", err)
	}
	if self.ranContaining("uninstall") {
		t.Errorf("nothing is uninstalled, ran %v", self.ran)
	}
}

func TestUninstallWipesTheWorkersTheClusterRecords(t *testing.T) {
	rec := &recorder{}
	var out bytes.Buffer
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil, nil, "", &out)
	cluster := twoWorkerCluster()
	d.ReadNodes = func() ([]config.NodeRecord, error) {
		return []config.NodeRecord{
			{Name: "server", Host: cluster.Host, Role: "server"},
			cluster.Nodes[1],
			{Name: "worker-c", Host: "203.0.113.13", Role: "worker", MachineID: "cccccccccccccccccccccccccccccccc"},
		}, nil
	}

	var saved []config.NodeRecord
	d.SaveNodes = func(nodes []config.NodeRecord) error { saved = nodes; return nil }

	if _, err := uninstallCluster(cluster, true, d); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if rec.saw("wipe 203.0.113.13") {
		t.Errorf("--yes never wipes a worker the confirmation did not list, got %v", rec.events)
	}
	if !rec.saw("wipe 203.0.113.11") {
		t.Errorf("a confirmed worker the cluster records is wiped, got %v", rec.events)
	}
	if rec.saw("wipe 203.0.113.12") {
		t.Errorf("a worker the cluster no longer records is left alone, got %v", rec.events)
	}
	if strings.Contains(out.String(), "worker-b (203.0.113.12") {
		t.Errorf("a worker the cluster no longer records gets no manual cleanup advice, got %q", out.String())
	}
	for _, want := range []string{"also records 203.0.113.13", "no longer records 203.0.113.12", "worker-c (203.0.113.13, machine id cccc"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the operator must learn %q, got %q", want, out.String())
		}
	}
	if len(saved) != 3 {
		t.Errorf("the cluster's records are saved before the wipe, got %v", saved)
	}

	rec = &recorder{}
	out.Reset()
	d = deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil, nil, "y\n", &out)
	d.ReadNodes = func() ([]config.NodeRecord, error) {
		return []config.NodeRecord{cluster.Nodes[1], {Name: "worker-c", Host: "203.0.113.13", Role: "worker", MachineID: "cccccccccccccccccccccccccccccccc"}}, nil
	}
	if _, err := uninstallCluster(twoWorkerCluster(), false, d); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !rec.saw("wipe 203.0.113.13") {
		t.Errorf("an explicit yes wipes the extra worker, got %v", rec.events)
	}

	rec = &recorder{}
	out.Reset()
	d = deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil, nil, "", &out)
	d.ReadNodes = func() ([]config.NodeRecord, error) { return nil, errors.New("the API is down") }
	if _, err := uninstallCluster(twoWorkerCluster(), true, d); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if !rec.saw("wipe 203.0.113.11") || !strings.Contains(out.String(), "this machine's copy decides") {
		t.Errorf("unreadable records fall back to this machine's copy and say so, got %v %q", rec.events, out.String())
	}
}

func TestUnwipedWorkersComeWithTheIdentityToCheck(t *testing.T) {
	var out bytes.Buffer
	reportUnwipedWorkers(&out, []unwipedWorker{{config.NodeRecord{Name: "worker-a", Host: "203.0.113.11", MachineID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "the host now has machine id ffff"}})
	for _, want := range []string{"worker-a (203.0.113.11, machine id aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa)", "check that /etc/machine-id matches", "only then run"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %q", want, out.String())
		}
	}
}

func TestReleaseOnlyRunsListTheWorkersTheyCannotVouchFor(t *testing.T) {
	var out bytes.Buffer
	cluster := twoWorkerCluster()
	cluster.HostWiped = true
	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			return uninstallDeps{}, func() {}, errors.New("unused")
		},
		ReleaseOnly: func(*config.Cluster) uninstallDeps {
			return uninstallDeps{Release: func(string) error { return nil }, ReadMirror: func(string) string { return clusterTokenValue }, WriteMirror: func(string, string, string) error { return nil }, Out: &out}
		},
		RemoveLocal: func(string, string) error { return nil },
		SetWiped:    func(string, bool) error { return nil },
		Out:         &out,
	}
	_ = cmd.run(cluster, true, false)
	for _, want := range []string{"203.0.113.11", "203.0.113.12", "cannot tell whether the worker was wiped"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in %q", want, out.String())
		}
	}
}

type routedHost struct {
	routes map[string]string
	ran    []string
}

func (h *routedHost) Run(cmd string) (string, error) {
	h.ran = append(h.ran, cmd)
	for k, v := range h.routes {
		if strings.Contains(cmd, k) {
			return v, nil
		}
	}
	return "", nil
}

func (h *routedHost) RunStdin(cmd string, _ io.Reader) (string, error) { return h.Run(cmd) }

func (h *routedHost) ranContaining(s string) bool {
	for _, c := range h.ran {
		if strings.Contains(c, s) {
			return true
		}
	}
	return false
}

func TestUninstallConfirmationNamesTheWorkers(t *testing.T) {
	var out bytes.Buffer
	cmd := uninstallCommand{
		Connect: func(*config.Cluster) (uninstallDeps, func(), error) {
			return uninstallDeps{}, func() {}, errors.New("unused")
		},
		ReleaseOnly: func(*config.Cluster) uninstallDeps { return uninstallDeps{} },
		Confirm:     func(*config.Cluster) bool { return false },
		Out:         &out,
	}
	if err := cmd.run(twoWorkerCluster(), false, false); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{"Worker:   203.0.113.11", "Worker:   203.0.113.12"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the prompt must name %q before asking, got %q", want, out.String())
		}
	}
}

func TestUninstallStopsWhenTheRecordsCannotBeSaved(t *testing.T) {
	rec := &recorder{}
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil, nil, "", io.Discard)
	d.ReadNodes = func() ([]config.NodeRecord, error) { return twoWorkerCluster().Nodes, nil }
	d.SaveNodes = func([]config.NodeRecord) error { return errors.New("disk full") }

	if _, err := uninstallCluster(twoWorkerCluster(), true, d); err == nil || !strings.Contains(err.Error(), "nothing was wiped") {
		t.Fatalf("an unsaved record set must stop the uninstall, got %v", err)
	}
	if len(rec.events) != 0 && (rec.saw("wipe") || rec.saw("wipe 203.0.113.11")) {
		t.Errorf("nothing is wiped, got %v", rec.events)
	}
}

// An address can pass to another machine once the server is gone, so a recorded
// machine ID must match before wiping. Older entries without an ID can still be wiped.
func TestWipeServerChecksTheMachineID(t *testing.T) {
	cluster := testCluster()
	cluster.MachineID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

	other := &routedHost{routes: map[string]string{"cat /etc/machine-id": "ffffffffffffffffffffffffffffffff\n"}}
	err := wipeServer(other, cluster)
	if err == nil || !strings.Contains(err.Error(), "nothing was wiped") {
		t.Fatalf("a different machine must be left alone, got %v", err)
	}
	if other.ranContaining("k3s-uninstall.sh") {
		t.Errorf("nothing is uninstalled on a different machine, ran %v", other.ran)
	}

	same := &routedHost{routes: map[string]string{"cat /etc/machine-id": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n"}}
	if err := wipeServer(same, cluster); err != nil {
		t.Fatalf("wipe: %v", err)
	}
	if !same.ranContaining("k3s-uninstall.sh") {
		t.Errorf("the recorded server is uninstalled, ran %v", same.ran)
	}

	legacy := &routedHost{}
	if err := wipeServer(legacy, testCluster()); err != nil {
		t.Fatalf("an entry without a machine id keeps the old behaviour: %v", err)
	}
}

func TestUninstallChecksTheServerBeforeTouchingAnyWorker(t *testing.T) {
	rec := &recorder{}
	read := false
	d := deps(rec, &fakeHost{out: encodedToken(), duringRun: func() { read = true }}, &fakeMirror{}, nil, nil, "", io.Discard)
	d.CheckServer = func() error { return errors.New("203.0.113.10 now has machine id ffff") }

	if _, err := uninstallCluster(twoWorkerCluster(), true, d); err == nil {
		t.Fatal("a server that changed identity stops the uninstall")
	}
	if len(rec.events) != 0 {
		t.Errorf("no worker or server is wiped and no name released, got %v", rec.events)
	}
	if read {
		t.Error("no gateway token is read from a host that is not the cluster's server")
	}
}

// Workers recorded only on the server are saved as unconfirmed until approved,
// so retries using local records leave them for manual cleanup.
func TestUninstallRetryKeepsSkippedWorkersUnconfirmed(t *testing.T) {
	cluster := twoWorkerCluster()
	extra := config.NodeRecord{Name: "worker-c", Host: "203.0.113.13", Role: "worker", MachineID: "cccccccccccccccccccccccccccccccc"}
	var saved []config.NodeRecord
	rec := &recorder{}
	d := deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, errors.New("ssh: connection lost"), nil, "", io.Discard)
	d.ReadNodes = func() ([]config.NodeRecord, error) {
		return append(append([]config.NodeRecord(nil), cluster.Nodes...), extra), nil
	}
	d.SaveNodes = func(n []config.NodeRecord) error { saved = n; return nil }

	if _, err := uninstallCluster(cluster, true, d); err == nil {
		t.Fatal("the server wipe failed, so the run fails")
	}
	if rec.saw("wipe 203.0.113.13") {
		t.Fatal("--yes never wipes a worker the confirmation did not list")
	}

	retry := testCluster()
	retry.Nodes = saved
	rec = &recorder{}
	var out bytes.Buffer
	d = deps(rec, &fakeHost{out: encodedToken()}, &fakeMirror{}, nil, nil, "", &out)
	d.ReadNodes = func() ([]config.NodeRecord, error) { return nil, errors.New("the API is down") }
	if _, err := uninstallCluster(retry, true, d); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if rec.saw("wipe 203.0.113.13") {
		t.Errorf("the retry must not promote the skipped worker, got %v", rec.events)
	}
	if !strings.Contains(out.String(), "worker-c (203.0.113.13") {
		t.Errorf("the skipped worker is reported for manual cleanup, got %q", out.String())
	}
	if !rec.saw("wipe 203.0.113.11") {
		t.Errorf("confirmed workers are still wiped, got %v", rec.events)
	}
}

func TestReleaseOnlyRunsAlsoListUnconfirmedWorkers(t *testing.T) {
	cluster := twoWorkerCluster()
	cluster.Nodes = append(cluster.Nodes, config.NodeRecord{Name: "worker-c", Host: "203.0.113.13", Role: "worker", MachineID: "cccccccccccccccccccccccccccccccc", State: unconfirmedWorker})
	var hosts []string
	for _, w := range notWiped(cluster, "reason") {
		hosts = append(hosts, w.node.Host)
	}
	if !strings.Contains(strings.Join(hosts, " "), "203.0.113.13") {
		t.Errorf("an unconfirmed worker is reported before its record is dropped, got %v", hosts)
	}
}
