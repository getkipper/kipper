package cmd

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/serving"
	"github.com/getkipper/kipper/kip/internal/clusteridentity"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// cutoverStager stages and contracts the API server's OIDC trust over SSH
// around a domain cutover: it widens the loopback pin and authentication
// config to trust both the old and new issuers before the flip, and tightens
// them to the new issuer after the cluster settles. Without staging the
// console-api park gate refuses the flip, so a stager is required to complete
// a cutover.
type cutoverStager struct {
	client     *ssh.Client
	fromDomain string
	toDomain   string
}

// stage widens API-server trust to both the old and new Dex issuers so the
// park gate passes and the operator's old-issuer token survives the flip.
func (s *cutoverStager) stage(fromDex, toDex string) error {
	fmt.Printf("  ...  Staging API server trust for the new issuer\n")
	if err := installer.EnsureOperatorAuth(s.client, fromDex, toDex); err != nil {
		return fmt.Errorf("staging authentication config: %w", err)
	}
	if err := installer.EnsureAdminBindingSubjects(s.client, "admin@"+s.fromDomain, "admin@"+s.toDomain); err != nil {
		return fmt.Errorf("staging admin binding: %w", err)
	}
	fmt.Printf("  ✔  API server trusts both issuers\n")
	return nil
}

// contract tightens API-server trust to the new issuer alone once the cutover
// has settled. A failure here leaves the cluster fully serving the new
// identity with the old issuer harmlessly still trusted; --sync tidies it.
func (s *cutoverStager) contract(toDex string) {
	if err := installer.EnsureOperatorAuth(s.client, toDex); err != nil {
		fmt.Printf("  ⚠  could not tighten API server trust to the new issuer (%v); run 'kip cluster domain --sync' to finish\n", err)
		return
	}
	if err := installer.EnsureAdminBindingSubjects(s.client, "admin@"+s.toDomain); err != nil {
		fmt.Printf("  ⚠  could not tighten the admin binding (%v); run 'kip cluster domain --sync' to finish\n", err)
	}
}

// dialCutoverStager opens an SSH connection to the cluster host for staging.
// A domain cutover cannot complete without it, so a dial failure is fatal and
// nothing has changed yet.
func dialCutoverStager(cluster *config.Cluster, fromDomain, toDomain string) (*cutoverStager, error) {
	if cluster.Host == "" {
		return nil, fmt.Errorf("no host recorded for cluster %s; a domain change needs SSH access to stage the API server's trust", cluster.Name)
	}
	explicit, fallback := resolveSSHKey("", cluster)
	client, err := ssh.Dial(ssh.Config{
		Host:            cluster.Host,
		User:            "root",
		KeyPath:         explicit,
		FallbackKeyPath: fallback,
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to %s over SSH: %w", cluster.Host, err)
	}
	return &cutoverStager{client: client, fromDomain: fromDomain, toDomain: toDomain}, nil
}

const (
	domainWatchInterval = 2 * time.Second
	domainWatchTimeout  = 15 * time.Minute
	domainProbeTimeout  = 10 * time.Second
	// reconcilerLivenessTimeout bounds how long to wait for the reconciler to
	// observe a spec change before deciding it is not running.
	reconcilerLivenessTimeout = 60 * time.Second
)

// runClusterDomainForward drives a base-domain change through the ClusterIdentity
// reconciler's no-lockout transition: it opens the change, waits for the cluster
// to dual-serve, verifies the new hosts from outside the cluster, approves the
// one session-invalidating cutover, and watches it through to steady state. The
// old hosts keep serving until the cutover, and a failed verification leaves the
// cluster untouched.
func runClusterDomainForward(cmd *cobra.Command, newDomain string) error {
	yes, _ := cmd.Flags().GetBool("yes")

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ci := clusteridentity.New(k8sClient.Dynamic())
	ctx := context.Background()

	current, err := ci.Get(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("this cluster has no ClusterIdentity yet; run 'kip upgrade' to deploy the serving-identity reconciler, then retry")
		}
		return fmt.Errorf("reading serving identity: %w", err)
	}

	if err := refuseDomainChangeDuringCAReplacement(ctx, cluster, k8sClient); err != nil {
		return err
	}

	// "Already serving" requires the resolved hosts to match too: after a
	// rollback the spec can name this domain while explicit host overrides
	// still steer serving elsewhere, and that state must fall through to a
	// real transition rather than print a false success.
	target := serving.ResolveHosts(newDomain, serving.Overrides{})
	if current.Spec.Domain == newDomain && current.Status.Transition == nil && targetMatches(target, current.Status.ActiveHosts) {
		fmt.Printf("\n  Already serving %s. Nothing to do.\n\n", newDomain)
		return nil
	}

	printDomainPlan(current, newDomain, target)
	if current.Spec.Hosts != nil {
		fmt.Printf("  The existing per-service host overrides are cleared by this move; the\n")
		fmt.Printf("  hosts above are derived from %s.\n", newDomain)
	}
	if !yes && !confirmRepair(cmd.InOrStdin()) {
		fmt.Println("\n  Aborted; nothing changed.")
		return nil
	}

	// Acquire the SSH staging channel before any durable change. The stager
	// pins API-server trust on the host during the cutover, and a dial
	// failure (missing key, root login disabled, port 22 down) must abort
	// with nothing claimed and nothing patched — otherwise the reconciler is
	// left mid-transition while the command reports failure, breaking the
	// "nothing changed" contract a retry relies on.
	stager, err := dialCutoverStager(cluster, cluster.Domain, newDomain)
	if err != nil {
		return err
	}
	defer func() { _ = stager.client.Close() }()

	// A move to a different *.kipper.run label needs the gateway to route it.
	// Claim the label first (before any cluster change) so a conflict aborts
	// cleanly; the old label is removed only once the cutover completes.
	//
	// hosts is cleared in the same patch: a plain forward move means "serve
	// on the hosts derived from the new domain", and leaving stale overrides
	// in spec would make the reconciler's target disagree with the hosts this
	// command prints, probes, and approves.
	patch := map[string]any{"domain": newDomain, "hosts": nil}
	gwPatch, err := beginGatewayMove(k8sClient.Clientset(), cluster, current, newDomain)
	if err != nil {
		return err
	}
	if gwPatch != nil {
		fmt.Printf("  ✔  Claimed %s\n", newDomain)
		patch["gateway"] = gwPatch
	}

	// The liveness barrier is the generation the reconciler must observe. Only a
	// host-changing patch raises it; a resume without a host change reuses the
	// generation the reconciler already observed when it opened the transition,
	// because the reconciler does not re-publish observedGeneration for non-host
	// edits (e.g. an earlier SSO acknowledgement) while parked at
	// AwaitingApproval. Overrides in spec make this a host change even when the
	// domain already matches: the patch clears them.
	barrierGen := current.Status.ObservedGeneration
	if current.Spec.Domain != newDomain || current.Spec.Hosts != nil {
		if err := ci.PatchSpec(ctx, patch); err != nil {
			return err
		}
		fmt.Printf("\n  ...  Requested %s; bringing up the new hosts alongside the old ones\n", newDomain)
		if barrierGen, err = currentGeneration(ctx, ci); err != nil {
			return err
		}
	}
	ack, _ := cmd.Flags().GetBool("ack-sso-callbacks")

	if err := driveCutover(ctx, ci, target, barrierGen, ack, newDomain, stager); err != nil {
		if gwPatch != nil {
			// The claimed subdomain, its token, and the pending cleanup stay
			// recorded in the cluster, so re-running (or --sync) resumes
			// without re-registering.
			fmt.Printf("      %s stays claimed for this cluster; re-run or use 'kip cluster domain --sync' to resume.\n", newDomain)
		}
		return err
	}
	finishMoveOrWarn(k8sClient.Clientset())
	return nil
}

// finishMoveOrWarn completes any gateway move recorded in the cluster. The
// cleanup is retryable from the persisted record, so a failure warns and
// points at --sync instead of failing a cutover that already succeeded.
func finishMoveOrWarn(clientset kubernetes.Interface) {
	if err := finishGatewayMove(clientset, ""); err != nil {
		fmt.Printf("  ⚠   %v\n      Run 'kip cluster domain --sync' to retry the cleanup.\n", err)
	}
}

// runClusterDomainSync drives whatever change is already in flight to completion:
// it resumes an interrupted cutover, or finishes the leftovers of a completed
// one — a gateway cleanup still recorded in the cluster, a stale local config —
// on an already-converged cluster. It takes no domain argument; the desired end
// state is the CR spec.
func runClusterDomainSync(cmd *cobra.Command) error {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ci := clusteridentity.New(k8sClient.Dynamic())
	ctx := context.Background()

	if err := refuseDomainChangeDuringCAReplacement(ctx, cluster, k8sClient); err != nil {
		return err
	}

	current, err := ci.Get(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("this cluster has no ClusterIdentity yet; run 'kip upgrade' first")
		}
		return fmt.Errorf("reading serving identity: %w", err)
	}
	if current.Status.Transition == nil {
		// Converged, but an interrupted run may still owe the gateway cleanup
		// and the local-config repair that normally happen after the cutover.
		// Sync's whole purpose is finishing that work, so a cleanup failure is
		// this command's failure — the record stays for the next retry.
		fmt.Printf("\n  Already converged on %s.\n\n", current.Spec.Domain)
		if err := writeLocalDomainConfig(); err != nil {
			return err
		}
		// The janitor also tightens any lingering union trust from an
		// abandoned or partially-contracted earlier move back to the active
		// issuer.
		if stager, derr := dialCutoverStager(cluster, cluster.Domain, current.Spec.Domain); derr == nil {
			stager.contract(serving.ResolveHosts(current.Spec.Domain, overridesOf(current.Spec.Hosts)).Dex)
			_ = stager.client.Close()
		}
		return finishGatewayMove(k8sClient.Clientset(), "")
	}

	target := serving.ResolveHosts(current.Spec.Domain, overridesOf(current.Spec.Hosts))
	fmt.Printf("\n  Resuming an in-flight change toward %s (phase %s)\n", current.Spec.Domain, current.Phase())
	ack, _ := cmd.Flags().GetBool("ack-sso-callbacks")

	stager, err := dialCutoverStager(cluster, cluster.Domain, current.Spec.Domain)
	if err != nil {
		return err
	}
	defer func() { _ = stager.client.Close() }()

	// Resume makes no host change, so the barrier is the generation the reconciler
	// already observed when it opened the transition, not the current metadata
	// generation (which a prior ack-only edit may have advanced past it).
	if err := driveCutover(ctx, ci, target, current.Status.ObservedGeneration, ack, current.Spec.Domain, stager); err != nil {
		return err
	}
	return finishGatewayMove(k8sClient.Clientset(), "")
}

// runClusterDomainRollback returns the cluster to the previous steady identity
// recorded at the last contraction, as a fresh transition in the opposite
// direction with the same no-lockout gates.
func runClusterDomainRollback(cmd *cobra.Command) error {
	yes, _ := cmd.Flags().GetBool("yes")

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ci := clusteridentity.New(k8sClient.Dynamic())
	ctx := context.Background()

	if err := refuseDomainChangeDuringCAReplacement(ctx, cluster, k8sClient); err != nil {
		return err
	}

	current, err := ci.Get(ctx)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("this cluster has no ClusterIdentity yet; run 'kip upgrade' first")
		}
		return fmt.Errorf("reading serving identity: %w", err)
	}
	ls := current.Status.LastSteady
	if ls == nil || ls.Domain == "" {
		return fmt.Errorf("no previous identity is recorded to roll back to")
	}
	if current.Status.Transition != nil {
		return fmt.Errorf("a change is already in flight (phase %s); let it settle or run 'kip cluster domain --sync' first", current.Phase())
	}

	target := serving.ResolveHosts(ls.Domain, overridesOf(ls.Hosts))
	fmt.Printf("\n  Rolling back to %s\n\n", ls.Domain)
	fmt.Printf("    To:    %s\n           %s\n           %s\n", target.Console, target.ConsoleAPI, target.Dex)
	fmt.Printf("\n  This is a normal cutover in the opposite direction. Login moves back to the\n")
	fmt.Printf("  previous hosts and open sessions sign in again once it completes.\n")
	if !yes && !confirmRepair(cmd.InOrStdin()) {
		fmt.Println("\n  Aborted; nothing changed.")
		return nil
	}

	// Acquire the SSH staging channel before any durable change, so a dial
	// failure aborts with nothing reclaimed and nothing patched (same
	// "nothing changed" contract as the forward path).
	stager, err := dialCutoverStager(cluster, cluster.Domain, ls.Domain)
	if err != nil {
		return err
	}
	defer func() { _ = stager.client.Close() }()

	patch := map[string]any{"domain": ls.Domain}
	if ls.Hosts != nil {
		patch["hosts"] = map[string]any{"console": ls.Hosts.Console, "consoleAPI": ls.Hosts.ConsoleAPI, "dex": ls.Hosts.Dex}
	} else {
		// Clear any stale overrides so the reconciler derives hosts from the
		// domain — the same target the CLI resolves, probes, and approves.
		patch["hosts"] = nil
	}
	// If the previous identity was a *.kipper.run label whose registration was
	// removed by the forward move, reclaim it before serving through it again.
	gwPatch, err := beginGatewayMove(k8sClient.Clientset(), cluster, current, ls.Domain)
	if err != nil {
		return err
	}
	if gwPatch != nil {
		fmt.Printf("  ✔  Reclaimed %s\n", ls.Domain)
		patch["gateway"] = gwPatch
	}
	if err := ci.PatchSpec(ctx, patch); err != nil {
		return err
	}
	barrierGen, err := currentGeneration(ctx, ci)
	if err != nil {
		return err
	}
	ack, _ := cmd.Flags().GetBool("ack-sso-callbacks")

	if err := driveCutover(ctx, ci, target, barrierGen, ack, ls.Domain, stager); err != nil {
		return err
	}
	finishMoveOrWarn(k8sClient.Clientset())
	return nil
}

// driveCutover runs the shared no-lockout cutover from a requested spec change:
// confirm the reconciler is responding, wait for it to dual-serve, clear the SSO
// acknowledgement gate, verify the new hosts from outside, approve the one
// session-invalidating cutover bound to the requested target, and watch it
// through to steady. It is used by the forward, sync, and rollback paths.
func driveCutover(ctx context.Context, ci *clusteridentity.Client, target serving.HostSet, barrierGen int64, ack bool, label string, stager *cutoverStager) error {
	// Fail fast if the reconciler is not processing the change — an old or absent
	// console-api, rather than a slow rollout.
	if err := waitReconcilerObserved(ctx, ci, barrierGen); err != nil {
		return err
	}

	gate, err := waitForApprovalGate(ctx, ci, barrierGen, target)
	if err != nil {
		return err
	}
	if gate == nil {
		fmt.Printf("\n  ✔  Serving %s\n\n", label)
		return writeLocalDomainConfig()
	}

	if needsSSOAck(gate) {
		if !ack {
			printSSOCallbackGuidance(gate)
			return fmt.Errorf("SSO callbacks need acknowledgement; update each provider, then re-run with --ack-sso-callbacks")
		}
		// The acknowledgement names the target Dex host, so it authorises this
		// move only; a later move to another host needs a fresh one.
		if err := ci.PatchSpec(ctx, map[string]any{"ackSSOCallbacksFor": gate.Status.Transition.To.Dex}); err != nil {
			return err
		}
		fmt.Printf("  ✔  SSO callback update acknowledged for %s\n", gate.Status.Transition.To.Dex)
	}

	fmt.Printf("  ...  Verifying the new hosts answer with a valid certificate\n")
	if err := probeTargets(gate.Status.Transition.To); err != nil {
		fmt.Printf("\n  ✗  Verification failed: %v\n", err)
		fmt.Printf("      The old hosts are still serving and nothing was cut over. Fix DNS/TLS for\n")
		fmt.Printf("      the new hosts, then re-run to resume.\n\n")
		return fmt.Errorf("pre-cutover verification failed")
	}
	fmt.Printf("  ✔  New hosts reachable\n")

	// Re-read and re-check the target before approving: the transition could have
	// been reset by a concurrent spec edit while the probes ran, and approving a
	// stale snapshot would cut over to a target the operator never verified.
	fresh, err := ci.Get(ctx)
	if err != nil {
		return fmt.Errorf("reading serving identity: %w", err)
	}
	if fresh.Phase() != clusteridentity.PhaseAwaitingApproval || !targetMatches(target, fresh.Status.Transition.To) {
		return fmt.Errorf("the pending change moved while verifying; re-run to retry")
	}
	approval, ok := fresh.PendingApproval()
	if !ok {
		return fmt.Errorf("no approvable cutover found; re-run to retry")
	}

	// Stage the API server's trust for the new issuer BEFORE approving the
	// flip. The console-api park gate refuses the flip until it observes this
	// staged, so without it the cutover would park. Staging is additive (the
	// union keeps the old issuer trusted), so the operator's own session
	// survives the flip and this step is safe to leave if a later step aborts.
	if stager != nil {
		if err := stager.stage(fresh.Status.Transition.From.Dex, fresh.Status.Transition.To.Dex); err != nil {
			return err
		}
	}

	if err := ci.PatchSpec(ctx, map[string]any{"cutoverApproval": approval}); err != nil {
		return err
	}
	fmt.Printf("  ...  Approved; cutting over\n")

	if err := waitForSteady(ctx, ci); err != nil {
		return err
	}

	// Tighten trust to the new issuer alone now that the cluster serves it.
	if stager != nil {
		stager.contract(target.Dex)
	}

	fmt.Printf("\n  ✔  Cutover complete. Serving %s\n", label)
	fmt.Printf("      Anyone with an open session signs in again on the new hosts.\n\n")
	return writeLocalDomainConfig()
}

// printDomainPlan shows the from/to identity and warns about the login impact.
func printDomainPlan(current *clusteridentity.ClusterIdentity, newDomain string, target serving.HostSet) {
	fmt.Printf("\n  Domain change to %s\n\n", newDomain)
	if a := current.Status.ActiveHosts; a != nil {
		fmt.Printf("    From:  %s\n", a.Console)
		fmt.Printf("           %s\n", a.ConsoleAPI)
		fmt.Printf("           %s\n", a.Dex)
	} else {
		fmt.Printf("    From:  (current serving identity)\n")
	}
	fmt.Printf("    To:    %s\n", target.Console)
	fmt.Printf("           %s\n", target.ConsoleAPI)
	fmt.Printf("           %s\n", target.Dex)
	fmt.Printf("\n  The old hosts keep serving until the cutover. The cutover moves the OIDC\n")
	fmt.Printf("  issuer, so every open session has to sign in again once it completes.\n")
	fmt.Printf("  Point DNS for the new hosts at this server before continuing.\n")
}

// waitForApprovalGate polls until the transition reaches AwaitingApproval
// (returned), converges with no cutover needed (nil), fails into Degraded
// (error), or times out. It ignores the phase until the reconciler has observed
// the requested generation, so it never mistakes the pre-transition steady state
// for a completed change.
func waitForApprovalGate(ctx context.Context, ci *clusteridentity.Client, targetGen int64, want serving.HostSet) (*clusteridentity.ClusterIdentity, error) {
	var gate *clusteridentity.ClusterIdentity
	err := pollDomain(ctx, ci, func(cur *clusteridentity.ClusterIdentity) (bool, error) {
		if cur.Status.ObservedGeneration < targetGen {
			return false, nil // the reconciler has not yet acted on the request
		}
		switch cur.Phase() {
		case "":
			return true, nil
		case clusteridentity.PhaseAwaitingApproval:
			// Bind to the requested target: if a concurrent edit reset the
			// transition to a different destination, refuse rather than probe and
			// approve something the operator never asked for.
			if !targetMatches(want, cur.Status.Transition.To) {
				return false, fmt.Errorf("the pending change is not the domain you requested (a concurrent edit changed it); re-run to retry")
			}
			gate = cur
			return true, nil
		case clusteridentity.PhaseDegraded:
			return false, degradedError(cur)
		default:
			return false, nil
		}
	})
	return gate, err
}

// currentGeneration re-reads the CR and returns its metadata.generation, the
// barrier the reconciler must observe before its phase can be trusted.
func currentGeneration(ctx context.Context, ci *clusteridentity.Client) (int64, error) {
	cur, err := ci.Get(ctx)
	if err != nil {
		return 0, fmt.Errorf("reading serving identity: %w", err)
	}
	return cur.Metadata.Generation, nil
}

// waitReconcilerObserved fails fast when the reconciler is not processing the
// change, distinguishing "reconciler not running / out of date" from a slow but
// live transition.
func waitReconcilerObserved(ctx context.Context, ci *clusteridentity.Client, barrierGen int64) error {
	deadline := time.Now().Add(reconcilerLivenessTimeout)
	for {
		cur, err := ci.Get(ctx)
		if err != nil {
			return fmt.Errorf("reading serving identity: %w", err)
		}
		if cur.Status.ObservedGeneration >= barrierGen {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the serving-identity reconciler is not responding to the change; run 'kip upgrade' to update console-api, then retry")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(domainWatchInterval):
		}
	}
}

// needsSSOAck reports whether the cutover is blocked on the operator confirming
// they have updated each SSO provider's callback URL.
func needsSSOAck(cur *clusteridentity.ClusterIdentity) bool {
	c := cur.Condition(clusteridentity.ConditionExternalCallbacks)
	return c != nil && c.Status == "False" && c.Reason == clusteridentity.ReasonNeedsAck
}

// printSSOCallbackGuidance tells the operator what to change in their external
// OAuth providers before the cutover can proceed — the one thing no cluster-side
// tool can do for them.
func printSSOCallbackGuidance(cur *clusteridentity.ClusterIdentity) {
	fmt.Printf("\n  ⚠   This cluster has SSO connectors. The cutover moves the Dex issuer, so each\n")
	fmt.Printf("      provider's allowed callback URL must be updated first.\n")
	if to := cur.Status.Transition.To; to != nil {
		fmt.Printf("      New callback URL: https://%s/dex/callback\n", to.Dex)
	}
	fmt.Printf("      Update every SSO provider, then re-run with --ack-sso-callbacks.\n\n")
}

// overridesOf maps CR host pins into the serving package's Overrides.
func overridesOf(h *clusteridentity.Hosts) serving.Overrides {
	if h == nil {
		return serving.Overrides{}
	}
	return serving.Overrides{Console: h.Console, ConsoleAPI: h.ConsoleAPI, Dex: h.Dex}
}

// targetMatches reports whether an in-flight transition's destination equals the
// host set the operator requested.
func targetMatches(want serving.HostSet, got *clusteridentity.ResolvedHosts) bool {
	if got == nil {
		return false
	}
	return got.Console == want.Console &&
		got.ConsoleAPI == want.ConsoleAPI &&
		got.Dex == want.Dex &&
		got.Issuer == want.Issuer()
}

// waitForSteady polls through the cutover until the transition clears, or fails
// into Degraded (auto-revert), or times out.
func waitForSteady(ctx context.Context, ci *clusteridentity.Client) error {
	return pollDomain(ctx, ci, func(cur *clusteridentity.ClusterIdentity) (bool, error) {
		switch cur.Phase() {
		case "":
			return true, nil
		case clusteridentity.PhaseDegraded:
			return false, degradedError(cur)
		default:
			return false, nil
		}
	})
}

// pollDomain reads the CR on an interval, printing each phase change, until the
// handler signals completion, returns an error, or the deadline passes.
func pollDomain(ctx context.Context, ci *clusteridentity.Client, handle func(*clusteridentity.ClusterIdentity) (bool, error)) error {
	deadline := time.Now().Add(domainWatchTimeout)
	lastPhase := "\x00" // sentinel that no real phase equals, so the first read prints
	for {
		cur, err := ci.Get(ctx)
		if err != nil {
			return fmt.Errorf("reading serving identity: %w", err)
		}
		if p := cur.Phase(); p != lastPhase {
			printPhase(p)
			lastPhase = p
		}
		done, err := handle(cur)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for the cluster (phase %q)", domainWatchTimeout, cur.Phase())
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(domainWatchInterval):
		}
	}
}

// printPhase narrates a transition phase in plain language.
func printPhase(phase string) {
	switch phase {
	case "":
		// Steady: no line; callers print their own completion message.
	case clusteridentity.PhaseDualServe:
		fmt.Printf("  ...  Serving old and new hosts\n")
	case clusteridentity.PhaseAwaitingApproval:
		fmt.Printf("  ...  New hosts up; ready to cut over\n")
	case clusteridentity.PhaseCuttingOver:
		fmt.Printf("  ...  Moving the login issuer to the new hosts\n")
	case clusteridentity.PhaseVerifying:
		fmt.Printf("  ...  Verifying the new issuer in-cluster\n")
	case clusteridentity.PhaseContracting:
		fmt.Printf("  ...  Removing the old hosts\n")
	case clusteridentity.PhaseDegraded:
		fmt.Printf("  ⚠   Reverted to the previous identity\n")
	default:
		fmt.Printf("  ...  %s\n", phase)
	}
}

// degradedError turns a Degraded transition into an actionable error using the
// Ready condition the reconciler left behind.
func degradedError(cur *clusteridentity.ClusterIdentity) error {
	if c := cur.Condition(clusteridentity.ConditionReady); c != nil && c.Message != "" {
		return fmt.Errorf("cutover reverted: %s. The previous identity is still serving; fix the cause and re-run", c.Message)
	}
	return fmt.Errorf("cutover reverted to the previous identity; the previous hosts are still serving")
}

// probeTargets verifies each new host answers over HTTPS with a valid chain,
// from outside the cluster. This is the external gate: a valid TLS chain proves
// the certificate issued and routing works, and the reconciler will not cut over
// until the operator approves on the strength of it.
func probeTargets(to *clusteridentity.ResolvedHosts) error {
	if to == nil {
		return fmt.Errorf("the transition has no target hosts")
	}
	checks := []struct {
		host, path, label string
	}{
		{to.Console, "/", "console"},
		{to.ConsoleAPI, "/health", "console-api"},
		{to.Dex, "/dex/.well-known/openid-configuration", "Dex discovery"},
	}
	for _, c := range checks {
		if err := probeHost(c.host, c.path); err != nil {
			return fmt.Errorf("%s at %s: %w", c.label, c.host, err)
		}
	}
	return nil
}

func probeHost(host, path string) error {
	if _, err := net.LookupHost(host); err != nil {
		return fmt.Errorf("DNS does not resolve (point it at this server): %w", err)
	}
	// The default client verifies the TLS chain, which is the certificate proof.
	return probeURL("https://"+host+path, http.DefaultClient)
}

// probeURL performs one reachability check: a GET that must succeed with a valid
// TLS chain (enforced by the client) and a non-error status.
func probeURL(url string, client *http.Client) error {
	ctx, cancel := context.WithTimeout(context.Background(), domainProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("returned HTTP %d", resp.StatusCode)
	}
	return nil
}

// writeLocalDomainConfig refreshes ~/.kip/config.yaml from the cluster's
// ClusterIdentity record so the local entry matches what the cluster actually
// serves.
func writeLocalDomainConfig() error {
	return runClusterDomainRepair()
}

// refuseDomainChangeDuringCAReplacement stops any of the three domain paths
// while the cluster's certificate authority is mid-replacement, or while the
// trust anchor on the host disagrees with the authorities the cluster holds.
//
// All three go through the same cutover driver and the same gate, so all three
// need the same refusal. Guarding only the forward path left the resync — the
// very command the parked cutover tells an operator to run — as the way in.
//
// The SSH connection is opened for the check and closed again. A cluster that
// cannot be reached is not blocked: this prevents a deadlock, and refusing to
// act because the check could not run would be worse than the deadlock.
func refuseDomainChangeDuringCAReplacement(ctx context.Context, cluster *config.Cluster, k8sClient *k8s.Client) error {
	var client *ssh.Client
	if cluster.Host != "" {
		explicit, fallback := resolveSSHKey("", cluster)
		if dialled, err := ssh.Dial(ssh.Config{
			Host:            cluster.Host,
			User:            "root",
			KeyPath:         explicit,
			FallbackKeyPath: fallback,
		}); err == nil {
			client = dialled
			defer func() { _ = client.Close() }()
		}
	}
	return installer.RefuseDuringCAReplacement(ctx, k8sClient.Clientset(), client)
}
