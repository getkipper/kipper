package platform

// The keys below mark a service whose data volume `kip service storage
// longhorn` is moving from local-path onto Longhorn. Admission policies,
// the service controller, console-api and kip all read them.
const (
	// StorageMoveAnnotation marks the Service CR and its StatefulSet while the
	// move runs. Its value names the move record in kipper-system.
	StorageMoveAnnotation = "kipper.run/storage-move"
	// StorageMoveReplicasAnnotation on the StatefulSet holds the replica count
	// the move restores once it finishes or rolls back.
	StorageMoveReplicasAnnotation = "kipper.run/storage-move-replicas"
	// StorageMoveNamespaceLabel, set to "true", puts a namespace under the
	// admission policy that keeps pods off the claims being moved.
	StorageMoveNamespaceLabel = "kipper.run/storage-move"
	// StorageMoveClaimsAnnotation on the namespace lists the claims being
	// moved, comma-separated. No pod may mount them except the copy pod.
	StorageMoveClaimsAnnotation = "kipper.run/storage-move-claims"
	// StorageMoveCopyPodPrefix starts the name of the only pod admission lets
	// mount a claim under move, and only when an admin creates it.
	StorageMoveCopyPodPrefix = "kipper-move-copy-"
	// StorageMovePodsPolicy is the admission policy that keeps pods off the
	// claims under move. A refused dry-run names it, which proves the policy
	// sees the namespace's label and annotation.
	StorageMovePodsPolicy = "kipper-storage-move-pods"

	// RetainedLocalCopyLabel, set to "true", marks a local-path volume kept
	// during and after a move to Longhorn, for rollback before commit or later pruning.
	RetainedLocalCopyLabel = "kipper.run/retained-local-copy"
	// RetainedLocalCopyStateAnnotation is "moving" while the move runs and
	// "moved" once it committed. Only a moved copy can be pruned.
	RetainedLocalCopyStateAnnotation = "kipper.run/retained-local-copy-state"
	// RetainedLocalCopyServiceAnnotation names the service as namespace/name.
	RetainedLocalCopyServiceAnnotation = "kipper.run/retained-local-copy-service"
	// RetainedLocalCopyMovedAtAnnotation holds the RFC 3339 time the committed
	// move marked its local copy as moved.
	RetainedLocalCopyMovedAtAnnotation = "kipper.run/retained-local-copy-moved-at"
	// RetainedLocalCopyUsedBytesAnnotation holds the larger of allocated and
	// apparent bytes, measured when the move started.
	RetainedLocalCopyUsedBytesAnnotation = "kipper.run/retained-local-copy-used-bytes"

	RetainedLocalCopyMoving = "moving"
	RetainedLocalCopyMoved  = "moved"
)
