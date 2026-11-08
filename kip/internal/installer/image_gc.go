package installer

import (
	"fmt"
	"strings"
)

const k3sConfigDropInDir = "/etc/rancher/k3s/config.yaml.d"

const (
	imageGCDropIn      = "kipper-image-gc.yaml"
	agentKubeletDropIn = "kipper-kubelet.yaml"
)

// Longhorn's default storage-minimal-available-percentage of 25 blocks new
// replicas at 75% disk use. Lower image GC thresholds let the kubelet reclaim
// unused images before disk space prevents replica placement.
const (
	imageGCHighThreshold = 70
	imageGCLowThreshold  = 60
)

// imageGCConfig appends image GC thresholds to the kubelet arguments from
// earlier k3s configuration files, preserving those arguments.
var imageGCConfig = fmt.Sprintf(`# Managed by Kipper. The kubelet removes unused images from %[1]d%% disk use,
# before Longhorn stops placing replicas on a disk that is 75%% full.
kubelet-arg+:
  - "image-gc-high-threshold=%[1]d"
  - "image-gc-low-threshold=%[2]d"
`, imageGCHighThreshold, imageGCLowThreshold)

// agentKubeletConfig sorts after kipper-image-gc.yaml, so it must also use
// kubelet-arg+ to preserve the image GC thresholds.
const agentKubeletConfig = "kubelet-arg+:\n  - \"protect-kernel-defaults=true\"\n"

// imageGCCommand writes the image GC drop-in in dir and rewrites an existing
// worker kubelet drop-in to append its arguments. Unchanged files retain their
// modification times so the restart check can compare them with the k3s start
// time. dir must be safe to use unquoted in a shell.
func imageGCCommand(dir string) string {
	return fmt.Sprintf(`set -e
mkdir -p %[1]s
put() {
  [ "$(cat "$1" 2>/dev/null)" != "$(printf '%%s' "$2")" ] || return 0
  staged=$(mktemp %[1]s/.kipper-XXXXXX)
  printf '%%s' "$2" > "$staged"
  chmod 600 "$staged"
  mv -f "$staged" "$1"
}
put %[1]s/%[2]s '%[3]s'
if [ -e %[1]s/%[4]s ]; then put %[1]s/%[4]s '%[5]s'; fi`,
		dir, imageGCDropIn, imageGCConfig, agentKubeletDropIn, agentKubeletConfig)
}

// imageGCStaleUnitCommand checks the first active unit, k3s before k3s-agent.
// It prints the unit if its start time is at or before either kubelet drop-in's
// modification time, or is unreadable. Otherwise, it prints nothing.
// The comparison uses nanoseconds, with microsecond start times on systemd 248
// and later. Older systemd versions report whole seconds, so a start in the
// same second as a write conservatively counts as stale.
// dir must be safe to use unquoted in a shell.
func imageGCStaleUnitCommand(dir string) string {
	return fmt.Sprintf(`for unit in k3s k3s-agent; do
  systemctl is-active --quiet "$unit" || continue
  ts=$(systemctl show --timestamp=us+utc -p ExecMainStartTimestamp --value "$unit" 2>/dev/null) ||
    ts=$(systemctl show -p ExecMainStartTimestamp --value "$unit")
  [ -n "$ts" ] && started=$(date -d "$ts" +%%s%%N 2>/dev/null) || started=0
  for f in %[1]s/%[2]s %[1]s/%[3]s; do
    if [ -e "$f" ] && [ "$(date -r "$f" +%%s%%N)" -ge "$started" ]; then echo "$unit"; exit 0; fi
  done
  exit 0
done`, dir, imageGCDropIn, agentKubeletDropIn)
}

// configureImageGC writes the image GC drop-in for the next k3s start.
func configureImageGC(client CommandRunner) error {
	if _, err := client.Run(imageGCCommand(k3sConfigDropInDir)); err != nil {
		return fmt.Errorf("writing the kubelet image cleanup settings: %w", err)
	}
	return nil
}

// ApplyImageGC writes the image GC drop-in and updates an existing worker
// kubelet drop-in to preserve the thresholds. It restarts the active k3s or
// k3s-agent unit if its start time is at or before either drop-in's modification
// time, or is unreadable, because the kubelet loads these flags at startup.
// notify must be non-nil and runs before a restart.
//
// It returns "k3s" or "k3s-agent" after a successful restart and, on servers,
// a successful API readiness check. It returns "" if no restart is needed or an
// error occurs. Errors leave any settings already written in place. Running
// pods keep running while the server's API pauses during its restart. A host
// with both units inactive loads the settings at its next k3s start. After a
// failed restart, a later call retries if the active unit's start time still
// requires it.
func ApplyImageGC(client CommandRunner, notify func(string)) (string, error) {
	if err := configureImageGC(client); err != nil {
		return "", err
	}
	out, err := client.Run(imageGCStaleUnitCommand(k3sConfigDropInDir))
	if err != nil {
		return "", fmt.Errorf("checking whether k3s loaded the kubelet image cleanup settings: %w", err)
	}
	unit := strings.TrimSpace(out)
	var restart func(CommandRunner) error
	switch unit {
	case "":
		return "", nil
	case RoleServer.k3sUnit():
		restart = restartK3s
	case RoleWorker.k3sUnit():
		restart = restartK3sAgent
	default:
		return "", fmt.Errorf("checking whether k3s loaded the kubelet image cleanup settings: unexpected output %q", unit)
	}
	notify(fmt.Sprintf("Restarting %s so the kubelet loads the image cleanup settings. Running pods keep running.", unit))
	// systemd before 248 reports start times in whole seconds, so a restart in
	// the second of the write would still read as stale on the next check.
	if _, err := client.Run("sleep 1"); err != nil {
		return "", fmt.Errorf("waiting before restarting %s: %w", unit, err)
	}
	if err := restart(client); err != nil {
		return "", fmt.Errorf("%w. The kubelet image cleanup settings are written. Check 'journalctl -u %s' on the node, then run the command again", err, unit)
	}
	return unit, nil
}

// restartK3sAgent waits up to 300 seconds for systemctl to restart k3s-agent.
// The agent reports started after connecting to the API server. Its systemd
// unit has no start timeout, so the caller's wait is bounded here.
func restartK3sAgent(client CommandRunner) error {
	if _, err := client.Run("timeout 300 systemctl restart k3s-agent"); err != nil {
		return fmt.Errorf("restarting k3s-agent: %w", err)
	}
	return nil
}
