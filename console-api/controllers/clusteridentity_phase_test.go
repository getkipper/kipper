package controllers

import (
	"testing"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/serving"
)

func TestAdvanceHappyPath(t *testing.T) {
	// DualServe waits until serving is ready, then moves to AwaitingApproval.
	if next := advance(phaseDualServe, phaseSignals{}); next != phaseDualServe {
		t.Fatalf("DualServe not ready should park, got %q", next)
	}
	if next := advance(phaseDualServe, phaseSignals{servingReady: true}); next != phaseAwaitingApproval {
		t.Fatalf("DualServe ready -> AwaitingApproval, got %q", next)
	}

	// AwaitingApproval needs serving-ready AND approval AND preconditions.
	stuck := []phaseSignals{
		{servingReady: true, approvalMatches: true},     // preconditions missing
		{servingReady: true, preconditionsMet: true},    // approval missing
		{approvalMatches: true, preconditionsMet: true}, // serving not ready
	}
	for i, s := range stuck {
		if next := advance(phaseAwaitingApproval, s); next != phaseAwaitingApproval {
			t.Fatalf("case %d: AwaitingApproval should park without all gates, got %q", i, next)
		}
	}
	if next := advance(phaseAwaitingApproval, phaseSignals{servingReady: true, approvalMatches: true, preconditionsMet: true}); next != phaseCuttingOver {
		t.Fatalf("AwaitingApproval fully gated -> CuttingOver, got %q", next)
	}

	// CuttingOver waits for rollouts, then Verifying.
	if next := advance(phaseCuttingOver, phaseSignals{}); next != phaseCuttingOver {
		t.Fatalf("CuttingOver without rollouts should park, got %q", next)
	}
	if next := advance(phaseCuttingOver, phaseSignals{cutoverRolloutsReady: true}); next != phaseVerifying {
		t.Fatalf("CuttingOver rollouts ready -> Verifying, got %q", next)
	}

	// Verifying -> Contracting on success.
	if next := advance(phaseVerifying, phaseSignals{verifyOK: true}); next != phaseContracting {
		t.Fatalf("Verifying ok -> Contracting, got %q", next)
	}

	// Contracting -> steady once the grace period elapsed.
	if next := advance(phaseContracting, phaseSignals{}); next != phaseContracting {
		t.Fatalf("Contracting should wait for grace period, got %q", next)
	}
	if next := advance(phaseContracting, phaseSignals{contractReady: true}); next != "" {
		t.Fatalf("Contracting ready -> steady (empty), got %q", next)
	}
}

// A revert is entered only after the issuer flip: a failure in CuttingOver or
// Verifying moves to Reverting (whose phase write persists before any reverse
// effect runs); nothing before the flip reverts.
func TestAdvanceRevertsOnlyAfterFlip(t *testing.T) {
	for _, phase := range []string{phaseCuttingOver, phaseVerifying} {
		if next := advance(phase, phaseSignals{verifyFailed: true}); next != phaseReverting {
			t.Fatalf("%s failure -> Reverting, got %q", phase, next)
		}
	}
	// A pre-flip failure signal must not revert (there is nothing to undo).
	for _, phase := range []string{phaseDualServe, phaseAwaitingApproval} {
		if next := advance(phase, phaseSignals{verifyFailed: true}); next == phaseReverting || next == phaseDegraded {
			t.Fatalf("%s must never revert, got %q", phase, next)
		}
	}
}

func TestAdvanceRevertingParksUntilComplete(t *testing.T) {
	// Reverting holds until the reverse rollout is proven, then parks Degraded.
	if next := advance(phaseReverting, phaseSignals{}); next != phaseReverting {
		t.Fatalf("Reverting without proof should park, got %q", next)
	}
	// A lingering failure signal must not bounce the revert anywhere else.
	if next := advance(phaseReverting, phaseSignals{verifyFailed: true}); next != phaseReverting {
		t.Fatalf("Reverting with a stale failure signal should park, got %q", next)
	}
	if next := advance(phaseReverting, phaseSignals{revertComplete: true}); next != phaseDegraded {
		t.Fatalf("Reverting complete -> Degraded, got %q", next)
	}
}

func TestAdvanceDegradedParks(t *testing.T) {
	// Degraded never advances itself, even with every green signal — only the
	// operator (new generation / rollback) leaves it, via the shell.
	all := phaseSignals{servingReady: true, approvalMatches: true, preconditionsMet: true, cutoverRolloutsReady: true, verifyOK: true, contractReady: true, revertComplete: true}
	if next := advance(phaseDegraded, all); next != phaseDegraded {
		t.Fatalf("Degraded must park, got %q", next)
	}
}

func TestIdentityIsNewMatchesFlipPhases(t *testing.T) {
	newIdentity := map[string]bool{
		phaseDualServe:        false,
		phaseAwaitingApproval: false,
		phaseCuttingOver:      true,
		phaseVerifying:        true,
		phaseContracting:      true,
		phaseReverting:        true, // a target reset mid-revert would fight the rollback
		phaseDegraded:         false,
	}
	for phase, want := range newIdentity {
		if got := identityIsNew(phase); got != want {
			t.Errorf("identityIsNew(%q) = %v, want %v", phase, got, want)
		}
	}
}

func TestServingPhaseForMapping(t *testing.T) {
	cases := map[string]serving.Phase{
		phaseDualServe:        serving.PhaseDualServe,
		phaseAwaitingApproval: serving.PhaseDualServe,
		phaseReverting:        serving.PhaseDualServe, // revert renders old identity, union hosts
		phaseDegraded:         serving.PhaseDualServe,
		phaseCuttingOver:      serving.PhaseCuttingOver,
		phaseVerifying:        serving.PhaseVerifying,
		phaseContracting:      serving.PhaseContracting,
		"":                    serving.PhaseSteady,
	}
	for crPhase, want := range cases {
		if got := servingPhaseFor(crPhase); got != want {
			t.Errorf("servingPhaseFor(%q) = %q, want %q", crPhase, got, want)
		}
	}
}

func TestResolvedFromHostSetFillsIssuer(t *testing.T) {
	hs := serving.ResolveHosts("acme.kipper.run", serving.Overrides{})
	got := resolvedFromHostSet(hs)
	if got.Dex != "dex--acme.kipper.run" || got.Issuer != "https://dex--acme.kipper.run/dex" {
		t.Fatalf("resolved hosts wrong: %+v", got)
	}
	if got.Console != "console--acme.kipper.run" {
		t.Fatalf("console host wrong: %+v", got)
	}
}

func TestOverridesFromHosts(t *testing.T) {
	if o := overridesFromHosts(nil); (o != serving.Overrides{}) {
		t.Fatalf("nil hosts should be empty overrides, got %+v", o)
	}
	o := overridesFromHosts(&kipperv1.IdentityHosts{Console: "console.example.com"})
	if o.Console != "console.example.com" || o.Dex != "" {
		t.Fatalf("override mapping wrong: %+v", o)
	}
}
