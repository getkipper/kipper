package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// FunctionSpec defines the desired state of a serverless function.
type FunctionSpec struct {
	// Image is the container image for the function.
	// +optional
	Image string `json:"image,omitempty"`

	// Port is the container port the function listens on.
	// +kubebuilder:default=8080
	// +optional
	Port int32 `json:"port,omitempty"`

	// Runtime is the function runtime (node, python, go, custom).
	// +kubebuilder:validation:Enum=node;python;go;custom
	// +kubebuilder:default=node
	// +optional
	Runtime string `json:"runtime,omitempty"`

	// Source holds inline function code for the code editor.
	// +optional
	Source *FunctionSource `json:"source,omitempty"`

	// Resources configures CPU and memory for the function.
	// +optional
	Resources FunctionResources `json:"resources,omitempty"`

	// Env holds non-sensitive environment variables.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// ServiceBindings lists Service CRs whose credentials should be
	// injected into the function as prefixed env vars. The keys are
	// service-type-specific (e.g. DB_HOST/DB_PASSWORD for databases,
	// S3_ENDPOINT/S3_ACCESS_KEY/S3_SECRET_KEY for MinIO). Same shape as
	// App.ServiceBindings; both reuse the ServiceBinding type defined in
	// app_types.go.
	// +optional
	ServiceBindings []ServiceBinding `json:"serviceBindings,omitempty"`

	// Volumes lists shared Volume CRs to mount into the function pod
	// (and into the CronJob pod for cron triggers). Reuses the
	// AppVolumeMount type from app_types.go — each entry names a
	// Volume CR and the container path it should mount at.
	// +optional
	Volumes []AppVolumeMount `json:"volumes,omitempty"`

	// Triggers defines what activates the function.
	// +optional
	Triggers []FunctionTrigger `json:"triggers,omitempty"`

	// NoSecurityHeaders disables the default security response headers.
	// +optional
	NoSecurityHeaders bool `json:"noSecurityHeaders,omitempty"`

	// CSPAllowlist adds external domains to the Content Security Policy.
	// +optional
	CSPAllowlist []string `json:"cspAllowlist,omitempty"`
}

// FunctionSource holds inline function source code.
type FunctionSource struct {
	// Code is the function source code.
	// +optional
	Code string `json:"code,omitempty"`

	// Handler is the entry point file name.
	// +optional
	Handler string `json:"handler,omitempty"`

	// Dependencies maps third-party package names to version specifiers.
	// For Node functions the controller emits this as a package.json
	// alongside the handler; for Python it becomes a requirements.txt.
	// The runtime image installs them at container startup. Use exact
	// versions to avoid lockfile drift across pod restarts.
	// +optional
	Dependencies map[string]string `json:"dependencies,omitempty"`
}

// FunctionResources configures CPU and memory for the function. Request
// and limit may be set independently; if only one is set, the other
// defaults to it.
type FunctionResources struct {
	// CPURequest is the CPU reserved on the node (e.g. "50m").
	// +optional
	CPURequest string `json:"cpuRequest,omitempty"`

	// CPULimit is the maximum CPU the container can use (e.g. "50m").
	// +optional
	CPULimit string `json:"cpuLimit,omitempty"`

	// MemoryRequest is the memory reserved on the node (e.g. "64Mi").
	// +optional
	MemoryRequest string `json:"memoryRequest,omitempty"`

	// MemoryLimit is the memory cap before the container is OOMKilled (e.g. "64Mi").
	// +optional
	MemoryLimit string `json:"memoryLimit,omitempty"`
}

// FunctionTrigger defines an event source that activates the function.
type FunctionTrigger struct {
	// Type is the trigger type (http, cron, minio, postgres, redis, mysql, rabbitmq).
	// +kubebuilder:validation:Enum=http;cron;minio;postgres;redis;mysql;rabbitmq
	Type string `json:"type"`

	// Schedule is the cron expression for cron triggers.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Config holds trigger-specific configuration.
	// +optional
	Config map[string]string `json:"config,omitempty"`
}

// FunctionStatus defines the observed state of the function.
type FunctionStatus struct {
	// Phase represents the current lifecycle phase.
	// +kubebuilder:validation:Enum=Idle;Scaling;Running;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Replicas is the current number of running replicas (0 when idle).
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// Endpoint is the public URL for the function.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// PublishedEnv is the environment generation the last successful pass
	// published, which is the object a pod started now would read.
	//
	// The console compares it with the generation the pod template names to
	// answer whether a restart would apply anything. Two names settle that; the
	// timestamp comparison it replaced had to reason about when a stamp was
	// written relative to when a kubelet started a container.
	// +optional
	PublishedEnv string `json:"publishedEnv,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Runtime",type=string,JSONPath=`.spec.runtime`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Replicas",type=integer,JSONPath=`.status.replicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Function is the Schema for the functions API.
type Function struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FunctionSpec   `json:"spec,omitempty"`
	Status FunctionStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// FunctionList contains a list of Function.
type FunctionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Function `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Function{}, &FunctionList{})
}
