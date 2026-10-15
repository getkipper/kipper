package installer

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testJoinTokenID = "abc123"
	testJoinToken   = "K10" + "0123456789abcdef" + "::" + testJoinTokenID + ".secretsecret0000"
)

// tokenServer stands in for the k3s server that mints and deletes bootstrap tokens.
type tokenServer struct {
	createOut string
	createErr error
	deleteErr error
	commands  []string
}

func (s *tokenServer) Run(cmd string) (string, error) {
	s.commands = append(s.commands, cmd)
	switch {
	case strings.HasPrefix(cmd, "k3s token create"):
		return s.createOut, s.createErr
	case strings.HasPrefix(cmd, "k3s token delete"):
		return "", s.deleteErr
	}
	return "", nil
}

func (s *tokenServer) RunStdin(cmd string, _ io.Reader) (string, error) { return s.Run(cmd) }

func (s *tokenServer) deleted() []string {
	var out []string
	for _, c := range s.commands {
		if strings.HasPrefix(c, "k3s token delete") {
			out = append(out, c)
		}
	}
	return out
}

func TestWithJoinTokenPassesAShortLivedTokenAndDeletesItByID(t *testing.T) {
	s := &tokenServer{createOut: testJoinToken + "\n"}
	var got string

	err := withJoinToken(s, "203.0.113.10", func(token string) error {
		got = token
		assert.Empty(t, s.deleted(), "the token must stay valid while the agent joins")
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, testJoinToken, got)
	require.NotEmpty(t, s.commands)
	assert.Contains(t, s.commands[0], "--ttl 1h", "a token that outlives a failed cleanup must still expire")
	assert.Equal(t, []string{"k3s token delete " + testJoinTokenID}, s.deleted())
	for _, c := range s.commands {
		assert.NotContains(t, c, "secretsecret0000", "command lines reach the process list and error messages")
	}
}

func TestWithJoinTokenDeletesTheTokenWhenTheJoinFails(t *testing.T) {
	s := &tokenServer{createOut: testJoinToken}

	err := withJoinToken(s, "203.0.113.10", func(string) error { return errors.New("agent install failed") })

	require.ErrorContains(t, err, "agent install failed")
	assert.Equal(t, []string{"k3s token delete " + testJoinTokenID}, s.deleted())
}

func TestWithJoinTokenIgnoresWarningsAroundTheToken(t *testing.T) {
	s := &tokenServer{createOut: "W1016 00:00:00 warning: something\n" + testJoinToken + "\n"}
	var got string

	require.NoError(t, withJoinToken(s, "203.0.113.10", func(token string) error { got = token; return nil }))

	assert.Equal(t, testJoinToken, got, "the SSH runner merges stderr into the output")
}

func TestWithJoinTokenNamesTheServerWhenMintingFails(t *testing.T) {
	s := &tokenServer{createErr: fmt.Errorf("running k3s token create: %w: %s", errors.New("exit status 1"), testJoinToken)}
	called := false

	err := withJoinToken(s, "203.0.113.10", func(string) error { called = true; return nil })

	require.ErrorContains(t, err, "203.0.113.10")
	assert.NotContains(t, err.Error(), "secretsecret0000")
	assert.False(t, called)
	assert.Empty(t, s.deleted())
}

func TestWithJoinTokenRefusesOutputWithoutAToken(t *testing.T) {
	for _, out := range []string{"", "no token here\n", "K10abc::short.secret\n", testJoinToken + "; rm -rf /\n", "x; " + testJoinToken + "\n"} {
		s := &tokenServer{createOut: out}
		called := false

		err := withJoinToken(s, "203.0.113.10", func(string) error { called = true; return nil })

		require.Error(t, err, "%q", out)
		assert.NotContains(t, err.Error(), "secretsecret0000")
		assert.False(t, called, "%q", out)
	}
}

func TestWithJoinTokenSucceedsWhenTheDeleteFails(t *testing.T) {
	s := &tokenServer{createOut: testJoinToken, deleteErr: errors.New("connection reset")}

	assert.NoError(t, withJoinToken(s, "203.0.113.10", func(string) error { return nil }), "the TTL removes the token anyway")
}

func TestJoinWorkerNodeJoinsWithAFreshTokenAndDeletesItAfterRegistration(t *testing.T) {
	id := HostIdentity{MachineID: strings.Repeat("a", 32), BootID: "11111111-2222-3333-4444-555555555555"}
	server := &storageHostRunner{replies: map[string]string{ //nolint:gosec // G101: a synthetic token fixture
		"k3s token create":            testJoinToken + "\n",
		k3sVersionProbeCmd:            "k3s version " + k3sVersion + " (abc)\n",
		"get svc zot":                 "10.43.0.10",
		zotPullSecret:                 "cHVsbA==",
		zotTLSSecret:                  "Y2E=",
		nodeIdentityQuery():           "worker-1|" + id.MachineID + "|" + id.BootID + "\n",
		"/var/lib/rancher/k3s/server": "stale-static-token",
	}}
	worker := &storageHostRunner{replies: map[string]string{k3sVersionProbeCmd: "KIP_K3S_ABSENT\n"}}

	name, err := JoinWorkerNode(server, worker, "203.0.113.10", id)

	require.NoError(t, err)
	assert.Equal(t, "worker-1", name)
	assert.Contains(t, worker.stdin, testJoinToken+"\n", "the agent reads the minted token on stdin")
	register, del := -1, -1
	for i, c := range server.commands {
		if c == nodeIdentityQuery() && register == -1 {
			register = i
		}
		if c == "k3s token delete "+testJoinTokenID {
			del = i
		}
	}
	require.NotEqual(t, -1, del, "a leftover join token lets any host that sees it join the cluster")
	assert.Less(t, register, del, "the agent needs the token until it has registered")
	for _, c := range server.commands {
		assert.NotContains(t, c, "/var/lib/rancher/k3s/server/", "joins use a minted token, never a static one from the server's files")
	}
}

func TestWithJoinTokenKeepsTheTokenOutOfJoinErrors(t *testing.T) {
	s := &tokenServer{createOut: testJoinToken}

	err := withJoinToken(s, "203.0.113.10", func(token string) error {
		return fmt.Errorf("running k3s agent installer: %w: + K3S_TOKEN=%s\nsecret=%s", errors.New("exit status 1"), token, "secretsecret0000")
	})

	require.ErrorContains(t, err, "exit status 1", "the installer's output is what explains the failure")
	assert.NotContains(t, err.Error(), "secretsecret0000")
	assert.Equal(t, []string{"k3s token delete " + testJoinTokenID}, s.deleted())
}

func TestWithJoinTokenKeepsJoinErrorsWithoutTheTokenIntact(t *testing.T) {
	s := &tokenServer{createOut: testJoinToken}
	cause := errors.New("worker is busy")

	err := withJoinToken(s, "203.0.113.10", func(string) error { return fmt.Errorf("joining: %w", cause) })

	assert.ErrorIs(t, err, cause)
}

func TestWithJoinTokenDeletesATokenMintedBeforeTheConnectionFailed(t *testing.T) {
	s := &tokenServer{createOut: testJoinToken + "\n", createErr: errors.New("connection lost")}
	called := false

	err := withJoinToken(s, "203.0.113.10", func(string) error { called = true; return nil })

	require.ErrorContains(t, err, "203.0.113.10")
	assert.False(t, called, "an unsuccessful mint is no basis for a join")
	assert.Equal(t, []string{"k3s token delete " + testJoinTokenID}, s.deleted())
}
