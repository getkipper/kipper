package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

var nodeRebootCmd = &cobra.Command{
	Use:   "reboot",
	Short: "Reboot the cluster's server and wait until it is healthy again",
	Long: `Reboots the cluster's server to apply a new kernel or pending service
restarts listed by 'kip status'.

Apps on a single-server cluster are unavailable during the reboot.
kip takes a backup, reboots the server, waits for all nodes and the components
checked by 'kip status' to be ready, then runs the host checks.`,
	Args: cobra.NoArgs,
	RunE: runNodeReboot,
}

func init() {
	nodeRebootCmd.Flags().Bool("yes", false, "reboot without asking for confirmation")
	nodeRebootCmd.Flags().Bool("skip-backup", false, "reboot without taking a backup first")
	nodeRebootCmd.Flags().String("ssh-key", "", "path to SSH private key for the host (overrides cluster.ssh_key in config and KIP_SSH_KEY env)")
	nodeRebootCmd.Flags().Duration("timeout", 20*time.Minute, "how long to wait for the server and every component to be ready again")
	nodeCmd.AddCommand(nodeRebootCmd)
}

// nodeReboot injects host operations and time to test sequencing and failures.
type nodeReboot struct {
	Out              io.Writer
	SSH              ssh.Config
	Confirm          func(prompt string) (bool, error)
	Backup           func() (string, error)
	BootID           func(ssh.Config) (string, error)
	Reboot           func(ssh.Config) error
	Healthy          func(within time.Duration, bootID string) (bool, string)
	Report           func(ssh.Config)
	Sleep            func(time.Duration)
	Now              func() time.Time
	Interval         time.Duration
	Probe            time.Duration
	NotRebootedAfter time.Duration
	Timeout          time.Duration
}

func (r *nodeReboot) run(host string, yes, skipBackup bool) error {
	before, err := r.BootID(r.SSH)
	if err != nil {
		return fmt.Errorf("reaching %s over SSH before the reboot: %w", host, err)
	}

	if !yes {
		ok, err := r.Confirm(fmt.Sprintf("\n  Reboot %s? Its apps will be unavailable during the reboot. [y/N] ", host))
		if err != nil {
			return err
		}
		if !ok {
			say(r.Out, "  Not rebooted.\n")
			return nil
		}
	}

	if !skipBackup {
		say(r.Out, "\n  ...  Backing up before the reboot\n")
		name, err := r.Backup()
		if err != nil {
			return fmt.Errorf("backup before the reboot: %w; reboot canceled. A timed-out backup may still be running. Check 'kip backup list' before using --skip-backup", err)
		}
		say(r.Out, "  ✔  Backup %s completed\n", name)
	}

	say(r.Out, "\n  If console-api uses :latest, it may start with a newer image after reboot.\n")
	say(r.Out, "  Run 'kip upgrade --check' to check for required data updates.\n")
	say(r.Out, "\n  ...  Rebooting %s\n", host)
	if err := r.Reboot(r.SSH); err != nil {
		return fmt.Errorf("rebooting %s: %w", host, err)
	}
	start := r.Now()
	deadline := start.Add(r.Timeout)
	remaining := func() time.Duration { return deadline.Sub(r.Now()) }

	say(r.Out, "  ...  Waiting for %s to start again\n", host)
	var after string
	for {
		r.Sleep(min(r.Interval, max(remaining(), 0)))
		if remaining() <= 0 {
			return fmt.Errorf("%s did not come back within %s. Some servers boot slowly; check it with 'kip status', and through your provider's console if it stays unreachable", host, r.Timeout)
		}
		id, err := r.BootID(r.SSH)
		if err == nil && id != before {
			after = id
			break
		}
		if err == nil && r.Now().Sub(start) >= r.NotRebootedAfter {
			return fmt.Errorf("%s did not reboot: its boot ID is unchanged %s after the reboot was scheduled. Check 'systemctl list-jobs' and the system journal on the host for the cause", host, r.NotRebootedAfter)
		}
	}
	say(r.Out, "  ✔  %s is back after %s\n", host, r.Now().Sub(start).Round(time.Second))

	say(r.Out, "  ...  Waiting for every node and component to be ready\n")
	// Require consecutive healthy checks to reduce transient readiness results.
	healthy := 0
	for healthy < 2 {
		if remaining() <= 0 {
			say(r.Out, "\n")
			r.Report(r.SSH)
			return fmt.Errorf("not ready within %s of the reboot. Check it with 'kip status'", r.Timeout)
		}
		ok, notReady := r.Healthy(min(r.Probe, remaining()), after)
		if ok {
			healthy++
		} else {
			healthy = 0
		}
		if !ok && remaining() <= r.Interval {
			say(r.Out, "\n")
			r.Report(r.SSH)
			return fmt.Errorf("not ready within %s of the reboot: %s. Check it with 'kip status'", r.Timeout, notReady)
		}
		if healthy < 2 {
			r.Sleep(r.Interval)
		}
	}
	say(r.Out, "  ✔  Every node and component is ready\n\n")
	r.Report(r.SSH)
	return nil
}

func runNodeReboot(cmd *cobra.Command, args []string) error {
	cluster, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	if cluster.Host == "" {
		return fmt.Errorf("cluster %s has no host recorded, so kip cannot reach its server", cluster.Name)
	}
	yes, _ := cmd.Flags().GetBool("yes")
	skipBackup, _ := cmd.Flags().GetBool("skip-backup")
	keyFlag, _ := cmd.Flags().GetString("ssh-key")
	timeout, _ := cmd.Flags().GetDuration("timeout")

	r := &nodeReboot{
		Out:     os.Stdout,
		SSH:     rebootSSHConfig(cluster, keyFlag),
		Confirm: confirmReboot,
		Backup: func() (string, error) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			return backupBeforeReboot(ctx, cluster, client, 10*time.Second)
		},
		BootID: func(cfg ssh.Config) (string, error) {
			return onHost(cfg, func(c *ssh.Client) (string, error) { return installer.ReadBootID(c) })
		},
		Reboot: func(cfg ssh.Config) error {
			_, err := onHost(cfg, func(c *ssh.Client) (string, error) {
				return c.Run(rebootCommand(time.Now()))
			})
			return err
		},
		Healthy: func(within time.Duration, bootID string) (bool, string) {
			return clusterReady(client, within, bootID)
		},
		Report:           func(cfg ssh.Config) { reportHostAfterReboot(cfg, cluster, client) },
		Sleep:            time.Sleep,
		Now:              time.Now,
		Interval:         10 * time.Second,
		Probe:            30 * time.Second,
		NotRebootedAfter: 3 * time.Minute,
		Timeout:          timeout,
	}
	return r.run(cluster.Host, yes, skipBackup)
}

// rebootSSHConfig limits command duration because a booting server can accept
// SSH connections before it can complete commands.
func rebootSSHConfig(cluster *config.Cluster, keyFlag string) ssh.Config {
	cfg := hostSSHConfig(cluster, keyFlag)
	cfg.CommandTimeout = 30 * time.Second
	return cfg
}

func onHost(cfg ssh.Config, fn func(*ssh.Client) (string, error)) (string, error) {
	c, err := ssh.Dial(cfg)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	return fn(c)
}

func confirmReboot(prompt string) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, fmt.Errorf("stdin is not a terminal. Pass --yes to reboot without confirmation")
	}
	fmt.Print(prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	return parseYesNo(line), nil
}

// backupBeforeReboot uses the defaults from 'kip backup create' and waits for completion.
func backupBeforeReboot(ctx context.Context, cluster *config.Cluster, client *k8s.Client, interval time.Duration) (string, error) {
	name := "before-reboot-" + time.Now().UTC().Format("20060102-150405")
	backups := client.Dynamic().Resource(backupGVR).Namespace("velero")
	if _, err := backups.Create(ctx, buildBackupCR(name, "", "", "168h", false, cluster), metav1.CreateOptions{}); err != nil {
		return "", fmt.Errorf("creating backup %s: %w", name, err)
	}
	phase := func(ctx context.Context) (string, error) {
		current, err := backups.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		p, _, _ := unstructured.NestedString(current.Object, "status", "phase")
		return p, nil
	}
	if err := waitForBackupPhase(ctx, name, phase, interval); err != nil {
		return "", err
	}
	return name, nil
}

// waitForBackupPhase retries read errors until ctx ends or Velero reports a
// terminal phase. On timeout, it includes the most recent read error, if any.
func waitForBackupPhase(ctx context.Context, name string, phase func(context.Context) (string, error), interval time.Duration) error {
	var lastErr error
	for {
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("backup %s did not complete in time (last read failed: %v); see 'kip backup list'", name, lastErr)
			}
			return fmt.Errorf("backup %s did not complete in time; see 'kip backup list'", name)
		case <-time.After(interval):
		}
		p, err := phase(ctx)
		if err != nil {
			lastErr = err
			continue
		}
		switch p {
		case "Completed":
			return nil
		case "PartiallyFailed", "Failed", "FailedValidation":
			return fmt.Errorf("backup %s ended %s; see 'kip backup list'", name, p)
		}
	}
}

// clusterReady uses the node and component health checks from 'kip status'.
func clusterReady(client *k8s.Client, within time.Duration, bootID string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	nodes, err := client.ListNodes(ctx)
	if err != nil {
		return false, "the API server is not answering yet"
	}
	health, err := client.ClusterHealth(ctx)
	if err != nil {
		return false, "component health could not be read yet"
	}
	return readyAfterBoot(nodes, health, bootID)
}

// readyAfterBoot requires the new boot ID before accepting cluster readiness.
func readyAfterBoot(nodes []k8s.NodeInfo, health []k8s.ComponentStatus, bootID string) (bool, string) {
	rebooted := false
	for _, n := range nodes {
		rebooted = rebooted || n.BootID == bootID
	}
	if !rebooted {
		return false, "the kubelet has not reported the new boot yet"
	}
	var notReady []string
	for _, n := range nodes {
		if n.Status != "Ready" {
			notReady = append(notReady, "node "+n.Name+" "+n.Status)
		}
	}
	for _, c := range health {
		if !c.Healthy {
			notReady = append(notReady, c.Name+" "+c.Message)
		}
	}
	return len(notReady) == 0, strings.Join(notReady, ", ")
}

func reportHostAfterReboot(sshConfig ssh.Config, cluster *config.Cluster, client *k8s.Client) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	nodes, err := client.ListNodes(ctx)
	if err != nil {
		// SSH host checks can proceed even if the API request fails.
		fmt.Printf("  ⚠  could not list nodes: %v\n\n", err)
	}
	checkHostWith(sshConfig, cluster, nodes)
}

// rebootCommand delays reboot so SSH can return the scheduling result.
// The timestamp avoids reusing units from attempts made in earlier seconds.
func rebootCommand(now time.Time) string {
	return fmt.Sprintf("systemd-run --on-active=3 --unit=kip-node-reboot-%d systemctl reboot", now.Unix())
}
