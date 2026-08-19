// Package labels is the single source of truth for every label, annotation,
// and finalizer key used by Kipper across all components (kip, console-api,
// controller, gateway, sidecar). Importing constants from this package
// instead of repeating string literals turns label drift between the CLI
// and the web console into a compile error.
package labels

// Core management labels.
//
// Every Kipper-owned Kubernetes resource carries ManagedBy=Kipper. The CLI
// and the console-api both use it to find resources they manage, and any
// reconciler treats the absence of this label as "not ours, leave alone".
//
// App is the generic identity label used in Pod/Deployment selectors. It is
// not Kipper-specific (it predates Kipper conventions in the wider
// Kubernetes ecosystem) but we treat its name as a constant here so call
// sites do not hardcode it.
const (
	ManagedBy = "app.kubernetes.io/managed-by"
	Kipper    = "kipper"
	App       = "app"
)

// Resource classification labels.
//
// ResourceType distinguishes resources that share the same underlying
// Kubernetes kind (for example, both functions and apps are Deployments).
// ResourceProfile is set on apps to record their declared profile, used by
// the resource recommendation system.
const (
	ResourceType    = "kipper.run/resource-type"
	ResourceProfile = "kipper.run/resource-profile"

	ResourceTypeFunction     = "function"
	ResourceTypeSharedVolume = "shared-volume"
)

// Project and environment labels applied to namespaces.
//
// Kipper maps a (Project, Environment) pair to a Kubernetes namespace
// named "<project>-<environment>" (or "<project>" when environment is the
// default). EnvOrder is the integer ordering used by the console to display
// environments in a consistent sequence. Environments is a comma-separated
// annotation listing every environment owned by a Project CR.
const (
	Project      = "kipper.run/project"
	Environment  = "kipper.run/environment"
	EnvOrder     = "kipper.run/env-order"
	Environments = "kipper.run/environments"
)

// Service labels.
//
// ServiceType identifies which database or cache engine a Service CR
// represents. The CLI and the console-api both use it to render
// type-specific connection hints.
const (
	ServiceType = "kipper.run/service-type"

	ServiceTypePostgres = "postgres"
	ServiceTypeMySQL    = "mysql"
	ServiceTypeRedis    = "redis"
	ServiceTypeMongoDB  = "mongodb"
	ServiceTypeMinIO    = "minio"
)

// Function labels.
const (
	FunctionTrigger   = "kipper.run/trigger"
	FunctionNamespace = "kipper.run/fn-namespace"
	FunctionRuntime   = "kipper.run/runtime"

	FunctionTriggerHTTP = "http"
)

// Volume labels.
const (
	VolumeName = "kipper.run/volume-name"
)

// Build labels and annotations.
//
// Build is set on Job objects produced by the build system to identify
// them. AppRef stores the app name that triggered the build. Commit is the
// git commit SHA recorded as an annotation on built artifacts.
const (
	Build  = "kipper.run/build"
	AppRef = "kipper.run/app"

	BuildTrue = "true"

	AnnoCommit        = "kipper.run/commit"
	AnnoDeployHistory = "kipper.run/deploy-history"

	// AnnoGitAuthority records the clone host a per-app git credential was
	// stored for. The token and the URL are written as a pair but not in one
	// transaction, so this is what lets every path that would send the token
	// catch a pair that stopped agreeing. It lives here because kip, console-api
	// and the builder all write or read it, and two spellings would mean a
	// credential one of them stored is refused by another.
	AnnoGitAuthority = "kipper.run/git-authority"

	// AnnoGitCredentialClaimed is when a writer last took a git credential to
	// commit an App onto it. A credential object is shared by every writer of
	// the same token and host, so one already there is reused rather than
	// created, and its age then says nothing about whether a commit is in
	// flight. The sweep waits out its grace from this or from creation,
	// whichever is later.
	AnnoGitCredentialClaimed = "kipper.run/git-credential-claimed"

	// SourceNamespace records the namespace of the app a build was triggered
	// for. Builds run in BuildsNamespace rather than beside their app, so this
	// is what ties a build back to it, and what keeps two projects with an app
	// of the same name apart.
	SourceNamespace = "kipper.run/source-namespace"
)

// BuildsNamespace is where the build system runs every build, whatever
// namespace the app itself lives in. Anything looking for a build Pod or Job
// looks here: searching the app's own namespace finds nothing, which is what
// made `kip app build-logs` report "no build found" for builds that had run
// perfectly well.
const BuildsNamespace = "kipper-builds"

// Job labels.
const (
	JobType = "kipper.run/job-type"

	JobTypeJob = "job"

	TriggeredBy        = "kipper.run/triggered-by"
	TriggeredByConsole = "console"
)

// Registry, binding, and middleware labels.
const (
	Registry       = "kipper.run/registry"
	Binding        = "kipper.run/binding"
	MiddlewareType = "kipper.run/middleware-type"

	RegistryTrue           = "true"
	BindingTrue            = "true"
	MiddlewareTypeRedirect = "redirect"
)

// Migration labels.
//
// Migration tags secrets owned by the cross-cluster migration flow.
// Values are "token" (target cluster), "export"/"import" (transient).
const (
	Migration = "kipper.run/migration"

	MigrationToken  = "token"
	MigrationExport = "export"
	MigrationImport = "import"
)

// Operational annotations.
//
// AnnoRestartedAt is set on Pods/Deployments to force a restart by mutating
// the pod template. AnnoMonitoringDisabled opts a resource out of the
// observability stack. AnnoPromoted* track GitOps promotions between
// environments. AnnoRecommendationDismissedAt records when a user dismissed
// a resource-sizing recommendation so we stop nagging.
const (
	AnnoRestartedAt             = "kipper.run/restartedAt"
	AnnoMonitoringDisabled      = "kipper.run/monitoring-disabled"
	AnnoPromotedAt              = "kipper.run/promoted-at"
	AnnoPromotedFrom            = "kipper.run/promoted-from"
	AnnoPromotedImage           = "kipper.run/promoted-image"
	AnnoRecommendationDismissed = "kipper.run/recommendation-dismissed-at"

	// AnnoTuningPausedUntil holds an RFC 3339 timestamp on a Deployment or
	// StatefulSet. Until it passes, the resource controller leaves the
	// workload's resources alone — set during bulk operations (kip service
	// import/export) so a tuning restart cannot kill them. The deadline
	// bounds the pause: a crashed client cannot switch tuning off for good.
	AnnoTuningPausedUntil = "kipper.run/tuning-paused-until"

	MonitoringDisabledTrue = "true"
)

// Finalizers.
//
// Each Kipper CR uses a unique finalizer so its reconciler can perform
// cleanup before Kubernetes deletes the object. Keep these in sync with the
// CRD definitions in console-api/controllers/.
const (
	FinalizerApp      = "kipper.run/app-cleanup"
	FinalizerService  = "kipper.run/service-cleanup"
	FinalizerFunction = "kipper.run/function-cleanup"
	FinalizerJob      = "kipper.run/job-cleanup"
	FinalizerProject  = "kipper.run/project-cleanup"
	FinalizerVolume   = "kipper.run/volume-cleanup"
)

// KipperManagedSelector is the label selector used to list every resource
// Kipper owns of a given Kubernetes kind. Callers that need a more specific
// filter should append additional selectors (e.g. ServiceType=postgres).
const KipperManagedSelector = ManagedBy + "=" + Kipper
