package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/auth"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/doctor"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/issuelink"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

const (
	gatewayHealthURL = "https://kipper.run/health"
	// doctorSSHCommandTimeout bounds each command on the server, and
	// doctorAPITimeout the API reads when SSH is unavailable, so a stalled
	// cluster produces a report instead of a hang.
	doctorSSHCommandTimeout = 45 * time.Second
	doctorAPITimeout        = 60 * time.Second
)

// doctorLoginTimeout bounds the wait for a login check. Identity verification
// allows a minute for retries during cluster startup, but doctor needs a prompt
// diagnostic result. Token refresh ignores the context, so cancellation alone
// cannot enforce this limit.
var doctorLoginTimeout = 15 * time.Second

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Diagnose a cluster that installed but does not work",
	Long: `Checks the server, Kubernetes and, for a kipper.run cluster, the hop between the
kipper.run gateway and the cluster. It reads the cluster over SSH first, so it
still works when the Kubernetes API rejects your login. If a check fails, it
prints a link to report what it found.`,
	RunE:         runDoctor,
	SilenceUsage: true,
}

func init() {
	doctorCmd.Flags().String("ssh-key", "", "path to the SSH private key for the server")
	rootCmd.AddCommand(doctorCmd)
}

type doctorGroup struct {
	Title   string
	Results []doctor.Result
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	keyFlag, _ := cmd.Flags().GetString("ssh-key")
	ctx := context.Background()

	var runner doctor.Runner
	sshResult := doctor.Result{ID: "ssh", Label: "SSH", Status: doctor.Pass, Message: "reachable over SSH"}
	sshConfig := hostSSHConfig(cluster, keyFlag)
	sshConfig.CommandTimeout = doctorSSHCommandTimeout
	client, dialErr := ssh.Dial(sshConfig)
	if dialErr != nil {
		sshResult = doctor.Result{ID: "ssh", Label: "SSH", Status: doctor.Warn,
			Message: "could not reach the server over SSH, so the cluster is read through the Kubernetes API instead", Detail: dialErr.Error()}
	} else {
		runner = client
		defer func() { _ = client.Close() }()
	}

	var snap doctor.Snapshot
	var host installer.HostOS
	switch {
	case runner != nil:
		snap = doctor.FetchOverSSH(runner)
		host, _ = installer.DetectHostOS(client)
	default:
		snap = fetchWithoutSSH(ctx, cluster, dialErr)
	}

	groups := collectDoctorResults(ctx, cluster, runner, snap, sshResult)
	out := os.Stdout
	failures := renderDoctor(out, cluster.Name, groups)
	if failures == 0 {
		return nil
	}

	var nodeNames []string
	for _, n := range snap.Nodes {
		nodeNames = append(nodeNames, n.Name)
	}
	secrets, names := doctorKnownValues(cluster, keyFlag, operatorEmail(cluster), nodeNames)
	issue := issueContext{
		KipVersion: rootCmd.Version,
		Client:     runtime.GOOS + "/" + runtime.GOARCH,
		Flags:      setFlagNames(cmd.Flags()),
	}
	reportFailure(out, "Think this is a Kipper bug? This link opens a GitHub issue form with the failing checks, versions and OS filled in:",
		"doctor-report", doctorIssueLink(groups, issue, host, snap.Profile), doctorReport(groups, secrets, names))
	return fmt.Errorf("kip doctor found %d %s", failures, plural(failures, "problem", "problems"))
}

// nodesUnavailableError directs failed node-list requests to kip doctor,
// which can diagnose the cluster over SSH even when API access fails.
func nodesUnavailableError(err error) error {
	return fmt.Errorf("listing nodes: %w\n\n  Run kip doctor to find out why the cluster is not answering", err)
}

// fetchWithoutSSH reads the cluster through the Kubernetes API when the
// server cannot be reached over SSH, and marks every part unread when the API
// client cannot be built either.
func fetchWithoutSSH(ctx context.Context, cluster *config.Cluster, sshErr error) doctor.Snapshot {
	client, err := k8s.NewFromCluster(cluster)
	if err != nil {
		return unreadSnapshot(fmt.Errorf("no SSH connection (%w) and no API client (%w)", sshErr, err))
	}
	ctx, cancel := context.WithTimeout(ctx, doctorAPITimeout)
	defer cancel()
	return doctor.FetchViaAPI(ctx, client.Clientset(), client.Dynamic())
}

func unreadSnapshot(cause error) doctor.Snapshot {
	err := cause
	missing := map[doctor.Part]error{}
	for _, p := range []doctor.Part{doctor.PartNodes, doctor.PartWorkloads, doctor.PartHopCert, doctor.PartTLSStores,
		doctor.PartClusterHost, doctor.PartProfile, doctor.PartHeartbeat} {
		missing[p] = err
	}
	return doctor.Snapshot{Missing: missing}
}

func collectDoctorResults(ctx context.Context, cluster *config.Cluster, runner doctor.Runner, snap doctor.Snapshot, sshResult doctor.Result) []doctorGroup {
	hostResults := []doctor.Result{sshResult}
	if runner != nil {
		hostResults = append(hostResults, doctor.CheckK3s(runner))
	}

	kube := kubernetesAPIChecks(ctx, cluster)
	kube = append(kube, doctor.CheckNodes(snap))
	kube = append(kube, doctor.CheckWorkloads(snap)...)

	groups := []doctorGroup{{"Host", hostResults}, {"Kubernetes", kube}}
	if !strings.HasSuffix(cluster.Domain, ".kipper.run") {
		return groups
	}
	hopSecret, keys := doctor.CheckHopCertificate(snap)
	groups = append(groups,
		doctorGroup{"Gateway hop", []doctor.Result{
			hopSecret,
			doctor.CheckTLSStore(snap),
			doctor.CheckServedCertificate(ctx, cluster.Host, cluster.DexHost(), keys),
			doctor.CheckHeartbeat(snap, time.Now()),
			doctor.CheckClusterHost(snap, cluster.Host),
		}},
		doctorGroup{"kipper.run gateway", []doctor.Result{
			doctor.CheckGateway(ctx, gatewayHealthURL),
			doctor.CheckRoute(ctx, "https://"+cluster.DexHost()+"/dex/.well-known/openid-configuration"),
		}},
	)
	return groups
}

// kubernetesAPIChecks checks that the API server answers and accepts the
// operator's login, or reports both as not checked when the kubeconfig
// cannot be read.
func kubernetesAPIChecks(ctx context.Context, cluster *config.Cluster) []doctor.Result {
	endpoint, err := readClusterEndpoint(cluster.Kubeconfig)
	if err != nil {
		return []doctor.Result{
			{ID: "api", Label: "Kubernetes API", Status: doctor.Skip, Message: "not checked (could not read the kubeconfig)", Detail: err.Error()},
			{ID: "login", Label: "Your login", Status: doctor.Skip, Message: "not checked (could not read the kubeconfig)", Detail: err.Error()},
		}
	}
	return []doctor.Result{doctor.CheckAPI(ctx, endpoint.server), checkLogin(ctx, cluster, endpoint)}
}

// verifyOperatorIdentity is replaced in tests.
var verifyOperatorIdentity = installer.VerifyOperatorIdentity

// checkLogin asks whether the API server accepts the operator's own login.
// Both of the ways it fails after a bad restart surface here: the session
// cannot refresh because Dex is unreachable through the gateway, or the API
// server rejects the token because it cannot load Dex's discovery document.
func checkLogin(ctx context.Context, cluster *config.Cluster, endpoint clusterEndpoint) doctor.Result {
	const id, label = "login", "Your login"
	ctx, cancel := context.WithTimeout(ctx, doctorLoginTimeout)
	defer cancel()
	type outcome struct {
		result installer.ProofResult
		detail string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, detail, err := verifyOperatorIdentity(ctx, cluster.Domain, cluster.DexHost(), cluster.Domain, endpoint.server, endpoint.caData)
		done <- outcome{result, detail, err}
	}()
	var result installer.ProofResult
	var detail string
	var err error
	select {
	case o := <-done:
		result, detail, err = o.result, o.detail, o.err
	case <-ctx.Done():
		result, detail = installer.ProofTransportError, ctx.Err().Error()
	}
	switch {
	case errors.Is(err, auth.ErrDexUnreachable):
		return doctor.Result{ID: id, Label: label, Status: doctor.Fail,
			Message: "your login cannot refresh: Dex is unreachable through the gateway (see the gateway checks)", Detail: err.Error()}
	case err != nil && result == installer.ProofAuthnRejected:
		return doctor.Result{ID: id, Label: label, Status: doctor.Warn,
			Message: "you are not logged in to this cluster, so your login was not checked (run kip auth login)"}
	case err != nil:
		return doctor.Result{ID: id, Label: label, Status: doctor.Warn, Message: "could not check your login", Detail: err.Error()}
	case result == installer.ProofPass || result == installer.ProofPassNonAdmin:
		return doctor.Result{ID: id, Label: label, Status: doctor.Pass, Message: "the API server accepts your login"}
	case result == installer.ProofTransportError && strings.Contains(detail, context.DeadlineExceeded.Error()):
		return doctor.Result{ID: id, Label: label, Status: doctor.Fail,
			Message: fmt.Sprintf("could not verify your login within %s: refreshing your session or checking it with the API server did not finish", doctorLoginTimeout),
			Detail:  detail}
	case result == installer.ProofTransportError:
		return doctor.Result{ID: id, Label: label, Status: doctor.Fail,
			Message: "your login could not be checked: the API server is unreachable", Detail: detail}
	case result == installer.ProofAuthzDeniedAsAdmin:
		return doctor.Result{ID: id, Label: label, Status: doctor.Fail, Message: "the API server accepts your login but denies access", Detail: detail}
	}
	return doctor.Result{ID: id, Label: label, Status: doctor.Fail,
		Message: "the API server rejects your login. It may be unable to load Dex's discovery document (see the served certificate)", Detail: detail}
}

var doctorMarks = map[doctor.Status]string{doctor.Pass: "✔", doctor.Warn: "⚠", doctor.Fail: "✗", doctor.Skip: "⚠"}

// renderDoctor prints the checks under their headings and returns how many failed.
func renderDoctor(out io.Writer, clusterName string, groups []doctorGroup) int {
	failures := 0
	say(out, "\n  kip doctor: %s\n\n", clusterName)
	for _, g := range groups {
		say(out, "  %s\n", g.Title)
		for _, r := range g.Results {
			say(out, "    %s  %s\n", doctorMarks[r.Status], r.Message)
			if r.Status == doctor.Fail {
				failures++
			}
		}
		say(out, "\n")
	}
	if failures == 0 {
		say(out, "  No problems found.\n\n")
	} else {
		say(out, "  %d %s found.\n\n", failures, plural(failures, "problem", "problems"))
	}
	return failures
}

var doctorStatusWords = map[doctor.Status]string{doctor.Pass: "pass", doctor.Warn: "warn", doctor.Fail: "fail", doctor.Skip: "not checked"}

// doctorIssueLink returns a prefilled bug report naming the failing checks,
// or "" when nothing failed. It includes check IDs and statuses, version and
// environment metadata, and flag names. Messages and details stay out of the URL
// because they may contain private data.
func doctorIssueLink(groups []doctorGroup, issue issueContext, host installer.HostOS, profile string) string {
	var lines []string
	firstFailure := ""
	for _, g := range groups {
		for _, r := range g.Results {
			lines = append(lines, r.ID+": "+doctorStatusWords[r.Status])
			if r.Status == doctor.Fail && firstFailure == "" {
				firstFailure = r.ID
			}
		}
	}
	if firstFailure == "" {
		return ""
	}
	repro := "Ran `kip doctor`"
	if len(issue.Flags) > 0 {
		repro += " with " + strings.Join(issue.Flags, " ")
	}
	return issuelink.Report{
		Title:        "kip doctor: " + firstFailure + " failed",
		WhatHappened: "kip doctor reported:\n\n" + strings.Join(lines, "\n"),
		Repro:        repro,
		KipVersion:   issue.KipVersion,
		Environment:  fmt.Sprintf("kip on %s, server %s, profile %s", issue.Client, serverDescription(host), plainProfile(profile)),
	}.URL()
}

// doctorReport lists failed, warning and skipped checks with their evidence,
// scrubs the text for sharing and keeps only the last errorReportLines lines.
func doctorReport(groups []doctorGroup, secrets, names []string) string {
	var b strings.Builder
	for _, g := range groups {
		for _, r := range g.Results {
			if r.Status == doctor.Pass {
				continue
			}
			fmt.Fprintf(&b, "[%s] %s\n", r.ID, r.Message)
			if r.Detail != "" {
				b.WriteString(r.Detail + "\n")
			}
		}
	}
	lines := strings.Split(strings.TrimRight(issuelink.Scrub(b.String(), secrets, names), "\n"), "\n")
	if len(lines) > errorReportLines {
		lines = lines[len(lines)-errorReportLines:]
	}
	return strings.Join(lines, "\n")
}

// doctorKnownValues collects the gateway credential and known identifiers for
// the cluster, server, nodes, operator and local machine for report redaction.
func doctorKnownValues(cluster *config.Cluster, keyFlag, operatorEmail string, nodeNames []string) (secrets, names []string) {
	explicitKey, fallbackKey := resolveSSHKey(keyFlag, cluster)
	secrets = []string{cluster.GatewayToken}
	names = []string{cluster.Name, cluster.Host, cluster.Domain, cluster.ConsoleHost(), cluster.ConsoleAPIHost(), cluster.DexHost(),
		sshUserEntry(cluster.SSHUser), cluster.SSHKey, keyFlag, explicitKey, fallbackKey, cluster.Kubeconfig, cluster.Org,
		cluster.OrgDisplayName, cluster.MachineID, operatorEmail}
	names = append(names, cluster.TrustedProxies...)
	names = append(names, nodeNames...)
	for _, n := range cluster.Nodes {
		names = append(names, n.Name, n.Host)
	}
	if home, err := os.UserHomeDir(); err == nil {
		names = append(names, home)
	}
	return secrets, names
}

// operatorEmail returns the email of the operator's stored login for the
// cluster, or "" when there is none.
func operatorEmail(cluster *config.Cluster) string {
	store, err := auth.Load()
	if err != nil {
		return ""
	}
	if creds := store.Credential(cluster.Domain); creds != nil {
		return creds.Email
	}
	return ""
}
