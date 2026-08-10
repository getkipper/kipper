package installer

import (
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
)

type scriptedRunner struct {
	out      string
	err      error
	commands []string
}

func (r *scriptedRunner) Run(command string) (string, error) {
	r.commands = append(r.commands, command)
	return r.out, r.err
}

func (r *scriptedRunner) RunStdin(command string, _ io.Reader) (string, error) {
	return r.Run(command)
}

// The token is the only thing that can release a *.kipper.run name, and it lives
// on the cluster rather than in the operator's config. Reading it is what lets
// an uninstall hand the name back instead of stranding it.
func TestReadGatewayCredentialsDecodesTheStoredToken(t *testing.T) {
	runner := &scriptedRunner{out: "'" + base64.StdEncoding.EncodeToString([]byte("tok-abc123")) + "'\n"}

	got, err := ReadGatewayCredentials(runner)
	if err != nil {
		t.Fatalf("ReadGatewayCredentials: %v", err)
	}
	if got != "tok-abc123" {
		t.Errorf("token = %q, want tok-abc123", got)
	}
	if len(runner.commands) != 1 || !strings.Contains(runner.commands[0], "--ignore-not-found") {
		t.Errorf("the lookup must tolerate an absent secret, got %q", runner.commands)
	}
}

// A cluster that never registered holds no secret. That is an empty answer, not
// a failure, or every custom-domain uninstall would warn about nothing.
func TestReadGatewayCredentialsTreatsAnAbsentSecretAsEmpty(t *testing.T) {
	got, err := ReadGatewayCredentials(&scriptedRunner{out: "\n"})
	if err != nil {
		t.Fatalf("ReadGatewayCredentials: %v", err)
	}
	if got != "" {
		t.Errorf("token = %q, want empty", got)
	}
}

// A cluster that cannot be asked is not a cluster with no token. Returning empty
// would let the caller conclude there is nothing to release.
func TestReadGatewayCredentialsSurfacesALookupFailure(t *testing.T) {
	_, err := ReadGatewayCredentials(&scriptedRunner{err: errors.New("connection refused")})
	if err == nil {
		t.Fatal("a failed lookup must be an error, not an empty token")
	}
}
