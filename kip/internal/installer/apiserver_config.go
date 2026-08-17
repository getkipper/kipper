package installer

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	// k3sConfigPath is the config k3s reads its server flags from. kip writes
	// it at install; this is the only other thing that touches it.
	k3sConfigPath = "/etc/rancher/k3s/config.yaml"

	// k3sConfigBackupPath holds the file as it was before the arguments were
	// added, both for the rollback below and for an operator who wants to see
	// what changed.
	k3sConfigBackupPath = k3sConfigPath + ".kipper-bak"

	// apiServerArgKey is the mapping key the argument block defines.
	apiServerArgKey = "kube-apiserver-arg"
)

// apiServerArgState says what a cluster's config already has to say about the
// API server arguments.
type apiServerArgState int

const (
	// apiServerArgsMissing: no kube-apiserver-arg key at all, which is every
	// cluster installed before kip wrote one.
	apiServerArgsMissing apiServerArgState = iota
	// apiServerArgsCurrent: every argument this kip installs is already there.
	apiServerArgsCurrent
	// apiServerArgsPartial: the key exists and does not carry all of them,
	// whether because someone else wrote it or because a previous attempt got
	// halfway. Both are refused rather than merged.
	apiServerArgsPartial
)

// EnsureAPIServerConfig brings the API server arguments on an existing cluster
// up to what this version of kip installs, and reports whether it changed
// anything. It is the upgrade path for operator login: a cluster installed
// before those arguments existed has no authenticator at all, so its API server
// rejects every Dex token while accepting the admin certificate, and nothing
// inside the cluster can fix that because the flags live on the host.
//
// The order is the safety argument. The files the flags name are written before
// anything else, including before a refusal, because a refusal tells the
// operator to add those flags by hand and an API server pointed at a missing
// file does not start. The authentication config lands as the
// zero-authenticator stub, which leaves the cluster exactly as authenticated as
// it was: the issuer is configured afterwards, by EnsureOperatorAuth, which
// verifies the API server loaded it.
//
// notify, when given, is called before anything restarts.
func EnsureAPIServerConfig(client commandRunner, notify func(string)) (changed bool, err error) {
	if err := refuseANodeThatRunsNoServer(client); err != nil {
		return false, err
	}

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

	// Before the refusal below, not after it: an operator who follows that
	// refusal adds flags naming these files, and flags naming files that are
	// not there stop the API server from starting.
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
		notify("The API server is missing the arguments this kip installs. Adding them and restarting k3s once; workloads keep running through it.")
	}

	if _, err := client.Run(writeK3sConfigScript(desired)); err != nil {
		return false, fmt.Errorf("writing %s: %w", k3sConfigPath, err)
	}

	if err := restartK3s(client); err != nil {
		return false, rollBackK3sConfig(client, err)
	}
	return true, nil
}

// restartIfTheServerIsBehind handles a config file that already carries the
// arguments. The file is not the process: a run interrupted between the rename
// and the restart leaves exactly this state, and the audit policy is read once
// at startup, so replacing it changes nothing until k3s restarts.
//
// An API server that cannot be asked is left alone rather than restarted or
// failed. Not knowing is not the same as knowing it is behind, and the next run
// asks again; restarting a healthy control plane on a guess is the one outcome
// worth avoiding here.
func restartIfTheServerIsBehind(client commandRunner, policyChanged bool, notify func(string)) (bool, error) {
	loaded, err := apiServerLoadedAnAuthnConfig(client)
	if err != nil {
		if notify != nil {
			notify(fmt.Sprintf("The API server could not be asked what it is running (%v). Its arguments are already on disk, so nothing was changed; run this again when the cluster answers.", err))
		}
		return false, nil
	}
	if loaded && !policyChanged {
		return false, nil
	}

	if notify != nil {
		switch {
		case !loaded:
			notify("The API server arguments are on disk but the running API server has not loaded them, which is where an interrupted run leaves a server. Restarting k3s once.")
		default:
			notify("The audit policy changed, and the API server reads it only at startup. Restarting k3s once; workloads keep running through it.")
		}
	}
	if rerr := restartK3s(client); rerr != nil {
		return false, rollBackK3sConfig(client, rerr)
	}
	return true, nil
}

// classifyAPIServerArgs decides from the parsed document rather than from the
// text. A comment naming an argument, or a value that happens to contain one,
// reads as configuration to a substring match and as nothing at all to YAML,
// and this decision either skips a control-plane repair or refuses one, so it
// has to be the answer k3s itself would give.
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

// asStringSlice reads k3s's own tolerance of a single scalar where a list is
// expected, so `kube-apiserver-arg: "x"` is not mistaken for an empty block.
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

// apiServerArgList is the argument values, read from the same constant the
// install path writes, so the check and the block can never disagree about
// what "current" means.
func apiServerArgList() []string {
	var doc map[string]interface{}
	if err := yaml.Unmarshal([]byte(k3sAPIServerArgs), &doc); err != nil {
		return nil
	}
	return asStringSlice(doc[apiServerArgKey])
}

// missingAPIServerArgs returns the arguments a config does not already carry.
// A refusal lists these rather than all six: an operator pasting an argument
// their block already sets ends up with the flag twice, and what a duplicate
// flag does is a question nobody should have to answer while repairing a
// control plane.
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

// renderAPIServerArgs renders arguments as the YAML list entries an operator
// would paste into a block of their own.
func renderAPIServerArgs(args []string) string {
	var b strings.Builder
	for _, arg := range args {
		fmt.Fprintf(&b, "  - %q\n", arg)
	}
	return b.String()
}

// withAPIServerArgs appends the block to a config that has no
// kube-apiserver-arg key, and proves the result parses before it can reach the
// server. Exactly one newline joins them: YAML does not require a file to end
// in one, and a config ending at EOF would otherwise be spliced into its own
// last line and restart k3s on a file that cannot be read.
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

// refuseANodeThatRunsNoServer stops the repair on a node that runs the k3s
// agent rather than the server. These are server flags, and `systemctl restart
// k3s` finds no unit there, so without this the repair writes control-plane
// configuration onto a worker and then fails trying to apply it.
func refuseANodeThatRunsNoServer(client commandRunner) error {
	out, err := client.Run("systemctl list-unit-files k3s.service --no-legend 2>/dev/null | wc -l")
	if err != nil {
		return fmt.Errorf("checking for the k3s server on this host: %w", err)
	}
	// Anything but a positive count is refused, including an empty answer: not
	// knowing whether this host runs the server is not a reason to write
	// server flags onto it.
	if count, cerr := strconv.Atoi(strings.TrimSpace(out)); cerr != nil || count < 1 {
		return fmt.Errorf("this host does not appear to run a k3s server (no k3s.service), so the API server arguments do not belong here. Point the cluster's host at the server node")
	}
	return nil
}

// apiServerLoadedAnAuthnConfig reports whether the running API server has an
// authentication config loaded, which is the flag taking effect rather than the
// file existing. The metric appears only when the server was started with
// --authentication-config.
func apiServerLoadedAnAuthnConfig(client commandRunner) (bool, error) {
	out, err := client.Run(`raw=$(kubectl get --raw /metrics) || exit 97
printf '%s\n' "$raw" | grep -c 'apiserver_authentication_config_controller_last_config_info' || true`)
	if err != nil {
		return false, fmt.Errorf("%w: %v", errMetricsUnreadable, err)
	}
	return strings.TrimSpace(out) != "0", nil
}

// readK3sConfig returns the config file, distinguishing absent from unreadable
// the way readHopCA does: a permission or I/O error must not read as "there is
// no config", because the caller would then be told to write a fresh one and
// silently drop the settings that disable traefik and pin the cluster's
// resolvers.
func readK3sConfig(client commandRunner) (string, error) {
	out, err := client.Run(fmt.Sprintf(
		"if [ ! -e %s ] && [ ! -L %s ]; then exit 0; fi; cat %s", k3sConfigPath, k3sConfigPath, k3sConfigPath))
	if err != nil {
		return "", fmt.Errorf("reading %s: %w", k3sConfigPath, err)
	}
	return out, nil
}

// writeK3sConfigScript renders the whole file rather than appending to it, so a
// failure leaves either the old config or the new one. The content travels
// base64-encoded and single-quoted, so no byte in it is ever interpreted by the
// shell that carries it.
//
// Mode and ownership are carried across from the file being replaced. That file
// can hold a datastore endpoint or a node token, so an operator who tightened
// it must not find it world-readable because an upgrade rewrote it, and `mv`
// replaces the inode rather than the contents.
//
// The rename lands on what the path resolves to, so a config an operator keeps
// as a symlink into their own configuration management still points there
// afterwards. Replacing the link with a plain file would leave their tooling
// writing somewhere nothing reads.
func writeK3sConfigScript(content string) string {
	return fmt.Sprintf(`set -e
target=$(readlink -f %s)
mode=$(stat -c %%a "$target")
owner=$(stat -c %%u:%%g "$target")
cp -p "$target" %s
staged=$(mktemp "$(dirname "$target")"/.kipper-XXXXXX)
trap 'rm -f "$staged"' EXIT
printf %%s %s | base64 -d > "$staged"
chmod "$mode" "$staged"
chown "$owner" "$staged"
mv "$staged" "$target"
trap - EXIT`,
		k3sConfigPath,
		k3sConfigBackupPath,
		shellQuote(base64.StdEncoding.EncodeToString([]byte(content))))
}

// restartK3s restarts the server and waits for its API server to answer.
//
// Readiness is asked of the API server rather than of the nodes. This restarts
// one server's control plane, and the caller answers a failure by rolling the
// configuration back, so a `kubectl wait node --all` would let any unrelated
// agent that happened to be NotReady cost a second restart and a repair that
// never lands.
func restartK3s(client commandRunner) error {
	if _, err := client.Run("systemctl restart k3s"); err != nil {
		return fmt.Errorf("restarting k3s: %w", err)
	}
	if _, err := client.Run(
		`for i in $(seq 1 60); do kubectl get --raw /readyz >/dev/null 2>&1 && exit 0; sleep 2; done; exit 1`); err != nil {
		return fmt.Errorf("the API server did not answer within two minutes of the restart: %w", err)
	}
	return nil
}

// rollBackK3sConfig puts the previous config back and restarts k3s on it, so a
// cluster whose API server did not accept the new arguments is left running the
// configuration it had. The returned error carries what the rollback managed,
// because an operator reading it needs to know whether their control plane is
// up, and the one case where that is unknown says so rather than guessing.
func rollBackK3sConfig(client commandRunner, cause error) error {
	// A restart that follows someone else's interrupted run has nothing of its
	// own to restore, and the config on disk is the one the operator wants.
	// Saying so beats reporting a rollback that never happened.
	if out, err := client.Run(fmt.Sprintf("[ -f %s ] && echo yes || echo no", k3sConfigBackupPath)); err == nil && strings.TrimSpace(out) == "no" {
		return fmt.Errorf("%w; nothing was changed on disk by this run and there is no earlier configuration to restore, so %s still holds the arguments and this server needs looking at directly", cause, k3sConfigPath)
	}
	if _, err := client.Run(fmt.Sprintf(`cp -p %s "$(readlink -f %s)"`, k3sConfigBackupPath, k3sConfigPath)); err != nil {
		return fmt.Errorf("%w; the configuration could not be restored either (%v), so the state of this server is unknown. Its previous configuration is at %s: put it back with 'cp -p %s %s' and run 'systemctl restart k3s' on the server",
			cause, err, k3sConfigBackupPath, k3sConfigBackupPath, k3sConfigPath)
	}
	if err := restartK3s(client); err != nil {
		return fmt.Errorf("%w; the previous configuration was restored but k3s did not come back on it either (%v), so this server needs looking at directly", cause, err)
	}
	return fmt.Errorf("%w; the previous configuration was restored and k3s is running on it again, so operator login is still unconfigured", cause)
}

// indentBlock prefixes each line so a refusal can print the lines to add
// without them reading as part of the sentence around them.
func indentBlock(block string) string {
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
		b.WriteString("  " + line + "\n")
	}
	return b.String()
}
