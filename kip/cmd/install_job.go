package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/installjob"
)

var (
	jobRoot                       = installjob.DefaultRoot
	jobSystemd installjob.Systemd = installjob.HostSystemd{}
)

func useJobHost(root string, sd installjob.Systemd) func() {
	prevRoot, prevSD := jobRoot, jobSystemd
	jobRoot, jobSystemd = root, sd
	return func() { jobRoot, jobSystemd = prevRoot, prevSD }
}

// installJobCmd exposes the internal job protocol used by the driver over SSH.
var installJobCmd = &cobra.Command{
	Use:           "install-job",
	Short:         "Server side of an install driven from another machine",
	Hidden:        true,
	SilenceUsage:  true,
	SilenceErrors: false,
}

func init() {
	claim := &cobra.Command{
		Use:  "claim",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			kipPath, _ := cmd.Flags().GetString("kip")
			return runInstallJobClaim(os.Stdin, os.Stdout, kipPath)
		},
	}
	claim.Flags().String("kip", "/usr/local/bin/kip", "path to the server's kip binary")

	follow := &cobra.Command{
		Use:  "follow",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			run, _ := cmd.Flags().GetString("run")
			offset, _ := cmd.Flags().GetInt64("offset")
			return installjob.Follow(jobRoot, run, offset, os.Stdout, jobSystemd, 2*time.Second)
		},
	}
	follow.Flags().String("run", "", "run ID")
	follow.Flags().Int64("offset", 0, "log offset to start from")

	result := &cobra.Command{
		Use:  "result",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			run, _ := cmd.Flags().GetString("run")
			if err := installjob.CheckRunID(run); err != nil {
				return err
			}
			r, err := installjob.ReadResult(filepath.Join(jobRoot, run))
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(r)
		},
	}
	result.Flags().String("run", "", "run ID")

	ack := &cobra.Command{
		Use:  "ack",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			run, _ := cmd.Flags().GetString("run")
			return installjob.Ack(jobRoot, run)
		},
	}
	ack.Flags().String("run", "", "run ID")

	cancel := &cobra.Command{
		Use:  "cancel",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			done, err := installjob.Cancel(jobRoot, jobSystemd)
			if err != nil {
				return err
			}
			fmt.Println(done)
			return nil
		},
	}

	status := &cobra.Command{
		Use:  "status",
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			st, err := installjob.ReadStatus(jobRoot, jobSystemd)
			if err != nil {
				return err
			}
			return json.NewEncoder(os.Stdout).Encode(st)
		},
	}

	installJobCmd.AddCommand(claim, follow, result, ack, cancel, status)
	rootCmd.AddCommand(installJobCmd)
}

func runInstallJobClaim(in io.Reader, out io.Writer, kipPath string) error {
	body, err := io.ReadAll(in)
	if err != nil {
		return err
	}
	req, err := installjob.ParseRequest(body)
	if err != nil {
		return err
	}
	outcome, err := installjob.Claim(jobRoot, req, kipPath, jobSystemd)
	if err != nil {
		return err
	}
	return json.NewEncoder(out).Encode(outcome)
}

// serverEntryFromPushed keeps cluster settings from the driver and preserves
// the server's kubeconfig path and machine ID for the same host.
func serverEntryFromPushed(e config.Cluster, server *config.Cluster) config.Cluster {
	e.Kubeconfig = ""
	e.SSHKey = ""
	e.CurrentProject = ""
	e.CurrentEnvironment = ""
	e.MachineID = ""
	e.HostWiped = false
	if server != nil && strings.EqualFold(server.Host, e.Host) {
		e.Kubeconfig = server.Kubeconfig
		e.MachineID = server.MachineID
	}
	return e
}

// recordPushedEntry replaces the server's entry for this host, including after
// a rename on the client.
func recordPushedEntry(cfg *config.Config, pushed config.Cluster) {
	var server *config.Cluster
	if existing := cfg.GetClusterByHost(pushed.Host); existing != nil {
		copied := *existing
		server = &copied
	}
	kept := cfg.Clusters[:0]
	for _, c := range cfg.Clusters {
		if c.Name == pushed.Name || !strings.EqualFold(c.Host, pushed.Host) {
			kept = append(kept, c)
		} else if cfg.CurrentCluster == c.Name {
			cfg.CurrentCluster = pushed.Name
		}
	}
	cfg.Clusters = kept
	cfg.AddCluster(serverEntryFromPushed(pushed, server))
}

// jobResult includes a cluster entry only when its kubeconfig is readable,
// non-empty, and passes credentialFree, so the driver can import it.
func jobResult(runID string, installErr error, adminEmail, adminPassword string, cfg *config.Config, host string) installjob.Result {
	r := installjob.Result{RunID: runID, Outcome: installjob.PhaseSucceeded, AdminEmail: adminEmail, AdminPassword: adminPassword}
	if installErr != nil {
		r.Outcome = installjob.PhaseFailed
	}
	if cfg == nil {
		return r
	}
	entry := cfg.GetClusterByHost(host)
	if entry == nil || entry.Kubeconfig == "" {
		return r
	}
	kc, err := os.ReadFile(entry.Kubeconfig)
	if err != nil || len(kc) == 0 || !credentialFree(kc) {
		return r
	}
	copied := *entry
	r.Cluster = &copied
	r.Kubeconfig = string(kc)
	return r
}

// credentialFree checks that kubeconfig parses and its AuthInfos contain
// neither credentials nor impersonation settings.
func credentialFree(kubeconfig []byte) bool {
	parsed, err := clientcmd.Load(kubeconfig)
	if err != nil {
		return false
	}
	for _, authInfo := range parsed.AuthInfos {
		if authInfo != nil && (carriesCredential(authInfo) || impersonates(authInfo)) {
			return false
		}
	}
	return true
}

// runInstallRequest installs locally and saves state and results for the client.
// The admin password is returned in the result rather than the replayable log.
func runInstallRequest(path string) error {
	body, err := os.ReadFile(path) //nolint:gosec // the job's own request under the root-only jobs directory
	if err != nil {
		return err
	}
	// Remove the request promptly because it may contain backup credentials.
	_ = os.Remove(path)
	req, err := installjob.ParseRequest(body)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	state := installjob.State{RunID: req.RunID, KipVersion: req.KipVersion, Started: time.Now().UTC(), Phase: installjob.PhaseRunning}
	if err := installjob.WriteState(dir, state); err != nil {
		return err
	}

	if req.Entry != nil {
		if err := config.Update(func(cfg *config.Config) error {
			recordPushedEntry(cfg, *req.Entry)
			return nil
		}); err != nil {
			return fmt.Errorf("recording the cluster's settings before the reinstall: %w", err)
		}
	}

	opts := req.Options()
	opts.KubeconfigMode = installer.ResolveKubeconfigMode(false, true, false)
	opts.LoginGate = func(ctx context.Context, domain, dexHost, server string, caData []byte) installer.GateResult {
		return installer.DefaultLoginGate(domain, dexHost)(ctx, domain, server, caData)
	}
	result, installErr := installer.Run(opts)

	var adminEmail, password string
	if result != nil {
		adminEmail, password = "admin@"+result.Domain, result.AdminPassword
	}
	cfg, err := config.Load()
	if err != nil {
		cfg = nil
	}
	r := jobResult(req.RunID, installErr, adminEmail, password, cfg, req.Host)
	if err := installjob.WriteResult(dir, r); err != nil {
		return err
	}
	state.Phase = r.Outcome
	if err := installjob.WriteState(dir, state); err != nil {
		return err
	}
	if installErr == nil {
		fmt.Printf("\n  ✔  Installed. Collect the result on your machine to view the admin password.\n")
	}
	return installErr
}

var requestFlagUsage = strings.TrimSpace(`
install from a client-generated request file (internal)`)
