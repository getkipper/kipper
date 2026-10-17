package installer

import (
	"fmt"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

// checkLocalPathStorage checks that the local-path StorageClass exists.
func checkLocalPathStorage(client CommandRunner) error {
	out, err := client.Run("kubectl get storageclass local-path --ignore-not-found -o name")
	if err != nil {
		return fmt.Errorf("checking local storage: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return fmt.Errorf("local storage requires the local-path StorageClass. Reinstall k3s with its local-storage component enabled")
	}
	return nil
}

// ReadStorageMode reads the cluster's storage mode from its PlatformConfig. A
// missing PlatformConfig, empty storage field or a cluster without the
// PlatformConfig resource type defaults to Longhorn. Other command failures
// and unrecognised storage values return an empty mode and an error.
func ReadStorageMode(client CommandRunner) (string, error) {
	out, err := client.Run("kubectl get platformconfig platform --ignore-not-found -o jsonpath='{.spec.storage}' 2>&1")
	if err != nil {
		if identityAbsentFromKubectl(out) {
			return StorageLonghorn, nil
		}
		return "", fmt.Errorf("reading the storage mode: %w: %s", err, strings.TrimSpace(out))
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	mode := strings.Trim(strings.TrimSpace(lines[len(lines)-1]), "'")
	if mode != "" && !platform.IsStorageMode(mode) {
		return "", fmt.Errorf("reading the storage mode: unexpected output %q", strings.TrimSpace(out))
	}
	return platform.StorageMode(mode), nil
}
