package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceTuningSpec names the workload a ResourceTuning belongs to.
type ResourceTuningSpec struct {
	// Kind is the kind of the owning workload.
	// +kubebuilder:validation:Enum=App;Service;Function
	Kind string `json:"kind"`

	// Name is the owning workload's name in the same namespace.
	Name string `json:"name"`
}

// TunedResources is a recommended request and limit for CPU and memory, as
// Kubernetes quantity strings. An empty field means no recommendation.
type TunedResources struct {
	// +optional
	CPURequest string `json:"cpuRequest,omitempty"`
	// +optional
	CPULimit string `json:"cpuLimit,omitempty"`
	// +optional
	MemoryRequest string `json:"memoryRequest,omitempty"`
	// +optional
	MemoryLimit string `json:"memoryLimit,omitempty"`
}

// OOMRecord identifies the last OOM kill the auto-sizer acted on.
type OOMRecord struct {
	// Identity combines the container name and OOM finish time, preserving
	// nanoseconds for deduplication across restarts.
	Identity string `json:"identity"`

	// At is when the container was killed.
	At metav1.Time `json:"at"`

	// Alerted marks an OOM handled by an increase or a stored alert.
	// An increase is recorded before its alert is persisted.
	// +optional
	Alerted bool `json:"alerted,omitempty"`
}

// ResourceTuningStatus is the auto-sizer's state for one workload. Only the
// auto-sizer writes it; the owning reconciler reads the recommendation and
// applies it inside the user's bounds.
type ResourceTuningStatus struct {
	// Recommendation is what the auto-sizer wants the container to get,
	// already kept inside the user's bounds.
	// +optional
	Recommendation TunedResources `json:"recommendation,omitempty"`

	// RecommendedAt is when the recommendation last changed.
	// +optional
	RecommendedAt *metav1.Time `json:"recommendedAt,omitempty"`

	// DecreaseBlockedUntil stops CPU and memory decreases until this time,
	// set after an OOM kill raised memory.
	// +optional
	DecreaseBlockedUntil *metav1.Time `json:"decreaseBlockedUntil,omitempty"`

	// LastOOM is the last OOM kill the auto-sizer acted on.
	// +optional
	LastOOM *OOMRecord `json:"lastOOM,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Workload",type=string,JSONPath=`.spec.name`
// +kubebuilder:printcolumn:name="Memory",type=string,JSONPath=`.status.recommendation.memoryRequest`
// +kubebuilder:printcolumn:name="CPU",type=string,JSONPath=`.status.recommendation.cpuRequest`

// ResourceTuning holds the auto-sizer's recommendation, OOM record and
// cooldown for one App, Service or Function. It is owned by that workload
// and deleted with it.
type ResourceTuning struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourceTuningSpec   `json:"spec,omitempty"`
	Status ResourceTuningStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ResourceTuningList contains a list of ResourceTuning.
type ResourceTuningList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceTuning `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceTuning{}, &ResourceTuningList{})
}
