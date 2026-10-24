package v1alpha1

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/apiservertest"
)

const (
	copiesChangePolicy = "../../../deploy/policies/kipper-storage-copies-change.yaml"
	copiesOfferPolicy  = "../../../deploy/policies/kipper-storage-copies-offer.yaml"
	// offeredLine is the rendered offer in the shipped policy, which kip
	// replaces with the admin's offer.
	offeredLine = "    - name: offered\n      expression: \"1\"\n"
)

func createManifest(ctx context.Context, t *testing.T, c crclient.Client, data []byte) {
	t.Helper()
	dec := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	for {
		obj := &unstructured.Unstructured{}
		if err := dec.Decode(&obj.Object); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}
			t.Fatal(err)
		}
		if len(obj.Object) == 0 {
			continue
		}
		if err := c.Create(ctx, obj); err != nil {
			t.Fatalf("creating %s: %v", obj.GetKind(), err)
		}
	}
}

func readPolicy(t *testing.T, path string, offered int) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), offeredLine) && path == copiesOfferPolicy {
		t.Fatal("the offer policy no longer carries the line kip renders")
	}
	return []byte(strings.Replace(string(data), offeredLine, fmt.Sprintf("    - name: offered\n      expression: \"%d\"\n", offered), 1))
}

// asUser returns a client that acts as user, holding role's rules in the
// default namespace.
func asUser(ctx context.Context, t *testing.T, admin crclient.Client, cfg *rest.Config, s *runtime.Scheme, user string, rules []rbacv1.PolicyRule) crclient.Client {
	t.Helper()
	role := &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: user, Namespace: "default"}, Rules: rules}
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: user, Namespace: "default"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: user},
		Subjects:   []rbacv1.Subject{{APIGroup: "rbac.authorization.k8s.io", Kind: "User", Name: user}},
	}
	for _, obj := range []crclient.Object{role, binding} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	impersonated := rest.CopyConfig(cfg)
	impersonated.Impersonate = rest.ImpersonationConfig{UserName: user}
	c, err := crclient.New(impersonated, crclient.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// asClusterUser returns a client that acts as user, holding rules across the
// cluster, which cluster-scoped Projects need.
func asClusterUser(ctx context.Context, t *testing.T, admin crclient.Client, cfg *rest.Config, s *runtime.Scheme, user string, rules []rbacv1.PolicyRule) crclient.Client {
	t.Helper()
	role := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: user}, Rules: rules}
	binding := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: user},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: user},
		Subjects:   []rbacv1.Subject{{APIGroup: "rbac.authorization.k8s.io", Kind: "User", Name: user}},
	}
	for _, obj := range []crclient.Object{role, binding} {
		if err := admin.Create(ctx, obj); err != nil {
			t.Fatal(err)
		}
	}
	impersonated := rest.CopyConfig(cfg)
	impersonated.Impersonate = rest.ImpersonationConfig{UserName: user}
	c, err := crclient.New(impersonated, crclient.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

var writeProjects = rbacv1.PolicyRule{APIGroups: []string{"kipper.run"}, Resources: []string{"projects"}, Verbs: []string{"get", "update", "patch"}}

var setProjectCopies = rbacv1.PolicyRule{APIGroups: []string{"kipper.run"}, Resources: []string{"projects"}, Verbs: []string{"set-copies"}}

var writeKipperObjects = rbacv1.PolicyRule{
	APIGroups: []string{"kipper.run"}, Resources: []string{"services", "volumes"},
	Verbs: []string{"get", "list", "watch", "create", "update", "patch", "delete"},
}

var setCopies = rbacv1.PolicyRule{APIGroups: []string{"kipper.run"}, Resources: []string{"services", "volumes"}, Verbs: []string{"set-copies"}}

func copiesService(name string, copies *int32) *Service {
	return &Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: ServiceSpec{Type: "redis", Copies: copies}}
}

// eventually retries f until it returns nil, because policies reach the
// admission chain asynchronously.
func eventually(t *testing.T, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var err error
	for time.Now().Before(deadline) {
		if err = f(); err == nil {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("%s: %v", what, err)
}

func TestStorageCopiesPolicies(t *testing.T) {
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
	createManifest(ctx, t, admin, readPolicy(t, copiesChangePolicy, 1))
	createManifest(ctx, t, admin, readPolicy(t, copiesOfferPolicy, 1))

	deployer := asUser(ctx, t, admin, cfg, s, "deployer", []rbacv1.PolicyRule{writeKipperObjects})
	owner := asUser(ctx, t, admin, cfg, s, "owner", []rbacv1.PolicyRule{writeKipperObjects, setCopies})

	probe := 0
	eventually(t, "the offer policy never refused a deployer's copies-2 service", func() error {
		probe++
		if err := deployer.Create(ctx, copiesService(fmt.Sprintf("probe-%d", probe), ptr.To[int32](2))); err == nil {
			return errors.New("accepted")
		}
		return nil
	})

	t.Run("a deployer creates one copy, and two only when the cluster offers them", func(t *testing.T) {
		if err := deployer.Create(ctx, copiesService("one", ptr.To[int32](1))); err != nil {
			t.Fatalf("one copy: %v", err)
		}
		err := deployer.Create(ctx, copiesService("two", ptr.To[int32](2)))
		if err == nil || !strings.Contains(err.Error(), "kip cluster storage --max-copies 2") {
			t.Fatalf("want the offer refusal, got %v", err)
		}
	})

	t.Run("an admin and a restore are not bound by the offer", func(t *testing.T) {
		if err := admin.Create(ctx, copiesService("restored", ptr.To[int32](2))); err != nil {
			t.Fatalf("admin copies 2: %v", err)
		}
	})

	t.Run("a deployer cannot change copies, an owner can", func(t *testing.T) {
		svc := &Service{}
		if err := deployer.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "one"}, svc); err != nil {
			t.Fatal(err)
		}
		svc.Spec.Storage = "2Gi"
		if err := deployer.Update(ctx, svc); err != nil {
			t.Fatalf("an update that keeps copies: %v", err)
		}
		svc.Spec.Copies = nil
		if err := deployer.Update(ctx, svc); err == nil || !strings.Contains(err.Error(), "project owner") {
			t.Fatalf("want the set-copies refusal for removing copies, got %v", err)
		}
		if err := owner.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "one"}, svc); err != nil {
			t.Fatal(err)
		}
		svc.Spec.Copies = nil
		if err := owner.Update(ctx, svc); err != nil {
			t.Fatalf("an owner unsetting copies: %v", err)
		}
		svc.Spec.Copies = ptr.To[int32](1)
		if err := owner.Update(ctx, svc); err != nil {
			t.Fatalf("an owner adopting copies: %v", err)
		}
		svc.Spec.Copies = ptr.To[int32](2)
		if err := owner.Update(ctx, svc); err == nil || !strings.Contains(err.Error(), "--max-copies 2") {
			t.Fatalf("an owner is still bound by the offer, got %v", err)
		}
	})

	t.Run("a deployer can still update a copies-2 object it could not create today", func(t *testing.T) {
		svc := &Service{}
		if err := deployer.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "restored"}, svc); err != nil {
			t.Fatal(err)
		}
		svc.Spec.Storage = "3Gi"
		if err := deployer.Update(ctx, svc); err != nil {
			t.Fatalf("updating an existing copies-2 service: %v", err)
		}
	})

	t.Run("volumes follow the same rules", func(t *testing.T) {
		if err := deployer.Create(ctx, &Volume{ObjectMeta: metav1.ObjectMeta{Name: "files", Namespace: "default"}, Spec: VolumeSpec{Size: "1Gi", Copies: ptr.To[int32](1)}}); err != nil {
			t.Fatalf("one copy: %v", err)
		}
		err := deployer.Create(ctx, &Volume{ObjectMeta: metav1.ObjectMeta{Name: "files-2", Namespace: "default"}, Spec: VolumeSpec{Size: "1Gi", Copies: ptr.To[int32](2)}})
		if err == nil || !strings.Contains(err.Error(), "--max-copies 2") {
			t.Fatalf("want the offer refusal, got %v", err)
		}
		vol := &Volume{}
		if err := deployer.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "files"}, vol); err != nil {
			t.Fatal(err)
		}
		vol.Spec.Copies = nil
		if err := deployer.Update(ctx, vol); err == nil || !strings.Contains(err.Error(), "project owner") {
			t.Fatalf("want the set-copies refusal, got %v", err)
		}
		if err := owner.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "files"}, vol); err != nil {
			t.Fatal(err)
		}
		vol.Spec.Copies = nil
		if err := owner.Update(ctx, vol); err != nil {
			t.Fatalf("an owner unsetting copies: %v", err)
		}
	})

	t.Run("project shared storage copies need set-copies on the project", func(t *testing.T) {
		if err := admin.Create(ctx, &Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}, Spec: ProjectSpec{SharedStorage: &ProjectSharedStorage{Enabled: true, Size: "5Gi"}}}); err != nil {
			t.Fatal(err)
		}
		editor := asClusterUser(ctx, t, admin, cfg, s, "project-editor", []rbacv1.PolicyRule{writeProjects})
		projectOwner := asClusterUser(ctx, t, admin, cfg, s, "project-owner", []rbacv1.PolicyRule{writeProjects, setProjectCopies})

		project := &Project{}
		if err := editor.Get(ctx, crclient.ObjectKey{Name: "shop"}, project); err != nil {
			t.Fatal(err)
		}
		project.Spec.SharedStorage.Copies = ptr.To[int32](1)
		if err := editor.Update(ctx, project); err == nil || !strings.Contains(err.Error(), "project owner") {
			t.Fatalf("want the set-copies refusal for the nested field, got %v", err)
		}
		if err := projectOwner.Get(ctx, crclient.ObjectKey{Name: "shop"}, project); err != nil {
			t.Fatal(err)
		}
		project.Spec.SharedStorage.Copies = ptr.To[int32](1)
		if err := projectOwner.Update(ctx, project); err != nil {
			t.Fatalf("an owner recording copies: %v", err)
		}
		project.Spec.SharedStorage = nil
		if err := editor.Update(ctx, project); err == nil || !strings.Contains(err.Error(), "project owner") {
			t.Fatalf("removing shared storage removes its copies, which needs set-copies, got %v", err)
		}
		if err := projectOwner.Get(ctx, crclient.ObjectKey{Name: "shop"}, project); err != nil {
			t.Fatal(err)
		}
		project.Spec.SharedStorage.Copies = ptr.To[int32](2)
		if err := projectOwner.Update(ctx, project); err == nil || !strings.Contains(err.Error(), "--max-copies 2") {
			t.Fatalf("an owner is still bound by the offer, got %v", err)
		}
	})

	t.Run("no one changes copies while a conversion runs", func(t *testing.T) {
		if err := admin.Create(ctx, copiesService("converting", ptr.To[int32](1))); err != nil {
			t.Fatal(err)
		}
		setReason := func(reason string) {
			t.Helper()
			svc := &Service{}
			if err := admin.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "converting"}, svc); err != nil {
				t.Fatal(err)
			}
			svc.Status.Conditions = []metav1.Condition{{
				Type: ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: reason,
				Message: "Longhorn is building a copy of the volume (1 of 2 copies healthy).", LastTransitionTime: metav1.Now(),
			}}
			if err := admin.Status().Update(ctx, svc); err != nil {
				t.Fatal(err)
			}
		}
		setReason(StorageCopiesConverting)
		svc := &Service{}
		for _, writer := range []crclient.Client{owner, admin} {
			if err := writer.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "converting"}, svc); err != nil {
				t.Fatal(err)
			}
			svc.Spec.Copies = nil
			err := writer.Update(ctx, svc)
			if err == nil || !strings.Contains(err.Error(), "while a conversion runs. Longhorn is building a copy") {
				t.Fatalf("want the running-conversion refusal with its message, got %v", err)
			}
		}
		svc.Spec.Copies = ptr.To[int32](1)
		svc.Spec.Storage = "2Gi"
		if err := owner.Update(ctx, svc); err != nil {
			t.Fatalf("an update that keeps copies during a conversion: %v", err)
		}
		setReason(StorageCopiesBlocked)
		if err := owner.Get(ctx, crclient.ObjectKey{Namespace: "default", Name: "converting"}, svc); err != nil {
			t.Fatal(err)
		}
		svc.Spec.Copies = nil
		if err := owner.Update(ctx, svc); err != nil {
			t.Fatalf("a blocked conversion can be set back: %v", err)
		}
	})

	t.Run("a project's shared storage copies do not change while an environment converts", func(t *testing.T) {
		project := &Project{}
		if err := admin.Get(ctx, crclient.ObjectKey{Name: "shop"}, project); err != nil {
			t.Fatal(err)
		}
		project.Status.Conditions = []metav1.Condition{{
			Type: ConditionStorageCopies, Status: metav1.ConditionFalse, Reason: StorageCopiesConverting,
			Message: "test: Building a copy on the server. prod: The volume is not attached.", LastTransitionTime: metav1.Now(),
		}}
		if err := admin.Status().Update(ctx, project); err != nil {
			t.Fatal(err)
		}
		if err := admin.Get(ctx, crclient.ObjectKey{Name: "shop"}, project); err != nil {
			t.Fatal(err)
		}
		project.Spec.SharedStorage.Copies = nil
		if err := admin.Update(ctx, project); err == nil || !strings.Contains(err.Error(), "while a conversion runs. test: Building a copy") {
			t.Fatalf("want the running-conversion refusal for a project, got %v", err)
		}
	})

	t.Run("an offer of two lets a deployer create two copies", func(t *testing.T) {
		policy := &unstructured.Unstructured{}
		policy.SetAPIVersion("admissionregistration.k8s.io/v1")
		policy.SetKind("ValidatingAdmissionPolicy")
		if err := admin.Get(ctx, crclient.ObjectKey{Name: "kipper-storage-copies-offer"}, policy); err != nil {
			t.Fatal(err)
		}
		variables, _, _ := unstructured.NestedSlice(policy.Object, "spec", "variables")
		variables[0].(map[string]interface{})["expression"] = "2"
		if err := unstructured.SetNestedSlice(policy.Object, variables, "spec", "variables"); err != nil {
			t.Fatal(err)
		}
		if err := admin.Update(ctx, policy); err != nil {
			t.Fatal(err)
		}
		n := 0
		eventually(t, "a deployer's copies-2 service under an offer of two", func() error {
			n++
			return deployer.Create(ctx, copiesService(fmt.Sprintf("offered-%d", n), ptr.To[int32](2)))
		})
	})
}
