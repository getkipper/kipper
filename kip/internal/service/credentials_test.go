package service

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

func serviceCR(name string, uid types.UID) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": manifest.ServiceGVR.GroupVersion().String(),
		"kind":       "Service",
		"metadata": map[string]interface{}{
			"name": name, "namespace": "shop-prod", "uid": string(uid),
		},
		"spec": map[string]interface{}{"type": "postgres"},
	}}
}

func credentialsSecret(name string, owner *metav1.OwnerReference) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop-prod", UID: types.UID("uid-" + name)},
		Data:       map[string][]byte{"PASSWORD": []byte("s3cret")},
	}
	if owner != nil {
		s.OwnerReferences = []metav1.OwnerReference{*owner}
	}
	return s
}

func ownedBy(kind, name string, uid types.UID) *metav1.OwnerReference {
	controller := true
	return &metav1.OwnerReference{
		APIVersion: manifest.ServiceGVR.GroupVersion().String(),
		Kind:       kind, Name: name, UID: uid, Controller: &controller,
	}
}

func managerWith(objects []runtime.Object, crs ...*unstructured.Unstructured) *Manager {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(manifest.ServiceGVR.GroupVersion().WithKind("ServiceList"), &unstructured.UnstructuredList{})
	var crObjects []runtime.Object
	for _, cr := range crs {
		crObjects = append(crObjects, cr)
	}
	return &Manager{
		Client: fake.NewSimpleClientset(objects...),
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
			map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"}, crObjects...),
	}
}

// The audit has to compare the owner reference against the live CR's UID.
// Anything less answers a different question: a Secret can name the right
// service and still belong to a CR that no longer exists, which is what a
// restore leaves behind, and its bindings are refused exactly as if it had no
// owner at all.
func TestAuditCredentials_ClassifiesByOwnerUID(t *testing.T) {
	live := serviceCR("db", "uid-db")
	restored := serviceCR("cache", "uid-cache-restored")
	fresh := serviceCR("queue", "uid-queue")

	m := managerWith([]runtime.Object{
		credentialsSecret("db-credentials", ownedBy("Service", "db", "uid-db")),
		credentialsSecret("cache-credentials", ownedBy("Service", "cache", "uid-cache-before-the-restore")),
		// queue-credentials is absent entirely.
		credentialsSecret("mail-credentials", nil),
	}, live, restored, fresh)

	audit, err := m.AuditCredentials(context.Background(), "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}

	states := map[string]CredentialState{}
	for _, r := range audit.Services {
		states[r.Service] = r.State
	}
	for service, want := range map[string]CredentialState{
		"db": CredentialOwned, "cache": CredentialForeign, "queue": CredentialMissing,
	} {
		if states[service] != want {
			t.Errorf("%s = %q, want %q", service, states[service], want)
		}
	}
	if len(audit.Services) != 3 {
		t.Fatalf("audited %d services, want 3", len(audit.Services))
	}
}

// An unowned per-binding Secret is a credential nothing maintains: the
// workload's controller refuses to write through an object it does not own, so
// the values sit there while the workload that needs them fails to reconcile.
func TestAuditCredentials_FindsUnownedProjections(t *testing.T) {
	m := managerWith([]runtime.Object{
		withBindingLabel(credentialsSecret("db-app-api-credentials", nil)),
		withBindingLabel(credentialsSecret("db-app-web-credentials", ownedBy("App", "web", "uid-web"))),
		credentialsSecret("db-credentials", ownedBy("Service", "db", "uid-db")),
	}, serviceCR("db", "uid-db"))

	audit, err := m.AuditCredentials(context.Background(), "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if len(audit.Projections) != 1 || audit.Projections[0].Name != "db-app-api-credentials" {
		t.Fatalf("projections = %+v, want only the unowned one", audit.Projections)
	}
	if !audit.NeedsRepair() {
		t.Fatal("an unowned projection needs repair")
	}
}

// Repair is the operator asserting what the platform refuses to infer, so it
// gives the Secret the live CR's UID: a reference carrying any other UID is
// refused by the injection gate exactly as an absent one is.
func TestRepairCredentials_ClaimsWithTheLiveUID(t *testing.T) {
	ctx := context.Background()
	m := managerWith([]runtime.Object{
		credentialsSecret("db-credentials", nil),
		withBindingLabel(credentialsSecret("db-app-api-credentials", nil)),
	}, serviceCR("db", "uid-db"))

	audit, err := m.AuditCredentials(ctx, "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if _, err := m.RepairCredentials(ctx, "shop-prod", audit); err != nil {
		t.Fatalf("repair: %v", err)
	}

	got, err := m.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading repaired secret: %v", err)
	}
	owner := metav1.GetControllerOf(got)
	if owner == nil || owner.UID != types.UID("uid-db") {
		t.Fatalf("owner = %+v, want the live CR's UID", got.OwnerReferences)
	}
	if string(got.Data["PASSWORD"]) != "s3cret" {
		t.Fatalf("repair must not touch the credentials themselves, got %q", got.Data["PASSWORD"])
	}

	if _, err := m.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-app-api-credentials", metav1.GetOptions{}); err == nil {
		t.Fatal("an unowned projection must be removed so its workload renders one it owns")
	}

	after, err := m.AuditCredentials(ctx, "shop-prod")
	if err != nil {
		t.Fatalf("re-audit: %v", err)
	}
	if after.NeedsRepair() {
		t.Fatalf("the namespace must be clean after a repair, got %+v", after)
	}
}

// The audit and the repair are separate reads, so the object can gain an owner
// in between. Claiming it then would take it from that owner, which is the one
// thing this must never do.
func TestRepairCredentials_RefusesASecretThatGainedAnOwner(t *testing.T) {
	ctx := context.Background()
	m := managerWith([]runtime.Object{credentialsSecret("db-credentials", nil)}, serviceCR("db", "uid-db"))

	audit, err := m.AuditCredentials(ctx, "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}

	claimed := credentialsSecret("db-credentials", ownedBy("Service", "db", "uid-somebody-else"))
	if _, err := m.Client.CoreV1().Secrets("shop-prod").Update(ctx, claimed, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("simulating a concurrent claim: %v", err)
	}

	if _, err := m.RepairCredentials(ctx, "shop-prod", audit); err == nil {
		t.Fatal("a Secret that gained an owner since the audit must not be claimed")
	}

	got, err := m.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading secret: %v", err)
	}
	if owner := metav1.GetControllerOf(got); owner == nil || owner.UID != types.UID("uid-somebody-else") {
		t.Fatalf("the other owner must be left alone, got %+v", got.OwnerReferences)
	}
}

func withBindingLabel(s *corev1.Secret) *corev1.Secret {
	s.Labels = map[string]string{"kipper.run/binding": "true"}
	return s
}

// A projection a running workload still reads must survive the repair. Its pods
// hold those values already, but the envFrom naming it is optional, so a pod
// restarting after the Secret went would come up with no credentials and fail on
// its first connection, which is the failure this whole check exists to catch.
func TestRepairCredentials_KeepsAProjectionAWorkloadStillReads(t *testing.T) {
	ctx := context.Background()
	inUse := withBindingLabel(credentialsSecret("db-app-api-credentials", nil))
	unused := withBindingLabel(credentialsSecret("db-app-gone-credentials", nil))
	reader := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-prod"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "api", EnvFrom: []corev1.EnvFromSource{{
				SecretRef: &corev1.SecretEnvSource{LocalObjectReference: corev1.LocalObjectReference{
					Name: "db-app-api-credentials",
				}},
			}}}},
		}}},
	}

	m := managerWith([]runtime.Object{inUse, unused, reader}, serviceCR("db", "uid-db"))
	audit, err := m.AuditCredentials(ctx, "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if _, err := m.RepairCredentials(ctx, "shop-prod", audit); err != nil {
		t.Fatalf("repair: %v", err)
	}

	if _, err := m.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-app-api-credentials", metav1.GetOptions{}); err != nil {
		t.Fatalf("a projection a Deployment reads must be left in place: %v", err)
	}
	if _, err := m.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-app-gone-credentials", metav1.GetOptions{}); err == nil {
		t.Fatal("a projection nothing reads must be removed")
	}
}
