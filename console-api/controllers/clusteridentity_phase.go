package controllers

import (
	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/serving"
)

// ClusterIdentityName is the singleton CR name the reconciler acts on. Other
// names are ignored so a stray `kubectl create clusteridentity wrong` cannot
// drive the cluster's serving identity.
const ClusterIdentityName = "cluster"

// Transition phases, matching the CRD's status.transition.phase enum. The empty
// phase is steady state: no transition object at all.
const (
	phaseDualServe        = "DualServe"
	phaseAwaitingApproval = "AwaitingApproval"
	phaseCuttingOver      = "CuttingOver"
	phaseVerifying        = "Verifying"
	phaseContracting      = "Contracting"
	phaseReverting        = "Reverting"
	phaseDegraded         = "Degraded"
)

// phaseSignals are the observed facts the phase machine decides on. The
// reconcile shell gathers them from live state and the CR; advance keeps no
// state of its own, so a reconciler restart re-derives the same decision from
// the persisted phase plus fresh signals.
type phaseSignals struct {
	// servingReady means the union Ingresses for the transition are admitted and
	// their blocking certificates are ready. For *.kipper.run hosts a failed
	// per-host cert is not blocking (Caddy's wildcard terminates public TLS), so
	// the shell folds only custom-domain cert state into this.
	servingReady bool

	// approvalMatches means spec.cutoverApproval equals the recomputed hash for
	// the pending transition.
	approvalMatches bool

	// preconditionsMet gates the cutover on everything that must be true before
	// the issuer flip: the dex-oidc-client Secret exists and is non-empty, and
	// when connectors will be rehosted the operator has acknowledged the SSO
	// callback updates for this target (spec.ackSSOCallbacksFor).
	preconditionsMet bool

	// cutoverRolloutsReady means Dex and console-api have rolled onto the new
	// identity and report Ready.
	cutoverRolloutsReady bool

	// verifyOK means every post-cutover check passed: in-cluster discovery on the
	// new issuer, JWKS fetch, console-api OIDC health, rollouts Ready, and the
	// external CLI handshake.
	verifyOK bool

	// verifyFailed means a post-flip step definitively failed (a rollout timed
	// out, or a verification check went red). It sends the transition into
	// Reverting.
	verifyFailed bool

	// contractReady means the old hosts may be pruned now: any keepOldHostsUntil
	// grace period has elapsed.
	contractReady bool

	// revertComplete means Dex and console-api have finished rolling back onto
	// the previous identity, so the transition may park in Degraded.
	revertComplete bool
}

// advance is the pure transition function of the phase machine. Given the
// current phase and the observed signals it returns the next phase.
//
// The machine never proceeds on missing evidence: an un-ready or un-approved
// phase returns itself (park and wait). A post-flip failure (CuttingOver and
// Verifying) moves to Reverting, whose phase write is persisted before any
// reverse effect runs; before the flip nothing session-affecting has changed,
// so a stuck phase simply parks. The empty return phase means "transition
// complete, return to steady".
func advance(current string, s phaseSignals) (next string) {
	switch current {
	case phaseDualServe:
		if s.servingReady {
			return phaseAwaitingApproval
		}
		return phaseDualServe

	case phaseAwaitingApproval:
		if s.servingReady && s.approvalMatches && s.preconditionsMet {
			return phaseCuttingOver
		}
		return phaseAwaitingApproval

	case phaseCuttingOver:
		if s.verifyFailed {
			return phaseReverting
		}
		if s.cutoverRolloutsReady {
			return phaseVerifying
		}
		return phaseCuttingOver

	case phaseVerifying:
		if s.verifyFailed {
			return phaseReverting
		}
		if s.verifyOK {
			return phaseContracting
		}
		return phaseVerifying

	case phaseReverting:
		if s.revertComplete {
			return phaseDegraded
		}
		return phaseReverting

	case phaseContracting:
		if s.contractReady {
			return ""
		}
		return phaseContracting

	case phaseDegraded:
		// Parking state. It is left only by an operator: a new spec generation
		// starts a fresh transition, or --rollback patches spec to lastSteady.
		// Both are handled by the reconcile shell, not here.
		return phaseDegraded
	}
	return current
}

// identityIsNew reports whether a transition phase must not be reset toward a
// changed spec target: the issuer has flipped (CuttingOver onward), or the
// transition is rolling back and a reset would fight the revert. It mirrors
// the render engine's own phase table so the reconciler and serving.Render
// agree on when the session-invalidating change has happened.
func identityIsNew(phase string) bool {
	switch phase {
	case phaseCuttingOver, phaseVerifying, phaseContracting, phaseReverting:
		return true
	default:
		return false
	}
}

// servingPhaseFor maps a CR transition phase to the render engine's phase.
// Reverting and Degraded render as DualServe: the revert restores the old
// identity while the old hosts (which never stopped serving) and the new hosts
// both keep answering, so nothing a live client uses disappears.
func servingPhaseFor(crPhase string) serving.Phase {
	switch crPhase {
	case phaseDualServe, phaseAwaitingApproval, phaseReverting, phaseDegraded:
		return serving.PhaseDualServe
	case phaseCuttingOver:
		return serving.PhaseCuttingOver
	case phaseVerifying:
		return serving.PhaseVerifying
	case phaseContracting:
		return serving.PhaseContracting
	default:
		return serving.PhaseSteady
	}
}

// resolvedFromHostSet converts a resolved serving host set into the CR status
// shape, filling the issuer from the Dex host so the two can never disagree.
func resolvedFromHostSet(h serving.HostSet) *kipperv1.ResolvedHosts {
	return &kipperv1.ResolvedHosts{
		Console:    h.Console,
		ConsoleAPI: h.ConsoleAPI,
		Dex:        h.Dex,
		Issuer:     h.Issuer(),
	}
}

// overridesFromHosts maps the CR's optional host overrides into the serving
// package's Overrides. A nil hosts block means "derive everything".
func overridesFromHosts(h *kipperv1.IdentityHosts) serving.Overrides {
	if h == nil {
		return serving.Overrides{}
	}
	return serving.Overrides{Console: h.Console, ConsoleAPI: h.ConsoleAPI, Dex: h.Dex}
}
