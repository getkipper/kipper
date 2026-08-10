package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// VolumeSpec defines the desired state of a shared volume.
type VolumeSpec struct {
	// Size is the persistent volume claim size (e.g. "5Gi").
	// +kubebuilder:default="1Gi"
	Size string `json:"size"`

	// Mounts lists apps and their mount paths for this volume.
	// +optional
	Mounts []VolumeMountTarget `json:"mounts,omitempty"`
}

// VolumeMountTarget defines which app mounts this volume and where.
type VolumeMountTarget struct {
	// App is the App CR name to mount the volume into.
	App string `json:"app"`

	// MountPath is the path inside the container.
	MountPath string `json:"mountPath"`
}

// VolumeStatus defines the observed state of the volume.
type VolumeStatus struct {
	// Phase represents the current lifecycle phase.
	// +kubebuilder:validation:Enum=Pending;Bound;Released
	// +optional
	Phase string `json:"phase,omitempty"`

	// ActualSize is the provisioned volume size.
	// +optional
	ActualSize string `json:"actualSize,omitempty"`

	// MountedApps lists the apps that currently have this volume mounted.
	// +optional
	MountedApps []string `json:"mountedApps,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.spec.size`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Mounts",type=integer,JSONPath=`.status.mountedApps`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Volume is the Schema for the volumes API.
type Volume struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   VolumeSpec   `json:"spec,omitempty"`
	Status VolumeStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// VolumeList contains a list of Volume.
type VolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Volume `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Volume{}, &VolumeList{})
}
