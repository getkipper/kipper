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

// Storage classes Kipper creates volumes in.
const (
	LocalPathClass      = "local-path"
	LonghornSingleClass = "longhorn-single"
	LonghornSharedClass = "longhorn"
)

// SingleWriterClass selects a ReadWriteOnce storage class. It returns LocalPathClass
// for local storage and LonghornSingleClass for all other values, including an empty mode.
func SingleWriterClass(mode string) string {
	if StorageMode(mode) == StorageLocal {
		return LocalPathClass
	}
	return LonghornSingleClass
}

// SupportsSharedVolumes reports whether mode selects Longhorn for ReadWriteMany
// volumes. An empty mode defaults to Longhorn for compatibility with older clusters.
func SupportsSharedVolumes(mode string) bool {
	return StorageMode(mode) == StorageLonghorn
}

// SharedVolumesUnsupported is the shared-volume error message used by the CLI,
// API, console and admission policy.
const SharedVolumesUnsupported = "This cluster uses local storage. Shared volumes (ReadWriteMany) require Longhorn. " +
	"See Cluster Migration in the Kipper docs to move to a cluster with Longhorn storage."
