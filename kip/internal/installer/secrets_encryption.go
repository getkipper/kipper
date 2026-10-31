package installer

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

const (
	secretsEncryptionKey = "secrets-encryption"

	// encryptionConfigPath holds the key k3s generates. It lives on the
	// server's disk next to the datastore it protects.
	encryptionConfigPath = "/var/lib/rancher/k3s/server/cred/encryption-config.json"

	// encryptionStatePath holds the last rotation stage k3s completed and the
	// sha256 of the encryption config it completed it with.
	encryptionStatePath = "/var/lib/rancher/k3s/server/cred/encryption-state.json"

	encryptionFinishedStage = "reencrypt_finished"

	// UnencryptedSecretsAdvice names the command that encrypts an older
	// cluster's Secrets.
	UnencryptedSecretsAdvice = "k3s stores this cluster's Secrets unencrypted. Run 'kip cluster encrypt-secrets' to encrypt them" //nolint:gosec // This is advice text, not a credential.
)

// encryptionStatusWait bounds how long kip waits for k3s to answer a status
// request. settle covers transient status failures after node updates on a
// running server. ready covers restarts and re-encryption.
var encryptionStatusWait = struct{ settle, ready, interval time.Duration }{
	settle:   30 * time.Second,
	ready:    10 * time.Minute,
	interval: 3 * time.Second,
}

// encryptionStatus is what `k3s secrets-encrypt status` reports.
type encryptionStatus struct {
	configured  bool
	enabled     bool
	stage       string
	hashesMatch bool
}

// reencrypting reports whether k3s has a re-encryption requested or running.
func (s encryptionStatus) reencrypting() bool {
	return s.stage == "reencrypt_request" || s.stage == "reencrypt_active"
}

// done reports whether k3s reports completed re-encryption with matching
// server hashes and encryption enabled.
func (s encryptionStatus) done() bool {
	return s.enabled && s.stage == encryptionFinishedStage && s.hashesMatch
}

func parseEncryptionStatus(out string) (encryptionStatus, error) {
	var s encryptionStatus
	found := false
	for _, line := range strings.Split(out, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "Encryption Status":
			found = true
			s.configured = !strings.Contains(value, "no configuration file found")
			s.enabled = value == "Enabled"
		case "Current Rotation Stage":
			s.stage = value
		case "Server Encryption Hashes":
			s.hashesMatch = value == "All hashes match"
		}
	}
	if !found {
		return s, fmt.Errorf("k3s did not report an encryption status: %s", strings.TrimSpace(out))
	}
	return s, nil
}

func readEncryptionStatus(client CommandRunner) (encryptionStatus, error) {
	out, err := client.Run("k3s secrets-encrypt status")
	if err != nil {
		return encryptionStatus{}, fmt.Errorf("asking k3s for its encryption status: %w", err)
	}
	return parseEncryptionStatus(out)
}

// waitForEncryptionStatus polls for up to timeout until k3s reports a status
// that satisfies ready. Right after a restart k3s answers with an error until
// the server node carries its encryption annotations.
func waitForEncryptionStatus(client CommandRunner, timeout time.Duration, ready func(encryptionStatus) bool) (encryptionStatus, error) {
	var last encryptionStatus
	err := pollUntil(timeout, encryptionStatusWait.interval, func() (bool, error) {
		s, err := readEncryptionStatus(client)
		if err != nil {
			return false, err
		}
		last = s
		return ready(s), nil
	})
	return last, err
}

// SecretsEncrypted reports whether k3s has encryption enabled for new Secret
// writes. Existing Secrets may still need re-encryption. It returns an error
// if the server status cannot be read or parsed.
func SecretsEncrypted(client CommandRunner) (bool, error) {
	s, err := readEncryptionStatus(client)
	if err != nil {
		return false, err
	}
	return s.enabled, nil
}

// EncryptSecrets turns on k3s secrets encryption on the server and rewrites
// every stored Secret with the new key, following the k3s procedure for an
// existing cluster. It usually restarts k3s twice, briefly interrupting API
// access. already is true when encryption was complete before this run.
// Interrupted runs resume from the stage k3s reports. If non-nil, notify
// receives progress messages before encryption steps and while waiting.
//
// EncryptSecrets requires a supported k3s server and a nonempty kip config.
// It holds the config lock and rejects pending network changes, explicit
// encryption opt-outs, and unfinished manual key rotations. Errors may leave
// partial changes in place. The encryption flag is retained to keep encrypted
// Secrets readable, so callers can retry after resolving the reported error.
func EncryptSecrets(client CommandRunner, notify func(string)) (already bool, err error) {
	say := func(m string) {
		if notify != nil {
			notify(m)
		}
	}
	if err := refuseANodeThatRunsNoServer(client); err != nil {
		return false, err
	}
	if err := requireEncryptionSupport(client); err != nil {
		return false, err
	}

	locked, release, err := lockAPIServerConfig(client)
	if err != nil {
		return false, err
	}
	defer release()
	client = locked

	// Recovering an interrupted network change restores the config saved
	// before it, which would take the flag out again. Network changes take the
	// same lock, so none can start after this check.
	if pending, err := NetworkChangePending(client); err != nil {
		return false, err
	} else if pending {
		return false, fmt.Errorf("a pod network change on this server is unfinished. Run 'kip upgrade' to finish it, then run this again")
	}

	current, err := readK3sConfig(client)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(current) == "" {
		return false, fmt.Errorf("%s is missing or empty. This command expects a cluster installed by kip", k3sConfigPath)
	}
	flagged, err := secretsEncryptionSetting(current)
	if err != nil {
		return false, err
	}

	// Once the flag is in the config, Secrets k3s writes may be encrypted, so
	// no rollback may take the flag out again.
	loaded := false
	if flagged {
		if err := forgetPendingWrite(client); err != nil {
			return false, err
		}
		s, err := waitForEncryptionStatus(client, encryptionStatusWait.settle, func(encryptionStatus) bool { return true })
		if err == nil && s.done() {
			recorded, err := encryptionStateCurrent(client)
			if err != nil {
				return false, err
			}
			if recorded {
				return true, nil
			}
		}
		// k3s answers without a key too, so only a reported key shows that
		// the running server loaded the flag.
		loaded = err == nil && s.configured
	}

	keyExists, err := encryptionKeyExists(client)
	if err != nil {
		return false, err
	}
	if !keyExists {
		say("Creating the encryption key.")
		if _, err := client.Run("k3s secrets-encrypt enable"); err != nil {
			return false, fmt.Errorf("creating the encryption key with 'k3s secrets-encrypt enable': %w", err)
		}
		loaded = false
	}

	// Restarting with the flag loads the key and makes its status available.
	if !flagged {
		say(fmt.Sprintf("Adding %s: true to %s and restarting k3s.", secretsEncryptionKey, k3sConfigPath))
		desired := strings.TrimRight(current, "\n") + "\n" + secretsEncryptionKey + ": true\n"
		if _, err := client.RunStdin(writeK3sConfigWithoutRollbackScript(), strings.NewReader(desired)); err != nil {
			return false, fmt.Errorf("writing %s: %w", k3sConfigPath, withoutOutput(err))
		}
	} else if !loaded {
		say("Restarting k3s to load the encryption key.")
	}
	if !loaded {
		if err := restartK3s(client); err != nil {
			return false, fmt.Errorf("%w. kip leaves %s: true in %s, because once k3s has started with it the Secrets it writes are encrypted and removing it makes them unreadable. Check the k3s service on the server, then run 'kip cluster encrypt-secrets' again",
				err, secretsEncryptionKey, k3sConfigPath)
		}
	}

	s, err := waitForEncryptionStatus(client, encryptionStatusWait.ready, func(s encryptionStatus) bool { return s.hashesMatch })
	if err != nil {
		return false, fmt.Errorf("k3s did not report its encryption status after loading the key: %w", err)
	}
	recorded, err := encryptionStateCurrent(client)
	if err != nil {
		return false, err
	}
	// A key listing the identity provider first, as 'k3s secrets-encrypt
	// enable' writes it, reports encryption off at stage start until the
	// rotation below, and k3s records that stage for it on start. Any other
	// state was turned off by hand, and a forced reencrypt would rewrite every
	// Secret in plaintext.
	if !s.enabled && (s.stage != "start" || !recorded) {
		return false, fmt.Errorf("k3s reports encryption turned off, as 'k3s secrets-encrypt disable' leaves it. Run 'k3s secrets-encrypt enable' on the server, then run this again")
	}
	if s.stage == "prepare" || s.stage == "rotate" {
		return false, fmt.Errorf("a key rotation started with 'k3s secrets-encrypt %s' is in progress. Finish it as the k3s documentation describes, then run this again", s.stage)
	}
	// A forced reencrypt needs the API server to load the current encryption
	// config before it rewrites Secrets and removes the old key. rotate-keys
	// waits for that reload, so we restart here to provide the same guarantee.
	// Encryption must be enabled to avoid rewriting Secrets in plaintext.
	reencrypt := func(restartFirst bool) error {
		if restartFirst {
			say("Restarting k3s to load the key from its encryption config.")
			if err := restartK3s(client); err != nil {
				return fmt.Errorf("%w. Check the k3s service on the server, then run 'kip cluster encrypt-secrets' again", err)
			}
		}
		s, err := waitForEncryptionStatus(client, encryptionStatusWait.ready, func(s encryptionStatus) bool { return s.hashesMatch })
		if err != nil {
			return fmt.Errorf("k3s did not report its encryption status before the re-encryption: %w", err)
		}
		if !s.enabled {
			return fmt.Errorf("k3s reports encryption turned off. kip stops here because re-encryption would write every Secret unencrypted. Run 'k3s secrets-encrypt enable' on the server, then run this again")
		}
		if _, err := client.Run("k3s secrets-encrypt reencrypt --force"); err != nil {
			return fmt.Errorf("re-encrypting the Secrets with 'k3s secrets-encrypt reencrypt --force': %w", err)
		}
		return nil
	}
	switch {
	case s.reencrypting():
		// A rotation under way records its stage only when it is done, so
		// this comes before the check of the recorded stage.
		say("Waiting for the re-encryption already in progress.")
	case !recorded:
		// rotate-keys changes the key before it re-encrypts, so a restart in
		// between leaves the old stage recorded, and k3s refuses another
		// rotation. A forced reencrypt rewrites every Secret with the key now
		// in use. A restart in this run already loaded the current config.
		say("An earlier key rotation stopped partway. Re-encrypting every Secret with the current key.")
		restartedHere := !loaded
		if err := reencrypt(!restartedHere); err != nil {
			return false, err
		}
	case s.stage == encryptionFinishedStage:
		// An earlier run re-encrypted everything, and the restart above loaded
		// the result.
		return false, nil
	default:
		say("Rotating to a new key and re-encrypting every Secret. This takes a minute or more.")
		if _, err := client.Run("k3s secrets-encrypt rotate-keys"); err != nil {
			// The client times out after 90 seconds while the server continues,
			// so we wait below if re-encryption is still in progress.
			if s, statusErr := readEncryptionStatus(client); statusErr != nil || !s.reencrypting() && s.stage != encryptionFinishedStage {
				return false, fmt.Errorf("re-encrypting the Secrets with 'k3s secrets-encrypt rotate-keys': %w. Run 'kip cluster encrypt-secrets' again", err)
			}
		}
	}

	finished := func(s encryptionStatus) bool { return s.stage == encryptionFinishedStage }
	s, err = waitForEncryptionStatus(client, encryptionStatusWait.ready, finished)
	if err != nil && s.reencrypting() {
		// k3s keeps the stage when an error stops its re-encryption, and
		// refuses rotate-keys from there.
		say("k3s stopped re-encrypting partway. Starting the re-encryption again.")
		if err := reencrypt(true); err != nil {
			return false, err
		}
		s, err = waitForEncryptionStatus(client, encryptionStatusWait.ready, finished)
	}
	if err != nil {
		return false, fmt.Errorf("k3s has not finished re-encrypting the Secrets (stage %q, waiting for %s): %w", s.stage, encryptionFinishedStage, err)
	}

	// The k3s procedure ends with a restart. The API server already reloads
	// the encryption config on its own (k3s runs it with
	// --encryption-provider-config-automatic-reload), which is why a rerun that
	// finds the re-encryption finished counts the cluster as done.
	say("Restarting k3s on the new key.")
	if err := restartK3s(client); err != nil {
		return false, fmt.Errorf("%w. Every Secret is already re-encrypted. Check the k3s service on the server, then run 'kip cluster encrypt-secrets' again to confirm", err)
	}
	s, err = waitForEncryptionStatus(client, encryptionStatusWait.ready, encryptionStatus.done)
	if err != nil {
		return false, fmt.Errorf("k3s does not report every Secret encrypted after the restart (enabled %t, stage %q, hashes match %t): %w", s.enabled, s.stage, s.hashesMatch, err)
	}
	return false, nil
}

// encryptionKeyExists reports whether k3s has an encryption config on this
// server.
func encryptionKeyExists(client CommandRunner) (bool, error) {
	out, err := client.Run(fmt.Sprintf("test -f %s && echo yes || echo no", encryptionConfigPath))
	if err != nil {
		return false, fmt.Errorf("checking for %s: %w", encryptionConfigPath, err)
	}
	return strings.TrimSpace(out) == "yes", nil
}

// encryptionStateCurrent reports whether the stage k3s last recorded belongs to
// the current encryption config. A rotation interrupted before re-encryption
// leaves a newer config than the one recorded with the stage.
func encryptionStateCurrent(client CommandRunner) (bool, error) {
	out, err := client.Run(fmt.Sprintf(`state=$(cat %s) || exit 1; config=$(sha256sum %s) || exit 1; [ "${state##*-}" = "${config%%%% *}" ] && echo current || echo stale`,
		encryptionStatePath, encryptionConfigPath))
	if err != nil {
		return false, fmt.Errorf("comparing %s with %s: %w", encryptionStatePath, encryptionConfigPath, err)
	}
	return strings.TrimSpace(out) == "current", nil
}

// forgetPendingWrite removes the marker that lets a failed restart restore the
// config from before the last write. A config with the flag must never be
// restored to one without it.
func forgetPendingWrite(client CommandRunner) error {
	if _, err := client.Run("rm -f " + k3sConfigPendingPath); err != nil {
		return fmt.Errorf("removing %s, which would let a later failed restart restore a config without %s: %w", k3sConfigPendingPath, secretsEncryptionKey, err)
	}
	return nil
}

// secretsEncryptionSetting reports whether the config turns encryption on. A
// missing setting returns false. An explicit value other than a recognized
// true value returns an error so the caller preserves the operator's choice.
func secretsEncryptionSetting(config string) (bool, error) {
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(config), &doc); err != nil {
		return false, fmt.Errorf("parsing %s: %w. kip will not rewrite a config it cannot read", k3sConfigPath, err)
	}
	raw, ok := doc[secretsEncryptionKey]
	if !ok {
		return false, nil
	}
	// k3s passes every config value as a flag string, so "true" and 1 turn
	// encryption on as well.
	if on, err := strconv.ParseBool(fmt.Sprint(raw)); err == nil && on {
		return true, nil
	}
	return false, fmt.Errorf("%s sets %s: %v, so kip leaves it alone. Remove that line to let kip turn encryption on", k3sConfigPath, secretsEncryptionKey, raw)
}

// encryptionSupportedSince lists the first release of each Kubernetes minor
// whose k3s can turn encryption on for a cluster that already holds Secrets.
// Every minor after the last one listed supports it.
var encryptionSupportedSince = map[int]int{33: 10, 34: 6, 35: 3}

func requireEncryptionSupport(client CommandRunner) error {
	installed, err := installedK3sVersion(client)
	if err != nil {
		return err
	}
	if installed == "" {
		return fmt.Errorf("k3s is not installed on this host")
	}
	v, err := parseK3sVersion(installed)
	if err != nil {
		return err
	}
	minor, patch := v[1], v[2]
	first, listed := encryptionSupportedSince[minor]
	if v[0] == 1 && (minor > 35 || (listed && patch >= first)) {
		return nil
	}
	refusal := fmt.Sprintf("k3s %s cannot turn on encryption for a cluster that already holds Secrets. That needs v1.33.10, v1.34.6, v1.35.3 or any later release", installed)
	// kip upgrade --k3s moves k3s at most one minor release.
	if pinned, err := parseK3sVersion(k3sVersion); err == nil && v[0] == pinned[0] && pinned[1]-minor <= 1 {
		return fmt.Errorf("%s. Run 'kip upgrade --k3s' first", refusal)
	}
	return errors.New(refusal)
}
