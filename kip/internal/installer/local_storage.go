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
// missing PlatformConfig or empty storage field defaults to Longhorn.
// Command failures return an empty mode and an error.
func ReadStorageMode(client CommandRunner) (string, error) {
	out, err := client.Run("kubectl get platformconfig platform --ignore-not-found -o jsonpath='{.spec.storage}'")
	if err != nil {
		return "", fmt.Errorf("reading the storage mode: %w", err)
	}
	return platform.StorageMode(strings.Trim(strings.TrimSpace(out), "'")), nil
}
