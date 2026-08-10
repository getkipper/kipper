package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterIdentitySpec is the desired serving identity of the whole cluster: the
// base domain, any per-service host overrides, gateway registration, and the
// operator's approval for the one session-invalidating step of a host change.
// A single cluster-scoped ClusterIdentity named "cluster" is the source of
// truth; the reconciler ignores any other name.
//
// The XValidation rule mirrors the gateway's full registrable-label contract for
// *.kipper.run domains, so the constraint is enforced at the API boundary as well
// as in the shared hostnames package: a single 1-63 char DNS label, with no "--"
// (the derived-route separator) and not a reserved name. clusteridentity_cel_test.go
// pins the "--" literal to hostnames.DerivedRouteSeparator and the reserved set to
// hostnames.ReservedLabels so the CEL rule can never drift from the gateway.
//
// +kubebuilder:validation:XValidation:rule="!self.domain.endsWith('.kipper.run') || (self.domain.matches('^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?[.]kipper[.]run$') && !self.domain.contains('--') && !(self.domain in ['console.kipper.run','console-api.kipper.run','dex.kipper.run','api.kipper.run','www.kipper.run','admin.kipper.run','kipper.kipper.run','register.kipper.run','health.kipper.run']))",message="a *.kipper.run cluster domain must be a single 1-63 char DNS label, with no '--' (reserved for derived service routes) and not a reserved name (console, console-api, dex, api, www, admin, kipper, register, health)"
// +kubebuilder:validation:XValidation:rule="self.domain != 'kipper.run'",message="domain must not be the kipper.run apex; use a <label>.kipper.run subdomain or a custom domain"
type ClusterIdentitySpec struct {
	// Domain is the cluster's base identity: either a custom domain
	// (example.com) or a single-label *.kipper.run domain
	// (203-0-113-20.kipper.run). Service hosts are derived from it unless
	// overridden below. The pattern validates each DNS label (1-63 chars, no
	// leading/trailing hyphen, no empty labels); the *.kipper.run single-label
	// and reserved-name rules are enforced by the XValidation rules above.
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`
	Domain string `json:"domain"`

	// Hosts pins per-service hosts. An empty field is derived from Domain by
	// convention; a non-empty field is a custom host used verbatim.
	// +optional
	Hosts *IdentityHosts `json:"hosts,omitempty"`

	// Gateway configures registration with the *.kipper.run subdomain gateway.
	// Omitted or Register=false for pure custom-domain clusters.
	// +optional
	Gateway *GatewaySpec `json:"gateway,omitempty"`

	// CutoverApproval is the operator's signed go-ahead for the single
	// session-invalidating step of a host change (the Dex issuer flip). The CLI
	// writes hash(observedGeneration, from, to, targetIssuer, nonce) here after
	// its external probes pass; the reconciler refuses to leave AwaitingApproval
	// until this matches the pending transition. A stale or replayed approval
	// never matches: the hash binds the transition's endpoints and its
	// per-transition nonce, so editing the domain or a host override resets the
	// nonce and voids a prior approval. Non-host edits (acknowledging SSO
	// callbacks, adjusting the grace period) keep the same from/to and so keep a
	// valid approval, since they do not change what is being cut over to.
	// +optional
	CutoverApproval string `json:"cutoverApproval,omitempty"`

	// AckSSOCallbacksFor records that the operator has updated (or will
	// update) each SSO provider's OAuth callback URL for the Dex host named
	// here. When a host change rehosts connectors, approval is refused until
	// this names the pending transition's target Dex host, so a silent rehost
	// cannot lock out SSO users. Naming the host binds the acknowledgement to
	// one move: a later transition to a different Dex host always requires a
	// fresh acknowledgement, no clearing step needed.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	AckSSOCallbacksFor string `json:"ackSSOCallbacksFor,omitempty"`

	// KeepOldHostsUntil optionally delays contraction (pruning the old hosts)
	// until this time, giving external clients a grace period after cutover.
	// +optional
	KeepOldHostsUntil *metav1.Time `json:"keepOldHostsUntil,omitempty"`
}

// IdentityHosts pins the three serving hosts. An empty string means "derive from
// the domain by convention" (a valid, meaningful state used by adoption and
// rollback, so the patterns accept ""); a non-empty value is a custom host used
// verbatim. Derived *.kipper.run hosts legitimately contain "--", so the patterns
// allow it. The same shape is used for spec overrides and the rollback target.
type IdentityHosts struct {
	// Console is the web console host (e.g. console.example.com).
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^(([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)?$`
	Console string `json:"console,omitempty"`

	// ConsoleAPI is the console backend API host.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^(([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)?$`
	ConsoleAPI string `json:"consoleAPI,omitempty"`

	// Dex is the OIDC provider host.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^(([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)?$`
	Dex string `json:"dex,omitempty"`
}

// GatewaySpec configures registration with the *.kipper.run subdomain gateway.
type GatewaySpec struct {
	// KipperRunDomain is the gateway's base domain (kipper.run). Retained so the
	// heartbeat can address the subdomain even after a custom-domain move
	// overwrites Domain.
	// +optional
	// +kubebuilder:validation:MaxLength=253
	// +kubebuilder:validation:Pattern=`^(([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)*[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)?$`
	KipperRunDomain string `json:"kipperRunDomain,omitempty"`

	// ClusterHost is the public IP address the gateway routes to, and the value
	// console-api serves as CLUSTER_HOST. An address, never a name: the gateway
	// refuses to register anything that is not a public IP, so a hostname here
	// would produce a heartbeat that can never succeed. The schema checks the
	// syntax; kip additionally holds a value it records to the gateway's own
	// routable-address policy (controller/pkg/pubip), which the schema cannot
	// express. It lives on the CR because the reconciler patches the console-api
	// env family on every identity change and can only preserve what the CR can
	// express: with nowhere to carry the host, a host transition blanked it and
	// silently stopped the heartbeat, the hop pin, and the registration proof.
	// +optional
	// +kubebuilder:validation:MaxLength=45
	// +kubebuilder:validation:XValidation:rule="self == '' || isIP(self)",message="clusterHost must be an IP address; the gateway registers addresses, not names"
	ClusterHost string `json:"clusterHost,omitempty"`

	// Register controls whether this cluster registers its subdomain with the
	// gateway. Defaults to true; set false for pure custom-domain clusters. The
	// default only applies when the gateway block is present: a nil gateway
	// carries no defaulted register, so the reconciler treats a *.kipper.run
	// domain with no gateway block as register=true (kipperRunDomain=kipper.run).
	// +optional
	// +kubebuilder:default=true
	Register *bool `json:"register,omitempty"`
}

// ClusterIdentityStatus is the observed serving identity and any in-flight host
// transition. The reconciler is the sole writer of this subresource.
type ClusterIdentityStatus struct {
	// ObservedGeneration is the spec generation the reconciler last acted on.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ActiveHosts is the host set currently verified as serving, with the OIDC
	// issuer it presents.
	// +optional
	ActiveHosts *ResolvedHosts `json:"activeHosts,omitempty"`

	// Steady is the spec identity (domain plus host overrides) recorded while
	// the cluster is steady. A transition snapshots it as its FromIdentity the
	// moment it opens: once the domain edit lands, the previous spec is no
	// longer readable anywhere else, and inferring it from a rendered hostname
	// is ambiguous under host overrides.
	// +optional
	Steady *SteadyIdentity `json:"steady,omitempty"`

	// LastSteady is the previous steady identity, used as the rollback target: a
	// --rollback patches spec back to this.
	// +optional
	LastSteady *SteadyIdentity `json:"lastSteady,omitempty"`

	// Transition is present only while a host change is in flight. It is written
	// before each phase's mutations so progress survives a reconciler restart.
	// +optional
	Transition *TransitionStatus `json:"transition,omitempty"`

	// Conditions are the latest observations of serving state: IngressesReady,
	// CertificatesReady, DNSReady, GatewayRouted, OIDCAligned, ExternalCallbacks,
	// Ready.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ResolvedHosts is a fully-resolved serving host set plus the OIDC issuer it
// presents. Used for the active identity and for a transition's endpoints.
type ResolvedHosts struct {
	// Console is the resolved console host.
	// +optional
	Console string `json:"console,omitempty"`

	// ConsoleAPI is the resolved console-api host.
	// +optional
	ConsoleAPI string `json:"consoleAPI,omitempty"`

	// Dex is the resolved Dex host.
	// +optional
	Dex string `json:"dex,omitempty"`

	// Issuer is the OIDC issuer URL this host set presents (derived from Dex).
	// +optional
	Issuer string `json:"issuer,omitempty"`
}

// SteadyIdentity is a complete steady identity: the base domain and its host
// overrides, enough to patch spec back to it on rollback.
type SteadyIdentity struct {
	// Domain is the base domain of this steady identity.
	// +optional
	Domain string `json:"domain,omitempty"`

	// Hosts are the per-service overrides of this steady identity.
	// +optional
	Hosts *IdentityHosts `json:"hosts,omitempty"`
}

// TransitionStatus is the state of an in-flight host change. The reconciler
// owns every field.
type TransitionStatus struct {
	// Phase is the current stage of the transition. Steady state has no
	// transition at all (this whole object is absent).
	// +kubebuilder:validation:Enum=DualServe;AwaitingApproval;CuttingOver;Verifying;Contracting;Reverting;Degraded
	Phase string `json:"phase"`

	// From is the identity being moved away from (old issuer still valid until
	// the flip).
	// +optional
	From *ResolvedHosts `json:"from,omitempty"`

	// FromIdentity is the complete steady identity (spec.domain plus host
	// overrides) being moved away from, snapshotted from status.steady when the
	// transition opens. finishTransition records it as lastSteady, so a
	// rollback restores the exact previous spec instead of a domain guessed
	// from a rendered hostname. Renders for the Reverting and Degraded phases
	// derive their spec-driven values (cluster domain, cookie scope, admin
	// email) from it, so a revert restores the old identity coherently.
	// +optional
	FromIdentity *SteadyIdentity `json:"fromIdentity,omitempty"`

	// ToIdentity is the spec identity this transition was opened toward,
	// snapshotted when the transition opens. Post-flip renders and
	// finishTransition read it instead of the live spec, so a spec edit that
	// lands after the issuer flip can neither skew what the transition applies
	// nor record a steady identity that was never made active.
	// +optional
	ToIdentity *SteadyIdentity `json:"toIdentity,omitempty"`

	// To is the identity being moved to.
	// +optional
	To *ResolvedHosts `json:"to,omitempty"`

	// Nonce is a random value written when the transition is created. The
	// cutover approval hash binds to it, so an approval can never be replayed
	// against a later transition.
	// +optional
	Nonce string `json:"nonce,omitempty"`

	// CutoverStartedAt is set the moment the Dex issuer flip is durably written
	// (the dex-config ConfigMap now holds the new issuer), not on entry to
	// CuttingOver. It bounds the post-flip window: if the cutover has not verified
	// within the deadline the reconciler auto-reverts to the previous identity
	// rather than leaving Dex on the new issuer indefinitely (a stuck rollout, a
	// deleted Deployment, or a persistently failing write). It stays nil while a
	// pre-flip blocker is retried, so an unstarted flip never trips the deadline.
	// +optional
	CutoverStartedAt *metav1.Time `json:"cutoverStartedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Domain",type=string,JSONPath=`.spec.domain`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.transition.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ClusterIdentity is the Schema for the cluster serving-identity API. One
// cluster-scoped instance named "cluster" owns the console, console-api, and
// Dex hosts, the OIDC issuer, and gateway registration, and drives no-lockout
// host changes through a phased transition.
type ClusterIdentity struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ClusterIdentitySpec   `json:"spec,omitempty"`
	Status ClusterIdentityStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ClusterIdentityList contains a list of ClusterIdentity.
type ClusterIdentityList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ClusterIdentity `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ClusterIdentity{}, &ClusterIdentityList{})
}
