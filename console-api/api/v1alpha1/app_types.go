package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ConditionAPIKeyGateReady is the App status condition reporting whether the
// forwardAuth gate behind Route.RequireAPIKey is actually in place. It goes
// False when the gate's middlewares fail to reconcile, so the console can warn
// that a route with the toggle on may still be reachable without an API key.
const ConditionAPIKeyGateReady = "APIKeyGateReady"

// ConditionRouteReady is the App status condition reporting whether the app's
// route host is serving. It goes False when the host is refused — because the
// platform reserves it, or another workload already claimed it — so the console
// can tell the user their chosen hostname is unavailable rather than silently
// dropping the Ingress.
const ConditionRouteReady = "RouteReady"

// ConditionServiceBindingsReady is the App status condition reporting whether
// every declared service binding was injected. A binding whose credentials
// Secret carries no proof that Kipper created it is refused, and the app then
// starts without the env it expects — a database URL left as a literal
// ${DB_HOST} placeholder, for example. Without this the refusal is invisible:
// the pod simply crashes on config it never received.
const ConditionServiceBindingsReady = "ServiceBindingsReady"

// ConditionLinksOpen is the App status condition reporting whether every link
// this app declares actually carries traffic. A link opens nothing when the
// target project has not consented, has withdrawn consent, or the target app is
// gone or serves no port. Without this the refusal is invisible from the app's
// own side: the caller gets a connection refused, the reason is a line in the
// controller log, and the surface that recorded the link shows it as present.
const ConditionLinksOpen = "LinksOpen"

// ConditionEnvResolved reports what became of the ${NAME} references in a
// workload's environment variables. It is carried by Apps, Functions and Jobs
// alike, since all three render the same templates.
//
// It goes False when a reference names something nothing defines, when a
// variable set in env is overridden by a higher-precedence source and so never
// reaches the process, and when one template references another — resolution is
// a single pass, so the inner reference survives into the resolved value. All
// three are invisible otherwise: the workload starts and fails later on a
// connection string nobody can see, because the CR shows the template and only
// the pod holds what it resolved to.
const ConditionEnvResolved = "EnvResolved"

// ConditionEnvPublished reports whether the workload's environment reached the
// object its pods read.
//
// A pod's only env source is not optional, so an environment that fails to
// publish is not a degraded pod but one that never starts, and the reason
// appears as CreateContainerConfigError on the pod rather than anywhere an
// operator looks first. Most failures repair themselves: a generation deleted
// out of band is rewritten on the next pass, because its name is a digest of
// its own content. What does not repair itself is another object sitting at
// that name, and that is what this exists to say out loud.
const ConditionEnvPublished = "EnvPublished"

// ConditionChildrenAdopted says whether every child this workload owns was
// reconciled on the last pass.
//
// Any child that fails stops the pass, and everything after it is skipped: the
// ingress, the autoscaler and the status write all sit downstream. False says
// the pass stopped and carries the failing step's own message, which is where
// the distinction lives — a transient API error clears itself, while an object
// Kipper cannot establish as its own keeps failing until that object is renamed
// or removed.
//
// Refusing such an object is the right answer: adopting one somebody else
// created would make it die with the workload. Refusing it silently was not.
const ConditionChildrenAdopted = "ChildrenAdopted"

// DataUpdatedAtAnnotation is stamped on a workload's env and secrets Secrets
// whenever their data changes. A pod reads both via envFrom only at startup, so
// comparing this to a pod's start time tells the console whether a restart is
// pending. The reconciler stamps the env Secret (app-<app>-env for an App) and
// the secret handler stamps app-<app>-secrets.
const DataUpdatedAtAnnotation = "kipper.run/data-updated-at"

// AppSpec defines the desired state of a deployed application.
type AppSpec struct {
	// Image is the container image to deploy.
	Image string `json:"image"`

	// Port is the container port the application listens on.
	Port int32 `json:"port"`

	// Replicas is the desired number of pod replicas.
	// +kubebuilder:default=1
	// +optional
	Replicas *int32 `json:"replicas,omitempty"`

	// Resources configures CPU and memory allocation.
	// +optional
	Resources AppResources `json:"resources,omitempty"`

	// Env holds non-sensitive environment variables.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// SecretRefs lists the names of Secret keys to inject as environment variables.
	// +optional
	SecretRefs []string `json:"secretRefs,omitempty"`

	// Route configures external access to the application.
	// +optional
	Route *AppRoute `json:"route,omitempty"`

	// Autoscale configures horizontal pod autoscaling.
	// +optional
	Autoscale *AppAutoscale `json:"autoscale,omitempty"`

	// ServiceBindings lists Service CRs whose credentials should be injected.
	// +optional
	ServiceBindings []ServiceBinding `json:"serviceBindings,omitempty"`

	// Volumes lists volume mounts for this app.
	// +optional
	Volumes []AppVolumeMount `json:"volumes,omitempty"`

	// Git configures source-based deployment from a Git repository.
	// When set, builds are triggered by webhooks or manual rebuild commands.
	// +optional
	Git *AppGitSource `json:"git,omitempty"`

	// Links are apps this app reaches. Each one is this app's declaration that
	// it depends on another, and a link naming a namespace other than its own
	// is what opens the egress the workload policy otherwise denies.
	//
	// Bounded because an App can be written straight to the API server: an
	// unbounded list renders a policy the API server or the CNI may refuse, and
	// a rejected update leaves the previous policy in place — so an allowance
	// meant to be replaced would stay open.
	// +kubebuilder:validation:MaxItems=64
	// +optional
	Links []AppLink `json:"links,omitempty"`
}

// AppLink is one app this app reaches.
//
// The namespace is recorded rather than the project because namespace naming
// depends on the operator's cluster configuration, which the reconciler cannot
// see. The CLI resolves a project and environment to a namespace and writes it
// here, so what the policy acts on is unambiguous.
type AppLink struct {
	// App is the target app's name.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	App string `json:"app"`

	// Namespace is where the target app runs. A link within this app's own
	// namespace needs no egress allowance and produces no policy peer; it is
	// still recorded, so this list is the app's dependencies rather than a
	// by-product of one.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=63
	// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	Namespace string `json:"namespace"`
}

// AppResources configures CPU and memory for the application. Request and
// limit may be set independently to enable burstable workloads — typically
// JVM apps that need a high CPU ceiling for cold-start JIT but sip CPU at
// steady state. If only one side is set, the other defaults to it
// (preserving Guaranteed QoS).
type AppResources struct {
	// Profile selects a predefined resource configuration.
	// +kubebuilder:validation:Enum=lightweight;standard;compute-heavy;memory-heavy;jvm;custom
	// +kubebuilder:default=standard
	// +optional
	Profile string `json:"profile,omitempty"`

	// CPURequest is the CPU reserved on the node (e.g. "100m").
	// +optional
	CPURequest string `json:"cpuRequest,omitempty"`

	// CPULimit is the maximum CPU the container can use (e.g. "1000m").
	// +optional
	CPULimit string `json:"cpuLimit,omitempty"`

	// MemoryRequest is the memory reserved on the node (e.g. "128Mi").
	// +optional
	MemoryRequest string `json:"memoryRequest,omitempty"`

	// MemoryLimit is the memory cap before the container is OOMKilled (e.g. "2Gi").
	// +optional
	MemoryLimit string `json:"memoryLimit,omitempty"`
}

// AppRoute configures ingress routing for the application.
type AppRoute struct {
	// Host is the fully qualified domain for custom domain routing.
	// +optional
	Host string `json:"host,omitempty"`

	// RedirectFrom lists additional hostnames that permanently redirect
	// (301) to this route's canonical host, preserving path and query.
	// Each hostname is claimed for this project like the route host; a
	// hostname another project already serves is skipped and reported on
	// the RouteReady condition. Hostnames under kipper.run are not
	// supported.
	// +kubebuilder:validation:MaxItems=10
	// +kubebuilder:validation:items:MaxLength=253
	// +optional
	RedirectFrom []string `json:"redirectFrom,omitempty"`

	// Path is the URL path prefix for path-based routing.
	// +optional
	Path string `json:"path,omitempty"`

	// Group is the route group name for grouping multiple apps under one domain.
	// +optional
	Group string `json:"group,omitempty"`

	// NoSecurityHeaders disables the default security response headers.
	// +optional
	NoSecurityHeaders bool `json:"noSecurityHeaders,omitempty"`

	// NoInstanceHeader disables the X-Instance-ID response header sidecar.
	// When enabled (default), a lightweight reverse proxy sidecar is injected
	// into app pods that adds an X-Instance-ID header identifying which pod
	// served the request.
	// +optional
	NoInstanceHeader bool `json:"noInstanceHeader,omitempty"`

	// RateLimit sets the maximum requests per second (0 = cluster default).
	// +optional
	RateLimit int `json:"rateLimit,omitempty"`

	// RequireAPIKey gates this route behind API key authentication: only
	// requests carrying a valid X-API-Key for this app are served. When
	// the authz service is unavailable the route fails closed.
	// +optional
	RequireAPIKey bool `json:"requireApiKey,omitempty"`

	// CSPAllowlist adds external domains to the Content Security Policy.
	// These domains are added to style-src, font-src, script-src, and connect-src.
	// Example: ["fonts.googleapis.com", "cdn.example.com"]
	// +optional
	CSPAllowlist []string `json:"cspAllowlist,omitempty"`

	// Redirects defines URL redirect rules for this app's route.
	// Each rule creates a Traefik redirectRegex middleware.
	// +optional
	Redirects []RedirectRule `json:"redirects,omitempty"`

	// BasicAuth enables HTTP basic authentication on this route.
	// Credentials are stored in a Secret named {app}-basic-auth.
	// +optional
	BasicAuth bool `json:"basicAuth,omitempty"`
}

// MaxRedirectFromHosts caps route.redirectFrom, matching the MaxItems marker
// on AppRoute.RedirectFrom. Keep the two in sync.
const MaxRedirectFromHosts = 10

// RedirectRule defines a URL redirect from a source pattern to a target.
type RedirectRule struct {
	// Source is a regex pattern to match against the request URL.
	Source string `json:"source"`

	// Target is the destination URL or path. Supports regex group references ($1, $2).
	Target string `json:"target"`

	// Permanent uses 301 (true) instead of 302 (false).
	// +optional
	Permanent bool `json:"permanent,omitempty"`
}

// AppAutoscale configures horizontal pod autoscaling.
type AppAutoscale struct {
	// Enabled turns autoscaling on or off.
	Enabled bool `json:"enabled"`

	// MinReplicas is the minimum number of replicas.
	// +kubebuilder:default=1
	// +optional
	MinReplicas int32 `json:"minReplicas,omitempty"`

	// MaxReplicas is the maximum number of replicas.
	// +optional
	MaxReplicas int32 `json:"maxReplicas,omitempty"`

	// CPUTarget is the target CPU utilisation percentage.
	// +optional
	CPUTarget int32 `json:"cpuTarget,omitempty"`

	// MemoryTarget is the target memory utilisation percentage.
	// +optional
	MemoryTarget int32 `json:"memoryTarget,omitempty"`
}

// ServiceBinding defines a bound service with an optional env var prefix.
type ServiceBinding struct {
	// Name is the Service CR name.
	Name string `json:"name"`

	// Prefix is prepended to credential env var names. The keys are
	// service-type-specific: databases produce <PREFIX>HOST/PORT/USERNAME/
	// PASSWORD, while MinIO (S3) produces <PREFIX>ENDPOINT/ACCESS_KEY/
	// SECRET_KEY. If empty, defaults to a type-based prefix like DB_,
	// REDIS_, S3_, etc.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// Database is the per-app database name created within the service.
	// When set, a per-binding credentials secret is used with this database
	// name instead of the shared service credentials.
	// +optional
	Database string `json:"database,omitempty"`
}

// AppVolumeMount defines a volume mount for the app container.
type AppVolumeMount struct {
	// Name is the Volume CR name to mount.
	Name string `json:"name"`

	// MountPath is the path inside the container.
	MountPath string `json:"mountPath"`
}

// AppGitSource configures a Git repository as the deployment source.
type AppGitSource struct {
	// URL is the Git repository URL (HTTPS or SSH).
	URL string `json:"url"`

	// Branch is the branch to build from.
	// +kubebuilder:default="main"
	// +optional
	Branch string `json:"branch,omitempty"`

	// CredentialsSecret names the git credential the build uses. Either the
	// app's own per-app Secret, one generation per token and host, in the app's
	// namespace (a "token" key), or the name of a shared credential configured
	// in kipper-system. A shared credential is only used when the app's project
	// is on its allow-list and its host matches the git URL; any other name is
	// rejected at build time.
	// +optional
	CredentialsSecret string `json:"credentialsSecret,omitempty"`

	// DockerfilePath is the path to the Dockerfile relative to the repo root.
	// +kubebuilder:default="Dockerfile"
	// +optional
	DockerfilePath string `json:"dockerfilePath,omitempty"`

	// Context is the build context path relative to the repo root.
	// +kubebuilder:default="."
	// +optional
	Context string `json:"context,omitempty"`

	// BuildArgs are additional build arguments passed to the Dockerfile.
	// +optional
	BuildArgs map[string]string `json:"buildArgs,omitempty"`

	// BuildResources overrides the CPU and memory the in-cluster build
	// container may use, for apps whose build needs more than the cluster
	// default (heavy SSR/bundler builds routinely need several GiB). Unset
	// values fall back to the cluster default and then the built-in default.
	// +optional
	BuildResources *BuildResources `json:"buildResources,omitempty"`
}

// BuildResources overrides the build container's resource limits for one app.
type BuildResources struct {
	// Memory is the build container's memory limit, e.g. "6Gi".
	// +optional
	Memory string `json:"memory,omitempty"`

	// CPU is the build container's CPU limit, e.g. "2".
	// +optional
	CPU string `json:"cpu,omitempty"`
}

// AppBuildStatus holds the status of the latest build for Git-based apps.
type AppBuildStatus struct {
	// Phase is the current build state. Discarded means the build finished but
	// its image was not deployed, because it did not build from the source the
	// app declares now.
	// +kubebuilder:validation:Enum=Pending;Building;Succeeded;Failed;Discarded
	// +optional
	Phase string `json:"phase,omitempty"`

	// Commit is the Git commit SHA that was built.
	// +optional
	Commit string `json:"commit,omitempty"`

	// StartedAt is when the build started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the build completed.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// Message provides details on build failure.
	// +optional
	Message string `json:"message,omitempty"`

	// Build names the build job this status is about. It is what tells a
	// completion that was already applied from one belonging to an earlier
	// build, which a phase alone cannot, and it needs no clock.
	// +optional
	Build string `json:"build,omitempty"`
}

// AppStatus defines the observed state of the application.
type AppStatus struct {
	// Phase represents the current lifecycle phase.
	// +kubebuilder:validation:Enum=Pending;Running;Failed;Stopped
	// +optional
	Phase string `json:"phase,omitempty"`

	// Replicas is the total number of desired replicas.
	// +optional
	Replicas int32 `json:"replicas,omitempty"`

	// ReadyReplicas is the number of ready replicas.
	// +optional
	ReadyReplicas int32 `json:"readyReplicas,omitempty"`

	// Image is the currently deployed image.
	// +optional
	Image string `json:"image,omitempty"`

	// Build holds the latest build information for Git-based apps.
	// +optional
	Build *AppBuildStatus `json:"build,omitempty"`

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
// +kubebuilder:printcolumn:name="Image",type=string,JSONPath=`.spec.image`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyReplicas`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// App is the Schema for the apps API.
type App struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AppSpec   `json:"spec,omitempty"`
	Status AppStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AppList contains a list of App.
type AppList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []App `json:"items"`
}

func init() {
	SchemeBuilder.Register(&App{}, &AppList{})
}
