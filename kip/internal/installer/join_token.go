package installer

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The capture group extracts the token ID for deletion without exposing the secret.
var bootstrapTokenPattern = regexp.MustCompile(`^K10[0-9a-f]+::([a-z0-9]{6})\.[a-z0-9]{16}$`)

const createJoinTokenCmd = `k3s token create --ttl 1h --description "kip node add"` //nolint:gosec // G101: a command, not a credential

// withJoinToken creates a one-hour bootstrap token and attempts deletion after
// join returns. Cleanup failures only warn because the token expires on its own.
func withJoinToken(server CommandRunner, serverHost string, join func(token string) error) error {
	out, mintErr := server.Run(createJoinTokenCmd)
	// The token can arrive even when the connection fails afterwards.
	token, id, ok := parseBootstrapToken(out)
	if ok {
		defer deleteJoinToken(server, id)
	}
	if mintErr != nil {
		return fmt.Errorf("creating a join token on the k3s server at %s: %v", serverHost, withoutOutput(mintErr))
	}
	if !ok {
		return fmt.Errorf("creating a join token on the k3s server at %s: no valid bootstrap token found in the command output", serverHost)
	}
	return redactToken(join(token), token)
}

func deleteJoinToken(server CommandRunner, id string) {
	if _, err := server.Run("k3s token delete " + id); err != nil {
		fmt.Printf("  ⚠  could not delete join token %s on the server: %v\n      The token expires within an hour. To delete it now, run 'k3s token delete %s' on the server.\n", id, withoutOutput(err), id)
	}
}

// redactToken removes the token's secret from err's text, which can carry
// remote command output. Errors without it are returned unchanged.
func redactToken(err error, token string) error {
	if err == nil {
		return nil
	}
	secret := token[strings.LastIndex(token, ".")+1:]
	msg := err.Error()
	if !strings.Contains(msg, secret) {
		return err
	}
	msg = strings.ReplaceAll(msg, token, "<join token>")
	return errors.New(strings.ReplaceAll(msg, secret, "<join token>"))
}

// parseBootstrapToken finds the last valid token line, ignoring other output
// such as warnings merged from stderr by the SSH runner.
func parseBootstrapToken(out string) (token, id string, ok bool) {
	lines := strings.Split(out, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if m := bootstrapTokenPattern.FindStringSubmatch(line); m != nil {
			return line, m[1], true
		}
	}
	return "", "", false
}
