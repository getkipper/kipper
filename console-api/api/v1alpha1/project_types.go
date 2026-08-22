package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ProjectSpec defines the desired state of a project.
//
// The XValidation rule caps the environment count. For tiered projects that
// stops an owner multiplying the granted capacity (each namespace gets a full
// tier-sized quota); for tierless projects, whose environments carry no quota
// at all, it is the only synchronous brake on uncapped namespace fan-out.
// Because it references oldSelf it is a transition rule, which the API server
// evaluates only on update, not on create: an update may not grow the count
// past the effective limit, but one that keeps or reduces the count is
// allowed, so a forced tier downgrade leaves an over-limit project editable
// instead of frozen. A create that exceeds the limit is not rejected here;
// the reconciler is the backstop that caps namespace creation and flags the
// project. The effective limit is maxEnvironments when set, otherwise the
// tier default, with an absent tier meaning tierless. These literals must
// match TierEnvLimit (a drift test asserts it).
// +kubebuilder:validation:XValidation:rule="size(self.environments) <= (has(self.maxEnvironments) ? self.maxEnvironments : (!has(self.tier) || size(self.tier) == 0 ? 6 : (self.tier == 'large' ? 10 : (self.tier == 'medium' ? 6 : 4)))) || size(self.environments) <= size(oldSelf.environments)",message="environment count exceeds the project limit (6 without a tier; small 4, medium 6, large 10); a cluster admin can raise it by setting maxEnvironments, or assign a tier for managed capacity"
type ProjectSpec struct {
	// DisplayName is the human-readable project name.
	// +optional
	DisplayName string `json:"displayName,omitempty"`

	// Environments lists the environment stages for this project.
	// +kubebuilder:default={{name: "test"}}
	// +optional
	Environments []ProjectEnvironment `json:"environments,omitempty"`

	// MaxEnvironments overrides the tier's default environment-count cap for
	// this project. It is admin-set, since more environments means more total
	// granted capacity. Unset means the tier default (TierEnvLimit) applies.
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxEnvironments *int `json:"maxEnvironments,omitempty"`

	// SharedStorage configures a shared PVC across apps in each environment.
	// +optional
	SharedStorage *ProjectSharedStorage `json:"sharedStorage,omitempty"`

	// Members lists the users with access to this project and their role
	// within it. Cluster admins have access to every project regardless of
	// this list.
	// +optional
	Members []ProjectMember `json:"members,omitempty"`

	// Tier selects the default CPU/memory quota applied to each of the
	// project's environment namespaces. A tier is a capacity label, not a
	// price. Individual environments can override it via their Quota field.
	// Unset means tierless: the project's environments get no quota objects
	// and only cluster-wide limits apply. Assigning a tier is the opt-in for
	// managed per-environment capacity.
	// +kubebuilder:validation:Enum=small;medium;large;""
	// +optional
	Tier string `json:"tier,omitempty"`

	// AllowLinksFrom names the projects whose apps may link to apps in this
	// project. It is this project's consent, given by whoever owns it.
	//
	// A link opens a direct route to a backend, past the ingress and so past
	// every control attached to a public route: forward auth, API keys, rate
	// limits. The calling side declaring it is not enough, because an app's own
	// project cannot grant access to somebody else's. Without an entry here the
	// link is recorded and no egress is opened.
	//
	// Bounded and validated because a Project can be written straight to the
	// API server, and this list is an authorisation: an entry that is not a
	// project name matches nothing, but it still has to be read and stored on
	// every reconcile of every app that links here.
	// +kubebuilder:validation:MaxItems=256
	// +kubebuilder:validation:items:MinLength=1
	// +kubebuilder:validation:items:MaxLength=253
	// +kubebuilder:validation:items:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`
	// +optional
	AllowLinksFrom []string `json:"allowLinksFrom,omitempty"`
}

// memberRolePattern constrains a member's role name. It is the same string as
// the Pattern marker below, and a test asserts the generated CRD carries it:
// a marker edited without this constant, or the reverse, is drift nobody would
// otherwise see.
//
// One optional dotted suffix, both halves DNS labels. That covers the three
// built-ins, a shared role's dot-free name, and a tenant role's
// `<project>.<name>`. It stays this narrow because a role name reaches a
// generated object name, and a name carrying a slash or a space is one nothing
// can address.
const memberRolePattern = `^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)?$`

// ProjectMemberRole is a user's capability within a single project.
//
// The three built-ins are ordered viewer < deployer < owner. The schema does
// not list them, because a member may also name a role this build does not
// know: written with kubectl, restored from a backup, or carried in by a
// migration from a cluster that had it. A closed enum would fail the whole
// Project object in that case, and the member holding the unknown role is
// exactly the one an operator needs to be able to remove.
//
// Such a member holds nothing. The projection binds only the roles it knows, so
// an unrecognised name grants no access, and the console reports it as
// unrecognised rather than showing it as a role.
// +kubebuilder:validation:MaxLength=127
// +kubebuilder:validation:Pattern=`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)?$`
type ProjectMemberRole string

const (
	// ProjectRoleOwner can deploy and manage the project's members.
	ProjectRoleOwner ProjectMemberRole = "owner"
	// ProjectRoleDeployer can deploy and mutate workloads in the project.
	ProjectRoleDeployer ProjectMemberRole = "deployer"
	// ProjectRoleViewer has read-only access to the project.
	ProjectRoleViewer ProjectMemberRole = "viewer"
)

// ProjectMember grants a user a role within a project.
type ProjectMember struct {
	// Email identifies the user, matching their Dex identity.
	Email string `json:"email"`

	// Role is the user's capability within this project.
	Role ProjectMemberRole `json:"role"`
}

// ProjectEnvironment defines a single environment stage.
type ProjectEnvironment struct {
	// Name is the environment name (e.g. test, acc, prod).
	Name string `json:"name"`

	// Quota overrides the project tier's CPU/memory quota for this
	// environment's namespace.
	// +optional
	Quota *EnvQuota `json:"quota,omitempty"`
}

// EnvQuota caps the aggregate CPU and memory an environment namespace may
// request and limit, in Kubernetes quantity form (e.g. "2", "500m", "4Gi").
type EnvQuota struct {
	// CPURequest caps the sum of container CPU requests.
	CPURequest string `json:"cpuRequest"`

	// CPULimit caps the sum of container CPU limits.
	CPULimit string `json:"cpuLimit"`

	// MemoryRequest caps the sum of container memory requests.
	MemoryRequest string `json:"memoryRequest"`

	// MemoryLimit caps the sum of container memory limits.
	MemoryLimit string `json:"memoryLimit"`
}

// ProjectSharedStorage configures shared persistent storage.
type ProjectSharedStorage struct {
	// Enabled turns shared storage on or off.
	Enabled bool `json:"enabled"`

	// Size is the PVC size (e.g. "5Gi").
	// +kubebuilder:default="5Gi"
	// +optional
	Size string `json:"size,omitempty"`
}

// ProjectStatus defines the observed state of the project.
type ProjectStatus struct {
	// Phase represents the current lifecycle phase.
	// +kubebuilder:validation:Enum=Active;Terminating
	// +optional
	Phase string `json:"phase,omitempty"`

	// Namespaces lists the Kubernetes namespaces created for this project.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Display Name",type=string,JSONPath=`.spec.displayName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Environments",type=string,JSONPath=`.status.namespaces`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// Project is the Schema for the projects API. It is cluster-scoped because
// it manages namespaces across the cluster.
type Project struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ProjectSpec   `json:"spec,omitempty"`
	Status ProjectStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ProjectList contains a list of Project.
type ProjectList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Project `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Project{}, &ProjectList{})
}
