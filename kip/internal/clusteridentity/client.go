// Package clusteridentity is the CLI's read/write access to the cluster-scoped
// ClusterIdentity CR that the console-api reconciler drives. It reads status
// (phase, endpoints, conditions) and patches spec (domain, approval), using a
// dynamic client so the CLI does not depend on the console-api module. The
// approval hash is computed through the shared controller/pkg/identity, so the
// CLI and the reconciler can never disagree on it.
package clusteridentity

import (
	"context"
	"encoding/json"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/identity"
)

// SingletonName is the only ClusterIdentity the reconciler acts on.
const SingletonName = "cluster"

var gvr = schema.GroupVersionResource{Group: "kipper.run", Version: "v1alpha1", Resource: "clusteridentities"}

// Condition status/phase strings mirrored from the CR.
const (
	PhaseDualServe        = "DualServe"
	PhaseAwaitingApproval = "AwaitingApproval"
	PhaseCuttingOver      = "CuttingOver"
	PhaseVerifying        = "Verifying"
	PhaseContracting      = "Contracting"
	PhaseReverting        = "Reverting"
	PhaseDegraded         = "Degraded"

	ConditionReady             = "Ready"
	ConditionExternalCallbacks = "ExternalCallbacks"

	// ReasonNeedsAck is the ExternalCallbacks reason when SSO connectors will be
	// rehosted and the operator has not acknowledged updating their callbacks.
	ReasonNeedsAck = "NeedsAck"
)

// Client reads and patches the ClusterIdentity singleton.
type Client struct {
	dyn dynamic.Interface
}

// New wraps a dynamic client.
func New(dyn dynamic.Interface) *Client {
	return &Client{dyn: dyn}
}

// Get returns the ClusterIdentity singleton. The caller inspects the error with
// apierrors.IsNotFound to detect a cluster that predates the reconciler.
func (c *Client) Get(ctx context.Context) (*ClusterIdentity, error) {
	u, err := c.dyn.Resource(gvr).Get(ctx, SingletonName, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var ci ClusterIdentity
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, &ci); err != nil {
		return nil, fmt.Errorf("decoding ClusterIdentity: %w", err)
	}
	return &ci, nil
}

// PatchSpec applies a JSON merge patch to spec. Only the given keys change.
func (c *Client) PatchSpec(ctx context.Context, spec map[string]any) error {
	patch, err := json.Marshal(map[string]any{"spec": spec})
	if err != nil {
		return fmt.Errorf("building spec patch: %w", err)
	}
	if _, err := c.dyn.Resource(gvr).Patch(ctx, SingletonName, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("patching ClusterIdentity spec: %w", err)
	}
	return nil
}

// ClusterIdentity is the CLI-side view of the CR: only the fields the CLI reads.
type ClusterIdentity struct {
	Metadata Metadata `json:"metadata"`
	Spec     Spec     `json:"spec"`
	Status   Status   `json:"status"`
}

// Metadata carries the fields the CLI needs for optimistic reads.
type Metadata struct {
	Generation      int64  `json:"generation"`
	ResourceVersion string `json:"resourceVersion"`
}

// Spec is the desired serving identity.
type Spec struct {
	Domain          string   `json:"domain"`
	Hosts           *Hosts   `json:"hosts,omitempty"`
	Gateway         *Gateway `json:"gateway,omitempty"`
	CutoverApproval string   `json:"cutoverApproval,omitempty"`
	// AckSSOCallbacksFor names the Dex host whose SSO provider callbacks the
	// operator has confirmed updating; the reconciler honours it only for a
	// transition targeting exactly that host.
	AckSSOCallbacksFor string `json:"ackSSOCallbacksFor,omitempty"`
}

// Gateway configures *.kipper.run gateway registration.
type Gateway struct {
	KipperRunDomain string `json:"kipperRunDomain,omitempty"`
	Register        *bool  `json:"register,omitempty"`
}

// KipperRunDomain returns the cluster's registered *.kipper.run domain, or empty.
func (ci *ClusterIdentity) KipperRunDomain() string {
	if ci.Spec.Gateway == nil {
		return ""
	}
	return ci.Spec.Gateway.KipperRunDomain
}

// Hosts pins per-service hosts.
type Hosts struct {
	Console    string `json:"console,omitempty"`
	ConsoleAPI string `json:"consoleAPI,omitempty"`
	Dex        string `json:"dex,omitempty"`
}

// Status is the observed serving state and any in-flight transition.
type Status struct {
	ObservedGeneration int64          `json:"observedGeneration,omitempty"`
	ActiveHosts        *ResolvedHosts `json:"activeHosts,omitempty"`
	// Steady is the spec identity recorded while the cluster is steady; during
	// a transition it still names the outgoing identity.
	Steady     *SteadyIdentity `json:"steady,omitempty"`
	LastSteady *SteadyIdentity `json:"lastSteady,omitempty"`
	Transition *Transition     `json:"transition,omitempty"`
	Conditions []Condition     `json:"conditions,omitempty"`
}

// ResolvedHosts is a fully-resolved host set plus its OIDC issuer.
type ResolvedHosts struct {
	Console    string `json:"console,omitempty"`
	ConsoleAPI string `json:"consoleAPI,omitempty"`
	Dex        string `json:"dex,omitempty"`
	Issuer     string `json:"issuer,omitempty"`
}

// SteadyIdentity is the rollback target recorded at contraction.
type SteadyIdentity struct {
	Domain string `json:"domain,omitempty"`
	Hosts  *Hosts `json:"hosts,omitempty"`
}

// Transition is the in-flight host change. FromIdentity and ToIdentity are the
// spec identities at the transition's two endpoints, snapshotted by the
// reconciler when the transition opened.
type Transition struct {
	Phase        string          `json:"phase"`
	From         *ResolvedHosts  `json:"from,omitempty"`
	FromIdentity *SteadyIdentity `json:"fromIdentity,omitempty"`
	To           *ResolvedHosts  `json:"to,omitempty"`
	ToIdentity   *SteadyIdentity `json:"toIdentity,omitempty"`
	Nonce        string          `json:"nonce,omitempty"`
	// CutoverStartedAt is set the moment the Dex issuer flip is durably
	// written; within CuttingOver it is the boundary between "old identity
	// still authenticates" and "target identity is live".
	CutoverStartedAt *metav1.Time `json:"cutoverStartedAt,omitempty"`
}

// Condition is one observed serving-state condition.
type Condition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// PendingApproval returns the approval hash the CLI must write to authorise the
// current transition's cutover, and whether a cutover-approvable transition
// exists. It recomputes exactly what the reconciler will check.
func (ci *ClusterIdentity) PendingApproval() (string, bool) {
	t := ci.Status.Transition
	if t == nil || t.From == nil || t.To == nil {
		return "", false
	}
	from := identity.HostKey(t.From.Console, t.From.ConsoleAPI, t.From.Dex, t.From.Issuer)
	to := identity.HostKey(t.To.Console, t.To.ConsoleAPI, t.To.Dex, t.To.Issuer)
	return identity.ApprovalHash(ci.Status.ObservedGeneration, from, to, t.Nonce), true
}

// Phase is the current transition phase, or empty when steady.
func (ci *ClusterIdentity) Phase() string {
	if ci.Status.Transition == nil {
		return ""
	}
	return ci.Status.Transition.Phase
}

// Condition returns the named condition, or nil.
func (ci *ClusterIdentity) Condition(condType string) *Condition {
	for i := range ci.Status.Conditions {
		if ci.Status.Conditions[i].Type == condType {
			return &ci.Status.Conditions[i]
		}
	}
	return nil
}
