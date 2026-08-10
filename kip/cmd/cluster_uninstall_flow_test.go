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
	events []string
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
