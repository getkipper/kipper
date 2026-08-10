package manifest

// Manifest represents a kipper.yaml file that declares the desired state
// of apps, services, volumes, jobs, and functions for a project environment.
type Manifest struct {
	Project      string              `yaml:"project"`
	Environment  string              `yaml:"environment,omitempty"`
	Environments []string            `yaml:"environments,omitempty"`
	DisplayName  string              `yaml:"displayName,omitempty"`
	Apps         map[string]AppSpec  `yaml:"apps,omitempty"`
	Services     map[string]SvcSpec  `yaml:"services,omitempty"`
	Volumes      map[string]VolSpec  `yaml:"volumes,omitempty"`
	Jobs         map[string]JobSpec  `yaml:"jobs,omitempty"`
	Functions    map[string]FuncSpec `yaml:"functions,omitempty"`
}

// AppSpec defines an application in the manifest.
type AppSpec struct {
	Image           string            `yaml:"image,omitempty"`
	Port            int32             `yaml:"port"`
	Replicas        int32             `yaml:"replicas,omitempty"`
	Env             map[string]string `yaml:"env,omitempty"`
	SecretRefs      []string          `yaml:"secretRefs,omitempty"`
	Route           *RouteSpec        `yaml:"route,omitempty"`
	Resources       *ResourceSpec     `yaml:"resources,omitempty"`
	ServiceBindings []BindingSpec     `yaml:"serviceBindings,omitempty"`
	Volumes         []VolumeMountSpec `yaml:"volumes,omitempty"`
	Autoscale       *AutoscaleSpec    `yaml:"autoscale,omitempty"`
	Git             *GitSpec          `yaml:"git,omitempty"`
}

// RouteSpec configures ingress routing.
type RouteSpec struct {
	Host              string         `yaml:"host,omitempty"`
	RedirectFrom      []string       `yaml:"redirectFrom,omitempty"`
	Path              string         `yaml:"path,omitempty"`
	Group             string         `yaml:"group,omitempty"`
	NoSecurityHeaders bool           `yaml:"noSecurityHeaders,omitempty"`
	NoInstanceHeader  bool           `yaml:"noInstanceHeader,omitempty"`
	RateLimit         int            `yaml:"rateLimit,omitempty"`
	CSPAllowlist      []string       `yaml:"cspAllowlist,omitempty"`
	Redirects         []RedirectSpec `yaml:"redirects,omitempty"`
	BasicAuth         bool           `yaml:"basicAuth,omitempty"`
	RequireAPIKey     bool           `yaml:"requireApiKey,omitempty"`
}

// RedirectSpec defines a URL redirect rule on a route.
type RedirectSpec struct {
	Source    string `yaml:"source"`
	Target    string `yaml:"target"`
	Permanent bool   `yaml:"permanent,omitempty"`
}

// VolumeMountSpec mounts a Volume CR into an App or Function pod. Same
// shape as the controller's AppVolumeMount type.
type VolumeMountSpec struct {
	Name      string `yaml:"name"`
	MountPath string `yaml:"mountPath"`
}

// ResourceSpec configures CPU and memory. Request and limit may be set
// independently to enable burstable workloads. If only one side is set,
// the other defaults to it (preserves Guaranteed QoS).
type ResourceSpec struct {
	Profile       string `yaml:"profile,omitempty"`
	CPURequest    string `yaml:"cpuRequest,omitempty"`
	CPULimit      string `yaml:"cpuLimit,omitempty"`
	MemoryRequest string `yaml:"memoryRequest,omitempty"`
	MemoryLimit   string `yaml:"memoryLimit,omitempty"`
}

// BindingSpec defines a service binding.
type BindingSpec struct {
	Name     string `yaml:"name"`
	Prefix   string `yaml:"prefix,omitempty"`
	Database string `yaml:"database,omitempty"`
}

// AutoscaleSpec configures horizontal pod autoscaling.
type AutoscaleSpec struct {
	Enabled      bool  `yaml:"enabled"`
	MinReplicas  int32 `yaml:"minReplicas,omitempty"`
	MaxReplicas  int32 `yaml:"maxReplicas,omitempty"`
	CPUTarget    int32 `yaml:"cpuTarget,omitempty"`
	MemoryTarget int32 `yaml:"memoryTarget,omitempty"`
}

// GitSpec configures source-based deployment.
type GitSpec struct {
	URL    string `yaml:"url"`
	Branch string `yaml:"branch,omitempty"`
	// CredentialsSecret names the git credential the build uses: the app's own
	// "<app>-git-credentials" Secret, or a shared credential configured in
	// kipper-system (used only for an allow-listed project and matching host).
	CredentialsSecret string            `yaml:"credentialsSecret,omitempty"`
	DockerfilePath    string            `yaml:"dockerfilePath,omitempty"`
	Context           string            `yaml:"context,omitempty"`
	BuildArgs         map[string]string `yaml:"buildArgs,omitempty"`
	BuildResources    *BuildResources   `yaml:"buildResources,omitempty"`
}

// BuildResources overrides the in-cluster build container's limits.
type BuildResources struct {
	Memory string `yaml:"memory,omitempty"`
	CPU    string `yaml:"cpu,omitempty"`
}

// SvcSpec defines a stateful service in the manifest. Resources carries the
// tuned request/limit values so applying an exported manifest cannot revert
// a service to catalog defaults; Profile has no meaning for services and
// stays empty.
type SvcSpec struct {
	Type      string        `yaml:"type"`
	Version   string        `yaml:"version,omitempty"`
	Storage   string        `yaml:"storage,omitempty"`
	Resources *ResourceSpec `yaml:"resources,omitempty"`
}

// VolSpec defines a shared volume in the manifest.
type VolSpec struct {
	Size   string      `yaml:"size"`
	Mounts []MountSpec `yaml:"mounts,omitempty"`
}

// MountSpec defines where a volume is mounted.
type MountSpec struct {
	App       string `yaml:"app"`
	MountPath string `yaml:"mountPath"`
}

// JobSpec defines a scheduled or one-off job in the manifest.
type JobSpec struct {
	Image    string            `yaml:"image"`
	Schedule string            `yaml:"schedule,omitempty"`
	Command  []string          `yaml:"command,omitempty"`
	Env      map[string]string `yaml:"env,omitempty"`
}

// FuncSpec defines a serverless function in the manifest.
type FuncSpec struct {
	Image             string            `yaml:"image,omitempty"`
	Port              int32             `yaml:"port,omitempty"`
	Runtime           string            `yaml:"runtime,omitempty"`
	Source            *FuncSourceSpec   `yaml:"source,omitempty"`
	Env               map[string]string `yaml:"env,omitempty"`
	Resources         *ResourceSpec     `yaml:"resources,omitempty"`
	ServiceBindings   []BindingSpec     `yaml:"serviceBindings,omitempty"`
	Volumes           []VolumeMountSpec `yaml:"volumes,omitempty"`
	Triggers          []TriggerSpec     `yaml:"triggers,omitempty"`
	NoSecurityHeaders bool              `yaml:"noSecurityHeaders,omitempty"`
	CSPAllowlist      []string          `yaml:"cspAllowlist,omitempty"`
}

// FuncSourceSpec holds inline function source uploaded from the console
// code editor or referenced from the CR's spec.source block. Restored
// verbatim on a round-trip — a Function whose `source.code` is missing
// runs an empty body, which we hit on acme-tools migration when the
// pre-rename exporter dropped this block silently.
type FuncSourceSpec struct {
	Code         string            `yaml:"code,omitempty"`
	Handler      string            `yaml:"handler,omitempty"`
	Dependencies map[string]string `yaml:"dependencies,omitempty"`
}

// TriggerSpec defines what triggers a function.
type TriggerSpec struct {
	Type     string            `yaml:"type"`
	Schedule string            `yaml:"schedule,omitempty"`
	Config   map[string]string `yaml:"config,omitempty"`
}
