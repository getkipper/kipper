package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// WorkloadNameSpec records which kind of workload holds a name.
type WorkloadNameSpec struct {
	// Kind is the workload kind holding the name.
	// +kubebuilder:validation:Enum=app;function;job
	Kind string `json:"kind"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:shortName=wln
// +kubebuilder:printcolumn:name="Held By",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// WorkloadName reserves a workload name within a namespace.
//
// An App, a Function and a Job cannot share a name: an App and a Function both
// reconcile a Deployment of that name and a Kubernetes object has one
// controller, and all three label their children with the workload's name, so a
// shared name leaves objects whose owner is decided by a single label. An App's
// Service selects `app=<name>`, which a Job's pods also carry, so a collision
// can route one workload's traffic to another's pods.
//
// Kubernetes indexes names per kind, so nothing in the API stops the second
// claim. This object is what does: it is named after the workload, so creating
// it succeeds exactly once and everybody else is told it already exists. That
// atomic create is the reservation. Reading the other kinds and then creating
// cannot achieve it, because two creates racing each other both read first.
//
// It is owned by the workload that holds it, so it is garbage-collected when
// that workload is deleted and the name becomes free again.
type WorkloadName struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec WorkloadNameSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// WorkloadNameList contains a list of WorkloadName.
type WorkloadNameList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []WorkloadName `json:"items"`
}

func init() {
	SchemeBuilder.Register(&WorkloadName{}, &WorkloadNameList{})
}
