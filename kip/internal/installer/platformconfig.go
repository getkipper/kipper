package installer

import (
	"fmt"
	"slices"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

const (
	ProfileNano   = platform.ProfileNano
	ProfileSmall  = platform.ProfileSmall
	ProfileMedium = platform.ProfileMedium
	ProfileLarge  = platform.ProfileLarge
	ProfileXLarge = platform.ProfileXLarge

	StorageLocal    = platform.StorageLocal
	StorageLonghorn = platform.StorageLonghorn

	BackupNone      = platform.BackupNone
	BackupInCluster = platform.BackupInCluster
	BackupExternal  = platform.BackupExternal
)

// nanoRAMMB is the detected RAM cutoff in MiB for disabling monitoring by default
// to leave memory for the platform and apps.
const nanoRAMMB = 6000

var profilesBySize = []string{ProfileNano, ProfileSmall, ProfileMedium, ProfileLarge, ProfileXLarge}

// platformCPURequestMilli estimates platform CPU requests in millicores.
// These fixed estimates guide initial sizing. Actual requests vary with
// enabled components, overrides, and node size.
var platformCPURequestMilli = map[string]int{
	ProfileNano:   1125,
	ProfileSmall:  1430,
	ProfileMedium: 1580,
	ProfileLarge:  1580,
	ProfileXLarge: 1580,
}

// defaultServiceCPURequestMilli is the request a database service gets without
// --cpu (the "database" preset in console-api's profileResources).
const defaultServiceCPURequestMilli = 500

func cpuHeadroomMilli(profile string, cpus int) int {
	return cpus*1000 - platformCPURequestMilli[profile]
}

// profileFitsCPU checks estimated CPU headroom for a default database service.
// A non-positive CPU count skips the check.
func profileFitsCPU(profile string, cpus int) bool {
	return cpus <= 0 || cpuHeadroomMilli(profile, cpus) >= defaultServiceCPURequestMilli
}

// profileForRAM selects a profile from total memory in MiB. Hosts below
// nanoRAMMB default to monitoring off. Higher cutoffs allow for host reservations.
func profileForRAM(ramMB int) string {
	switch {
	case ramMB < nanoRAMMB:
		return ProfileNano
	case ramMB < 7500:
		return ProfileSmall
	case ramMB < 15000:
		return ProfileMedium
	case ramMB < 30000:
		return ProfileLarge
	default:
		return ProfileXLarge
	}
}

// pickProfile selects a profile from memory in MiB, lowering it as needed
// to leave an estimated 500 millicores for a default database service.
// Nano is the minimum, even when it exceeds the CPU budget.
func pickProfile(ramMB, cpus int) string {
	profile := profileForRAM(ramMB)
	for profile != ProfileNano && !profileFitsCPU(profile, cpus) {
		profile = profilesBySize[slices.Index(profilesBySize, profile)-1]
	}
	return profile
}

// profilePickNote explains a pick that the CPU count lowered below the RAM
// pick, and is empty when RAM alone decided.
func profilePickNote(ramMB, cpus int, picked string) string {
	byRAM := profileForRAM(ramMB)
	if picked == byRAM {
		return ""
	}
	if !profileFitsCPU(picked, cpus) {
		return fmt.Sprintf("%d %s: using '%s', the smallest profile, instead of '%s'. Estimated platform CPU requests are %dm with %dm available.",
			cpus, plural(cpus, "CPU", "CPUs"), picked, byRAM, platformCPURequestMilli[picked], cpus*1000)
	}
	return fmt.Sprintf("%d %s: using '%s' instead of '%s' to leave an estimated %dm for a default database service. Estimated platform CPU requests are %dm on '%s'.",
		cpus, plural(cpus, "CPU", "CPUs"), picked, byRAM, defaultServiceCPURequestMilli, platformCPURequestMilli[byRAM], byRAM)
}

// keptProfileNotes explains a retained profile and checks its CPU headroom.
// It returns nil when the recorded profile is empty or matches the selection.
func keptProfileNotes(resolved, kept string, cpus int) []string {
	if kept == "" || kept == resolved {
		return nil
	}
	notes := []string{fmt.Sprintf("This cluster already runs the '%s' profile and keeps it. To change it, run kip platform profile set %s.", kept, resolved)}
	if w := cpuWarning(kept, cpus); w != "" {
		notes = append(notes, w)
	}
	return notes
}

// ignoredProfileNote explains why the recorded profile takes precedence over
// --profile. It returns "" when the request is empty or matches the saved profile.
func ignoredProfileNote(kept, requested string) string {
	if requested == "" || requested == kept {
		return ""
	}
	return fmt.Sprintf("The requested --profile %s was ignored. This cluster keeps its saved '%s' profile. To change it, run kip platform profile set %s.", requested, kept, requested)
}

// resolveProfile prefers the saved profile, then the requested profile, then
// a profile selected from host RAM and CPU capacity.
func resolveProfile(requested, saved string, sys SystemInfo) string {
	switch {
	case saved != "":
		return saved
	case requested != "":
		return requested
	}
	return pickProfile(sys.RAMMB, sys.CPUs)
}

// platformStateJSONPath prints a PlatformConfig as
// "<profile>/<storage>/<backup>|<name>=<memoryLimit>;<enabled>" with one
// component per line.
const platformStateJSONPath = `jsonpath='{.spec.profile}/{.spec.storage}/{.spec.backup}|{range .spec.components[*]}{.name}={.memoryLimit};{.enabled}{"\n"}{end}'`

// resolveStorage preserves an existing cluster's recorded mode, defaulting to
// Longhorn when empty. It rejects mode changes and requests for local storage
// when existing storage is unreadable. New clusters use the requested mode or
// default to local for nano and Longhorn for other profiles.
func resolveStorage(requested, profile string, existing, unreadable bool, recorded string) (string, error) {
	if existing {
		mode := platform.StorageMode(recorded)
		if unreadable && requested == StorageLocal {
			return "", fmt.Errorf("this host already runs k3s, but its storage configuration could not be read. " +
				"Run kip install again when the cluster is reachable to verify its storage mode")
		}
		if requested != "" && requested != mode {
			return "", fmt.Errorf("this cluster runs %s storage, which it keeps for its lifetime. "+
				"To use %s storage, install a new cluster with --storage %s and move your projects to it. See Cluster Migration in the Kipper docs",
				mode, requested, requested)
		}
		return mode, nil
	}
	switch {
	case requested != "":
		return requested, nil
	case profile == ProfileNano:
		return StorageLocal, nil
	}
	return StorageLonghorn, nil
}

// resolveBackup prefers supplied external storage, then an existing cluster's
// recorded mode. Legacy clusters default to in-cluster MinIO. New clusters use
// MinIO unless storage is local, because backups on the same disk cannot
// protect against disk loss.
func resolveBackup(external bool, storage string, existing bool, recorded string) string {
	switch {
	case external:
		return BackupExternal
	case existing && recorded != "":
		return recorded
	case existing:
		return BackupInCluster
	case storage == StorageLocal:
		return BackupNone
	}
	return BackupInCluster
}

// errExternalBackupUnknown stops an install that would otherwise point an
// external-backup cluster's Velero at a new MinIO.
var errExternalBackupUnknown = ErrExternalBackupUnknown

// ErrExternalBackupUnknown is returned when a cluster backs up to external
// storage whose connection settings are missing from the local kip configuration.
var ErrExternalBackupUnknown = fmt.Errorf("this cluster uses external backup storage. Its connection settings are missing from the local kip configuration. " +
	"Run kip install again with --backup-storage-bucket and its related flags so Velero keeps writing there")

// settleBackup resolves the mode from the freshly read PlatformConfig.
// It returns errExternalBackupUnknown if external backups are recorded but
// their connection settings are unavailable.
func settleBackup(external bool, storage, recorded string) (string, error) {
	mode := resolveBackup(external, storage, true, recorded)
	if mode == BackupExternal && !external {
		return "", errExternalBackupUnknown
	}
	return mode, nil
}

// settleLegacyBackup uses Velero's detected storage when no backup mode is recorded.
// If Velero uses external storage and its connection settings are missing, it
// returns errExternalBackupUnknown. Otherwise, it delegates to settleBackup.
func settleLegacyBackup(external bool, storage, recorded string, writesExternally bool) (string, error) {
	if recorded == "" && !external && writesExternally {
		return "", errExternalBackupUnknown
	}
	return settleBackup(external, storage, recorded)
}

// veleroWritesExternally reports whether the cluster's default Velero backup
// location uses an endpoint other than inClusterMinIOURL, including an empty
// endpoint for native AWS S3. A missing location or resource type returns false.
// Other read failures and malformed output return an error so an unreadable
// location cannot be mistaken for in-cluster backups.
func veleroWritesExternally(client CommandRunner) (bool, error) {
	out, err := client.Run(`kubectl -n velero get backupstoragelocation default --request-timeout=15s -o jsonpath='bsl[{.spec.config.s3Url}]' 2>&1`)
	if err != nil {
		if identityAbsentFromKubectl(out) {
			return false, nil
		}
		return false, fmt.Errorf("reading Velero's backup location: %w: %s", err, strings.TrimSpace(out))
	}
	start := strings.Index(out, "bsl[")
	if start < 0 {
		return false, fmt.Errorf("reading Velero's backup location: unexpected output %q", strings.TrimSpace(out))
	}
	end := strings.Index(out[start:], "]")
	if end < 0 {
		return false, fmt.Errorf("reading Velero's backup location: unexpected output %q", strings.TrimSpace(out))
	}
	return out[start+len("bsl["):start+end] != inClusterMinIOURL, nil
}

// discoverClusterStorage reports whether the host runs a cluster whose storage
// mode must be preserved, based on a saved profile, Longhorn's StorageClasses or
// its namespace, which exists before the classes do. If k3s may already exist
// and the query fails, both results are true to prevent treating an unreadable
// cluster as a fresh installation.
func discoverClusterStorage(client CommandRunner, savedProfile string) (existing, unreadable bool) {
	if savedProfile != "" {
		return true, false
	}
	freshness, err := client.Run("test -d /var/lib/rancher/k3s && echo existing || echo fresh")
	if !k3sPreexistingFromSample(freshness, err) {
		return false, false
	}
	out, err := client.Run("kubectl get storageclass/longhorn-single storageclass/longhorn namespace/longhorn-system --ignore-not-found -o name --request-timeout=15s")
	if err != nil {
		return true, true
	}
	return strings.TrimSpace(out) != "", false
}

// readSavedPlatformState reads the profile, storage and backup modes, and overrides for preflight.
// Lookup failures and missing or unknown profiles return an empty state.
func readSavedPlatformState(client CommandRunner) PlatformState {
	out, err := client.Run("kubectl get platformconfig platform --request-timeout=15s -o " +
		platformStateJSONPath + " 2>/dev/null || true")
	if err != nil {
		return PlatformState{}
	}
	state := parsePlatformStateJSONPath(strings.Trim(strings.TrimSpace(out), "'"))
	if !slices.Contains(profilesBySize, state.Profile) {
		return PlatformState{}
	}
	return state
}

// InstallPlatformConfig creates the initial "platform" CR with the chosen
// profile, storage mode and backup mode, with route guard enabled. It preserves
// existing settings, including the backup mode, and records Longhorn if the
// storage mode is empty. Call it after InstallCRDs. It waits up to
// 60 seconds for the CRD to become Established and returns an error if waiting,
// creation or recording fails.
func InstallPlatformConfig(client CommandRunner, profile, storage, backup string) error {
	if _, err := client.Run(
		"kubectl wait --for=condition=Established " +
			"crd/platformconfigs.kipper.run --timeout=60s",
	); err != nil {
		return fmt.Errorf("waiting for PlatformConfig CRD to be Established: %w", err)
	}

	manifest := fmt.Sprintf(`apiVersion: kipper.run/v1alpha1
kind: PlatformConfig
metadata:
  name: platform
spec:
  profile: %s
  storage: %s
  backup: %s
  routeGuard:
    blockInternalPaths: true
`, profile, storage, backup)

	cmd := fmt.Sprintf(
		"kubectl get platformconfig platform >/dev/null 2>&1 || "+
			"cat <<'KIPEOF' | kubectl create -f -\n%sKIPEOF",
		manifest,
	)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("creating PlatformConfig: %w", err)
	}
	if _, err := client.Run(recordLegacyStorageCommand); err != nil {
		return fmt.Errorf("recording the storage mode: %w", err)
	}
	return nil
}

// RecordBackupMode patches spec.backup on the "platform" PlatformConfig,
// replacing any recorded mode. It returns an error if the patch fails.
func RecordBackupMode(client CommandRunner, backup string) error {
	if _, err := client.Run(fmt.Sprintf(`kubectl patch platformconfig platform --type merge -p '{"spec":{"backup":%q}}'`, backup)); err != nil {
		return fmt.Errorf("recording the backup mode: %w", err)
	}
	return nil
}

// recordLegacyStorageCommand writes Longhorn into a PlatformConfig recorded
// before spec.storage existed, because every such cluster runs Longhorn.
const recordLegacyStorageCommand = `s=$(kubectl get platformconfig platform -o jsonpath='{.spec.storage}') || exit 1; ` +
	`[ -n "$s" ] || kubectl patch platformconfig platform --type merge -p '{"spec":{"storage":"longhorn"}}'`

// ReadPlatformStateViaSSH reads the profile, storage and backup modes, and
// component overrides through kubectl so reinstalls preserve them. Missing
// modes stay empty. Empty memory limits and unrecognised enablement values
// leave overrides unset. It returns an error if the command fails.
func ReadPlatformStateViaSSH(client CommandRunner) (PlatformState, error) {
	out, err := client.Run("kubectl get platformconfig platform -o " + platformStateJSONPath)
	if err != nil {
		return PlatformState{}, fmt.Errorf("reading PlatformConfig over SSH: %w", err)
	}
	return parsePlatformStateJSONPath(out), nil
}

// parsePlatformStateJSONPath reads "<profile>/<storage>/<backup>|<name>=<memoryLimit>;<enabled>"
// with one component per line. A missing storage or backup part leaves the
// field empty.
// Empty memory limits and unrecognised enablement values leave overrides unset.
func parsePlatformStateJSONPath(raw string) PlatformState {
	state := PlatformState{
		MemoryOverrides:  map[string]string{},
		EnabledOverrides: map[string]bool{},
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return state
	}
	parts := strings.SplitN(raw, "|", 2)
	profile, rest, _ := strings.Cut(strings.TrimSpace(parts[0]), "/")
	storage, backup, _ := strings.Cut(rest, "/")
	state.Profile, state.Storage, state.Backup = profile, storage, backup
	if len(parts) < 2 {
		return state
	}
	for _, line := range strings.Split(parts[1], "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		nameAndRest := strings.SplitN(line, "=", 2)
		if len(nameAndRest) != 2 {
			continue
		}
		name := strings.TrimSpace(nameAndRest[0])
		memAndEnabled := strings.SplitN(nameAndRest[1], ";", 2)
		mem := strings.TrimSpace(memAndEnabled[0])
		if mem != "" {
			state.MemoryOverrides[name] = mem
		}
		if len(memAndEnabled) == 2 {
			switch strings.TrimSpace(memAndEnabled[1]) {
			case "true":
				state.EnabledOverrides[name] = true
			case "false":
				state.EnabledOverrides[name] = false
			}
		}
	}
	return state
}
