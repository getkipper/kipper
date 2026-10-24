package v1alpha1

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/scheme"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/apiservertest"
)

const (
	serverReplicasPolicy = "../../../deploy/policies/kipper-longhorn-server-replicas.yaml"
	serverNodesLine      = "    - name: serverNodes\n      expression: \"[]\"\n"
	consoleAPIUser       = "system:serviceaccount:kipper-system:console-api"
)

// replicaCRD is the part of Longhorn's Replica CRD the policy reads.
func replicaCRD() *apiextensionsv1.CustomResourceDefinition {
	preserve := true
	return &apiextensionsv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{Name: "replicas.longhorn.io"},
		Spec: apiextensionsv1.CustomResourceDefinitionSpec{
			Group: "longhorn.io",
			Names: apiextensionsv1.CustomResourceDefinitionNames{Plural: "replicas", Singular: "replica", Kind: "Replica", ListKind: "ReplicaList"},
			Scope: apiextensionsv1.NamespaceScoped,
			Versions: []apiextensionsv1.CustomResourceDefinitionVersion{{
				Name: "v1beta2", Served: true, Storage: true,
				Schema: &apiextensionsv1.CustomResourceValidation{OpenAPIV3Schema: &apiextensionsv1.JSONSchemaProps{
					Type: "object", XPreserveUnknownFields: &preserve,
				}},
			}},
		},
	}
}

func longhornReplica(name, node string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "longhorn.io/v1beta2", "kind": "Replica",
		"metadata": map[string]interface{}{"name": name, "namespace": "longhorn-system"},
		"spec":     map[string]interface{}{"nodeID": node},
	}}
	return u
}

func renderServerNodes(t *testing.T, list string) []byte {
	t.Helper()
	data, err := os.ReadFile(serverReplicasPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), serverNodesLine) {
		t.Fatal("the server-replica policy no longer carries the line kip renders")
	}
	return []byte(strings.Replace(string(data), serverNodesLine, fmt.Sprintf("    - name: serverNodes\n      expression: %q\n", list), 1))
}

func TestLonghornServerReplicasPolicy(t *testing.T) {
	cfg := apiservertest.Start(t)
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{scheme.AddToScheme, apiextensionsv1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatal(err)
		}
	}
	admin, err := crclient.New(cfg, crclient.Options{Scheme: s})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := admin.Create(ctx, replicaCRD()); err != nil {
		t.Fatal(err)
	}
	if err := admin.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "longhorn-system"}}); err != nil {
		t.Fatal(err)
	}
	deleteReplicas := []rbacv1.PolicyRule{{APIGroups: []string{"longhorn.io"}, Resources: []string{"replicas"}, Verbs: []string{"get", "create", "delete"}}}
	console := asClusterUser(ctx, t, admin, cfg, s, consoleAPIUser, deleteReplicas)
	manager := asClusterUser(ctx, t, admin, cfg, s, "system:serviceaccount:longhorn-system:longhorn-service-account", deleteReplicas)

	n := 0
	replica := func(node string) *unstructured.Unstructured {
		n++
		r := longhornReplica(fmt.Sprintf("r-%d", n), node)
		eventually(t, "creating a replica", func() error { return admin.Create(ctx, r) })
		return r
	}

	createManifest(ctx, t, admin, renderServerNodes(t, "['server-1']"))
	eventually(t, "the policy never refused console-api a server replica", func() error {
		if err := console.Delete(ctx, replica("server-1")); err == nil {
			return errors.New("accepted")
		}
		return nil
	})

	t.Run("console-api cannot delete a server replica", func(t *testing.T) {
		err := console.Delete(ctx, replica("server-1"))
		if err == nil || !strings.Contains(err.Error(), "never deletes a Longhorn replica on a server node") {
			t.Fatalf("want the policy's refusal, got %v", err)
		}
	})

	t.Run("console-api deletes a worker replica", func(t *testing.T) {
		if err := console.Delete(ctx, replica("worker-1")); err != nil {
			t.Fatalf("worker replica: %v", err)
		}
	})

	t.Run("Longhorn and admins are not matched", func(t *testing.T) {
		if err := manager.Delete(ctx, replica("server-1")); err != nil {
			t.Fatalf("longhorn-manager: %v", err)
		}
		if err := admin.Delete(ctx, replica("server-1")); err != nil {
			t.Fatalf("admin: %v", err)
		}
	})

	t.Run("an empty server list denies console-api every replica delete, and nobody else", func(t *testing.T) {
		policy := &unstructured.Unstructured{}
		policy.SetAPIVersion("admissionregistration.k8s.io/v1")
		policy.SetKind("ValidatingAdmissionPolicy")
		if err := admin.Get(ctx, crclient.ObjectKey{Name: "kipper-longhorn-server-replicas"}, policy); err != nil {
			t.Fatal(err)
		}
		variables, _, _ := unstructured.NestedSlice(policy.Object, "spec", "variables")
		variables[0].(map[string]interface{})["expression"] = "[]"
		if err := unstructured.SetNestedSlice(policy.Object, variables, "spec", "variables"); err != nil {
			t.Fatal(err)
		}
		if err := admin.Update(ctx, policy); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the emptied list never refused a worker replica", func() error {
			if err := console.Delete(ctx, replica("worker-1")); err == nil {
				return errors.New("accepted")
			}
			return nil
		})
		if err := manager.Delete(ctx, replica("server-1")); err != nil {
			t.Fatalf("longhorn-manager with an empty list: %v", err)
		}
	})
}
