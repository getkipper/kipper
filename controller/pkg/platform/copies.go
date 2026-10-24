package platform

// LonghornReplicatedClass keeps two copies of a volume on nodes tagged
// ReplicaStorageTag, with data locality and auto-balance off.
const LonghornReplicatedClass = "longhorn-replicated"

// ReplicaStorageTag selects Longhorn nodes eligible to hold copies.
// Kipper tags the server during storage setup and workers after the disk check.
const ReplicaStorageTag = "kipper-replica"

// MaxCopies is the most copies any object may ask for.
const MaxCopies = 2

// RWXCopiesQualified controls whether new shared (ReadWriteMany) claims may
// request two copies. Enable it in a release only after Longhorn's share-manager
// failover passes Kipper's live test with that release's Longhorn version.
const RWXCopiesQualified = false

// WantsTwoCopies reports whether copies is at least two. A nil value returns false.
func WantsTwoCopies(copies *int32) bool {
	return copies != nil && *copies >= 2
}

// ClaimClassForCopies selects the storage class for a new data or shared claim.
// On Longhorn, values of at least two select LonghornReplicatedClass. Other
// values and local storage use SingleWriterClass. The class binds at creation.
// Callers enforce the allowed copy counts and shared-volume restrictions.
func ClaimClassForCopies(mode string, copies *int32) string {
	if WantsTwoCopies(copies) && StorageMode(mode) == StorageLonghorn {
		return LonghornReplicatedClass
	}
	return SingleWriterClass(mode)
}

// IsLonghornClass reports whether class is one of the Longhorn classes Kipper
// creates claims in.
func IsLonghornClass(class string) bool {
	switch class {
	case LonghornSharedClass, LonghornSingleClass, LonghornReplicatedClass:
		return true
	}
	return false
}

// OfferedCopies returns the most copies the admin offers, from
// PlatformConfig.spec.maxCopies. Unset, or any value outside 1 to MaxCopies,
// offers one.
func OfferedCopies(maxCopies *int32) int32 {
	if maxCopies == nil || *maxCopies < 1 || *maxCopies > MaxCopies {
		return 1
	}
	return *maxCopies
}
