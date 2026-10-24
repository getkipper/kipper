package v1alpha1

// ConditionStorageCopies reports whether a claim holds the copies its object
// asks for. Its reasons are the StorageCopies* constants.
const ConditionStorageCopies = "StorageCopies"

// Reasons of the StorageCopies condition.
const (
	// StorageCopiesHealthy: every wanted copy exists and is healthy. A local
	// storage claim is always reported as one healthy copy.
	StorageCopiesHealthy = "Healthy"
	// StorageCopiesDegraded: fewer healthy copies than wanted.
	StorageCopiesDegraded = "Degraded"
	// StorageCopiesNotSet: copies is unset, so Kipper only reports the live copies.
	StorageCopiesNotSet = "NotSet"
	// StorageCopiesConversionPending: copies differs from the existing claim's
	// live count, and nothing converts an existing claim yet.
	StorageCopiesConversionPending = "ConversionPending"
	// StorageCopiesNotEnoughReplicaNodes: copies 2 was asked for at creation with
	// fewer than two usable storage nodes, so no claim was created.
	StorageCopiesNotEnoughReplicaNodes = "NotEnoughReplicaNodes"
	// StorageCopiesRWXNotQualified: two copies were requested for a shared
	// (ReadWriteMany) claim, which this release does not provision. An existing
	// claim keeps its copies.
	StorageCopiesRWXNotQualified = "RWXNotQualified"
	// StorageCopiesLocalStorage: copies 2 needs Longhorn storage.
	StorageCopiesLocalStorage = "LocalStorage"
	// StorageCopiesPending: the copies cannot be read yet, for example because
	// the claim is unbound or its Longhorn volume is missing.
	StorageCopiesPending = "Pending"
	// StorageCopiesUnknown: fewer healthy copies than wanted, and the nodes of
	// some copies could not be read.
	StorageCopiesUnknown    = "Unknown"
	StorageCopiesConverting = "Converting"
)

// StorageCopiesStatus reports a claim's configured replica count and healthy
// replicas. Nodes contains healthy replica holders sorted by name. Local-path
// claims are reported as one healthy copy without a node list.
type StorageCopiesStatus struct {
	// Class is the claim's storage class.
	// +optional
	Class string `json:"class,omitempty"`

	// Copies is the replica count configured on the Longhorn volume, or 1 for
	// local storage.
	// +optional
	Copies int32 `json:"copies,omitempty"`

	// HealthyCopies is the number of Ready, unfenced nodes holding a current
	// copy. Local storage always reports 1.
	// +optional
	HealthyCopies int32 `json:"healthyCopies,omitempty"`

	// Nodes lists those nodes, sorted by name. It is empty for local storage.
	// +optional
	Nodes []string `json:"nodes,omitempty"`
}
