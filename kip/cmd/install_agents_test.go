package cmd

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// recordsOnlyServer answers the node records read and fails the test if
// anything takes the node lease.
type recordsOnlyServer struct {
	t       *testing.T
	records string
	err     error
}

func (s recordsOnlyServer) Run(command string) (string, error) {
	if !strings.Contains(command, "kipper-nodes") {
		s.t.Fatalf("unexpected command %q", command)
	}
	return s.records, s.err
}
func (s recordsOnlyServer) RunStdin(command string, _ io.Reader) (string, error) {
	return s.Run(command)
}
func (s recordsOnlyServer) Start(command string) (*ssh.Held, error) {
	s.t.Fatalf("the node lease was taken for %q", command)
	return nil, nil
}
func (recordsOnlyServer) SurvivesSSHRateLimit() bool             { return true }
func (recordsOnlyServer) ConfirmFreshConnection() error          { return nil }
func (recordsOnlyServer) ObservedAddress(string) (string, error) { return "", nil }

func TestUpgradeAgentsAfterInstallSkipsASingleNodeCluster(t *testing.T) {
	server := recordsOnlyServer{t: t, records: `{"0123456789abcdef0123456789abcdef":"{\"name\":\"server-1\",\"host\":\"203.0.113.10\",\"machineID\":\"0123456789abcdef0123456789abcdef\",\"role\":\"server\",\"state\":\"ready\"}"}`}
	dial := func(string) (installer.FirewallRunner, func(), error) {
		t.Fatal("a single-node cluster has no worker to connect to")
		return nil, nil, nil
	}
	require.NoError(t, upgradeAgentsAfterInstall(io.Discard, server, dial))
}

func TestUpgradeAgentsAfterInstallReportsUnreadableRecords(t *testing.T) {
	server := recordsOnlyServer{t: t, err: errors.New("connection reset")}
	dial := func(string) (installer.FirewallRunner, func(), error) { return nil, nil, nil }
	err := upgradeAgentsAfterInstall(io.Discard, server, dial)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connection reset")
}
