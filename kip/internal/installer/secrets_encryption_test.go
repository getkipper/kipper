package installer

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

// encryptingK3s models k3s encryption state across restarts and rotations.
// With a key present, status and rotate-keys require the encryption flag to
// be loaded. Restarting with the flag creates an AES key if none exists.
type encryptingK3s struct {
	version       string
	config        string
	keyFile       bool
	loaded        bool
	enabled       bool
	stage         string
	restartFail   bool
	networkMove   bool
	staleState    bool
	rotateTimeout bool
	rotateRefused bool
	rotateStalls  bool
	// Counts remaining status reads. Zero leaves re-encryption running indefinitely.
	finishAfter      int
	restartsAtForce  []int
	pending          bool
	pendingAtRestart []bool
	restarts         int
	rotations        int
	enables          int
	forcedReencrypts int
}

func unencryptedK3s() *encryptingK3s {
	return &encryptingK3s{version: k3sVersion, config: "tls-san:\n  - \"demo.example.com\"\n"}
}

func encryptedK3s() *encryptingK3s {
	k := unencryptedK3s()
	k.config += "secrets-encryption: true\n"
	k.keyFile, k.loaded, k.enabled, k.stage = true, true, true, "reencrypt_finished"
	return k
}

func (k *encryptingK3s) flagged() bool {
	var doc map[string]any
	_ = yaml.Unmarshal([]byte(k.config), &doc)
	return doc[secretsEncryptionKey] == true
}

func (k *encryptingK3s) host() *hostScript {
	h := &hostScript{}
	h.reply = func(command string) (string, error) {
		switch {
		case strings.Contains(command, "list-unit-files k3s.service"):
			return "1\n", nil
		case strings.Contains(command, networkChangeRecord):
			if k.networkMove {
				return "wireguard-native\n", nil
			}
			return "", nil
		case strings.Contains(command, k3sVersionProbeCmd):
			return fmt.Sprintf("k3s version %s (01b6f04a)\n", k.version), nil
		case strings.Contains(command, encryptionStatePath):
			if k.staleState {
				return "stale\n", nil
			}
			return "current\n", nil
		case strings.Contains(command, "test -f "+encryptionConfigPath):
			if k.keyFile {
				return "yes\n", nil
			}
			return "no\n", nil
		case strings.Contains(command, "k3s secrets-encrypt enable"):
			k.enables++
			k.keyFile = true
			return "secrets-encryption enabled\n", nil
		case strings.Contains(command, `cat > "$staged"`):
			k.config = h.stdins[len(h.commands)-1]
			k.pending = !strings.Contains(command, "rm -f "+k3sConfigPendingPath)
			return "", nil
		case strings.Contains(command, "rm -f "+k3sConfigPendingPath):
			k.pending = false
			return "", nil
		case strings.Contains(command, "cat "+k3sConfigPath):
			return k.config, nil
		case strings.Contains(command, "systemctl restart k3s"):
			k.restarts++
			k.pendingAtRestart = append(k.pendingAtRestart, k.pending)
			k.loaded = k.flagged()
			if k.stage == "reencrypt_active" && k.staleState {
				// k3s takes the stage from its state file on start.
				k.stage, k.finishAfter = "reencrypt_finished", 0
			}
			if k.loaded && !k.keyFile {
				k.keyFile, k.enabled, k.stage = true, true, "start"
			}
			if k.loaded && k.stage == "" {
				k.stage = "start"
			}
			return "", nil
		case strings.Contains(command, "/readyz"):
			if k.restartFail {
				return "", errors.New("exit status 1")
			}
			return "", nil
		case strings.Contains(command, "k3s secrets-encrypt status"):
			if k.loaded && k.stage == "reencrypt_active" && k.finishAfter > 0 {
				k.finishAfter--
				if k.finishAfter == 0 {
					k.stage, k.staleState = "reencrypt_finished", false
				}
			}
			if !k.keyFile {
				return "Encryption Status: Disabled, no configuration file found\n", nil
			}
			if !k.loaded {
				return "Error: see server log for details: secret-encrypt error ID 94640\n", errors.New("exit status 1")
			}
			status := "Disabled"
			if k.enabled {
				status = "Enabled"
			}
			return fmt.Sprintf("Encryption Status: %s\nCurrent Rotation Stage: %s\nServer Encryption Hashes: All hashes match\n\nActive  Key Type  Name\n------  --------  ----\n *      AES-CBC   aescbckey\n", status, k.stage), nil
		case strings.Contains(command, "k3s secrets-encrypt reencrypt --force"):
			if !k.loaded {
				return "Error: see server log for details\n", errors.New("exit status 1")
			}
			k.forcedReencrypts++
			k.restartsAtForce = append(k.restartsAtForce, k.restarts)
			k.stage, k.staleState = "reencrypt_finished", false
			return "reencryption started\n", nil
		case strings.Contains(command, "k3s secrets-encrypt rotate-keys"):
			if !k.loaded {
				return "Error: see server log for details: secret-encrypt error ID 16824\n", errors.New("exit status 1")
			}
			if k.rotateRefused {
				return "Error: see server log for details\n", errors.New("exit status 1")
			}
			k.rotations++
			if k.rotateStalls {
				k.stage = "reencrypt_active"
				return "Error: see server log for details\n", errors.New("exit status 1")
			}
			if k.rotateTimeout {
				// The client timed out while the server re-encrypts, which
				// it records only when done.
				k.enabled, k.stage, k.staleState, k.finishAfter = true, "reencrypt_active", true, 2
				return "Error: context deadline exceeded\n", errors.New("exit status 1")
			}
			k.enabled, k.stage = true, "reencrypt_finished"
			return "keys rotated, reencryption finished\n", nil
		}
		return "", nil
	}
	return h
}

func quickEncryptionWait(t *testing.T) {
	t.Helper()
	saved := encryptionStatusWait
	encryptionStatusWait.settle, encryptionStatusWait.ready, encryptionStatusWait.interval = 0, 0, 0
	t.Cleanup(func() { encryptionStatusWait = saved })
}

func TestEncryptingSecretsFollowsTheK3sProcedure(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	h := k.host()

	already, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.False(t, already)
	assert.True(t, k.flagged(), "the config must keep the flag, or the next restart cannot read the Secrets")
	assert.True(t, k.enabled)
	assert.Equal(t, "reencrypt_finished", k.stage)
	assert.Equal(t, 2, k.restarts, "k3s asks for a restart after the flag and another after the key rotation")

	enable := h.indexOf("k3s secrets-encrypt enable")
	write := h.indexOf(`cat > "$staged"`)
	rotate := h.indexOf("k3s secrets-encrypt rotate-keys")
	require.True(t, enable >= 0 && write >= 0 && rotate >= 0)
	assert.Less(t, enable, write)
	assert.Less(t, write, firstAfter(h, "systemctl restart k3s", 0))
	assert.Less(t, firstAfter(h, "systemctl restart k3s", 0), rotate)
	assert.Greater(t, firstAfter(h, "systemctl restart k3s", rotate), rotate)
}

// firstAfter returns the first command index at or after from containing
// fragment, or -1 if none matches.
func firstAfter(h *hostScript, fragment string, from int) int {
	for i := from; i < len(h.commands); i++ {
		if strings.Contains(h.commands[i], fragment) {
			return i
		}
	}
	return -1
}

func TestAClusterThatAlreadyEncryptsItsSecretsIsLeftAlone(t *testing.T) {
	quickEncryptionWait(t)
	k := encryptedK3s()
	h := k.host()

	already, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.True(t, already)
	assert.Zero(t, k.restarts)
	assert.Zero(t, k.rotations)
	assert.False(t, h.sent(`cat > "$staged"`))
}

func TestARunInterruptedBeforeTheRestartPicksUpThere(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	k.keyFile = true
	k.config += "secrets-encryption: true\n"
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.Zero(t, k.enables, "the encryption config already exists")
	assert.False(t, h.sent(`cat > "$staged"`), "the flag is already in the config")
	assert.Equal(t, 1, k.rotations)
	assert.Equal(t, 2, k.restarts)
	assert.True(t, k.enabled)
}

// A cluster installed with the flag encrypts new writes. Adding the flag
// later leaves earlier Secrets unencrypted, so the run rewrites them all.
func TestAClusterWithTheFlagButNoRotationHasItsSecretsRewritten(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	k.config += "secrets-encryption: true\n"
	k.keyFile, k.loaded, k.enabled, k.stage = true, true, true, "start"
	h := k.host()

	already, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.False(t, already)
	assert.Equal(t, 1, k.rotations)
	assert.Equal(t, 1, k.restarts, "only the restart after the rotation")
	assert.Less(t, h.indexOf("k3s secrets-encrypt rotate-keys"), h.indexOf("systemctl restart k3s"))
}

func TestAKeyRotationStartedByHandIsLeftToTheOperator(t *testing.T) {
	quickEncryptionWait(t)
	for _, stage := range []string{"prepare", "rotate"} {
		k := encryptedK3s()
		k.stage = stage
		h := k.host()

		_, err := EncryptSecrets(h, nil)

		require.Error(t, err, stage)
		assert.Contains(t, err.Error(), stage)
		assert.Zero(t, k.rotations, stage)
		assert.Zero(t, k.restarts, stage)
	}
}

func TestAK3sTooOldToEncryptAnExistingClusterIsAskedToUpgrade(t *testing.T) {
	quickEncryptionWait(t)
	for version, supported := range map[string]bool{
		"v1.32.9+k3s1":  false,
		"v1.33.9+k3s1":  false,
		"v1.33.10+k3s1": true,
		"v1.34.5+k3s1":  false,
		"v1.34.6+k3s1":  true,
		"v1.35.2+k3s1":  false,
		"v1.35.3+k3s1":  true,
		"v1.36.0+k3s1":  true,
		k3sVersion:      true,
	} {
		k := unencryptedK3s()
		k.version = version
		h := k.host()

		_, err := EncryptSecrets(h, nil)

		if supported {
			assert.NoError(t, err, version)
			continue
		}
		require.Error(t, err, version)
		assert.Contains(t, err.Error(), "v1.35.3", version)
		assert.Zero(t, k.enables, version)
		assert.False(t, h.sent(`cat > "$staged"`), version)
	}
}

// k3s may still finish starting after the readiness check gives up, and from
// then on it encrypts what it writes, so the flag stays in place.
func TestASlowRestartKeepsTheFlagAndAsksForARerun(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	k.restartFail = true
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "run 'kip cluster encrypt-secrets' again")
	assert.True(t, k.flagged())
	assert.False(t, h.sent("cp -p "+k3sConfigBackupPath), "nothing may restore the config without the flag")
	assert.Zero(t, k.rotations)
}

// A pending marker lets a later failed restart, such as the one in kip
// upgrade's API server repair, restore the config from before the write.
func TestNoRollbackMarkerSurvivesTheFlag(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.Equal(t, []bool{false, false}, k.pendingAtRestart, "the marker is gone before k3s first starts with the flag")
	assert.False(t, k.pending)

	k = encryptedK3s()
	k.pending = true
	already, err := EncryptSecrets(k.host(), nil)

	require.NoError(t, err)
	assert.True(t, already)
	assert.False(t, k.pending, "an interrupted run's marker is removed even when nothing else is left to do")
}

// Network changes take the config lock, so checking for one after taking it
// leaves no moment for a change to start unseen.
func TestThePendingNetworkChangeIsCheckedUnderTheLock(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.Greater(t, h.indexOf(networkChangeRecord), h.indexOf("mkdir "+apiServerLockPath))
}

// The marker goes before the new config lands, so no interruption can leave
// one that points at the backup without the flag.
func TestTheFlagIsWrittenWithoutARollbackMarker(t *testing.T) {
	script := writeK3sConfigWithoutRollbackScript()

	assert.NotContains(t, script, "> "+k3sConfigPendingPath)
	remove := strings.Index(script, "rm -f "+k3sConfigPendingPath)
	move := strings.Index(script, `mv "$staged" "$target"`)
	require.True(t, remove >= 0 && move >= 0)
	assert.Less(t, remove, move)
	assert.Contains(t, writeK3sConfigScript(), `> `+k3sConfigPendingPath, "the API server repair keeps its rollback")
}

// A run stopped between the re-encryption and the last restart finds the
// work done once k3s answers again, and rotates nothing.
func TestARunInterruptedAfterTheReencryptionDoesNotRotateAgain(t *testing.T) {
	quickEncryptionWait(t)
	k := encryptedK3s()
	k.loaded = false
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.Zero(t, k.rotations)
	assert.Equal(t, 1, k.restarts)
}

// The flag can be in the config before k3s has loaded it or created a key, and
// k3s then answers a status request without an error.
func TestAFlagThatK3sHasNotLoadedYetGetsItsRestart(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	k.config += "secrets-encryption: true\n"
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.Equal(t, 1, k.enables)
	assert.Equal(t, 2, k.restarts)
	assert.True(t, k.enabled)
	assert.Less(t, h.indexOf("k3s secrets-encrypt enable"), h.indexOf("systemctl restart k3s"))
}

// Finishing an interrupted network change restores the config saved before
// it, which has no flag.
func TestAnUnfinishedNetworkChangeIsFinishedFirst(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	k.networkMove = true
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip upgrade")
	assert.Zero(t, k.enables)
	assert.False(t, h.sent(`cat > "$staged"`))
	assert.Zero(t, k.restarts)
}

// kip upgrade --k3s moves one minor release, so only a cluster one minor
// behind kip's k3s is pointed at it.
func TestOnlyAK3sOneUpgradeAwayIsSentToKipUpgrade(t *testing.T) {
	quickEncryptionWait(t)
	pinned, err := parseK3sVersion(k3sVersion)
	require.NoError(t, err)
	for version, upgradable := range map[string]bool{
		fmt.Sprintf("v1.%d.0+k3s1", pinned[1]-1): true,
		fmt.Sprintf("v1.%d.5+k3s1", pinned[1]-2): false,
	} {
		k := unencryptedK3s()
		k.version = version

		err := requireEncryptionSupport(k.host())

		require.Error(t, err, version)
		assert.Equal(t, upgradable, strings.Contains(err.Error(), "kip upgrade --k3s"), version)
	}
}

func TestEncryptionTurnedOffWithK3sIsLeftToTheOperator(t *testing.T) {
	quickEncryptionWait(t)
	k := encryptedK3s()
	k.enabled = false
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "k3s secrets-encrypt enable")
	assert.Zero(t, k.rotations)
	assert.Zero(t, k.restarts)
}

// 'k3s secrets-encrypt disable' selects the identity provider, so encryption
// can be off in any stage after the first.
func TestEncryptionTurnedOffDuringAReencryptionStopsTheRun(t *testing.T) {
	quickEncryptionWait(t)
	k := encryptedK3s()
	k.enabled, k.stage = false, "reencrypt_active"
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "k3s secrets-encrypt enable")
	assert.Zero(t, k.restarts)
	assert.False(t, h.sent("k3s secrets-encrypt reencrypt"))
}

// An error in the re-encryption k3s runs leaves reencrypt_active with nothing
// running, and rotate-keys refuses that stage. The forced reencrypt runs after
// a restart, because unlike rotate-keys it does not wait for the API server to
// load the current key.
func TestAReencryptionThatStoppedIsStartedAgain(t *testing.T) {
	quickEncryptionWait(t)
	k := encryptedK3s()
	k.stage, k.staleState = "reencrypt_active", true
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.Equal(t, 1, k.forcedReencrypts)
	assert.Equal(t, []int{1}, k.restartsAtForce, "k3s restarts before the forced reencrypt")
	assert.Zero(t, k.rotations)
	assert.Equal(t, "reencrypt_finished", k.stage)
	assert.Equal(t, 2, k.restarts, "the procedure still ends with a restart")
}

// A rotation under way records its stage only when done, so its state looks
// like one that was cut off.
func TestARunningReencryptionIsWaitedFor(t *testing.T) {
	quickEncryptionWait(t)
	encryptionStatusWait.ready = time.Minute
	k := encryptedK3s()
	k.stage, k.staleState, k.finishAfter = "reencrypt_active", true, 3
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.Zero(t, k.forcedReencrypts)
	assert.Zero(t, k.rotations)
	assert.Equal(t, 1, k.restarts)
}

// After 'k3s secrets-encrypt disable' at stage start, the key lists identity
// first and the recorded stage is stale, so a forced reencrypt would write
// every Secret in plaintext.
func TestEncryptionTurnedOffAtTheStartIsNotForced(t *testing.T) {
	quickEncryptionWait(t)
	k := encryptedK3s()
	k.enabled, k.stage, k.staleState = false, "start", true
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "k3s secrets-encrypt enable")
	assert.Zero(t, k.forcedReencrypts)
	assert.Zero(t, k.rotations)
}

// Encryption is off at stage start until the rotation writes its key, so a
// rotation that failed before that leaves nothing a forced reencrypt could
// encrypt with.
func TestAStalledRotationWithEncryptionOffIsNotForced(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	k.rotateStalls = true
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "k3s secrets-encrypt enable")
	assert.Zero(t, k.forcedReencrypts)
}

func TestTheFlagWriteLeavesNoBackupToRestore(t *testing.T) {
	assert.Contains(t, writeK3sConfigWithoutRollbackScript(), "rm -f "+k3sConfigPendingPath+" "+k3sConfigBackupPath+"\nmv")
}

func TestAnUnreadableEncryptionStateIsAnError(t *testing.T) {
	quickEncryptionWait(t)
	k := encryptedK3s()
	h := k.host()
	reply := h.reply
	h.reply = func(command string) (string, error) {
		if strings.Contains(command, encryptionStatePath) {
			return "cat: can't open: No such file or directory\n", errors.New("exit status 1")
		}
		return reply(command)
	}

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), encryptionStatePath)
	assert.Zero(t, k.forcedReencrypts)
}

// After a rotation was cut off, k3s reports the stage from before it, so a
// cluster that rotated once before looks done.
func TestARotationCutOffBeforeItsStageWasRecordedIsFinished(t *testing.T) {
	quickEncryptionWait(t)
	for _, stage := range []string{"start", "reencrypt_finished"} {
		k := encryptedK3s()
		k.stage, k.staleState = stage, true
		h := k.host()

		already, err := EncryptSecrets(h, nil)

		require.NoError(t, err, stage)
		assert.False(t, already, stage)
		assert.Equal(t, 1, k.forcedReencrypts, stage)
		assert.Equal(t, []int{1}, k.restartsAtForce, "the server reloads the current key before the forced reencrypt")
		assert.Zero(t, k.rotations, "k3s refuses rotate-keys while the recorded stage is stale")
		assert.Equal(t, 2, k.restarts, stage)
	}
}

// The restart that loads the flag also loads the current key, so the forced
// reencrypt needs no restart of its own.
func TestACutOffRotationFoundAfterLoadingTheFlagNeedsNoExtraRestart(t *testing.T) {
	quickEncryptionWait(t)
	k := encryptedK3s()
	k.loaded, k.stage, k.staleState = false, "start", true
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.Equal(t, []int{1}, k.restartsAtForce)
	assert.Equal(t, 2, k.restarts)
}

func TestARotateKeysClientTimeoutWaitsForTheServer(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	k.rotateTimeout = true
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.NoError(t, err)
	assert.Equal(t, "reencrypt_finished", k.stage)
}

func TestARefusedRotationAsksForARerun(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	k.rotateRefused = true
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Run 'kip cluster encrypt-secrets' again")
	assert.Zero(t, k.forcedReencrypts)
}

// k3s reads every config value as a flag string.
func TestTheFlagCountsInEveryFormK3sAccepts(t *testing.T) {
	for _, value := range []string{"true", `"true"`, "1", `"t"`} {
		on, err := secretsEncryptionSetting("secrets-encryption: " + value + "\n")

		require.NoError(t, err, value)
		assert.True(t, on, value)
	}
	for _, value := range []string{"false", `"false"`, "0"} {
		_, err := secretsEncryptionSetting("secrets-encryption: " + value + "\n")

		assert.Error(t, err, value)
	}
}

func TestASettingTurnedOffByHandIsNotOverwritten(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	k.config += "secrets-encryption: false\n"
	h := k.host()

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "secrets-encryption: false")
	assert.False(t, h.sent(`cat > "$staged"`))
	assert.Zero(t, k.enables)
	assert.Zero(t, k.restarts)
}

func TestEncryptingSecretsRefusesANodeRunningNoServer(t *testing.T) {
	quickEncryptionWait(t)
	h := &hostScript{reply: func(command string) (string, error) {
		if strings.Contains(command, "list-unit-files k3s.service") {
			return "0\n", nil
		}
		return "", nil
	}}

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.False(t, h.sent("secrets-encrypt"))
}

func TestEncryptingSecretsHoldsTheConfigLock(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	h := k.host()
	h.lockHeld = true

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "lock")
	assert.False(t, h.sent("secrets-encrypt"))
}

func TestARunThatEndsWithoutEncryptionSaysSo(t *testing.T) {
	quickEncryptionWait(t)
	k := unencryptedK3s()
	h := k.host()
	reply := h.reply
	h.reply = func(command string) (string, error) {
		if strings.Contains(command, "k3s secrets-encrypt rotate-keys") {
			return "keys rotated\n", nil
		}
		return reply(command)
	}

	_, err := EncryptSecrets(h, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "reencrypt_finished")
}

func TestTheEncryptionStatusIsReadFromK3sOutput(t *testing.T) {
	s, err := parseEncryptionStatus("Encryption Status: Disabled, no configuration file found\n")
	require.NoError(t, err)
	assert.False(t, s.configured)
	assert.False(t, s.enabled)

	s, err = parseEncryptionStatus("Encryption Status: Enabled\nCurrent Rotation Stage: reencrypt_finished\nServer Encryption Hashes: All hashes match\n\nActive  Key Type  Name\n *      AES-CBC   aescbckey\n")
	require.NoError(t, err)
	assert.True(t, s.configured)
	assert.True(t, s.enabled)
	assert.Equal(t, "reencrypt_finished", s.stage)
	assert.True(t, s.hashesMatch)
	assert.True(t, s.done())

	s, err = parseEncryptionStatus("Encryption Status: Enabled\nCurrent Rotation Stage: start\nServer Encryption Hashes: hash does not match between node-1 and node-2\n")
	require.NoError(t, err)
	assert.False(t, s.hashesMatch)
	assert.False(t, s.done())

	_, err = parseEncryptionStatus("Error: see server log for details\n")
	assert.Error(t, err)
}

func TestSecretsEncryptedReportsWhatTheServerSays(t *testing.T) {
	encrypted, err := SecretsEncrypted(encryptedK3s().host())
	require.NoError(t, err)
	assert.True(t, encrypted)

	encrypted, err = SecretsEncrypted(unencryptedK3s().host())
	require.NoError(t, err)
	assert.False(t, encrypted)
}

func TestNewInstallsEncryptSecrets(t *testing.T) {
	h := &k3sInstallHost{}

	require.NoError(t, InstallK3s(h, "203.0.113.10", nil, false, false, StorageLonghorn))

	assert.Contains(t, h.k3sConfig(t), "\n"+secretsEncryptionKey+": true\n")
}

// Adding the flag on a rerun would encrypt only what k3s writes from then on
// and quiet kip upgrade's reminder, and removing it from a cluster with a key
// would leave its Secrets unreadable.
func TestAnInstallRerunKeepsTheClustersEncryptionAsItIs(t *testing.T) {
	for _, key := range []bool{false, true} {
		h := &k3sInstallHost{installed: true, encryptionKey: key}

		var err error
		out := stdoutOf(t, func() { err = InstallK3s(h, "203.0.113.10", nil, true, false, StorageLonghorn) })

		require.NoError(t, err)
		assert.Equal(t, key, strings.Contains(h.k3sConfig(t), "\n"+secretsEncryptionKey+": true\n"), "key=%v", key)
		assert.Equal(t, !key, strings.Contains(out, "kip cluster encrypt-secrets"), "key=%v", key)
	}
}

// kip cluster encrypt-secrets takes the same lock, so it cannot add the flag
// between the rerun's key check and its write.
func TestAnInstallRerunChecksTheKeyAndWritesUnderTheConfigLock(t *testing.T) {
	h := &k3sInstallHost{installed: true}

	_ = stdoutOf(t, func() { require.NoError(t, InstallK3s(h, "203.0.113.10", nil, true, false, StorageLonghorn)) })

	var checked, wrote bool
	for _, c := range h.locked {
		checked = checked || strings.HasPrefix(c, "test -f "+encryptionConfigPath)
		wrote = wrote || c == replaceK3sConfigScript
	}
	assert.True(t, checked, "the key check runs under the lock")
	assert.True(t, wrote, "the config write runs under the lock")
}

func TestTheRenderedConfigCarriesTheFlagOnlyWhenAsked(t *testing.T) {
	for _, backend := range []string{wireguardBackend, "vxlan"} {
		for _, encrypt := range []bool{false, true} {
			var doc map[string]any
			require.NoError(t, yaml.Unmarshal([]byte(renderK3sConfig("demo.example.com", backend, "", encrypt)), &doc))
			_, present := doc[secretsEncryptionKey]
			assert.Equal(t, encrypt, present, "%s %v", backend, encrypt)
			if encrypt {
				assert.Equal(t, true, doc[secretsEncryptionKey])
			}
		}
	}
}
