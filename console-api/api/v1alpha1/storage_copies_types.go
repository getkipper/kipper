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
	// StorageCopiesConversionPending: the claim's volume needs a conversion step
	// that has to wait before it can start or continue, for example while
	// another volume's conversion holds the cluster's one build slot.
	StorageCopiesConversionPending = "ConversionPending"
	// StorageCopiesConverting: Kipper is converting the claim's volume towards
	// the wanted copies, one step at a time. The copies cannot change meanwhile.
	StorageCopiesConverting = "Converting"
	// StorageCopiesBlocked: the conversion cannot go on, for example for lack of
	// room, a copy Kipper cannot confirm as current, or a step that keeps
	// failing. Every copy is kept, and setting the copies back stops the change.
	StorageCopiesBlocked = "Blocked"
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
	StorageCopiesUnknown = "Unknown"
)

// StorageCopiesStatus reports a claim's configured replica count and healthy
// replicas. Healthy copies are complete copies on Ready, unfenced nodes, as
// determined by platform.HealthyCopies.
// Nodes contains healthy replica holders sorted by name. Local-path claims
// are reported as one healthy copy without a node list.
type StorageCopiesStatus struct {
	// Class is the claim's storage class.
	// +optional
	Class string `json:"class,omitempty"`

	// Copies is the replica count configured on the Longhorn volume, or 1 for
	// local storage.
	// +optional
	Copies int32 `json:"copies,omitempty"`

	// HealthyCopies is the number of Ready, unfenced nodes holding a complete
	// copy: one rebuilt or serving again since its last failure, or one of a
	// volume created empty that was never attached. Local storage always
	// reports 1.
	// +optional
	HealthyCopies int32 `json:"healthyCopies,omitempty"`

	// Nodes lists those nodes, sorted by name. It is empty for local storage.
	// +optional
	Nodes []string `json:"nodes,omitempty"`
}
