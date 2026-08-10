package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// JobSpec defines the desired state of a scheduled or one-off job.
type JobSpec struct {
	// Image is the container image for the job.
	Image string `json:"image"`

	// Schedule is a cron expression. If empty, the job is one-off.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Command overrides the container entrypoint.
	// +optional
	Command []string `json:"command,omitempty"`

	// Resources configures CPU and memory for the job.
	// +optional
	Resources JobResources `json:"resources,omitempty"`

	// Env holds non-sensitive environment variables.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// SecretRefs lists the names of Secret keys to inject.
	// +optional
	SecretRefs []string `json:"secretRefs,omitempty"`

	// BackoffLimit is the number of retries before marking the job as failed.
	// +kubebuilder:default=3
	// +optional
	BackoffLimit *int32 `json:"backoffLimit,omitempty"`
}

// JobResources configures CPU and memory for the job. Request and limit
// may be set independently; if only one is set, the other defaults to it.
type JobResources struct {
	// CPURequest is the CPU reserved on the node (e.g. "100m").
	// +optional
	CPURequest string `json:"cpuRequest,omitempty"`

	// CPULimit is the maximum CPU the container can use (e.g. "100m").
	// +optional
	CPULimit string `json:"cpuLimit,omitempty"`

	// MemoryRequest is the memory reserved on the node (e.g. "128Mi").
	// +optional
	MemoryRequest string `json:"memoryRequest,omitempty"`

	// MemoryLimit is the memory cap before the container is OOMKilled (e.g. "128Mi").
	// +optional
	MemoryLimit string `json:"memoryLimit,omitempty"`
}

// JobStatus defines the observed state of the job.
type JobStatus struct {
	// Phase represents the current lifecycle phase.
	// +kubebuilder:validation:Enum=Scheduled;Running;Completed;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// LastRun is the timestamp of the most recent execution.
	// +optional
	LastRun *metav1.Time `json:"lastRun,omitempty"`

	// LastResult is the outcome of the most recent execution.
	// +kubebuilder:validation:Enum=Succeeded;Failed
	// +optional
	LastResult string `json:"lastResult,omitempty"`

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
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last Run",type=date,JSONPath=`.status.lastRun`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Job is the Schema for the jobs API.
type Job struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   JobSpec   `json:"spec,omitempty"`
	Status JobStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// JobList contains a list of Job.
type JobList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Job `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Job{}, &JobList{})
}
