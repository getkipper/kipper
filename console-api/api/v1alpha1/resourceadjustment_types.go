package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceAdjustmentSpec is one telemetry record: a user (or automation)
// changed a workload's CPU or memory budget. The cluster keeps a rolling
// log of these so future iterations of the platform-resources auto-bump
// have evidence to grow on. Records are append-only — every Apply writes
// a new ResourceAdjustment rather than updating an existing one.
type ResourceAdjustmentSpec struct {
	// Component is the workload identifier. For platform scope it's the
	// shared component name (prometheus, loki, grafana, ...); for app
	// and service scope it's the workload's own name.
	Component string `json:"component"`

	// Scope is the surface the adjustment came from.
	// +kubebuilder:validation:Enum=platform;app;service;function
	Scope string `json:"scope"`

	// Namespace identifies the project for app / service scope. Empty
	// for platform scope (the component name already pins the namespace).
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Kind is the dimension the user changed.
	// +kubebuilder:validation:Enum=memory;cpu
	Kind string `json:"kind"`

	// From is the previous limit as a Kubernetes quantity string
	// ("256Mi", "500m"). Empty when there was no prior value (the
	// workload was running on the profile default).
	// +optional
	From string `json:"from,omitempty"`

	// To is the limit just applied. Always populated.
	To string `json:"to"`

	// Reason is a free-form annotation. The console can fill this in
	// when a prompting state was visible at the time (e.g. "gauge was
	// 90%+"); manual edits via kubectl/CLI typically leave it empty.
	// +optional
	Reason string `json:"reason,omitempty"`

	// At is when the change was applied. Cluster authoritative time;
	// the controller's clock, not the client's.
	At metav1.Time `json:"at"`

	// AppliedBy is the user identifier from the request's JWT subject,
	// or "system" when the change came from an automated path. Empty
	// when authentication wasn't available (shouldn't happen in
	// practice).
	// +optional
	AppliedBy string `json:"appliedBy,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Scope",type=string,JSONPath=`.spec.scope`
// +kubebuilder:printcolumn:name="Component",type=string,JSONPath=`.spec.component`
// +kubebuilder:printcolumn:name="Kind",type=string,JSONPath=`.spec.kind`
// +kubebuilder:printcolumn:name="From",type=string,JSONPath=`.spec.from`
// +kubebuilder:printcolumn:name="To",type=string,JSONPath=`.spec.to`
// +kubebuilder:printcolumn:name="At",type=date,JSONPath=`.spec.at`

// ResourceAdjustment is one entry in the cluster's resource-change log.
// Recorded when PlatformConfig.spec.telemetry.recordResourceAdjustments
// is true. See the resource-controls plan for the broader purpose:
// surface patterns so future profile defaults and auto-bump targets can
// be raised based on evidence rather than guesswork.
type ResourceAdjustment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec ResourceAdjustmentSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// ResourceAdjustmentList contains a list of ResourceAdjustment.
type ResourceAdjustmentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourceAdjustment `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourceAdjustment{}, &ResourceAdjustmentList{})
}
