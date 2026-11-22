// Package readfailure formats failed cluster reads while protecting sensitive output.
package readfailure

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Error describes a failed cluster read, using what to identify the operation.
// The caller must supply a non-nil err. Empty or whitespace-only out wraps err.
// Otherwise, the result omits out and does not wrap err because both may contain
// secrets. It reports a timeout for context.DeadlineExceeded or an SSH failure
// for exit status 255. Other failures include the exit status when available and
// the last recognized kubectl error, restricted to fixed text and allowed API
// reasons. Unrecognized output is described by its byte count.
func Error(what, out string, err error) error {
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("%s: %w", what, err)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s timed out", what)
	}
	var exit interface{ ExitCode() int }
	if !errors.As(err, &exit) {
		return fmt.Errorf("%s failed: %s", what, kubectlFailure(out))
	}
	if exit.ExitCode() == 255 {
		return fmt.Errorf("%s failed: the ssh connection failed", what)
	}
	return fmt.Errorf("%s failed with exit status %d: %s", what, exit.ExitCode(), kubectlFailure(out))
}

// serverErrorReason restricts API error reasons to an allowlist of Kubernetes
// status reasons, such as "Error from server (NotFound)".
var serverErrorReason = regexp.MustCompile(`^Error from server \((NotFound|AlreadyExists|Conflict|Forbidden|Unauthorized|Invalid|BadRequest|MethodNotAllowed|NotAcceptable|Gone|Timeout|ServerTimeout|TooManyRequests|InternalError|ServiceUnavailable|Expired|RequestEntityTooLarge|UnsupportedMediaType)\)`)

// kubectlFailure classifies output using the last recognized kubectl error line,
// falling back to the byte count. It uses fixed text and allowlisted API reasons
// because even a line with a known error prefix can contain sensitive object data.
func kubectlFailure(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		switch {
		case serverErrorReason.MatchString(line):
			return "the API server answered " + serverErrorReason.FindStringSubmatch(line)[1]
		case strings.HasPrefix(line, "The connection to the server"), strings.HasPrefix(line, "Unable to connect to the server"):
			return "kubectl could not reach the API server"
		case strings.HasPrefix(line, "error: the server doesn't have a resource type"):
			return "the API server does not serve this resource type"
		case strings.HasPrefix(line, "error: You must be logged in to the server"):
			return "kubectl is not logged in to the API server"
		}
	}
	return fmt.Sprintf("the command wrote %d bytes, which are not shown because they may hold secrets", len(out))
}
