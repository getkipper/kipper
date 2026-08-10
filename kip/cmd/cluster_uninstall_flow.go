package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/domain"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// uninstallDeps is everything an uninstall needs from the world outside itself.
//
// It exists because the interesting behaviour of an uninstall is entirely in its
// failure paths — a wipe that dies halfway, a cluster that cannot be read, a
// local copy that cannot be written — and none of those could be reached with a
// real host and a real gateway on the other end. Three review rounds found real
// defects here, each one in the previous round's fix, because every correction
// was reasoned about rather than exercised.
type uninstallDeps struct {
	// Host runs commands on the machine being wiped.
	Host gatewayTokenReader
	// Wipe destroys the cluster. Bound to the host by the caller.
	Wipe func() error
	// Release hands the *.kipper.run name back to the gateway.
	Release func(token string) error
	// ReadMirror and WriteMirror are the local copy of the release token, whose
	// source of truth is the cluster the wipe destroys. WriteMirror replaces the
	// value its caller says it expects to find, so a credential some other
	// command recorded in the meantime is never written over.
	ReadMirror  func(clusterName string) string
	WriteMirror func(clusterName, expected, token string) error
	// Prompt is where a confirmation is read from, Out where the operator is told
	// what happened.
	Prompt io.Reader
	Out    io.Writer
}

// say reports progress to the operator. A failed write to a terminal is not
// actionable and must never derail a destructive operation that is under way.
func say(out io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(out, format, args...)
}

func (d uninstallDeps) say(format string, args ...any) {
	say(d.Out, format, args...)
}

// uninstallOutcome is what the caller needs to know to decide whether the local
// entry may be removed.
type uninstallOutcome struct {
	// Wiped reports that the host is done with: either this run destroyed it, or
	// an earlier one did and only the gateway name was outstanding, or the
	// operator confirmed the server is gone. False means nothing was touched, so
	// the cluster is still running and its entry is the operator's way back to
	// it.
	Wiped bool
	// NameStillRegistered reports that the gateway would not take the name back.
	// The local entry is kept when it is true, because that entry holds the only
	// token that can release the name later.
	NameStillRegistered bool
	// ReleaseErr is why the gateway refused, kept so a caller can report the
	// failure that actually decided the outcome rather than an earlier one it
	// happens to be holding.
	ReleaseErr error
	// SpentToken is the credential this run acted on. The local entry is only
	// removed while it still holds this, because the gap between reading it and
	// removing anything is a wipe or a human answer long, and another kip run
	// moving a domain in that gap leaves a live registration under the same name.
	SpentToken string
}

// uninstallCluster wipes a cluster and hands its gateway name back.
//
// The order is the whole point and is not an implementation detail: the token
// that releases a name lives on the cluster, so it must be read before the wipe;
// and a wipe that fails may have left a live cluster, so the name must not be
// released until the wipe has succeeded. Getting either half wrong strands a
// name permanently or takes a running cluster off the air.
func uninstallCluster(cluster *config.Cluster, autoYes bool, d uninstallDeps) (uninstallOutcome, error) {
	token, proceed, declined := releaseTokenFor(cluster, autoYes, d)
	if !proceed {
		d.say("  Aborted.\n\n")
		// Declining is a decision and exits cleanly, matching the typed-name
		// prompt. Being unable to ask is not: a script whose stdin is closed
		// would read a zero exit as "the host was wiped" when nothing was
		// touched.
		//
		// Either way nothing was wiped, and the caller must know that: removing
		// the local entry and kubeconfig after an abort takes the operator's
		// only credential for a cluster that is still running.
		if declined {
			return uninstallOutcome{}, nil
		}
		return uninstallOutcome{}, fmt.Errorf("could not confirm whether to wipe %s without releasing its gateway name", cluster.Name)
	}

	d.say("  ...  Uninstalling Kipper from %s\n", cluster.Host)
	if wipeErr := d.Wipe(); wipeErr != nil {
		// The name is deliberately not released here. Nothing available at this
		// point distinguishes a cluster this wipe destroyed from one it never
		// touched: any probe runs over the same connection whose failure most
		// plausibly caused the error, and a killall that has not yet reached the
		// uninstall script leaves a cluster that still restarts on boot.
		// Guessing wrong deregisters a live cluster and frees its name while it
		// is still serving.
		//
		// Nothing is lost by waiting: the token was mirrored locally before the
		// host was touched, so re-running finishes the wipe and releases from
		// that copy.
		if token != "" {
			d.say("  ·  %s stays registered. Re-run this command to finish the wipe and release it.\n", cluster.Domain)
		}
		return uninstallOutcome{}, fmt.Errorf("uninstalling: %w", wipeErr)
	}
	d.say("  ✔  Host wiped\n")

	stillRegistered, releaseErr := releaseName(token, cluster, d)
	return uninstallOutcome{Wiped: true, NameStillRegistered: stillRegistered, ReleaseErr: releaseErr, SpentToken: token}, nil
}

// releaseTokenFor reads the credential that releases this cluster's name and
// reports whether the uninstall should go ahead.
//
// The read happens before the wipe because the token lives on the cluster and
// nowhere else — the local mirror is absent on clusters installed before it
// existed, and on any cluster installed by another operator.
//
// The third return says whether a person actively declined, as opposed to the
// question never being answerable.
func releaseTokenFor(cluster *config.Cluster, autoYes bool, d uninstallDeps) (token string, proceed, declined bool) {
	named := strings.HasSuffix(cluster.Domain, ".kipper.run")

	// What the entry holds before this run goes looking. Reading the cluster is
	// a network round trip, and another command finishing a domain move inside
	// it would leave a newer credential here — which the write below must not
	// replace with the one this run just read.
	before := d.ReadMirror(cluster.Name)

	token, err := readGatewayCredentialsFrom(d.Host)
	if err == nil && token != "" {
		// Mirror it before the host is touched. The wipe takes the API server
		// down at its first step, so from that moment the cluster copy is
		// unreadable, and a wipe that then fails leaves the only other copy in
		// this process's memory. That makes the mirror the thing this path rests
		// on, so failing to write it cannot pass quietly.
		if mirrorErr := d.WriteMirror(cluster.Name, before, token); mirrorErr != nil {
			d.say("  !   Could not record this cluster's gateway credential locally: %v\n", mirrorErr)
			d.say("      If the wipe fails partway, %s cannot be released afterwards.\n", cluster.Domain)
			if autoYes {
				return token, true, false
			}
			if proceed, asked := askToProceed(d, "Wipe anyway?"); !proceed {
				return "", false, asked
			}
		}
		return token, true, false
	}

	// The cluster could not answer, or holds nothing. A token mirrored by an
	// earlier command is then the last copy anywhere, and spending a stale one
	// costs nothing: the gateway answers "not registered", which reads as
	// already released.
	if mirrored := d.ReadMirror(cluster.Name); mirrored != "" {
		return mirrored, true, false
	}

	if err != nil {
		d.say("  !   Could not read this cluster's gateway credentials: %v\n", err)
	} else if named {
		d.say("  !   This cluster holds no gateway token for %s.\n", cluster.Domain)
	}
	if !named {
		// A custom-domain cluster registered nothing and has nothing to give
		// back.
		return "", true, false
	}

	d.say("      Wiping now leaves that name registered with no way to release it,\n")
	d.say("      so installing here again cannot serve on it.\n")
	if autoYes {
		return "", true, false
	}
	proceed, asked := askToProceed(d, "Wipe anyway?")
	return "", proceed, asked
}

// releaseName spends the token read before the wipe and reports whether the name
// is still registered afterwards. It never fails the uninstall: the host is
// already gone, and a gateway that cannot be reached is not a reason to report
// failure for work that succeeded.
//
// The answer matters to the caller: the host is gone by now, so the local entry
// holds the only remaining copy of the token. Deleting it after a refused
// release makes the name unrecoverable for thirty days, which is the one case
// where this feature could still strand the thing it exists to protect.
func releaseName(token string, cluster *config.Cluster, d uninstallDeps) (stillRegistered bool, releaseErr error) {
	if token == "" {
		return false, nil
	}
	switch err := d.Release(token); {
	case errors.Is(err, domain.ErrNotRegistered):
		d.say("  ✔  Gateway name already released\n")
		return false, nil
	case err != nil:
		d.say("  !   Could not release %s: %v\n", cluster.Domain, err)
		recordRetryCredential(token, cluster, d)
		return true, err
	default:
		d.say("  ✔  Gateway name released\n")
		return false, nil
	}
}

// recordRetryCredential makes sure the token this run spent is the one a re-run
// will find, and says what it managed.
//
// The write goes in only where the entry holds nothing, and that condition is
// checked by the writer under its lock rather than here. A different credential
// under the same name is a registration some other command installed while this
// one was working — plausibly for minutes, since a wipe or a prompt sits in the
// middle — and overwriting it would throw away the only local copy of a live
// token to make room for one the gateway has just refused.
func recordRetryCredential(token string, cluster *config.Cluster, d uninstallDeps) {
	switch err := d.WriteMirror(cluster.Name, "", token); {
	case err == nil:
		d.say("      Its credential is recorded locally, so re-running this command can finish the job.\n")
	case errors.Is(err, ErrMirrorHolds):
		d.say("      A different credential is recorded for %s now, so this one was not written over it.\n", cluster.Name)
		d.say("      Check which cluster that entry belongs to before re-running.\n")
	default:
		d.say("      Its credential may not have been recorded locally (%v). Re-run this command;\n", err)
		d.say("      if nothing was recorded, %s waits for the gateway's 30-day sweep.\n", cluster.Domain)
	}
}

// finishPendingRelease hands back the name of a host not worth contacting,
// spending the token its caller has already read.
//
// The token is a parameter rather than something read in here, and that is the
// whole point of the signature: both callers decide *whether* to release on the
// strength of one particular credential — one recorded by a completed wipe, one
// shown to an operator who was asked to approve spending it. A second read after
// that decision would let a concurrent write substitute a different registration
// for the one that was authorised, and the wait for a human answer makes that
// window as long as the operator is slow.
func finishPendingRelease(token string, cluster *config.Cluster, d uninstallDeps) uninstallOutcome {
	if token == "" {
		// Nothing readable. Either the entry holds no credential or the config
		// could not be opened, and those two answer identically — so removing
		// the entry here could throw away the token it still holds. Keeping it
		// costs an explicit `kip cluster remove` when there really is nothing
		// there.
		d.say("  !   No gateway credential could be read for %s, so its name stays registered.\n", cluster.Domain)
		d.say("      Re-run this command first — a config that could not be opened may hold it still.\n")
		d.say("      If a re-run finds nothing either, `kip cluster remove %s` forgets the cluster.\n", cluster.Name)
		return uninstallOutcome{}
	}
	stillRegistered, releaseErr := releaseName(token, cluster, d)
	return uninstallOutcome{Wiped: true, NameStillRegistered: stillRegistered, ReleaseErr: releaseErr, SpentToken: token}
}

// askToProceed puts a yes/no question whose safe answer is no. Nothing to read
// means the question was never put to anyone, which the caller reports as a
// failure rather than a decline.
func askToProceed(d uninstallDeps, question string) (proceed, asked bool) {
	d.say("  %s [y/N]: ", question)
	scanner := bufio.NewScanner(d.Prompt)
	if !scanner.Scan() {
		// Nothing to read: a closed or non-interactive stdin. The question was
		// never put to anyone, so the answer is not "no", it is "unknown".
		return false, false
	}
	answer := strings.ToLower(strings.TrimSpace(scanner.Text()))
	return answer == "y" || answer == "yes", true
}

// gatewayTokenReader is the slice of an SSH client the release path needs, so a
// test can stand in for a cluster that answers, refuses, or holds nothing.
type gatewayTokenReader interface {
	Run(command string) (string, error)
	RunStdin(command string, stdin io.Reader) (string, error)
}

// readGatewayCredentialsFrom reads the release token off a cluster.
func readGatewayCredentialsFrom(client gatewayTokenReader) (string, error) {
	return installer.ReadGatewayCredentials(client)
}
