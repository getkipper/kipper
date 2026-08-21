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
	k8stesting "k8s.io/client-go/testing"

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

func appCR(name string, uid types.UID) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": manifest.AppGVR.GroupVersion().String(),
		"kind":       "App",
		"metadata": map[string]interface{}{
			"name": name, "namespace": "shop-prod", "uid": string(uid),
		},
		"spec": map[string]interface{}{"image": "app:1"},
	}}
}

func managerWith(objects []runtime.Object, crs ...*unstructured.Unstructured) *Manager {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(manifest.ServiceGVR.GroupVersion().WithKind("ServiceList"), &unstructured.UnstructuredList{})
	scheme.AddKnownTypeWithName(manifest.AppGVR.GroupVersion().WithKind("AppList"), &unstructured.UnstructuredList{})
	var crObjects []runtime.Object
	for _, cr := range crs {
		crObjects = append(crObjects, cr)
	}
	return &Manager{
		Client: fake.NewSimpleClientset(objects...),
		Dynamic: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
			map[schema.GroupVersionResource]string{
				manifest.ServiceGVR: "ServiceList",
				manifest.AppGVR:     "AppList",
			}, crObjects...),
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
		"db": CredentialOwned, "cache": CredentialAbandoned, "queue": CredentialMissing,
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
// The owner it gained has to be one that exists, or there is nothing to take it
// from. A reference naming a UID that resolves to nothing is the abandoned case,
// covered separately.
func TestRepairCredentials_RefusesASecretThatGainedAnOwner(t *testing.T) {
	ctx := context.Background()
	m := managerWith([]runtime.Object{credentialsSecret("db-credentials", nil)},
		serviceCR("db", "uid-db"), serviceCR("other", "uid-other"))

	audit, err := m.AuditCredentials(ctx, "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}

	claimed := credentialsSecret("db-credentials", ownedBy("Service", "other", "uid-other"))
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
	if owner := metav1.GetControllerOf(got); owner == nil || owner.UID != types.UID("uid-other") {
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

// A reference outlives the object it names. The owner may have been deleted, or
// come back from a restore under a new identity, and either way nobody is
// holding the Secret: Kubernetes is about to delete it by that dangling
// reference, and the password the volume was written under goes with it.
func TestAuditCredentials_SeparatesADeadReferenceFromALiveOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner *metav1.OwnerReference
		crs   []*unstructured.Unstructured
		want  CredentialState
	}{
		{
			name:  "an app that is still there holds it",
			owner: ownedBy("App", "web", "uid-web"),
			crs:   []*unstructured.Unstructured{appCR("web", "uid-web")},
			want:  CredentialForeign,
		},
		{
			name:  "the app it names has gone",
			owner: ownedBy("App", "web", "uid-web"),
			crs:   nil,
			want:  CredentialAbandoned,
		},
		{
			name:  "the app came back under a new identity",
			owner: ownedBy("App", "web", "uid-web-before-the-restore"),
			crs:   []*unstructured.Unstructured{appCR("web", "uid-web-restored")},
			want:  CredentialAbandoned,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			crs := append([]*unstructured.Unstructured{serviceCR("db", "uid-db")}, tc.crs...)
			m := managerWith([]runtime.Object{credentialsSecret("db-credentials", tc.owner)}, crs...)

			audit, err := m.AuditCredentials(context.Background(), "shop-prod")
			if err != nil {
				t.Fatalf("audit: %v", err)
			}
			if len(audit.Services) != 1 {
				t.Fatalf("audited %d services, want 1", len(audit.Services))
			}
			if audit.Services[0].State != tc.want {
				t.Errorf("state = %q, want %q", audit.Services[0].State, tc.want)
			}
		})
	}
}

// A controller this command cannot look up is a claim it cannot disprove, so it
// stands. Taking a Secret from an operator's own controller because its kind is
// unfamiliar is the one mistake repair must never make.
func TestAuditCredentials_LeavesAKindItCannotCheckAlone(t *testing.T) {
	controller := true
	foreign := &metav1.OwnerReference{
		APIVersion: "apps/v1", Kind: "StatefulSet",
		Name: "somebody-else", UID: "another-object", Controller: &controller,
	}
	m := managerWith(
		[]runtime.Object{credentialsSecret("db-credentials", foreign)},
		serviceCR("db", "uid-db"),
	)

	audit, err := m.AuditCredentials(context.Background(), "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if audit.Services[0].State != CredentialForeign {
		t.Errorf("state = %q, want %q", audit.Services[0].State, CredentialForeign)
	}
}

// A dangling reference is what the operator came here to fix, so the command has
// to offer to fix it.
func TestNeedsRepair_CountsAnAbandonedSecret(t *testing.T) {
	audit := CredentialAudit{Services: []CredentialReport{
		{Service: "db", Secret: "db-credentials", State: CredentialAbandoned},
	}}
	if !audit.NeedsRepair() {
		t.Error("a Secret about to be garbage collected was reported as nothing to do")
	}
}

// A live owner is still not something repair takes from.
func TestNeedsRepair_IgnoresAForeignSecret(t *testing.T) {
	audit := CredentialAudit{Services: []CredentialReport{
		{Service: "db", Secret: "db-credentials", State: CredentialForeign, Owner: "App/web"},
	}}
	if audit.NeedsRepair() {
		t.Error("repair offered to take a Secret from the object holding it")
	}
}

// The restore case, end to end: the Service came back under a new identity, so
// the Secret's reference names a CR that no longer exists and Kubernetes will
// delete it. Repair points it at the live service instead, which is what keeps
// the password the volume was written under.
func TestRepairCredentials_ClaimsASecretWhoseOwnerHasGone(t *testing.T) {
	ctx := context.Background()
	m := managerWith([]runtime.Object{
		credentialsSecret("db-credentials", ownedBy("Service", "db", "uid-db-before-the-restore")),
	}, serviceCR("db", "uid-db"))

	audit, err := m.AuditCredentials(ctx, "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if audit.Services[0].State != CredentialAbandoned {
		t.Fatalf("state = %q, want %q", audit.Services[0].State, CredentialAbandoned)
	}
	if _, err := m.RepairCredentials(ctx, "shop-prod", audit); err != nil {
		t.Fatalf("repair: %v", err)
	}

	got, err := m.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading repaired secret: %v", err)
	}
	// One controller, not two. Appending would leave the dead reference beside
	// the live one, and an object has exactly one controller.
	if len(got.OwnerReferences) != 1 {
		t.Fatalf("owner references = %+v, want exactly one", got.OwnerReferences)
	}
	owner := metav1.GetControllerOf(got)
	if owner == nil || owner.UID != types.UID("uid-db") {
		t.Fatalf("owner = %+v, want the live CR's UID", got.OwnerReferences)
	}
	if string(got.Data["PASSWORD"]) != "s3cret" {
		t.Fatalf("repair must not touch the credentials themselves, got %q", got.Data["PASSWORD"])
	}
}

// The audit and the write are separate reads. A Secret that gained an owner that
// exists in between is one repair must not take, whatever the audit said a
// moment ago.
func TestRepairCredentials_RefusesASecretThatGainedALiveOwner(t *testing.T) {
	ctx := context.Background()
	m := managerWith([]runtime.Object{
		credentialsSecret("db-credentials", nil),
	}, serviceCR("db", "uid-db"), appCR("web", "uid-web"))

	audit, err := m.AuditCredentials(ctx, "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}

	// Somebody claims it between the audit and the repair.
	taken, err := m.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading secret: %v", err)
	}
	taken.OwnerReferences = []metav1.OwnerReference{*ownedBy("App", "web", "uid-web")}
	if _, err := m.Client.CoreV1().Secrets("shop-prod").Update(ctx, taken, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("simulating the claim: %v", err)
	}

	if _, err := m.RepairCredentials(ctx, "shop-prod", audit); err == nil {
		t.Fatal("repair took a Secret from the object that had just claimed it")
	}

	got, err := m.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading secret: %v", err)
	}
	if owner := metav1.GetControllerOf(got); owner == nil || owner.Name != "web" {
		t.Fatalf("owner = %+v, want the App that claimed it", got.OwnerReferences)
	}
}

// A reference that died between the audit and the write is still repairable: the
// object it named is gone either way, and refusing would leave the Secret to be
// collected.
func TestRepairCredentials_ClaimsASecretWhoseOwnerDiedAfterTheAudit(t *testing.T) {
	ctx := context.Background()
	m := managerWith([]runtime.Object{
		credentialsSecret("db-credentials", nil),
	}, serviceCR("db", "uid-db"))

	audit, err := m.AuditCredentials(ctx, "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}

	taken, err := m.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading secret: %v", err)
	}
	taken.OwnerReferences = []metav1.OwnerReference{*ownedBy("App", "web", "uid-web-that-is-gone")}
	if _, err := m.Client.CoreV1().Secrets("shop-prod").Update(ctx, taken, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("simulating the dead reference: %v", err)
	}

	if _, err := m.RepairCredentials(ctx, "shop-prod", audit); err != nil {
		t.Fatalf("repair refused a reference with nothing behind it: %v", err)
	}

	got, err := m.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading repaired secret: %v", err)
	}
	if owner := metav1.GetControllerOf(got); owner == nil || owner.UID != types.UID("uid-db") {
		t.Fatalf("owner = %+v, want the live CR's UID", got.OwnerReferences)
	}
}

// Service is a core kind as well as one of ours. Matching on the kind alone
// would look this reference up as a kipper.run Service, find the service being
// audited under that name, compare a UID that was never going to match, and call
// a live claim abandoned. Repair would then take a Secret from an object that is
// still holding it, which is the one mistake it must never make.
func TestAuditCredentials_DoesNotMistakeACoreKindForOurs(t *testing.T) {
	controller := true
	coreService := &metav1.OwnerReference{
		APIVersion: "v1", Kind: "Service",
		Name: "db", UID: "the-core-service", Controller: &controller,
	}
	m := managerWith(
		[]runtime.Object{credentialsSecret("db-credentials", coreService)},
		serviceCR("db", "uid-db"),
	)

	audit, err := m.AuditCredentials(context.Background(), "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	if audit.Services[0].State != CredentialForeign {
		t.Errorf("state = %q, want %q: a claim this command cannot check was treated as lapsed",
			audit.Services[0].State, CredentialForeign)
	}
}

// The UID has to come from the service as it is at the moment of the write. A
// service deleted and recreated between the audit and the repair has a new
// identity, and writing the one the audit saw would install a fresh dangling
// reference while reporting success, which is the state this command exists to
// clear.
func TestRepairCredentials_ClaimsWithTheUIDTheServiceHasNow(t *testing.T) {
	ctx := context.Background()
	m := managerWith([]runtime.Object{credentialsSecret("db-credentials", nil)}, serviceCR("db", "uid-db"))

	audit, err := m.AuditCredentials(ctx, "shop-prod")
	if err != nil {
		t.Fatalf("audit: %v", err)
	}

	// The service is recreated inside the repair, after any list it might take
	// and before the write. Reading the Secret is the last step before that
	// write, so it is the moment to slip the new identity in.
	recreated := false
	m.Client.(*fake.Clientset).PrependReactor("get", "secrets",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			if recreated {
				return false, nil, nil
			}
			recreated = true
			if err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace("shop-prod").
				Delete(ctx, "db", metav1.DeleteOptions{}); err != nil {
				t.Fatalf("deleting the service: %v", err)
			}
			if _, err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace("shop-prod").
				Create(ctx, serviceCR("db", "uid-db-recreated"), metav1.CreateOptions{}); err != nil {
				t.Fatalf("recreating the service: %v", err)
			}
			return false, nil, nil
		})

	if _, err := m.RepairCredentials(ctx, "shop-prod", audit); err != nil {
		t.Fatalf("repair: %v", err)
	}

	got, err := m.Client.CoreV1().Secrets("shop-prod").Get(ctx, "db-credentials", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading repaired secret: %v", err)
	}
	owner := metav1.GetControllerOf(got)
	if owner == nil || owner.UID != types.UID("uid-db-recreated") {
		t.Fatalf("owner = %+v, want the identity the service has now", got.OwnerReferences)
	}
}

// Replacing the controller takes the controller and nothing else. An object can
// carry references that only mark a relationship, and repair has no business
// dropping them.
func TestRepairCredentials_KeepsReferencesThatDoNotControl(t *testing.T) {
	ctx := context.Background()
	secret := credentialsSecret("db-credentials", ownedBy("Service", "db", "uid-db-before-the-restore"))
	secret.OwnerReferences = append(secret.OwnerReferences, metav1.OwnerReference{
		APIVersion: manifest.AppGVR.GroupVersion().String(),
		Kind:       "App", Name: "web", UID: "uid-web",
	})
	m := managerWith([]runtime.Object{secret}, serviceCR("db", "uid-db"))

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
	if len(got.OwnerReferences) != 2 {
		t.Fatalf("owner references = %+v, want the marker kept beside the new controller", got.OwnerReferences)
	}
	var kept bool
	for _, ref := range got.OwnerReferences {
		if ref.Kind == "App" && ref.Name == "web" {
			kept = true
		}
	}
	if !kept {
		t.Fatalf("a reference that controls nothing was dropped: %+v", got.OwnerReferences)
	}
}
