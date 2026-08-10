package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PlatformConfigSpec defines the desired state of system component sizing for
// the Kipper platform layer (monitoring, storage, ingress, identity, registry).
// A single cluster-scoped PlatformConfig named "platform" is the source of truth;
// the reconciler ignores any other names.
type PlatformConfigSpec struct {
	// Profile selects a sizing profile for system components. The installer
	// picks one at install time based on node memory; users can change it
	// later via the console or `kip platform profile set`.
	// +kubebuilder:validation:Enum=nano;small;medium;large;xlarge
	Profile string `json:"profile"`

	// Components is the per-component override list. Anything not listed
	// follows the active profile defaults; anything listed wins.
	// +optional
	// +listType=map
	// +listMapKey=name
	Components []ComponentOverride `json:"components,omitempty"`

	// Telemetry toggles cluster-wide telemetry collection that feeds the
	// auto-bump growth decisions. Opt-in; default is the zero value
	// (everything off).
	// +optional
	Telemetry *TelemetrySpec `json:"telemetry,omitempty"`
}

// TelemetrySpec gates per-feature telemetry collection. Each flag is
// opt-in so an air-gapped or privacy-sensitive cluster ships with no
// background logging.
type TelemetrySpec struct {
	// RecordResourceAdjustments enables the cluster log of slider Apply
	// events. When true, every successful resize via the console or CLI
	// writes a cluster-scoped ResourceAdjustment CR. Defaults to false.
	// +optional
	RecordResourceAdjustments bool `json:"recordResourceAdjustments,omitempty"`
}

// ComponentOverride captures user intent for a single system component.
// Empty fields fall back to the profile default; explicit values override.
type ComponentOverride struct {
	// Name identifies the component. Expected values: "prometheus", "loki",
	// "grafana", "longhorn", "dex", "zot", "console", "console-api",
	// "gateway", "traefik", "cert-manager". The reconciler validates the
	// name against its known list and surfaces unknown names as a condition.
	Name string `json:"name"`

	// Enabled toggles the component on or off, overriding the profile.
	// Nil means "use profile default", true forces on, false forces off.
	// The pointer matters so a profile change does not silently flip a
	// user's explicit disable back to enabled.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// MemoryLimit overrides the profile's memory limit (e.g. "2Gi").
	// +optional
	MemoryLimit string `json:"memoryLimit,omitempty"`

	// CPULimit overrides the profile's CPU limit (e.g. "1000m").
	// +optional
	CPULimit string `json:"cpuLimit,omitempty"`
}

// PlatformConfigStatus reflects the observed state of system components and
// any auto-bumps the controller has performed.
type PlatformConfigStatus struct {
	// Profile mirrors spec.profile so it is visible in `kubectl get`.
	// +optional
	Profile string `json:"profile,omitempty"`

	// Components lists observed state per component.
	// +optional
	// +listType=map
	// +listMapKey=name
	Components []ComponentStatus `json:"components,omitempty"`

	// Conditions represents the latest available observations of the
	// platform's state (e.g. Ready, Degraded, AtCeiling).
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ComponentStatus is the observed state of a single system component.
type ComponentStatus struct {
	// Name identifies the component.
	Name string `json:"name"`

	// Phase summarises the current state.
	// +kubebuilder:validation:Enum=Running;Pending;CrashLoopBackOff;OOMKilled;Disabled;Unknown
	// +optional
	Phase string `json:"phase,omitempty"`

	// CurrentMemoryLimit is the memory limit currently applied to the
	// workload, after profile defaults, user overrides, and auto-bumps.
	// +optional
	CurrentMemoryLimit string `json:"currentMemoryLimit,omitempty"`

	// CurrentCPULimit is the CPU limit currently applied.
	// +optional
	CurrentCPULimit string `json:"currentCpuLimit,omitempty"`

	// RestartCount7d is the number of container restarts in the trailing
	// 7-day window.
	// +optional
	RestartCount7d int32 `json:"restartCount7d,omitempty"`

	// LastBumpAt records the timestamp of the most recent auto-bump.
	// +optional
	LastBumpAt *metav1.Time `json:"lastBumpAt,omitempty"`

	// LastBumpFrom is the memory limit before the most recent auto-bump.
	// +optional
	LastBumpFrom string `json:"lastBumpFrom,omitempty"`

	// LastBumpTo is the memory limit after the most recent auto-bump.
	// +optional
	LastBumpTo string `json:"lastBumpTo,omitempty"`

	// LastBumpReason explains why the controller bumped this component.
	// +optional
	LastBumpReason string `json:"lastBumpReason,omitempty"`

	// AtCeiling is true when the auto-bump has reached the per-component
	// ceiling and cannot grow further without a manual decision.
	// +optional
	AtCeiling bool `json:"atCeiling,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profile`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// PlatformConfig is the Schema for the platform configuration API. It controls
// system component sizing and per-component enable/disable for Kipper's
// platform layer.
type PlatformConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PlatformConfigSpec   `json:"spec,omitempty"`
	Status PlatformConfigStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// PlatformConfigList contains a list of PlatformConfig.
type PlatformConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PlatformConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PlatformConfig{}, &PlatformConfigList{})
}
