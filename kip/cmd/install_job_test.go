package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/installjob"
)

// The entry the driver pushes up describes the operator's machine as well as the
// cluster; only the cluster settings belong in the server's config.
func TestServerEntryFromPushed(t *testing.T) {
	pushed := config.Cluster{
		Name: "shop", Host: "203.0.113.10", Domain: "shop.example", GatewayToken: "tok",
		Kubeconfig: `C:\Users\ops\.kip\clusters\shop.yaml`, SSHKey: `C:\Users\ops\.ssh\id_ed25519`,
		CurrentProject: "billing", CurrentEnvironment: "staging", HostWiped: true, MachineID: "0123456789abcdef0123456789abcdef",
		BackupStorage: &config.BackupStorageRef{Mode: "external", Bucket: "b"},
		DNSResolvers:  []string{"192.0.2.53"},
	}

	got := serverEntryFromPushed(pushed, nil)

	assert.Equal(t, "tok", got.GatewayToken)
	assert.Equal(t, "b", got.BackupStorage.Bucket)
	assert.Equal(t, []string{"192.0.2.53"}, got.DNSResolvers)
	assert.Empty(t, got.Kubeconfig)
	assert.Empty(t, got.SSHKey)
	assert.Empty(t, got.CurrentProject)
	assert.Empty(t, got.CurrentEnvironment)
	assert.False(t, got.HostWiped)
	assert.Empty(t, got.MachineID)
}

func TestServerEntryFromPushedKeepsTheServersOwnFields(t *testing.T) {
	pushed := config.Cluster{Name: "shop", Host: "203.0.113.10", GatewayToken: "new", Kubeconfig: `C:\Users\ops\.kip\clusters\shop.yaml`}
	server := &config.Cluster{Name: "shop", Host: "203.0.113.10", GatewayToken: "old",
		Kubeconfig: "/root/.kip/clusters/shop.yaml", MachineID: "0123456789abcdef0123456789abcdef"}

	got := serverEntryFromPushed(pushed, server)

	assert.Equal(t, "new", got.GatewayToken)
	assert.Equal(t, "/root/.kip/clusters/shop.yaml", got.Kubeconfig)
	assert.Equal(t, "0123456789abcdef0123456789abcdef", got.MachineID)

	server.Host = "198.51.100.7"
	got = serverEntryFromPushed(pushed, server)
	assert.Empty(t, got.Kubeconfig, "another host's entry lends nothing")
}

func TestJobResultCarriesAClusterOnlyWithAKubeconfig(t *testing.T) {
	home := withFakeHome(t)
	kc := filepath.Join(home, "shop.yaml")
	cfg := &config.Config{}
	cfg.AddCluster(config.Cluster{Name: "shop", Host: "203.0.113.10", Kubeconfig: kc})

	r := jobResult("run-1", errors.New("bad resolver"), "", "", cfg, "203.0.113.10")
	assert.Equal(t, installjob.PhaseFailed, r.Outcome)
	assert.Nil(t, r.Cluster, "the kubeconfig file is missing")

	require.NoError(t, os.WriteFile(kc, []byte(resultKubeconfig), 0o600))
	r = jobResult("run-1", nil, "admin@shop.example", "pw", cfg, "203.0.113.10")
	assert.Equal(t, installjob.PhaseSucceeded, r.Outcome)
	require.NotNil(t, r.Cluster)
	assert.Equal(t, resultKubeconfig, r.Kubeconfig)
	assert.Equal(t, "pw", r.AdminPassword)
	assert.Equal(t, "admin@shop.example", r.AdminEmail)
}

// fakeJobSystemd stands in for the server's systemd in command tests.
type fakeJobSystemd struct{ started []string }

func (f *fakeJobSystemd) Start(unit string, _ []string, _ string) error {
	f.started = append(f.started, unit)
	return nil
}
func (f *fakeJobSystemd) Active(string) (bool, error) { return true, nil }
func (f *fakeJobSystemd) Stop(string) error           { return nil }

func TestInstallJobClaimReadsTheRequestFromStdin(t *testing.T) {
	root := t.TempDir()
	sd := &fakeJobSystemd{}
	restore := useJobHost(root, sd)
	defer restore()
	req := installjob.Request{Version: installjob.RequestVersion, RunID: "run-1", Host: "203.0.113.10"}
	body, err := json.Marshal(req)
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, runInstallJobClaim(bytes.NewReader(body), &out, "/usr/local/bin/kip"))

	var got installjob.Outcome
	require.NoError(t, json.Unmarshal(out.Bytes(), &got))
	assert.Equal(t, installjob.Outcome{Action: installjob.ActionStarted, RunID: "run-1"}, got)
	assert.Equal(t, []string{"kipper-install-run-1"}, sd.started)
	_, err = os.Stat(filepath.Join(root, "run-1", "request.json"))
	assert.NoError(t, err)
}

func TestInstallJobCommandsAreHidden(t *testing.T) {
	assert.True(t, installJobCmd.Hidden, "the driver's own plumbing, not an operator command")
	assert.True(t, installCmd.Flags().Lookup("request").Hidden)
	assert.False(t, strings.Contains(rootCmd.UsageString(), "install-job"))
}

func TestJobResultNeverCarriesACredential(t *testing.T) {
	home := withFakeHome(t)
	kc := filepath.Join(home, "shop.yaml")
	require.NoError(t, os.WriteFile(kc, []byte(adminCertKubeconfig), 0o600))
	cfg := &config.Config{}
	cfg.AddCluster(config.Cluster{Name: "shop", Host: "203.0.113.10", Kubeconfig: kc})

	r := jobResult("run-1", errors.New("bad resolver"), "", "", cfg, "203.0.113.10")

	assert.Nil(t, r.Cluster)
	assert.Empty(t, r.Kubeconfig)
}

// After a rename on the operator's machine, the server's entry for the host
// takes the new name, keeping its own fields, so the install's lookup by host
// finds the settings that were pushed.
func TestRecordPushedEntryReplacesTheServersEntryForTheHost(t *testing.T) {
	cfg := &config.Config{}
	cfg.AddCluster(config.Cluster{Name: "old-name", Host: "203.0.113.10", GatewayToken: "old", Kubeconfig: "/root/.kip/clusters/old-name.yaml"})
	cfg.AddCluster(config.Cluster{Name: "other", Host: "198.51.100.7"})
	cfg.CurrentCluster = "old-name"

	recordPushedEntry(cfg, config.Cluster{Name: "shop", Host: "203.0.113.10", GatewayToken: "new"})

	assert.Nil(t, cfg.GetCluster("old-name"))
	require.NotNil(t, cfg.GetCluster("other"))
	entry := cfg.GetClusterByHost("203.0.113.10")
	require.NotNil(t, entry)
	assert.Equal(t, "shop", entry.Name)
	assert.Equal(t, "new", entry.GatewayToken)
	assert.Equal(t, "/root/.kip/clusters/old-name.yaml", entry.Kubeconfig)
	assert.Equal(t, "shop", cfg.CurrentCluster, "the server's current cluster follows the rename")
}

func TestFreshConnectionMessagesNameEachCheck(t *testing.T) {
	first, second, third := freshConnectionMessage(1), freshConnectionMessage(2), freshConnectionMessage(3)

	assert.Contains(t, first, "before")
	assert.Contains(t, second, "new firewall rules")
	assert.NotEqual(t, first, second)
	assert.Equal(t, second, third)
}

func TestDrivenIssueReportBuildsTheLinkAndScrubsTheErrorOnTheServer(t *testing.T) {
	opts := installer.Options{Host: "203.0.113.10", Domain: "lab.example.org", AdminEmail: "ops@example.org"}
	err := &installer.StepError{Step: "Installing k3s", Host: installer.HostOS{ID: "debian", VersionID: "13"},
		Err: errors.New("curl https://lab.example.org from 203.0.113.10 failed: exit status 7")}

	link, report := drivenIssueReport(opts, err, nil, "v0.31.0")

	decoded, decodeErr := url.QueryUnescape(link)
	require.NoError(t, decodeErr)
	assert.Contains(t, decoded, "server-driven install")
	assert.Contains(t, decoded, "debian 13")
	assert.NotContains(t, decoded, "curl")
	assert.Contains(t, report, "exit status 7")
	assert.NotContains(t, report, "203.0.113.10")
	assert.NotContains(t, report, "lab.example.org")
}

func TestJobResultCarriesTheIssueReportToTheDriver(t *testing.T) {
	r := installjob.Result{RunID: "r1", Outcome: installjob.PhaseFailed,
		IssueLink: "https://github.com/getkipper/kipper/issues/new?template=bug_report.yml", ErrorReport: "E: boom"}
	raw, err := json.Marshal(r)
	require.NoError(t, err)

	var back installjob.Result
	require.NoError(t, json.Unmarshal(raw, &back))
	assert.Equal(t, r.IssueLink, back.IssueLink)
	assert.Equal(t, r.ErrorReport, back.ErrorReport)
}
