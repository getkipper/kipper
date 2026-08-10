package installer

import (
	"fmt"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// Platform sizing profile names. The installer picks one based on node memory;
// users can change it later via the console or `kip platform profile set`.
// Re-exported from the shared platform package so callers in kip don't need
// to import it directly.
const (
	ProfileNano   = platform.ProfileNano
	ProfileSmall  = platform.ProfileSmall
	ProfileMedium = platform.ProfileMedium
	ProfileLarge  = platform.ProfileLarge
	ProfileXLarge = platform.ProfileXLarge
)

// pickProfile selects a platform sizing profile from total node memory in MB.
//
// The thresholds sit a few percent below the marketed RAM values (3500 / 7500 /
// 15000 / 30000 MB) because /proc/meminfo MemTotal is typically 1-3% under the
// advertised size: kernel reservations, firmware, and so on. Without this
// margin, a real 4 GB or 8 GB VPS would slip into the next-smaller bucket.
func pickProfile(ramMB int) string {
	switch {
	case ramMB < 3500:
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

// InstallPlatformConfig writes the initial PlatformConfig CR named "platform"
// with the chosen profile. It is a no-op if the CR already exists, so re-runs
// of the installer never clobber user overrides applied via the console or
// `kip platform`.
//
// Must run after InstallCRDs so the CRD schema is registered first. Even then,
// `kubectl apply` on a CRD can return before the API server is ready to serve
// the new kind, so we wait for the CRD to be Established before creating the
// CR — otherwise the first create can race and fail with "no matches for kind".
func InstallPlatformConfig(client *ssh.Client, profile string) error {
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

// ReadPlatformStateViaSSH fetches the active PlatformConfig CR over SSH+kubectl
// and returns the effective state for that cluster: profile, per-component
// memory overrides, and explicit enable/disable flags. Used by the installer
// to honor overrides on a re-run (e.g. a user who disabled monitoring via
// `kip platform disable loki` then runs `kip install` again — we must not
// resurrect Loki just because we re-detected a non-nano profile from the
// host).
func ReadPlatformStateViaSSH(client *ssh.Client) (PlatformState, error) {
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
