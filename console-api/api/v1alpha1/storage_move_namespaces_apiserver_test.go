package v1alpha1

import (
	"context"
	"fmt"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/apiservertest"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

const moveNamespacesPolicy = "../../../deploy/policies/kipper-storage-move-namespaces.yaml"

func refusedByNamespacesPolicy(err error) bool {
	return err != nil && strings.Contains(err.Error(), platform.StorageMoveNamespacesPolicy)
}

func TestStorageMoveNamespacesPolicy(t *testing.T) {
	cfg := apiservertest.Start(t)
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := scheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	admin, err := crclient.New(cfg, crclient.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	createManifest(ctx, t, admin, policyFile(t, moveNamespacesPolicy))

	moving := map[string]string{platform.StorageMoveNamespaceLabel: "true"}
	for _, ns := range []*corev1.Namespace{
		{ObjectMeta: metav1.ObjectMeta{Name: "moving", Labels: moving}},
		{ObjectMeta: metav1.ObjectMeta{Name: "probe", Labels: moving}},
		{ObjectMeta: metav1.ObjectMeta{Name: "moved", Labels: moving}},
		{ObjectMeta: metav1.ObjectMeta{Name: "plain"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "other-labels", Labels: map[string]string{"team": "shop"}}},
		{ObjectMeta: metav1.ObjectMeta{Name: "not-true", Labels: map[string]string{platform.StorageMoveNamespaceLabel: "false"}}},
	} {
		if err := admin.Create(ctx, ns); err != nil {
			t.Fatalf("namespace %s: %v", ns.Name, err)
		}
	}

	deleteNamespaces := rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"namespaces"}, Verbs: []string{"get", "delete"}}
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: "namespace-deleter"}, Rules: []rbacv1.PolicyRule{deleteNamespaces}}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: "namespace-deleter"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: "namespace-deleter"},
		Subjects:   []rbacv1.Subject{{APIGroup: "rbac.authorization.k8s.io", Kind: "User", Name: "namespace-deleter"}},
	}
	for _, obj := range []crclient.Object{role, binding} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	impersonated := rest.CopyConfig(cfg)
	impersonated.Impersonate = rest.ImpersonationConfig{UserName: "namespace-deleter"}
	deleter, err := crclient.New(impersonated, crclient.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}

	namespace := func(name string) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
	}
	terminating := func(name string) bool {
		t.Helper()
		ns := &corev1.Namespace{}
		if err := admin.Get(ctx, crclient.ObjectKey{Name: name}, ns); err != nil {
			t.Fatal(err)
		}
		return ns.DeletionTimestamp != nil
	}

	eventually(t, "the namespaces policy never refused deleting a moving namespace", func() error {
		err := admin.Delete(ctx, namespace("probe"), crclient.DryRunAll)
		if !refusedByNamespacesPolicy(err) {
			return fmt.Errorf("not refused yet: %v", err)
		}
		return nil
	})

	t.Run("a server-side dry-run delete of a moving namespace is refused and deletes nothing", func(t *testing.T) {
		err := admin.Delete(ctx, namespace("moving"), crclient.DryRunAll)
		if !refusedByNamespacesPolicy(err) {
			t.Fatalf("want the move refusal, got %v", err)
		}
		if terminating("moving") {
			t.Fatal("the refused dry run started deleting the namespace")
		}
		if err := admin.Delete(ctx, namespace("plain"), crclient.DryRunAll); err != nil {
			t.Fatalf("a dry run on a namespace without the label: %v", err)
		}
		if terminating("plain") {
			t.Fatal("an admitted dry run deleted the namespace")
		}
	})

	t.Run("nobody deletes a moving namespace, admins included", func(t *testing.T) {
		err := admin.Delete(ctx, namespace("moving"))
		if !refusedByNamespacesPolicy(err) || !strings.Contains(err.Error(), "kip service storage longhorn") || !strings.Contains(err.Error(), "--rollback") {
			t.Fatalf("want the admin refused with the way out, got %v", err)
		}
		if err := deleter.Delete(ctx, namespace("moving")); !refusedByNamespacesPolicy(err) {
			t.Fatalf("want a namespace deleter refused, got %v", err)
		}
		if terminating("moving") {
			t.Fatal("a refused delete started deleting the namespace")
		}
	})

	t.Run("namespaces without the label delete normally", func(t *testing.T) {
		for _, name := range []string{"plain", "other-labels", "not-true"} {
			if err := deleter.Delete(ctx, namespace(name)); err != nil {
				t.Fatalf("namespace %s: %v", name, err)
			}
			if !terminating(name) {
				t.Fatalf("namespace %s is not terminating after its delete", name)
			}
		}
	})

	t.Run("a namespace deletes normally once its label is removed", func(t *testing.T) {
		ns := &corev1.Namespace{}
		if err := admin.Get(ctx, crclient.ObjectKey{Name: "moved"}, ns); err != nil {
			t.Fatal(err)
		}
		delete(ns.Labels, platform.StorageMoveNamespaceLabel)
		if err := admin.Update(ctx, ns); err != nil {
			t.Fatal(err)
		}
		if err := deleter.Delete(ctx, namespace("moved")); err != nil {
			t.Fatalf("the move ended and the namespace is still refused: %v", err)
		}
		if !terminating("moved") {
			t.Fatal("the namespace is not terminating after its delete")
		}
	})
}
