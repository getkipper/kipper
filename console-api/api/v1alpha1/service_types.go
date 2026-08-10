package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ServiceSpec defines the desired state of a stateful service.
type ServiceSpec struct {
	// Type is the service engine (postgres, mysql, redis, mongodb, rabbitmq, opensearch, minio, mailhog).
	// +kubebuilder:validation:Enum=postgres;mysql;redis;mongodb;rabbitmq;opensearch;minio;mailhog
	Type string `json:"type"`

	// Version is the image tag for the service engine.
	// +optional
	Version string `json:"version,omitempty"`

	// Storage is the persistent volume size (e.g. "5Gi", "10Gi").
	// +kubebuilder:default="1Gi"
	// +optional
	Storage string `json:"storage,omitempty"`

	// Resources configures CPU and memory for the service.
	// +optional
	Resources ServiceResources `json:"resources,omitempty"`

	// Bindings lists App CR names that should receive connection credentials.
	// +optional
	Bindings []string `json:"bindings,omitempty"`
}

// ServiceResources configures CPU and memory for the service. Request
// and limit may be set independently; if only one is set, the other
// defaults to it.
type ServiceResources struct {
	// CPURequest is the CPU reserved on the node (e.g. "100m").
	// +optional
	CPURequest string `json:"cpuRequest,omitempty"`

	// CPULimit is the maximum CPU the container can use (e.g. "100m").
	// +optional
	CPULimit string `json:"cpuLimit,omitempty"`

	// MemoryRequest is the memory reserved on the node (e.g. "256Mi").
	// +optional
	MemoryRequest string `json:"memoryRequest,omitempty"`

	// MemoryLimit is the memory cap before the container is OOMKilled (e.g. "256Mi").
	// +optional
	MemoryLimit string `json:"memoryLimit,omitempty"`
}

// ServiceStatus defines the observed state of the service.
type ServiceStatus struct {
	// Phase represents the current lifecycle phase.
	// +kubebuilder:validation:Enum=Pending;Running;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Host is the in-cluster DNS name for the service.
	// +optional
	Host string `json:"host,omitempty"`

	// Port is the service port.
	// +optional
	Port int32 `json:"port,omitempty"`

	// CredentialsSecret is the name of the Secret containing connection details.
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Storage",type=string,JSONPath=`.spec.storage`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Service is the Schema for the services API.
type Service struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ServiceSpec   `json:"spec,omitempty"`
	Status ServiceStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServiceList contains a list of Service.
type ServiceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Service `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Service{}, &ServiceList{})
}
