// Package mongotool builds shell scripts for MongoDB tools that authenticate
// with the root credentials from a service pod's environment.
package mongotool

import (
	"fmt"
	"strings"
)

// configWriter uses a double-quoted YAML scalar because bare passwords containing
// ": ", " #", or a leading "*" or "{" can be misparsed, and "yes" becomes a boolean.
// awk reads the password from its environment. sprintf("%c") supplies backslashes
// and quotes so only the program's double quotes need shell escaping.
// Unicode line breaks and C1 controls pass through unchanged. The mongo image
// rejects root passwords containing them because its default SCRAM-SHA-256
// mechanism applies SASLprep, which prohibits them.
const configWriter = `BEGIN { ` +
	`for (i = 1; i < 32; i++) ctl[sprintf("%c", i)] = i; ctl[sprintf("%c", 127)] = 127; ` +
	`bs = sprintf("%c", 92); dq = sprintf("%c", 34); ` +
	`p = ENVIRON["MONGO_INITDB_ROOT_PASSWORD"]; s = ""; ` +
	`for (i = 1; i <= length(p); i++) { ch = substr(p, i, 1); ` +
	`if (ch == bs || ch == dq) { s = s bs ch } else if (ch in ctl) { s = s sprintf("%sx%02x", bs, ctl[ch]) } else { s = s ch } } ` +
	`printf "password: %s%s%s%c", dq, s, dq, 10 }`

// Script returns a sh script that runs invocation with MONGO_INITDB_ROOT_USERNAME
// and MONGO_INITDB_ROOT_PASSWORD from the environment, authenticating against admin.
// It passes the password through a temporary config file with mode 0600 to keep
// it out of process arguments. It removes the file after the tool exits and
// returns the tool's exit status. A config-writing failure removes the file
// and exits with status 1 before running the tool.
//
// invocation must be trusted shell code with any arguments already escaped.
// The script requires mktemp, chmod, awk and rm. The wrapper adds no single
// quotes, so the result can be single-quoted in a sh -c command if invocation
// also contains none.
func Script(invocation string) string {
	writer := `awk "` + strings.ReplaceAll(configWriter, `"`, `\"`) + `"`
	return fmt.Sprintf(
		`c="$(mktemp)"; chmod 600 "$c"; %s >"$c" || { rm -f "$c"; exit 1; }; %s --config="$c" -u "$MONGO_INITDB_ROOT_USERNAME" --authenticationDatabase admin; rc=$?; rm -f "$c"; exit $rc`,
		writer, invocation)
}
