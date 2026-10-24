package installer

import (
	"fmt"
	"time"
)

// LonghornMinRAMMB is the least RAM in MiB a server needs to run Longhorn,
// both for a new 'kip install --storage longhorn' and for growing a local
// storage cluster onto Longhorn. It is provisional: a 3.8 GiB server running
// Longhorn was marginal with three small Postgres services. Existing Longhorn
// clusters below it keep installing and upgrading.
const LonghornMinRAMMB = 6000

// MemoryFloorOverrideAnnotation on the PlatformConfig records that Longhorn was
// installed below LonghornMinRAMMB on request.
const MemoryFloorOverrideAnnotation = "kipper.run/memory-floor-override"

// CheckLonghornMemory refuses a server with less than LonghornMinRAMMB of RAM.
// With skip, it returns a warning instead of the refusal.
func CheckLonghornMemory(ramMB int, skip bool) (warning string, err error) {
	if ramMB >= LonghornMinRAMMB {
		return "", nil
	}
	if skip {
		return fmt.Sprintf("this server has %d MiB of RAM, below the %d MiB Longhorn needs. Longhorn is installed anyway, and the cluster records the override", ramMB, LonghornMinRAMMB), nil
	}
	return "", fmt.Errorf("this server has %d MiB of RAM, and Longhorn needs at least %d MiB. Resize the server and run the command again", ramMB, LonghornMinRAMMB)
}

// installMemoryFloor applies the Longhorn floor to a new Longhorn cluster only.
func installMemoryFloor(storageMode string, existingCluster bool, ramMB int, skip bool) (warning string, err error) {
	if storageMode != StorageLonghorn || existingCluster {
		return "", nil
	}
	return CheckLonghornMemory(ramMB, skip)
}

// RecordMemoryFloorOverride annotates the PlatformConfig with the RAM that
// Longhorn was installed on below the floor, so the override stays visible.
func RecordMemoryFloorOverride(client CommandRunner, ramMB int) error {
	value := fmt.Sprintf("ram=%dMiB floor=%dMiB at=%s", ramMB, LonghornMinRAMMB, time.Now().UTC().Format(time.RFC3339))
	if _, err := client.Run(fmt.Sprintf("kubectl annotate --overwrite platformconfig platform %s=%s", MemoryFloorOverrideAnnotation, shellQuote(value))); err != nil {
		return fmt.Errorf("recording the memory floor override: %w", err)
	}
	return nil
}
