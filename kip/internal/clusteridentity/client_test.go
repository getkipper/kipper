package clusteridentity

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/controller/pkg/identity"
)

func fakeClient(t *testing.T, obj *unstructured.Unstructured) *Client {
	t.Helper()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "ClusterIdentityList"},
		obj,
	)
	return New(dyn)
}

func singleton(spec, status map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "ClusterIdentity",
		"metadata":   map[string]any{"name": SingletonName, "generation": int64(2)},
		"spec":       spec,
		"status":     status,
	}}
}

func TestGetDecodesTransitionAndApproval(t *testing.T) {
	from := map[string]any{"console": "console-acme.kipper.run", "consoleAPI": "console-api-acme.kipper.run", "dex": "dex-acme.kipper.run", "issuer": "https://dex-acme.kipper.run/dex"}
	to := map[string]any{"console": "console--acme.kipper.run", "consoleAPI": "console-api--acme.kipper.run", "dex": "dex--acme.kipper.run", "issuer": "https://dex--acme.kipper.run/dex"}
	obj := singleton(
		map[string]any{"domain": "acme.kipper.run"},
		map[string]any{
			"observedGeneration": int64(2),
			"transition":         map[string]any{"phase": PhaseAwaitingApproval, "from": from, "to": to, "nonce": "abcd"},
		},
	)
	c := fakeClient(t, obj)

	ci, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ci.Phase() != PhaseAwaitingApproval {
		t.Fatalf("phase = %q, want AwaitingApproval", ci.Phase())
	}
	if ci.Status.Transition.To.Dex != "dex--acme.kipper.run" {
		t.Fatalf("to.dex decoded wrong: %+v", ci.Status.Transition.To)
	}

	got, ok := ci.PendingApproval()
	if !ok {
		t.Fatal("expected an approvable transition")
	}
	// The CLI's approval must equal what the reconciler recomputes from the CR.
	want := identity.ApprovalHash(2,
		identity.HostKey("console-acme.kipper.run", "console-api-acme.kipper.run", "dex-acme.kipper.run", "https://dex-acme.kipper.run/dex"),
		identity.HostKey("console--acme.kipper.run", "console-api--acme.kipper.run", "dex--acme.kipper.run", "https://dex--acme.kipper.run/dex"),
		"abcd")
	if got != want {
		t.Fatalf("approval hash mismatch:\n got  %s\n want %s", got, want)
	}
}

func TestPendingApprovalRequiresTransition(t *testing.T) {
	c := fakeClient(t, singleton(map[string]any{"domain": "acme.kipper.run"}, map[string]any{}))
	ci, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, ok := ci.PendingApproval(); ok {
		t.Fatal("a steady cluster has no approvable transition")
	}
	if ci.Phase() != "" {
		t.Fatalf("steady phase should be empty, got %q", ci.Phase())
	}
}

func TestPatchSpecMergesOnlyGivenKeys(t *testing.T) {
	c := fakeClient(t, singleton(map[string]any{"domain": "old.kipper.run"}, map[string]any{}))
	ctx := context.Background()

	if err := c.PatchSpec(ctx, map[string]any{"cutoverApproval": "hash123"}); err != nil {
		t.Fatalf("patch: %v", err)
	}

	ci, err := c.Get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ci.Spec.Domain != "old.kipper.run" {
		t.Fatalf("merge patch clobbered domain: %q", ci.Spec.Domain)
	}
	if ci.Spec.CutoverApproval != "hash123" {
		t.Fatalf("cutoverApproval not applied: %q", ci.Spec.CutoverApproval)
	}
}

func TestPatchSpecNullClearsField(t *testing.T) {
	// Rollback relies on this: patching hosts:null must remove stale overrides so
	// the reconciler derives hosts from the domain.
	c := fakeClient(t, singleton(
		map[string]any{"domain": "acme.example.com", "hosts": map[string]any{"console": "console.acme.example.com"}},
		map[string]any{},
	))
	ctx := context.Background()

	if err := c.PatchSpec(ctx, map[string]any{"hosts": nil}); err != nil {
		t.Fatalf("patch: %v", err)
	}
	ci, err := c.Get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ci.Spec.Hosts != nil {
		t.Fatalf("a null merge patch must clear spec.hosts, got %+v", ci.Spec.Hosts)
	}
}

func TestConditionLookup(t *testing.T) {
	obj := singleton(map[string]any{"domain": "acme.kipper.run"}, map[string]any{
		"conditions": []any{
			map[string]any{"type": "Ready", "status": "False", "reason": "Degraded", "message": "cutover reverted"},
		},
	})
	c := fakeClient(t, obj)
	ci, err := c.Get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	cond := ci.Condition(ConditionReady)
	if cond == nil || cond.Reason != "Degraded" {
		t.Fatalf("Ready condition not decoded: %+v", cond)
	}
	if ci.Condition("Nonexistent") != nil {
		t.Fatal("missing condition should be nil")
	}
}
