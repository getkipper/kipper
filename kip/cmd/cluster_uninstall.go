package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/domain"
	"github.com/getkipper/kipper/kip/internal/infra"
	"github.com/getkipper/kipper/kip/internal/installer"
)

var clusterUninstallCmd = &cobra.Command{
	Use:   "uninstall [name]",
	Short: "Uninstall Kipper from a remote server and remove its local config",
	Long: `Wipes Kipper from a remote Linux server. Runs k3s's own uninstall
script (which stops k3s, removes its binary and systemd units, and
cleans up containers and CNI state), then sweeps the data directories
Kipper writes outside k3s's purview: Longhorn volumes, Rancher state,
k3s config, Zot blobs, and any AI bundle data. Removes the cluster
from your local kip config and deletes its cached kubeconfig.

This is destructive. All cluster state and persistent volume data on
the host is removed. The command prompts for the cluster name to
confirm; pass --yes to skip the prompt for automation.

Host firewall rules and OS hardening (rpcbind disabled etc.) are not
reverted, because they are general OS security improvements unrelated
to k3s.

Examples:
  kip cluster uninstall storefront
  kip cluster uninstall storefront --yes
  kip cluster uninstall storefront --ssh-key ~/.ssh/kipper_ed25519`,
	Args: cobra.MaximumNArgs(1),
	RunE: runClusterUninstall,
}

func init() {
	clusterUninstallCmd.Flags().String("ssh-key", "", "path to SSH private key (defaults to ~/.ssh/id_ed25519)")
	clusterUninstallCmd.Flags().Bool("yes", false, "skip the confirmation prompts, including consent to wipe a cluster whose gateway name cannot be released")
	clusterUninstallCmd.Flags().Bool("keep-local-config", false, "wipe the host but leave the cluster in your local kip config")
	clusterCmd.AddCommand(clusterUninstallCmd)
}

func runClusterUninstall(cmd *cobra.Command, args []string) error {
	cluster, err := resolveUninstallTarget(args)
	if err != nil {
		return err
	}

	flagKey, _ := cmd.Flags().GetString("ssh-key")
	yes, _ := cmd.Flags().GetBool("yes")
	keepLocal, _ := cmd.Flags().GetBool("keep-local-config")

	return uninstallCommand{
		Connect:     connectForUninstall(flagKey),
		ReleaseOnly: gatewayOnlyDeps,
		Confirm:     func(c *config.Cluster) bool { return confirmDestroy(os.Stdin, c.Name) },
		RemoveLocal: removeLocalClusterEntry,
		SetWiped:    setHostWiped,
		Out:         os.Stdout,
	}.run(cluster, yes, keepLocal)
}

// uninstallCommand is the command body once the target and its flags are known.
//
// The collaborators arrive as fields for one reason: the branch below decides
// whether a host is contacted at all, and that decision has to be provable. A
// wiped host's entry survives only to hand its gateway name back, and reaching
// for SSH first is what left the name held when the server had already been
// destroyed.
type uninstallCommand struct {
	// Connect opens a session on the host and returns the deps bound to it,
	// along with the function that closes it.
	Connect func(cluster *config.Cluster) (uninstallDeps, func(), error)
	// ReleaseOnly is the same set of deps with no host in them.
	ReleaseOnly func(cluster *config.Cluster) uninstallDeps
	// Confirm puts the typed-name question.
	Confirm func(cluster *config.Cluster) bool
	// RemoveLocal and SetWiped are the local config writers. RemoveLocal takes
	// the credential the caller acted on, so it can refuse an entry that changed.
	RemoveLocal func(name, ownedToken string) error
	SetWiped    func(name string, wiped bool) error
	Out         io.Writer
}

func (c uninstallCommand) run(cluster *config.Cluster, autoYes, keepLocal bool) error {
	if cluster.HostWiped {
		// An earlier run wiped this host and could not release its name. Nothing
		// is left to destroy, so nothing is confirmed and no host is contacted —
		// by now the server may not exist.
		say(c.Out, "\n  %s was already wiped. Releasing its gateway name.\n\n", cluster.Name)
		d := c.ReleaseOnly(cluster)
		token := d.ReadMirror(cluster.Name)
		outcome := finishPendingRelease(token, cluster, d)
		if !outcome.Wiped {
			// Nothing readable, so nothing was attempted. Exiting zero here would
			// tell a scripted retry the teardown had finished, and it would keep
			// finding the same entry and keep succeeding.
			return fmt.Errorf("no gateway credential could be read for %s, so its name was not released", cluster.Domain)
		}
		if err := finishUninstall(outcome, cluster, keepLocal, c.RemoveLocal, c.SetWiped, c.Out); err != nil {
			return err
		}
		return uninstallExit(outcome, cluster, false, nil)
	}

	say(c.Out, "\n  About to uninstall Kipper from:\n\n")
	say(c.Out, "    Cluster:  %s\n", cluster.Name)
	say(c.Out, "    Host:     %s\n", cluster.Host)
	say(c.Out, "    Kubeconfig: %s\n\n", cluster.Kubeconfig)
	say(c.Out, "  This removes k3s, all Longhorn data, the Zot registry blobs,\n")
	say(c.Out, "  and any AI bundle data on the host. The action cannot be undone.\n\n")

	if !autoYes && !c.Confirm(cluster) {
		say(c.Out, "  Aborted.\n\n")
		return nil
	}

	deps, closeSession, err := c.Connect(cluster)
	if err != nil {
		return c.releaseWithoutTheHost(cluster, err, autoYes, keepLocal)
	}
	defer closeSession()

	outcome, err := uninstallCluster(cluster, autoYes, deps)
	if err != nil {
		return err
	}
	if err := finishUninstall(outcome, cluster, keepLocal, c.RemoveLocal, c.SetWiped, c.Out); err != nil {
		return err
	}
	return uninstallExit(outcome, cluster, true, nil)
}

// gatewayOnlyDeps is the release path with no host in it.
func gatewayOnlyDeps(_ *config.Cluster) uninstallDeps {
	return uninstallDeps{
		Release:     domain.NewGatewayClient("").Deregister,
		ReadMirror:  mirroredGatewayToken,
		WriteMirror: mirrorGatewayTokenToConfig,
		Prompt:      os.Stdin,
		Out:         os.Stdout,
	}
}

// connectForUninstall opens an SSH session on the cluster's host and binds the
// wipe to it.
func connectForUninstall(flagKey string) func(*config.Cluster) (uninstallDeps, func(), error) {
	return func(cluster *config.Cluster) (uninstallDeps, func(), error) {
		explicit, fallback := resolveSSHKey(flagKey, cluster)
		provider := &infra.BareMetalProvider{
			Host:           cluster.Host,
			SSHKey:         explicit,
			FallbackSSHKey: fallback,
		}
		if err := provider.Connect(); err != nil {
			return uninstallDeps{}, func() {}, fmt.Errorf("connecting to %s: %w", cluster.Host, err)
		}
		return uninstallDeps{
			Host:        provider.Client(),
			Wipe:        func() error { return installer.UninstallHost(provider.Client()) },
			Release:     domain.NewGatewayClient("").Deregister,
			ReadMirror:  mirroredGatewayToken,
			WriteMirror: mirrorGatewayTokenToConfig,
			Prompt:      os.Stdin,
			Out:         os.Stdout,
		}, func() { _ = provider.Close() }, nil
	}
}

// uninstallExit reports the command's exit for an outcome.
//
// wipedNow says whether this run destroyed the host, and it is what decides
// whether a refused release counts as failure. A run that wiped a server did the
// irreversible part and reports success with the name outstanding; a run whose
// only job was the release has done nothing at all, and telling a retry loop
// otherwise leaves it succeeding forever against a name that stays registered.
//
// It lives in one function because until it did the rule differed by accident:
// the release-only path exited zero on a refusal that was its entire job, one
// branch away from the empty-credential case that had been fixed for exactly
// that reason. because is any earlier failure worth carrying as context — the
// refusal is what decided the outcome, so it stays the wrapped cause.
func uninstallExit(outcome uninstallOutcome, cluster *config.Cluster, wipedNow bool, because error) error {
	if !outcome.NameStillRegistered || wipedNow {
		return nil
	}
	if because != nil {
		return fmt.Errorf("%s could not be reached (%v), and releasing %s failed too: %w",
			cluster.Host, because, cluster.Domain, outcome.ReleaseErr)
	}
	return fmt.Errorf("releasing %s failed: %w", cluster.Domain, outcome.ReleaseErr)
}

// releaseWithoutTheHost is the way out when the server cannot be reached.
//
// The gateway name is held by a token that is recorded locally, so handing it
// back needs no host at all — and a server that does not answer is exactly when
// that matters, because the alternative is waiting thirty days for the sweep.
// What it needs instead is a person, since nothing here can tell a destroyed
// machine from one that is briefly down, and freeing the name of a cluster that
// is still serving takes it off the air. Automation is therefore never offered
// it: --yes means do not ask, and the safe unasked answer is no.
//
// The wiped marker short-circuits all of this when an earlier run already
// proved the host is gone. This is the case where nobody proved anything.
func (c uninstallCommand) releaseWithoutTheHost(cluster *config.Cluster, connectErr error, autoYes, keepLocal bool) error {
	d := c.ReleaseOnly(cluster)
	// Read once, and spend exactly what was read. The operator is about to
	// approve releasing this credential's registration, and they are slow in a
	// way a config file is not: another kip run moving a domain in that window
	// would otherwise substitute a different, live registration for the one they
	// agreed to give up.
	token := d.ReadMirror(cluster.Name)
	if autoYes || token == "" {
		return connectErr
	}

	say(c.Out, "\n  !   %v\n", connectErr)
	say(c.Out, "      A credential for %s is recorded here, so its name can be handed back\n", cluster.Domain)
	say(c.Out, "      without the server. Say yes only if the server is gone. A cluster that\n")
	say(c.Out, "      is merely unreachable is still serving, and this would take it off the air.\n")
	if proceed, _ := askToProceed(d, "Release the name without the host?"); !proceed {
		return connectErr
	}

	outcome := finishPendingRelease(token, cluster, d)
	if outcome.NameStillRegistered {
		// Nothing is recorded about the host. The operator vouched for it once,
		// and one answer is all they gave: writing the wiped marker here would
		// turn it into standing authority, so a later run — a scripted --yes one
		// included — would release the name without asking and without touching
		// a server that may have been alive the whole time. The likeliest reason
		// the gateway refused is the same local outage that hid the host, which
		// is exactly when that guess is most likely wrong.
		say(c.Out, "\n  Kept the local entry for %s. Re-running offers this again.\n\n", cluster.Name)
		return uninstallExit(outcome, cluster, false, connectErr)
	}
	return finishUninstall(outcome, cluster, keepLocal, c.RemoveLocal, c.SetWiped, c.Out)
}

// finishUninstall applies an outcome to local state.
//
// This is the seam the original defect crossed: uninstallCluster released the
// name correctly and the caller deleted the entry regardless, taking the only
// token with it. Keeping the decision here, apart from the wiring, is what lets
// a test hold the caller to the outcome it was handed.
func finishUninstall(
	outcome uninstallOutcome,
	cluster *config.Cluster,
	keepLocal bool,
	removeLocal func(name, ownedToken string) error,
	setWiped func(name string, wiped bool) error,
	out io.Writer,
) error {
	if !outcome.Wiped {
		// The operator aborted before the host was touched, so the cluster is
		// still running and its entry and kubeconfig are the way back to it.
		return nil
	}
	if outcome.NameStillRegistered {
		// The host is gone but its name is not. The entry is kept because it
		// holds the only token that can release it — deleting it here is what
		// would strand the name for thirty days. The flag records that the wipe
		// itself is done, so the retry releases the name without needing the
		// server, which by then may not exist.
		if err := setWiped(cluster.Name, true); err != nil {
			// The wipe succeeded and the release did not, and now the record of
			// that has not been written either. Reporting success here is what
			// makes it dangerous: a script reads the zero exit, decommissions the
			// server, and the re-run then reaches for a host that no longer
			// answers before it ever looks at the token sitting locally.
			say(out, "\n  !   %s was wiped and its gateway name was not released.\n", cluster.Name)
			say(out, "      That could not be recorded locally (%v), so re-running will try to\n", err)
			say(out, "      reach the host again first. Run it before decommissioning the server.\n\n")
			return fmt.Errorf("recording that %s was wiped: %w", cluster.Name, err)
		}
		say(out, "\n  Kept the local entry for %s so its gateway name can still be released.\n", cluster.Name)
		say(out, "  Re-run: kip cluster uninstall %s\n\n", cluster.Name)
		return nil
	}

	if keepLocal {
		// Either the name is back with the gateway, or nothing local can hand it
		// back. Both leave the entry an ordinary stale one, and both make the
		// marker wrong: it would send every later uninstall down the
		// release-only path with nothing there to release.
		if err := setWiped(cluster.Name, false); err != nil {
			say(out, "\n  !   Could not clear the wiped marker on %s: %v\n", cluster.Name, err)
		}
		say(out, "\n  Local config kept (--keep-local-config). Cluster %q still in ~/.kip/config.yaml.\n\n", cluster.Name)
		return nil
	}

	if err := removeLocal(cluster.Name, outcome.SpentToken); err != nil {
		return fmt.Errorf("removing local config entry: %w", err)
	}
	say(out, "  ✔  Local config entry for %s removed\n\n", cluster.Name)
	return nil
}

// setHostWiped records in the local config whether this cluster's host has been
// destroyed with only its gateway name still outstanding.
func setHostWiped(name string, wiped bool) error {
	return config.Update(func(cfg *config.Config) error {
		entry := cfg.GetCluster(name)
		if entry == nil {
			return fmt.Errorf("cluster %q is not in the local config", name)
		}
		if entry.HostWiped == wiped {
			return config.ErrNoChange
		}
		entry.HostWiped = wiped
		return nil
	})
}

// resolveUninstallTarget picks the cluster to uninstall. With no
// positional arg it falls back to the current cluster; with one arg it
// looks the cluster up by exact-or-partial match (same helper as the
// other cluster subcommands).
func resolveUninstallTarget(args []string) (*config.Cluster, error) {
	if len(args) == 0 {
		return loadCurrentClusterConfig()
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}
	cluster := findCluster(cfg, args[0])
	if cluster == nil {
		fmt.Fprintf(os.Stderr, "\n  Cluster %q not found. Available clusters:\n\n", args[0])
		for _, c := range cfg.Clusters {
			fmt.Fprintf(os.Stderr, "    %s\n", c.Name)
		}
		fmt.Fprintln(os.Stderr)
		return nil, fmt.Errorf("cluster %q not found", args[0])
	}
	return cluster, nil
}

// confirmDestroy reads one line from r and returns true only if it
// matches the cluster name exactly. Modelled on `kubectl delete`-style
// typed confirmation.
func confirmDestroy(r io.Reader, clusterName string) bool {
	fmt.Printf("  Type the cluster name to confirm (%s): ", clusterName)
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		return false
	}
	return strings.TrimSpace(scanner.Text()) == clusterName
}

// ErrEntryChanged reports that the local entry is no longer the one a command
// acted on, so it was left alone.
var ErrEntryChanged = errors.New("the local entry changed while this command was running")

// removeLocalClusterEntry deletes the cluster from ~/.kip/config.yaml and
// removes its cached kubeconfig. Idempotent: a cluster that is not there is not
// an error.
//
// ownedToken is the gateway credential the caller acted on, and the removal only
// happens while the entry still holds exactly that. The gap between reading the
// token and getting here is a wipe or a human answer long, and another kip run
// moving a domain in that gap leaves a live registration under this name —
// deleting it would discard the only local copy of a credential nobody agreed to
// give up.
//
// Empty has to match empty rather than match anything. A caller holding no
// credential read one from this entry and found none, so an entry holding one
// now is by definition an entry that changed. Letting empty pass unchecked would
// leave the guard open in one of the two cases it exists for: a wipe the
// operator consented to without a readable token, running for minutes while
// another command registers a name into the entry it is about to delete.
//
// The check and the delete are one locked operation because as two they are just
// a smaller version of the same race.
func removeLocalClusterEntry(name, ownedToken string) error {
	return config.Update(func(cfg *config.Config) error {
		var remaining []config.Cluster
		found := false
		for _, c := range cfg.Clusters {
			if c.Name != name {
				remaining = append(remaining, c)
				continue
			}
			if c.GatewayToken != ownedToken {
				return fmt.Errorf("%w: %s", ErrEntryChanged, name)
			}
			found = true
			_ = os.Remove(c.Kubeconfig)
		}
		if !found {
			return config.ErrNoChange
		}

		cfg.Clusters = remaining
		if cfg.CurrentCluster == name {
			if len(cfg.Clusters) > 0 {
				cfg.CurrentCluster = cfg.Clusters[0].Name
			} else {
				cfg.CurrentCluster = ""
			}
		}
		return nil
	})
}

// mirroredGatewayToken returns the locally recorded release token, or empty.
func mirroredGatewayToken(clusterName string) string {
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	if entry := cfg.GetCluster(clusterName); entry != nil {
		return entry.GatewayToken
	}
	return ""
}
