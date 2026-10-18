package installer

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	k3sConfigPath = "/etc/rancher/k3s/config.yaml"

	// k3sConfigBackupPath preserves the configuration preceding a pending write for rollback.
	k3sConfigBackupPath = k3sConfigPath + ".kipper-bak"

	// k3sConfigPendingPath records the checksum of a write awaiting confirmation.
	// A matching checksum allows rollback to the preserved backup.
	k3sConfigPendingPath = k3sConfigPath + ".kipper-pending"

	configChangedMarker = "kipper-config-changed"

	// apiServerLockPath serializes repairs that share a backup and pending-write marker.
	// Fenced commands refresh its mtime on completion. Acquisition can reclaim it
	// after more than apiServerLockTTL seconds without a refresh, once the guard is free.
	apiServerLockPath = "/run/kipper-apiserver-config.lock"
	apiServerLockTTL  = 180

	apiServerArgKey = "kube-apiserver-arg"
)

// apiServerArgState describes how the configured arguments match the install defaults.
type apiServerArgState int

const (
	apiServerArgsMissing apiServerArgState = iota
	apiServerArgsCurrent
	apiServerArgsPartial
)

// EnsureAPIServerConfig adds missing API server arguments and restarts k3s when
// needed. It prepares referenced files before suggesting manual edits to a partial
// argument block. The changed result reports a successful config update or an
// attempted restart of an already configured server. notify runs before a restart.
func EnsureAPIServerConfig(client CommandRunner, notify func(string)) (changed bool, err error) {
	if err := refuseANodeThatRunsNoServer(client); err != nil {
		return false, err
	}

	locked, release, err := lockAPIServerConfig(client)
	if err != nil {
		return false, err
	}
	defer release()
	client = locked

	current, err := readK3sConfig(client)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(current) == "" {
		return false, fmt.Errorf("%s is missing or empty, so there is no k3s configuration to bring up to date; this repair expects a cluster kip installed", k3sConfigPath)
	}

	state, err := classifyAPIServerArgs(current)
	if err != nil {
		return false, err
	}

	// Prepare referenced files before advising the operator to add their flags.
	policyChanged, err := writeAuthnStubAndAuditPolicy(client)
	if err != nil {
		return false, err
	}

	switch state {
	case apiServerArgsCurrent:
		return restartIfTheServerIsBehind(client, policyChanged, notify)
	case apiServerArgsPartial:
		return false, fmt.Errorf("%s already sets %s without all of the arguments kip installs, so it will not be rewritten. The files those arguments name are in place, so you can add the missing lines to that block and run 'systemctl restart k3s' on the server:\n%s",
			k3sConfigPath, apiServerArgKey, indentBlock(renderAPIServerArgs(missingAPIServerArgs(current))))
	}

	desired, err := withAPIServerArgs(current)
	if err != nil {
		return false, err
	}

	if notify != nil {
		notify("Adding the required API server arguments and restarting k3s.")
	}

	// The config may contain a token, so use stdin and omit the command and
	// captured output from errors to protect it.
	if _, err := client.RunStdin(writeK3sConfigScript(), strings.NewReader(desired)); err != nil {
		return false, fmt.Errorf("writing %s: %w", k3sConfigPath, withoutOutput(err))
	}

	if err := restartK3s(client); err != nil {
		return false, rollBackK3sConfig(client, err)
	}
	clearPendingWrite(client)
	return true, nil
}

// restartIfTheServerIsBehind restarts k3s for a changed policy or an authentication
// config that metrics indicate is unloaded. If the policy is unchanged and metrics
// are unavailable, it leaves the server running for a later retry.
func restartIfTheServerIsBehind(client CommandRunner, policyChanged bool, notify func(string)) (bool, error) {
	// Restart for a changed policy even if metrics are unavailable; later runs
	// will see matching policy content and cannot detect this pending restart.
	if policyChanged {
		if notify != nil {
			notify("Restarting k3s to load the updated audit policy.")
		}
		if err := restartAfterAPolicyChange(client, pendingWriteIsUnchanged(client)); err != nil {
			return true, err
		}
		clearPendingWrite(client)
		return true, nil
	}

	loaded, err := apiServerLoadedAnAuthnConfig(client)
	if err != nil {
		if notify != nil {
			notify(fmt.Sprintf("The API server could not be asked for its authentication status (%v). Its arguments are on disk. Run this again when the cluster responds.", err))
		}
		return false, nil
	}
	if loaded {
		clearPendingWrite(client)
		return false, nil
	}

	if notify != nil {
		notify("Restarting k3s to load the API server arguments already on disk.")
	}
	if !pendingWriteIsUnchanged(client) {
		return true, restartWithoutTouchingTheConfig(client)
	}
	if err := restartK3s(client); err != nil {
		return true, rollBackK3sConfig(client, err)
	}
	clearPendingWrite(client)
	return true, nil
}

const (
	lockAcquiredMarker = "kipper-lock-acquired"
	lockHeldMarker     = "kipper-lock-held"
	lockLostMarker     = "kipper-lock-lost"
)

// lockAPIServerConfig returns a runner guarded by the host lock and a release function.
// An exclusive flock on the guard file serializes acquisition, release, and
// each command. Commands check the owner token so a previous owner stops after
// a takeover.
func lockAPIServerConfig(client CommandRunner) (CommandRunner, func(), error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return nil, nil, fmt.Errorf("generating an API server lock token: %w", err)
	}
	token := hex.EncodeToString(buf)
	out, err := client.Run(fmt.Sprintf(`exec 9>%[1]s.guard && flock 9 || exit 1
if [ -d %[1]s ]; then age=$(( $(date +%%s) - $(stat -c %%Y %[1]s) )); if [ "$age" -gt %[2]d ]; then rm -rf %[1]s; else echo %[4]s "$age"; exit 5; fi; fi
mkdir %[1]s 2>/dev/null || { echo %[4]s; exit 5; }
echo %[3]s > %[1]s/owner || { rm -rf %[1]s; exit 1; }
echo %[5]s`,
		apiServerLockPath, apiServerLockTTL, token, lockHeldMarker, lockAcquiredMarker))
	switch {
	case strings.Contains(out, lockHeldMarker):
		return nil, nil, fmt.Errorf("another kip run holds the API server configuration lock (%s)%s. Wait for it to finish, then retry. If it was interrupted, retry after more than %d seconds without a lock refresh to reclaim the lock",
			apiServerLockPath, lockAgeClause(out), apiServerLockTTL)
	case err != nil:
		return nil, nil, fmt.Errorf("taking the API server configuration lock: %w", err)
	case !strings.Contains(out, lockAcquiredMarker):
		return nil, nil, fmt.Errorf("taking the API server configuration lock: unexpected response %q", strings.TrimSpace(out))
	}
	release := func() {
		_, _ = client.Run(fmt.Sprintf(`exec 9>%[1]s.guard && flock 9 || exit 1
[ "$(cat %[1]s/owner 2>/dev/null)" = %[2]s ] && rm -rf %[1]s`, apiServerLockPath, token))
	}
	return fencedRunner{base: client, token: token}, release, nil
}

// lockAgeClause formats the lock age from a lock-held response.
// It returns an empty string if the response has no valid age.
func lockAgeClause(out string) string {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == lockHeldMarker {
			if age, err := strconv.Atoi(fields[1]); err == nil {
				return fmt.Sprintf(", last refreshed %d seconds ago", age)
			}
		}
	}
	return ""
}

// fencedRunner runs each command only while this run still owns the host lock.
type fencedRunner struct {
	base  CommandRunner
	token string
}

// fence checks ownership under the guard and refreshes the lock after the command.
// A subshell keeps command exits and traps separate from the refresh while
// preserving the command's exit status.
func (f fencedRunner) fence(command string) string {
	return fmt.Sprintf(`exec 9>%[1]s.guard && flock 9 || exit 1
[ "$(cat %[1]s/owner 2>/dev/null)" = %[2]s ] || { echo %[3]s; exit 4; }
(
%[4]s
); rc=$?; touch -c %[1]s; exit $rc`, apiServerLockPath, f.token, lockLostMarker, command)
}

func (f fencedRunner) Run(command string) (string, error) {
	return f.base.Run(f.fence(command))
}

func (f fencedRunner) RunStdin(command string, stdin io.Reader) (string, error) {
	return f.base.RunStdin(f.fence(command), stdin)
}

// pendingWriteIsUnchanged reports whether the config matches the pending checksum.
// Rollback requires a successful match, including when retrying an interrupted run.
func pendingWriteIsUnchanged(client CommandRunner) bool {
	out, err := client.Run(fmt.Sprintf(
		`[ -f %s ] || { echo none; exit 0; }; [ "$(cat %s)" = "$(sha256sum "$(readlink -f %s)" | cut -d' ' -f1)" ] && echo ours || echo changed`,
		k3sConfigPendingPath, k3sConfigPendingPath, k3sConfigPath))
	return err == nil && strings.TrimSpace(out) == "ours"
}

// clearPendingWrite removes the marker on a best-effort basis after confirmation
// or rollback. A leftover marker can authorize rollback while its checksum matches.
func clearPendingWrite(client CommandRunner) {
	_, _ = client.Run("rm -f " + k3sConfigPendingPath)
}

// restartAfterAPolicyChange restarts k3s to load the new policy and attempts to
// restore the previous policy on failure. With ownConfigWrite, it also attempts
// to roll back the pending config write before restarting again.
func restartAfterAPolicyChange(client CommandRunner, ownConfigWrite bool) error {
	err := restartK3s(client)
	if err == nil {
		return nil
	}
	if _, rerr := client.Run(fmt.Sprintf(`cp -p %s "$(readlink -f %s)"`, auditPolicyBackupPath, auditPolicyPath)); rerr != nil {
		return fmt.Errorf("%w; restoring the previous audit policy failed (%v). Inspect %s and the k3s service on the server", err, rerr, auditPolicyPath)
	}
	if ownConfigWrite {
		return rollBackK3sConfig(client, fmt.Errorf("%w; the previous audit policy was restored", err))
	}
	if rerr := restartK3s(client); rerr != nil {
		return fmt.Errorf("%w; the previous audit policy was restored; restarting k3s failed (%v). Inspect the k3s service on the server", err, rerr)
	}
	return fmt.Errorf("%w; the previous audit policy was restored and the API server is ready again", err)
}

// restartWithoutTouchingTheConfig restarts k3s while preserving the config.
// Without a matching pending checksum, restoring a backup could overwrite
// operator changes made since the backup was saved.
func restartWithoutTouchingTheConfig(client CommandRunner) error {
	if err := restartK3s(client); err != nil {
		return fmt.Errorf("%w; this run changed no k3s configuration. Inspect %s and the k3s service on the server. An earlier repair may have saved a backup at %s",
			err, k3sConfigPath, k3sConfigBackupPath)
	}
	return nil
}

// classifyAPIServerArgs parses YAML so comments and unrelated values cannot
// be mistaken for configured arguments.
func classifyAPIServerArgs(config string) (apiServerArgState, error) {
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(config), &doc); err != nil {
		return 0, fmt.Errorf("parsing %s: %w. kip will not rewrite a config it cannot read", k3sConfigPath, err)
	}
	raw, ok := doc[apiServerArgKey]
	if !ok {
		return apiServerArgsMissing, nil
	}
	have := map[string]bool{}
	for _, arg := range asStringSlice(raw) {
		have[arg] = true
	}
	for _, want := range apiServerArgList() {
		if !have[want] {
			return apiServerArgsPartial, nil
		}
	}
	return apiServerArgsCurrent, nil
}

// asStringSlice accepts a single argument or a list of strings.
func asStringSlice(raw interface{}) []string {
	switch v := raw.(type) {
	case string:
		return []string{v}
	case []interface{}:
		var out []string
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// apiServerArgList uses the install defaults as the definition of current arguments.
func apiServerArgList() []string {
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(k3sAPIServerArgs), &doc); err != nil {
		return nil
	}
	return asStringSlice(doc[apiServerArgKey])
}

// missingAPIServerArgs lists only missing arguments so suggested edits avoid duplicates.
func missingAPIServerArgs(config string) []string {
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(config), &doc); err != nil {
		return apiServerArgList()
	}
	have := map[string]bool{}
	for _, arg := range asStringSlice(doc[apiServerArgKey]) {
		have[arg] = true
	}
	var missing []string
	for _, want := range apiServerArgList() {
		if !have[want] {
			missing = append(missing, want)
		}
	}
	return missing
}

func renderAPIServerArgs(args []string) string {
	var b strings.Builder
	for _, arg := range args {
		fmt.Fprintf(&b, "  - %q\n", arg)
	}
	return b.String()
}

// withAPIServerArgs appends and validates the argument block. A separating newline
// keeps the block valid even when the existing file has no trailing newline.
func withAPIServerArgs(current string) (string, error) {
	desired := strings.TrimRight(current, "\n") + "\n" + k3sAPIServerArgs
	state, err := classifyAPIServerArgs(desired)
	if err != nil {
		return "", fmt.Errorf("the repaired %s would not parse, so it was not written: %w", k3sConfigPath, err)
	}
	if state != apiServerArgsCurrent {
		return "", fmt.Errorf("the repaired %s does not carry the arguments it should, so it was not written", k3sConfigPath)
	}
	return desired, nil
}

// refuseANodeThatRunsNoServer requires a k3s server unit before writing server flags.
func refuseANodeThatRunsNoServer(client CommandRunner) error {
	out, err := client.Run("systemctl list-unit-files k3s.service --no-legend 2>/dev/null | wc -l")
	if err != nil {
		return fmt.Errorf("checking for the k3s server on this host: %w", err)
	}
	if count, cerr := strconv.Atoi(strings.TrimSpace(out)); cerr != nil || count < 1 {
		return fmt.Errorf("this host does not appear to run a k3s server (no k3s.service), so the API server arguments do not belong here. Point the cluster's host at the server node")
	}
	return nil
}

// apiServerLoadedAnAuthnConfig uses the authentication controller metric as a
// proxy for whether the running API server has loaded an authentication config.
func apiServerLoadedAnAuthnConfig(client CommandRunner) (bool, error) {
	out, err := client.Run(`raw=$(kubectl get --raw /metrics) || exit 97
printf '%s\n' "$raw" | grep -c 'apiserver_authentication_config_controller_last_config_info' || true`)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errMetricsUnreadable, err)
	}
	return strings.TrimSpace(out) != "0", nil
}

// readK3sConfig returns empty content for an absent file and reports read failures
// without command output, which may contain a token.
func readK3sConfig(client CommandRunner) (string, error) {
	out, err := client.Run(fmt.Sprintf(
		"if [ ! -e %s ] && [ ! -L %s ]; then exit 0; fi; cat %s", k3sConfigPath, k3sConfigPath, k3sConfigPath))
	if err != nil {
		return "", fmt.Errorf("reading %s failed", k3sConfigPath)
	}
	return out, nil
}

// writeK3sConfigScript stages and renames the config to avoid partial writes.
// It preserves symlinks, mode, and ownership so managed paths and access
// restrictions survive the replacement. The new content arrives on stdin.
func writeK3sConfigScript() string {
	// Preserve the original backup while the live config matches a pending write.
	// Otherwise, back up the live config before replacing it.
	return strings.NewReplacer(
		"CONFIG", k3sConfigPath,
		"BACKUP", k3sConfigBackupPath,
		"PENDING", k3sConfigPendingPath,
	).Replace(`set -e
target=$(readlink -f CONFIG)
mode=$(stat -c %a "$target")
owner=$(stat -c %u:%g "$target")
if [ ! -f PENDING ] || [ "$(cat PENDING)" != "$(sha256sum "$target" | cut -d' ' -f1)" ]; then
  cp -p "$target" BACKUP
fi
staged=$(mktemp "$(dirname "$target")"/.kipper-XXXXXX)
trap 'rm -f "$staged"' EXIT
cat > "$staged"
chmod "$mode" "$staged"
chown "$owner" "$staged"
sha256sum "$staged" | cut -d' ' -f1 > PENDING
mv "$staged" "$target"
trap - EXIT`)
}

// restartK3s restarts k3s and checks API readiness. Node readiness is unrelated
// to this repair and could trigger rollback because of an unhealthy worker.
func restartK3s(client CommandRunner) error {
	if _, err := client.Run("systemctl restart k3s"); err != nil {
		return fmt.Errorf("restarting k3s: %w", err)
	}
	if _, err := client.Run(
		`for i in $(seq 1 60); do kubectl get --raw /readyz >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1`); err != nil {
		return fmt.Errorf("the API server did not answer within two minutes of the restart: %w", err)
	}
	return nil
}

// rollBackK3sConfig attempts to restore the backup only while the config matches
// the pending checksum, then restarts k3s. Its error includes the recovery outcome.
func rollBackK3sConfig(client CommandRunner, cause error) error {
	if out, err := client.Run(fmt.Sprintf("[ -f %s ] && echo yes || echo no", k3sConfigBackupPath)); err == nil && strings.TrimSpace(out) == "no" {
		return fmt.Errorf("%w; no configuration backup is available. Inspect %s and the k3s service on the server", cause, k3sConfigPath)
	}
	// Recheck the checksum immediately before copying: the config may have been
	// edited during the failed restart. Both operations share the command lock.
	out, err := client.Run(fmt.Sprintf(
		`t=$(readlink -f %s); if [ ! -f %s ] || [ "$(cat %s)" != "$(sha256sum "$t" | cut -d' ' -f1)" ]; then echo %s; exit 3; fi; cp -p %s "$t"`,
		k3sConfigPath, k3sConfigPendingPath, k3sConfigPendingPath, configChangedMarker, k3sConfigBackupPath))
	if err != nil && strings.Contains(out, configChangedMarker) {
		return fmt.Errorf("%w; rollback skipped: %s changed since kip wrote it, or its pending-write marker is missing. Inspect the server; the backup is at %s",
			cause, k3sConfigPath, k3sConfigBackupPath)
	}
	if err != nil {
		return fmt.Errorf("%w; restoring the configuration failed (%v); the server state is unknown. Review the backup at %s before restoring it with 'cp -p %s %s' and running 'systemctl restart k3s' on the server",
			cause, err, k3sConfigBackupPath, k3sConfigBackupPath, k3sConfigPath)
	}
	clearPendingWrite(client)
	if err := restartK3s(client); err != nil {
		return fmt.Errorf("%w; the previous configuration was restored; restarting k3s failed (%v). Inspect the k3s service on the server", cause, err)
	}
	return fmt.Errorf("%w; the previous configuration was restored and the API server is ready again", cause)
}

func indentBlock(block string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}
