package installer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/hopca"
	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

// authorityFixture mints a usable authority and the leaf it signs.
func authorityFixture(t *testing.T) (caPEM, caKeyPEM, leafPEM, leafKeyPEM string) {
	t.Helper()
	m, err := hopca.New(hostnames.GatewayDomain)
	if err != nil {
		t.Fatalf("minting fixture authority: %v", err)
	}
	return string(m.CACertPEM), string(m.CAKeyPEM), string(m.LeafCertPEM), string(m.LeafKeyPEM)
}

// The phase is derived from material, never remembered. An operator carrying
// out the documented replacement leaves the cluster at one of these points, and
// the status has to name which one — a state that fell through to steady would
// tell them nothing is in flight while half a replacement sits on the cluster.
func TestPhaseIsDerivedFromMaterial(t *testing.T) {
	caA, _, leafA, _ := authorityFixture(t)
	caB, _, _, _ := authorityFixture(t)

	both := string(hopca.Bundle([]byte(caA), []byte(caB)))

	tests := []struct {
		name  string
		state CAState
		want  CAPhase
	}{
		{
			name:  "one authority, anchored, signing",
			state: CAState{Active: caA, LeafCert: leafA, Anchor: caA},
			want:  CAPhaseSteady,
		},
		{
			name:  "incoming authority exists but nothing trusts it",
			state: CAState{Active: caA, Pending: caB, LeafCert: leafA, Anchor: caA},
			want:  CAPhaseStaged,
		},
		{
			name:  "incoming authority is trusted, still signing nothing",
			state: CAState{Active: caA, Pending: caB, LeafCert: leafA, Anchor: both},
			want:  CAPhaseExpanded,
		},
		{
			name:  "incoming has taken over, outgoing still trusted",
			state: CAState{Active: caB, Retained: caA, LeafCert: leafA, Anchor: both},
			want:  CAPhasePromoted,
		},
		{
			name:  "trust narrowed, outgoing not yet destroyed",
			state: CAState{Active: caB, Retained: caA, LeafCert: leafA, Anchor: caB},
			want:  CAPhaseNarrowed,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.Phase(); got != tt.want {
				t.Errorf("phase = %q, want %q", got, tt.want)
			}
		})
	}
}

// Every state a replacement can stop in must name where to carry on. Without
// this an operator reads "a replacement is part-way through (expanded)" and is
// given nothing to act on, which is the state the whole status exists to avoid.
func TestEveryStateInFlightNamesAResumePoint(t *testing.T) {
	caA, caKeyA, leafA, leafKeyA := authorityFixture(t)
	caB, caKeyB, _, _ := authorityFixture(t)
	both := string(hopca.Bundle([]byte(caA), []byte(caB)))
	leafUnderB := mustSignUnder(t, caB, caKeyB, leafKeyA)

	inFlight := []CAState{
		{Active: caA, ActiveKey: caKeyA, Pending: caB, PendingKey: caKeyB, LeafCert: leafA, LeafKey: leafKeyA, Anchor: caA},
		{Active: caA, ActiveKey: caKeyA, Pending: caB, PendingKey: caKeyB, LeafCert: leafA, LeafKey: leafKeyA, Anchor: both},
		{Active: caB, ActiveKey: caKeyB, Retained: caA, LeafCert: leafA, LeafKey: leafKeyA, Anchor: both},
		{Active: caB, ActiveKey: caKeyB, Retained: caA, LeafCert: leafUnderB, LeafKey: leafKeyA, Anchor: both},
		{Active: caB, ActiveKey: caKeyB, Retained: caA, LeafCert: leafUnderB, LeafKey: leafKeyA, Anchor: caB},
	}
	for _, state := range inFlight {
		if state.ResumePoint() == "" {
			t.Errorf("a cluster at %q names no resume point", state.Phase())
		}
	}

	steady := CAState{Active: caA, ActiveKey: caKeyA, LeafCert: leafA, LeafKey: leafKeyA, Anchor: caA}
	if steady.ResumePoint() != "" {
		t.Error("a steady cluster must offer nothing to resume")
	}

	// The authority moving and the certificate following it are two steps that
	// leave the same keys behind, so the phase cannot tell them apart. Sending
	// an operator to confirm the wire before the certificate has been installed
	// is a wait that never ends.
	beforeLeaf := CAState{Active: caB, ActiveKey: caKeyB, Retained: caA, LeafCert: leafA, LeafKey: leafKeyA, Anchor: both}
	afterLeaf := CAState{Active: caB, ActiveKey: caKeyB, Retained: caA, LeafCert: leafUnderB, LeafKey: leafKeyA, Anchor: both}
	if beforeLeaf.Phase() != afterLeaf.Phase() {
		t.Fatal("this test is meaningless unless both states share a phase")
	}
	if beforeLeaf.ResumePoint() == afterLeaf.ResumePoint() {
		t.Errorf("both halves of the promotion resume at %q; the certificate's signature distinguishes them", beforeLeaf.ResumePoint())
	}
}

// The invariant behind the order of the documented procedure: the authorities
// the API server trusts must always cover whatever signed the certificate the
// cluster serves. Trust widens before the signature moves and narrows only
// after, and a sequence that violates this locks every operator out of the
// login path.
func TestTrustAlwaysCoversTheSigningAuthority(t *testing.T) {
	caA, keyA, _, leafKey := authorityFixture(t)
	caB, keyB, _, _ := authorityFixture(t)

	// The leaf as it exists before and after the signature moves. The key is
	// the same one throughout, which is the property the gateway pin relies on.
	leafUnderA, err := hopca.SignLeaf([]byte(caA), []byte(keyA), []byte(leafKey), hostnames.GatewayDomain)
	if err != nil {
		t.Fatalf("signing under the outgoing authority: %v", err)
	}
	leafUnderB, err := hopca.SignLeaf([]byte(caB), []byte(keyB), []byte(leafKey), hostnames.GatewayDomain)
	if err != nil {
		t.Fatalf("signing under the incoming authority: %v", err)
	}
	both := string(hopca.Bundle([]byte(caA), []byte(caB)))

	// Each entry is the cluster as it stands at the end of one phase.
	sequence := []struct {
		phase  CAPhase
		anchor string
		leaf   []byte
	}{
		{CAPhaseSteady, caA, leafUnderA},
		{CAPhaseStaged, caA, leafUnderA},
		{CAPhaseExpanded, both, leafUnderA},
		{CAPhasePromoted, both, leafUnderB},
		{CAPhaseNarrowed, caB, leafUnderB},
		{CAPhaseSteady, caB, leafUnderB},
	}

	for _, step := range sequence {
		if !hopca.SignedByAny(step.leaf, []byte(step.anchor)) {
			t.Errorf("at %s the cluster serves a certificate no trusted authority signed", step.phase)
		}
	}
}

// Malformed material must stop the operator with an explanation rather than be
// read as progress through a replacement. Each of these would destroy something
// load-bearing if the procedure continued through it.
func TestMalformedMaterialRefusesRatherThanGuessing(t *testing.T) {
	caA, caKeyA, leafA, leafKeyA := authorityFixture(t)
	caB, caKeyB, unrelatedLeaf, unrelatedKey := authorityFixture(t)
	sound := CAState{
		Active: caA, ActiveKey: caKeyA, LeafCert: leafA, LeafKey: leafKeyA,
		Anchor: caA, DexHosts: []string{"demo.kipper.run"},
	}

	if got := sound.Anomalies(); len(got) != 0 {
		t.Fatalf("a sound cluster reported problems: %v", got)
	}

	tests := []struct {
		name    string
		mutate  func(*CAState)
		wantSub string
	}{
		{
			name:    "authority with no key cannot sign",
			mutate:  func(s *CAState) { s.ActiveKey = "" },
			wantSub: "missing its certificate or its key",
		},
		{
			name:    "no hop key would force a new one and move the pin",
			mutate:  func(s *CAState) { s.LeafKey = "" },
			wantSub: "gateway pins",
		},
		{
			name:    "half-written incoming authority",
			mutate:  func(s *CAState) { s.Pending = caB },
			wantSub: "half-written",
		},
		{
			name: "incoming and outgoing at once is not a reachable replacement",
			mutate: func(s *CAState) {
				s.Pending, s.PendingKey, s.Retained = caB, caKeyB, caB
			},
			wantSub: "both an incoming and an outgoing",
		},
		{
			name:    "served certificate chains to nothing this cluster holds",
			mutate:  func(s *CAState) { s.LeafCert = unrelatedLeaf },
			wantSub: "signed by none of the authorities it holds",
		},
		{
			name:    "no issuer configured",
			mutate:  func(s *CAState) { s.DexHosts = nil },
			wantSub: "names no issuer",
		},
		{
			name:    "a staged hop key would fight the authority replacement",
			mutate:  func(s *CAState) { s.Candidate = leafA },
			wantSub: "replacement hop key is staged",
		},
		{
			name:    "authority key does not match its certificate",
			mutate:  func(s *CAState) { s.ActiveKey = caKeyB },
			wantSub: "authority's private key does not match",
		},
		{
			name:    "hop key does not match its certificate",
			mutate:  func(s *CAState) { s.LeafKey = unrelatedKey },
			wantSub: "would move the public key the gateway pins",
		},
		{
			name: "incoming authority is not a certificate authority",
			mutate: func(s *CAState) {
				s.Pending, s.PendingKey = unrelatedLeaf, unrelatedKey
			},
			wantSub: "not a certificate authority",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := sound
			tt.mutate(&state)
			got := state.Anomalies()
			if len(got) == 0 {
				t.Fatal("expected a refusal, got none")
			}
			if !strings.Contains(strings.Join(got, "\n"), tt.wantSub) {
				t.Errorf("expected a problem mentioning %q, got: %v", tt.wantSub, got)
			}
		})
	}
}

// Stopping between promoting the authority and re-signing the certificate is a
// reachable, safe boundary in the documented procedure: the cluster serves a
// certificate the outgoing authority signed, both are still trusted, and
// carrying on finishes the job. Classifying it as malformed would tell the
// operator their cluster is broken at the exact moment it is not.
func TestInterruptedPromotionIsRecoverableNotMalformed(t *testing.T) {
	caA, caKeyA, leafA, leafKeyA := authorityFixture(t)
	caB, caKeyB, _, _ := authorityFixture(t)

	// Promoted the authority, not yet re-signed the leaf.
	afterPromote := CAState{
		Active: caB, ActiveKey: caKeyB, Retained: caA,
		LeafCert: leafA, LeafKey: leafKeyA,
		Anchor:   string(hopca.Bundle([]byte(caB), []byte(caA))),
		DexHosts: []string{"demo.kipper.run"},
	}
	if got := afterPromote.Anomalies(); len(got) != 0 {
		t.Errorf("an interrupted promotion must be recoverable, got refusals: %v", got)
	}
	if got := afterPromote.Phase(); got != CAPhasePromoted {
		t.Errorf("phase = %q, want %q", got, CAPhasePromoted)
	}

	// The mirror case, if the writes had landed the other way round: the
	// incoming authority signs the leaf while the Secret still names the
	// outgoing one. Pending counts toward what the leaf may chain to.
	resignedFirst := CAState{
		Active: caA, ActiveKey: caKeyA, Pending: caB, PendingKey: caKeyB,
		LeafCert: mustSignUnder(t, caB, caKeyB, leafKeyA), LeafKey: leafKeyA,
		Anchor:   string(hopca.Bundle([]byte(caA), []byte(caB))),
		DexHosts: []string{"demo.kipper.run"},
	}
	if got := resignedFirst.Anomalies(); len(got) != 0 {
		t.Errorf("a leaf signed by the pending authority must be recoverable, got refusals: %v", got)
	}
}

func mustSignUnder(t *testing.T, caPEM, caKeyPEM, leafKeyPEM string) string {
	t.Helper()
	signed, err := hopca.SignLeaf([]byte(caPEM), []byte(caKeyPEM), []byte(leafKeyPEM), hostnames.GatewayDomain)
	if err != nil {
		t.Fatalf("signing fixture leaf: %v", err)
	}
	return string(signed)
}

// The status may only offer a command that can actually fix what it found.
// Sync re-renders the authentication config from the anchor already on disk, so
// it repairs an anchor the API server never loaded and can do nothing at all
// about an anchor naming the wrong authority — sending an operator there while
// logins are down costs them the one thing they do not have.
func TestSyncIsOfferedOnlyWhenItCanRepairTheProblem(t *testing.T) {
	tests := []struct {
		name   string
		status CAStatus
		want   string
	}{
		{
			name:   "written but never loaded",
			status: CAStatus{AnchorCovers: true, AnchorLoaded: false},
			want:   "kip cluster auth sync",
		},
		{
			name:   "anchor names the wrong authority",
			status: CAStatus{AnchorCovers: false, AnchorLoaded: false},
			want:   "",
		},
		{
			name:   "malformed material comes first",
			status: CAStatus{AnchorCovers: true, AnchorLoaded: false, Problems: []string{"the authority is missing its key"}},
			want:   "",
		},
		{
			name:   "nothing to do",
			status: CAStatus{AnchorCovers: true, AnchorLoaded: true, TrustedByAPIServer: true},
			want:   "",
		},
		{
			name:   "the API server never answered",
			status: CAStatus{AnchorCovers: true, AnchorLoaded: false, AnchorLoadedUnknown: true},
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.status.NextCommand(); got != tt.want {
				t.Errorf("next command = %q, want %q", got, tt.want)
			}
		})
	}
}

// A domain change rewrites the same authentication config a replacement does,
// so the two must never overlap. The procedure tells an operator that a clean
// status is their whole preflight, which is only true if the status actually
// asks this question.
func TestADomainChangeInFlightStopsTheProcedure(t *testing.T) {
	caA, caKeyA, leafA, leafKeyA := authorityFixture(t)
	sound := CAState{
		Active: caA, ActiveKey: caKeyA, LeafCert: leafA, LeafKey: leafKeyA,
		Anchor: caA, DexHosts: []string{"demo.kipper.run"},
	}

	inFlight := sound
	inFlight.DomainTransition = "AwaitingApproval"
	if !mentions(inFlight.Anomalies(), "domain change is in flight") {
		t.Errorf("a cutover in flight must stop the procedure, got: %v", inFlight.Anomalies())
	}

	// A check that could not run must say so. Reporting nothing would tell the
	// operator every precondition passed when one of them was never asked.
	unknown := sound
	unknown.DomainTransitionUnknown = true
	if !mentions(unknown.Anomalies(), "could not be checked") {
		t.Errorf("an unanswerable cutover check must be reported, got: %v", unknown.Anomalies())
	}

	if got := sound.Anomalies(); len(got) != 0 {
		t.Errorf("no cutover and a clean check must report nothing, got: %v", got)
	}
}

// Damaged material is when an operator most needs the rest of the picture. The
// status must name the broken slot and carry on, rather than failing the whole
// command with a parse error that names nothing.
func TestUnreadableMaterialIsNamedRatherThanCrashingTheReport(t *testing.T) {
	caA, caKeyA, leafA, leafKeyA := authorityFixture(t)
	damaged := CAState{
		Active:    "-----BEGIN CERTIFICATE-----\nnot a certificate\n-----END CERTIFICATE-----",
		ActiveKey: caKeyA, LeafCert: leafA, LeafKey: leafKeyA,
		Anchor: caA, DexHosts: []string{"demo.kipper.run"},
	}

	got := damaged.Anomalies()
	if !mentions(got, "the authority is not a readable certificate") {
		t.Errorf("the unreadable slot must be named, got: %v", got)
	}
	// The key-match check reads that same certificate, so it can only report a
	// mismatch it cannot explain. Saying it twice sends the operator after a key
	// that is not the problem.
	if mentions(got, "private key does not match") {
		t.Errorf("an unreadable certificate must not also be reported as a key mismatch, got: %v", got)
	}

	if s := summarise(damaged.Active); s.Subject != "unreadable" {
		t.Errorf("summary of unreadable material = %q, want it to degrade rather than fail", s.Subject)
	}
}

func mentions(problems []string, substring string) bool {
	return strings.Contains(strings.Join(problems, "\n"), substring)
}

// The status sends an operator to a numbered step of the published procedure.
// If the page is renumbered or reordered and this is not, the command directs
// them to a step that no longer exists, at the worst possible moment.
func TestResumePointsMatchTheDocumentedProcedure(t *testing.T) {
	page, err := os.ReadFile("../../../docs/en/certificate-authority.md")
	if err != nil {
		t.Fatalf("reading the published procedure: %v", err)
	}
	docs := string(page)

	caA, caKeyA, leafA, leafKeyA := authorityFixture(t)
	caB, caKeyB, _, _ := authorityFixture(t)
	both := string(hopca.Bundle([]byte(caA), []byte(caB)))
	leafUnderB := mustSignUnder(t, caB, caKeyB, leafKeyA)

	tests := []struct {
		name     string
		state    CAState
		mentions string
		// headings are the destinations, each with a word that heading must
		// carry. Checking the word as well as the number means renumbering a
		// step or reordering the phases fails here, rather than sending an
		// operator to a heading that kept its number and changed its job.
		headings []docHeading
	}{
		{
			name:     "an incoming authority is stored but nothing trusts it",
			state:    CAState{Active: caA, ActiveKey: caKeyA, Pending: caB, PendingKey: caKeyB, LeafCert: leafA, LeafKey: leafKeyA, Anchor: caA},
			mentions: "step 1.4",
			headings: []docHeading{{"### 1.4 ", "anchor"}},
		},
		{
			name:     "both authorities are trusted, the old one still signs",
			state:    CAState{Active: caA, ActiveKey: caKeyA, Pending: caB, PendingKey: caKeyB, LeafCert: leafA, LeafKey: leafKeyA, Anchor: both},
			mentions: "step 2.2",
			headings: []docHeading{{"### 2.2 ", "promote"}},
		},
		{
			name:     "the authority moved but the certificate has not followed",
			state:    CAState{Active: caB, ActiveKey: caKeyB, Retained: caA, LeafCert: leafA, LeafKey: leafKeyA, Anchor: both},
			mentions: "step 2.3",
			headings: []docHeading{{"### 2.3 ", "install"}},
		},
		{
			name:     "the certificate has followed the authority",
			state:    CAState{Active: caB, ActiveKey: caKeyB, Retained: caA, LeafCert: leafUnderB, LeafKey: leafKeyA, Anchor: both},
			mentions: "gate 2",
			headings: []docHeading{{"### Gate 2", "gate 2"}},
		},
		{
			name:     "trust is narrowed, the old authority is not yet destroyed",
			state:    CAState{Active: caB, ActiveKey: caKeyB, Retained: caA, LeafCert: leafUnderB, LeafKey: leafKeyA, Anchor: caB},
			mentions: "gate 3",
			headings: []docHeading{{"### Gate 3", "gate 3"}, {"### 3.2 ", "destroy"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resume := tt.state.ResumePoint()
			if !strings.Contains(strings.ToLower(resume), tt.mentions) {
				t.Errorf("resume point %q no longer names %q", resume, tt.mentions)
			}
			for _, h := range tt.headings {
				line, found := headingLine(docs, h.prefix)
				if !found {
					t.Errorf("the procedure has no %q heading, so %q sends an operator nowhere", h.prefix, resume)
					continue
				}
				if !strings.Contains(strings.ToLower(line), h.means) {
					t.Errorf("heading %q does not mention %q, so it is no longer the step %q sends an operator to",
						strings.TrimSpace(line), h.means, resume)
				}
			}
		})
	}

	// The published resume table has to list every stopping point, or an
	// operator who walked away finds their state missing from it.
	for _, phase := range []CAPhase{CAPhaseStaged, CAPhaseExpanded, CAPhasePromoted, CAPhaseNarrowed} {
		if !strings.Contains(docs, "("+string(phase)+")") {
			t.Errorf("the procedure's resume table does not mention the %q phase", phase)
		}
	}
}

// An unanswered question is not a passed check. Reporting "nothing to do" on
// the strength of a probe that never ran is how an operator stops looking at a
// cluster that still needs them.
func TestAnUnansweredCheckIsNotAPass(t *testing.T) {
	answered := CAStatus{
		Phase: CAPhaseSteady, AnchorCovers: true, AnchorLoaded: true, TrustedByAPIServer: true,
	}
	if !answered.Healthy() {
		t.Fatal("a cluster whose checks all passed must read as healthy")
	}

	unanswered := answered
	unanswered.AnchorLoadedUnknown = true
	if unanswered.Healthy() {
		t.Error("a cluster whose API server did not answer must not read as healthy")
	}
	if got := unanswered.NextCommand(); got != "" {
		t.Errorf("next command = %q, want nothing offered on the strength of a check that did not run", got)
	}
}

// A re-render must keep exactly the issuers the API server already trusts.
// Taking them from the running config rather than from the caller is what
// stops a repair from silently redirecting authentication.
func TestIssuersAreReadBackFromTheRunningConfig(t *testing.T) {
	config := `apiVersion: apiserver.config.k8s.io/v1
kind: AuthenticationConfiguration
jwt:
  - issuer:
      url: https://demo.kipper.run/dex
      certificateAuthority: |
        -----BEGIN CERTIFICATE-----
        -----END CERTIFICATE-----
      audiences:
        - kipper-cli
  - issuer:
      url: https://console.example.com/dex
      audiences:
        - kipper-cli
`
	got := parseAuthnHosts(config)
	want := []string{"demo.kipper.run", "console.example.com"}

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("issuer %d = %q, want %q", i, got[i], want[i])
		}
	}
	if len(parseAuthnHosts("")) != 0 {
		t.Error("an empty config must yield no issuers rather than one empty host")
	}
}

// The wire check must ask the gateway-fronted host, verifying against the
// candidate authority rather than the system trust store: no public authority
// signed the hop certificate, so a default verifier can never accept it.
func TestWireCheckVerifiesAgainstTheGivenAuthority(t *testing.T) {
	cmd := servedByCmd("demo.kipper.run")

	if !strings.Contains(cmd, `--cacert "$tmp"`) {
		t.Error("the check must verify against the supplied authority, not the system trust store")
	}
	if !strings.Contains(cmd, "trap 'rm -f") {
		t.Error("the temporary anchor must be removed however the command exits")
	}
	if !strings.Contains(cmd, "-o /dev/null") {
		t.Error("the discovery document should not be echoed back over the SSH channel")
	}
}

// The status is the last step of a procedure someone runs because a private key
// leaked. Asking only whether the served certificate still verifies against the
// anchor cannot see an authority nobody meant to keep, so both routes to that
// state used to end with "Everything agrees. Nothing to do." while the leaked
// authority was still accepted for logins.
func TestAnAuthorityTheClusterDoesNotHoldIsReported(t *testing.T) {
	leaked, leakedKey, leafUnderLeaked, leafKey := authorityFixture(t)
	replacement, replacementKey, _, _ := authorityFixture(t)
	leafUnderReplacement := mustSignUnder(t, replacement, replacementKey, leafKey)
	bothAnchored := string(hopca.Bundle([]byte(replacement), []byte(leaked)))

	// Trust was never narrowed, but the outgoing authority was destroyed —
	// either by doing step 3.2 without 3.1, or by an empty capture in the patch
	// that was meant to record it. The phase derives as steady either way.
	stranded := CAState{
		Active: replacement, ActiveKey: replacementKey,
		LeafCert: leafUnderReplacement, LeafKey: leafKey,
		Anchor: bothAnchored, DexHosts: []string{"demo.kipper.run"},
	}
	if got := stranded.Phase(); got != CAPhaseSteady {
		t.Fatalf("phase = %q, want steady — this is the state that reads as finished", got)
	}
	if !hopca.SignedByAny([]byte(leafUnderReplacement), []byte(bothAnchored)) {
		t.Fatal("the served certificate must still verify, or the weaker check would have caught this")
	}
	if !mentions(stranded.Anomalies(), "this cluster does not hold") {
		t.Errorf("the leaked authority is still anchored and was not reported: %v", stranded.Anomalies())
	}
	if !mentions(stranded.Anomalies(), shortFingerprint(leaked)) {
		t.Errorf("the report must name which authority, got: %v", stranded.Anomalies())
	}

	// Every legitimate phase holds what it anchors, and none of them may trip
	// this. A false alarm here sends an operator narrowing trust mid-procedure,
	// which is the one move that locks everyone out.
	legitimate := []struct {
		name  string
		state CAState
	}{
		{"steady", CAState{Active: leaked, ActiveKey: leakedKey, LeafCert: leafUnderLeaked, LeafKey: leafKey, Anchor: leaked, DexHosts: []string{"demo.kipper.run"}}},
		{"expanded", CAState{Active: leaked, ActiveKey: leakedKey, Pending: replacement, PendingKey: replacementKey, LeafCert: leafUnderLeaked, LeafKey: leafKey, Anchor: bothAnchored, DexHosts: []string{"demo.kipper.run"}}},
		{"promoted", CAState{Active: replacement, ActiveKey: replacementKey, Retained: leaked, LeafCert: leafUnderReplacement, LeafKey: leafKey, Anchor: bothAnchored, DexHosts: []string{"demo.kipper.run"}}},
		{"narrowed", CAState{Active: replacement, ActiveKey: replacementKey, Retained: leaked, LeafCert: leafUnderReplacement, LeafKey: leafKey, Anchor: replacement, DexHosts: []string{"demo.kipper.run"}}},
		{"finished", CAState{Active: replacement, ActiveKey: replacementKey, LeafCert: leafUnderReplacement, LeafKey: leafKey, Anchor: replacement, DexHosts: []string{"demo.kipper.run"}}},
	}
	for _, tt := range legitimate {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.state.Anomalies(); len(got) != 0 {
				t.Errorf("a legitimate %s cluster reported problems: %v", tt.name, got)
			}
		})
	}
}

// A domain cutover and an authority replacement rewrite the same trust config
// and disagree about what it should hold, so the cutover gate never matches and
// parks. The status already refuses to start a replacement during a cutover;
// this is the same rule in the direction that was missing.
func TestACutoverIsRefusedWhileAnAuthorityIsBeingReplaced(t *testing.T) {
	caA, _, _, _ := authorityFixture(t)

	newSecret := func(data map[string][]byte) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: hopCASecret, Namespace: hopNamespace},
			Data:       data,
		}
	}

	tests := []struct {
		name    string
		objects []runtime.Object
		refuses bool
	}{
		{
			name:    "an incoming authority is staged",
			objects: []runtime.Object{newSecret(map[string][]byte{"tls.crt": []byte(caA), pendingCACertKey: []byte(caA)})},
			refuses: true,
		},
		{
			name:    "an outgoing authority is still trusted",
			objects: []runtime.Object{newSecret(map[string][]byte{"tls.crt": []byte(caA), retainedCAKey: []byte(caA)})},
			refuses: true,
		},
		{
			name:    "nothing in flight",
			objects: []runtime.Object{newSecret(map[string][]byte{"tls.crt": []byte(caA)})},
			refuses: false,
		},
		{
			name:    "a cluster with no authority secret at all",
			objects: nil,
			refuses: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RefuseDuringCAReplacement(context.Background(), fake.NewSimpleClientset(tt.objects...), nil)
			if tt.refuses && err == nil {
				t.Error("the operation must be refused while a replacement is in flight")
			}
			if !tt.refuses && err != nil {
				t.Errorf("nothing is in flight, so this must proceed: %v", err)
			}
			if tt.refuses && err != nil && !strings.Contains(err.Error(), "kip cluster ca status") {
				t.Errorf("the refusal must say how to find the next step, got: %v", err)
			}
		})
	}
}

// docHeading is a heading in the published procedure and a word it must carry.
type docHeading struct{ prefix, means string }

// headingLine returns the whole heading line starting with prefix.
func headingLine(docs, prefix string) (string, bool) {
	for _, line := range strings.Split(docs, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line, true
		}
	}
	return "", false
}

// The keys that mark a replacement in flight are cleared before the damage they
// cause is. A replacement finished out of order leaves an anchor naming an
// authority the cluster no longer holds, and every future cutover then parks on
// a gate that cannot pass — pointing the operator at a resync that reads the
// same file and produces the same rejected result.
func TestACutoverIsRefusedWhenTheAnchorDisagreesWithWhatTheClusterHolds(t *testing.T) {
	caA, _, _, _ := authorityFixture(t)
	caB, _, _, _ := authorityFixture(t)

	tests := []struct {
		name    string
		secret  map[string][]byte
		onHost  string
		refuses bool
	}{
		{
			name:   "the outgoing authority was destroyed without narrowing the anchor",
			secret: map[string][]byte{"tls.crt": []byte(caB)},
			onHost: string(hopca.Bundle([]byte(caB), []byte(caA))), refuses: true,
		},
		{
			name:   "steady and in agreement",
			secret: map[string][]byte{"tls.crt": []byte(caB)},
			onHost: caB, refuses: false,
		},
		{
			name:   "mid-replacement, both held and both anchored",
			secret: map[string][]byte{"tls.crt": []byte(caB), retainedCAKey: []byte(caA)},
			onHost: string(hopca.Bundle([]byte(caB), []byte(caA))), refuses: false,
		},
		{
			name:   "agreement apart from whitespace",
			secret: map[string][]byte{"tls.crt": []byte(caB)},
			onHost: "\n\n" + caB + "  \n", refuses: false,
		},
		{
			name:   "no anchor on the host at all is not this check's business",
			secret: map[string][]byte{"tls.crt": []byte(caB)},
			onHost: "", refuses: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := newFakeCluster(t)
			cluster.files[hopCAPath] = tt.onHost
			err := refuseWhenAnchorDisagreesWithSecret(tt.secret, cluster)
			if tt.refuses && err == nil {
				t.Error("this cutover would park on a gate that cannot pass, and was allowed to start")
			}
			if !tt.refuses && err != nil {
				t.Errorf("the anchor agrees with what the cluster holds, so this must proceed: %v", err)
			}
		})
	}
}

// A cluster with no readable active authority must not be told to narrow its
// anchor. Every fingerprint is unknown in that state, so every certificate in
// the anchor reads as surplus, and the advice names a list that is empty —
// following it destroys the last copy of the authority. An absent tls.crt is
// the reachable form: it is what an empty capture in a merge patch leaves.
func TestNoSurplusAdviceWithoutAReadableAuthority(t *testing.T) {
	caA, _, leafA, leafKeyA := authorityFixture(t)
	caB, _, _, _ := authorityFixture(t)
	anchor := string(hopca.Bundle([]byte(caA), []byte(caB)))

	for _, active := range []string{"", "-----BEGIN CERTIFICATE-----\nnope\n-----END CERTIFICATE-----"} {
		state := CAState{
			Active: active, LeafCert: leafA, LeafKey: leafKeyA,
			Anchor: anchor, DexHosts: []string{"demo.kipper.run"},
		}
		if got := state.surplusAnchorAuthorities(); len(got) != 0 {
			t.Errorf("with active = %q the status advises narrowing to nothing: %v", active, got)
		}
		// The real problem must still be reported.
		if !mentions(state.Anomalies(), "missing its certificate or its key") &&
			!mentions(state.Anomalies(), "not a readable certificate") {
			t.Errorf("with active = %q the actual problem was not reported: %v", active, state.Anomalies())
		}
	}
}

// "The handshake said no" and "the check never ran" are different answers, and
// only the first is about the authority. Reporting an unreachable host as an
// authority mismatch tells an operator their cluster is not serving its own
// active authority, during a procedure they are running because a key leaked.
func TestTheWireCheckSeparatesARefusalFromAFailureToAsk(t *testing.T) {
	tests := []struct {
		name         string
		out          string
		wantServed   bool
		wantAnswered bool
	}{
		{"verified", "KIPPER_SERVED_VERIFIED\n", true, true},
		{"not signed by this authority", "curl: (60) SSL certificate problem\nKIPPER_SERVED_UNVERIFIED\n", false, true},
		{"no answer at all", "", false, false},
		{"noise only", "Warning: Permanently added host to known hosts.\n", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			served, answered := servedAnswer(tt.out)
			if served != tt.wantServed || answered != tt.wantAnswered {
				t.Errorf("servedAnswer(%q) = (%v, %v), want (%v, %v)",
					tt.out, served, answered, tt.wantServed, tt.wantAnswered)
			}
		})
	}
}

// The command has to answer in words. Exiting non-zero cannot distinguish a
// certificate no supplied authority signed — curl's 60 — from a host that could
// not be reached, and the caller has to tell those apart.
func TestTheWireCheckAnswersInWordsRatherThanExitCodes(t *testing.T) {
	cmd := servedByCmd("demo.kipper.run")

	if !strings.Contains(cmd, servedVerified) || !strings.Contains(cmd, servedUnverified) {
		t.Error("the check must say which of the two answers it found")
	}
	if !strings.Contains(cmd, `[ "$rc" -eq 60 ]`) {
		t.Error("only curl's 60 means the authority did not sign what is served")
	}
	if !strings.Contains(cmd, `exit "$rc"`) {
		t.Error("any other failure must stay a failure rather than reading as a refusal")
	}
}

// The generated shell is the thing that has to behave, so run it. A stub curl
// stands in for the real one and exits with the code being exercised: the
// script must turn 0 and 60 into answers and leave everything else a failure,
// because only the first two say anything about the certificate.
func TestTheWireCheckScriptTurnsOnlyRealAnswersIntoAnswers(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash is required to run the generated script")
	}

	tests := []struct {
		name     string
		curlExit int
		wantOut  string
		wantErr  bool
	}{
		{"served by this authority", 0, servedVerified, false},
		{"the served certificate did not verify", 60, servedUnverified, false},
		{"could not connect", 7, "", true},
		{"timed out", 28, "", true},
		{"resolution failed", 6, "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := t.TempDir()
			script := fmt.Sprintf("#!/bin/sh\nexit %d\n", tt.curlExit)
			//nolint:gosec // G306: the stub stands in for curl on PATH, so it has to be executable; 0600 would make it unrunnable. It lives and dies in the test's own temp dir.
			if err := os.WriteFile(filepath.Join(stub, "curl"), []byte(script), 0o700); err != nil {
				t.Fatalf("writing the stub: %v", err)
			}

			cmd := exec.Command("bash", "-c", servedByCmd("demo.kipper.run")) //nolint:gosec // the script under test is what this test exists to run
			cmd.Stdin = strings.NewReader("-----BEGIN CERTIFICATE-----\n")
			cmd.Env = append(os.Environ(), "PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"))
			out, err := cmd.CombinedOutput()

			if tt.wantErr {
				if err == nil {
					t.Fatalf("curl exit %d must stay a failure, got output %q", tt.curlExit, out)
				}
				if served, answered := servedAnswer(string(out)); answered {
					t.Errorf("curl exit %d must not read as an answer, got served=%v", tt.curlExit, served)
				}
				return
			}
			if err != nil {
				t.Fatalf("curl exit %d should have produced an answer: %v (%s)", tt.curlExit, err, out)
			}
			if !strings.Contains(string(out), tt.wantOut) {
				t.Errorf("curl exit %d gave %q, want it to contain %q", tt.curlExit, out, tt.wantOut)
			}
		})
	}
}
