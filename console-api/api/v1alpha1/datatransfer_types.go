package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DataTransferSpec defines one unit of migration data movement: a shared
// volume, an in-cluster object-storage bucket, or a spooled database dump,
// streamed from this cluster to a target cluster's import endpoint.
type DataTransferSpec struct {
	// SessionID ties the transfer to a migration session.
	SessionID string `json:"sessionID"`

	// Kind selects the data source shape.
	// +kubebuilder:validation:Enum=volume;servicePVC;dbDump
	Kind string `json:"kind"`

	// Source describes where the data comes from on this cluster.
	Source DataTransferEndpoint `json:"source"`

	// Target describes where the data lands on the target cluster.
	Target DataTransferEndpoint `json:"target"`

	// TargetBaseURL is the target cluster's ingest base URL for this
	// transfer (the console-api host with the transfer path prefix).
	TargetBaseURL string `json:"targetBaseURL"`

	// SourceReplicas records the source service's replica count before a
	// servicePVC transfer paused it, so recovery restores the same size.
	// +optional
	SourceReplicas int32 `json:"sourceReplicas,omitempty"`

	// ChunkSizeBytes is the transfer chunk size.
	// +kubebuilder:default=134217728
	// +optional
	ChunkSizeBytes int64 `json:"chunkSizeBytes,omitempty"`

	// Concurrency is the number of parallel chunk uploads.
	// +kubebuilder:default=4
	// +optional
	Concurrency int32 `json:"concurrency,omitempty"`

	// MaxAttempts bounds automatic retries of the whole transfer.
	// +kubebuilder:default=3
	// +optional
	MaxAttempts int32 `json:"maxAttempts,omitempty"`
}

// DataTransferEndpoint identifies one side of a transfer.
type DataTransferEndpoint struct {
	// Volume is the Volume CR name (kind=volume).
	// +optional
	Volume string `json:"volume,omitempty"`

	// Service is the stateful Service CR name (kind=servicePVC, kind=dbDump).
	// +optional
	Service string `json:"service,omitempty"`
}

// DataTransferStatus is the observed transfer state, updated by the
// reconciler from the target ingest's progress endpoint. Mover retries
// within a run resume from the completed-chunk state held on the target;
// a console-api restart fails the owning migration session, whose sweep
// then cleans this CR up and restarts any paused service.
type DataTransferStatus struct {
	// Phase is the transfer lifecycle phase.
	// +kubebuilder:validation:Enum=Pending;Transferring;Verifying;Completed;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// ManifestDigest is the sha256 of the transfer manifest.
	// +optional
	ManifestDigest string `json:"manifestDigest,omitempty"`

	// TotalBytes is the manifest's total payload size.
	// +optional
	TotalBytes int64 `json:"totalBytes,omitempty"`

	// BytesDone counts bytes acknowledged by the target.
	// +optional
	BytesDone int64 `json:"bytesDone,omitempty"`

	// BytesResumed counts bytes skipped on retry because the target
	// already held their chunks.
	// +optional
	BytesResumed int64 `json:"bytesResumed,omitempty"`

	// TotalChunks and CompletedChunks track chunk progress.
	// +optional
	TotalChunks int64 `json:"totalChunks,omitempty"`
	// +optional
	CompletedChunks int64 `json:"completedChunks,omitempty"`

	// Attempt is the current attempt number, starting at 1.
	// +optional
	Attempt int32 `json:"attempt,omitempty"`

	// FilesVerified and VerifyMismatches summarise the finalize report.
	// +optional
	FilesVerified int64 `json:"filesVerified,omitempty"`
	// +optional
	VerifyMismatches int64 `json:"verifyMismatches,omitempty"`

	// LastSyncedAt records when the target last matched the source
	// manifest. The future syncing phase gates cutover on this.
	// +optional
	LastSyncedAt *metav1.Time `json:"lastSyncedAt,omitempty"`

	// LastError is the most recent failure, cleared on progress.
	// +optional
	LastError string `json:"lastError,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Done",type=integer,JSONPath=`.status.completedChunks`
// +kubebuilder:printcolumn:name="Chunks",type=integer,JSONPath=`.status.totalChunks`
// +kubebuilder:printcolumn:name="Attempt",type=integer,JSONPath=`.status.attempt`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DataTransfer is the Schema for the datatransfers API.
type DataTransfer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   DataTransferSpec   `json:"spec,omitempty"`
	Status DataTransferStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// DataTransferList contains a list of DataTransfer.
type DataTransferList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []DataTransfer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&DataTransfer{}, &DataTransferList{})
}
