package platform

// Storage modes are recorded in PlatformConfig.spec.storage and become immutable
// once set, as enforced by the PlatformConfig schema.
const (
	StorageLocal    = "local"
	StorageLonghorn = "longhorn"
)

// StorageMode returns the recorded value unchanged, or StorageLonghorn when
// empty for compatibility with clusters installed before storage was recorded.
func StorageMode(recorded string) string {
	if recorded == "" {
		return StorageLonghorn
	}
	return recorded
}

// IsStorageMode reports whether mode is StorageLocal or StorageLonghorn.
// An empty value is invalid even though StorageMode defaults it to Longhorn.
func IsStorageMode(mode string) bool {
	return mode == StorageLocal || mode == StorageLonghorn
}
