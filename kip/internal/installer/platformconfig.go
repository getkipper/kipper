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
)

const nanoRAMMB = 3500

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

// profileForRAM selects a profile from total memory in MiB. The thresholds
// allow for memory reserved by the host below its advertised capacity.
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

// readSavedPlatformState reads the profile and overrides used for preflight.
// Lookup failures and missing or unknown profiles return an empty state.
func readSavedPlatformState(client CommandRunner) PlatformState {
	out, err := client.Run("kubectl get platformconfig platform --request-timeout=15s -o " +
		`jsonpath='{.spec.profile}|{range .spec.components[*]}{.name}={.memoryLimit};{.enabled}{"\n"}{end}' 2>/dev/null || true`)
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
// profile and route guard enabled, preserving an existing CR and its overrides.
// Call it after InstallCRDs. It waits up to 60 seconds for the CRD to become
// Established and returns an error if the wait or creation fails.
func InstallPlatformConfig(client CommandRunner, profile string) error {
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
  routeGuard:
    blockInternalPaths: true
`, profile)

	cmd := fmt.Sprintf(
		"kubectl get platformconfig platform >/dev/null 2>&1 || "+
			"cat <<'KIPEOF' | kubectl create -f -\n%sKIPEOF",
		manifest,
	)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("creating PlatformConfig: %w", err)
	}
	return nil
}

// ReadPlatformStateViaSSH reads the recorded profile, memory overrides, and
// explicit component enablement through kubectl so reinstalls preserve them.
// It returns an error if the command fails.
func ReadPlatformStateViaSSH(client CommandRunner) (PlatformState, error) {
	out, err := client.Run(
		"kubectl get platformconfig platform -o " +
			`jsonpath='{.spec.profile}|{range .spec.components[*]}{.name}={.memoryLimit};{.enabled}{"\n"}{end}'`,
	)
	if err != nil {
		return PlatformState{}, fmt.Errorf("reading PlatformConfig over SSH: %w", err)
	}
	return parsePlatformStateJSONPath(out), nil
}

// parsePlatformStateJSONPath splits the jsonpath output produced by
// ReadPlatformStateViaSSH. Lifted into its own function so tests don't need
// an SSH client.
//
// Output shape: "<profile>|<name>=<memoryLimit>;<enabled>\n<name>=<memoryLimit>;<enabled>"
// Empty fields (e.g. unset enabled) yield empty strings; we tolerate that.
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
	state.Profile = strings.TrimSpace(parts[0])
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
