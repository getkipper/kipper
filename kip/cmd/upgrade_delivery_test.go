package cmd

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/installer"
)

// liveConsoleAPI is a console-api Deployment as an older install left it: no
// security context, no /tmp, and whatever env the cluster ended up with.
func liveConsoleAPI(env ...corev1.EnvVar) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "console-api", Namespace: "kipper-system"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "console-api", Image: "ghcr.io/example/console-api:latest", Env: env}},
				},
			},
		},
	}
}

// An upgrade must not restart a cluster-powerful component onto a new image and
// leave it with the privileges of whenever the cluster was installed.
func TestUpgradeAppliesPodHardening(t *testing.T) {
	dep := liveConsoleAPI()

	changed, err := applyConsoleAPIHardening(dep)
	if err != nil {
		t.Fatalf("harden: %v", err)
	}
	if !changed {
		t.Fatal("an unhardened deployment must be reported as changed")
	}

	pod := dep.Spec.Template.Spec
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Error("pod must run as non-root")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser != 65532 {
		t.Error("pod must run as UID 65532")
	}
	if pod.SecurityContext.SeccompProfile == nil || pod.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Error("pod must carry the RuntimeDefault seccomp profile")
	}
	ctr := pod.Containers[0]
	if ctr.SecurityContext == nil || ctr.SecurityContext.ReadOnlyRootFilesystem == nil || !*ctr.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("container needs a read-only root filesystem")
	}
	if ctr.SecurityContext.AllowPrivilegeEscalation == nil || *ctr.SecurityContext.AllowPrivilegeEscalation {
		t.Error("container must refuse privilege escalation")
	}
	if ctr.SecurityContext.Capabilities == nil || len(ctr.SecurityContext.Capabilities.Drop) == 0 {
		t.Error("container must drop capabilities")
	}
	// A read-only rootfs without the writable /tmp would crash-loop the pod.
	if volumeIndex(pod.Volumes, "tmp") < 0 {
		t.Error("the tmp volume must be added alongside the read-only rootfs")
	}
	if mountIndex(ctr.VolumeMounts, "tmp") < 0 {
		t.Error("the tmp mount must be added alongside the read-only rootfs")
	}

	// Idempotent: a second pass changes nothing, so a routine upgrade does not
	// churn the Deployment.
	again, err := applyConsoleAPIHardening(dep)
	if err != nil {
		t.Fatalf("second harden: %v", err)
	}
	if again {
		t.Error("hardening an already-hardened deployment must be a no-op")
	}
}

// Volumes an operator added by hand are merged with, not replaced by, the
// installer's set.
func TestUpgradeHardeningKeepsOperatorVolumes(t *testing.T) {
	dep := liveConsoleAPI()
	dep.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "extra", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	dep.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "extra", MountPath: "/extra"}}

	if _, err := applyConsoleAPIHardening(dep); err != nil {
		t.Fatalf("harden: %v", err)
	}
	if volumeIndex(dep.Spec.Template.Spec.Volumes, "extra") < 0 {
		t.Error("an operator-added volume must survive")
	}
	if volumeIndex(dep.Spec.Template.Spec.Volumes, "tmp") < 0 {
		t.Error("the installer volume must still be added")
	}
}

func clusterIdentityObj(gateway map[string]any) *unstructured.Unstructured {
	spec := map[string]any{"domain": "acme.kipper.run"}
	if gateway != nil {
		spec["gateway"] = gateway
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "ClusterIdentity",
		"metadata":   map[string]any{"name": "cluster"},
		"spec":       spec,
	}}
}

func fakeIdentityDynamic(objs ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(clusterIdentityGVR.GroupVersion().WithKind("ClusterIdentity"), &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(clusterIdentityGVR.GroupVersion().WithKind("ClusterIdentityList"), &unstructured.UnstructuredList{})
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{clusterIdentityGVR: "ClusterIdentityList"}, objs...)
}

func gatewayField(t *testing.T, dyn *dynamicfake.FakeDynamicClient, field string) string {
	t.Helper()
	ci, err := dyn.Resource(clusterIdentityGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get identity: %v", err)
	}
	v, _, _ := unstructured.NestedString(ci.Object, "spec", "gateway", field)
	return v
}

// The reconciler renders CLUSTER_HOST from the CR, so an upgrade has to record
// the host there — otherwise the next identity change drops the heartbeat.
func TestUpgradeRecordsClusterHostOnTheIdentity(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(map[string]any{"kipperRunDomain": "acme.kipper.run", "register": true}))
	clientset := fake.NewSimpleClientset(liveConsoleAPI()) //nolint:staticcheck // matches the pattern used across kip tests

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "203.0.113.7"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if got := gatewayField(t, dyn, "clusterHost"); got != "203.0.113.7" {
		t.Errorf("clusterHost = %q, want the host this CLI manages", got)
	}
	if got := gatewayField(t, dyn, "kipperRunDomain"); got != "acme.kipper.run" {
		t.Errorf("kipperRunDomain must be preserved, got %q", got)
	}
}

// A cluster installed before the CR carried a gateway block still has the truth
// in its running Deployment; adopt it rather than leaving the cluster one
// transition away from losing its registration.
func TestUpgradeAdoptsGatewayBlockFromLiveEnv(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(nil))
	clientset := fake.NewSimpleClientset(liveConsoleAPI( //nolint:staticcheck
		corev1.EnvVar{Name: "KIPPER_RUN_DOMAIN", Value: "acme.kipper.run"},
		corev1.EnvVar{Name: "CLUSTER_HOST", Value: "203.0.113.7"},
	))

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "203.0.113.7"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if got := gatewayField(t, dyn, "kipperRunDomain"); got != "acme.kipper.run" {
		t.Errorf("the live registration must be adopted onto the CR, got %q", got)
	}
	if got := gatewayField(t, dyn, "clusterHost"); got != "203.0.113.7" {
		t.Errorf("clusterHost = %q", got)
	}
}

// With no registration anywhere — no CR block, no live env — nothing is
// invented: only the operator knows whether the cluster should hold a
// *.kipper.run name.
func TestUpgradeInventsNoGatewayRegistration(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(nil))
	clientset := fake.NewSimpleClientset(liveConsoleAPI()) //nolint:staticcheck

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "203.0.113.7"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if got := gatewayField(t, dyn, "kipperRunDomain"); got != "" {
		t.Errorf("no registration must be invented, got %q", got)
	}
}

// The gateway registers an address and refuses anything that is not a public IP,
// so a cluster reached by hostname must not have that hostname recorded as the
// value the heartbeat will try to register.
func TestUpgradeRefusesToRecordANonIPHost(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(map[string]any{"kipperRunDomain": "acme.kipper.run"}))
	clientset := fake.NewSimpleClientset(liveConsoleAPI()) //nolint:staticcheck

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "cluster.example.com"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if got := gatewayField(t, dyn, "clusterHost"); got != "" {
		t.Errorf("a hostname must not be recorded as the registration address, got %q", got)
	}
}

// When the CLI is reaching the cluster by name, the address the cluster is
// already registering with is the one to keep.
func TestUpgradeKeepsTheAddressAlreadyInUse(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(map[string]any{"kipperRunDomain": "acme.kipper.run"}))
	clientset := fake.NewSimpleClientset(liveConsoleAPI( //nolint:staticcheck
		corev1.EnvVar{Name: "CLUSTER_HOST", Value: "203.0.113.7"},
	))

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "cluster.example.com"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if got := gatewayField(t, dyn, "clusterHost"); got != "203.0.113.7" {
		t.Errorf("clusterHost = %q, want the address the cluster already uses", got)
	}
}

// A cluster that moved to a custom domain still records the name it used to
// serve. The upgrade names it instead of leaving the operator to work out what
// went missing — but still does not decide to revive it.
func TestUpgradeNamesTheFormerKipperRunIdentity(t *testing.T) {
	ci := clusterIdentityObj(nil)
	ci.Object["spec"].(map[string]any)["domain"] = "console.example.com"
	ci.Object["status"] = map[string]any{
		"lastSteady": map[string]any{"domain": "acme.kipper.run"},
	}
	dyn := fakeIdentityDynamic(ci)
	clientset := fake.NewSimpleClientset(liveConsoleAPI()) //nolint:staticcheck

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "203.0.113.7"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if got := gatewayField(t, dyn, "kipperRunDomain"); got != "" {
		t.Errorf("a former identity must not be revived automatically, got %q", got)
	}
	if got := formerKipperRunDomain(ci); got != "acme.kipper.run" {
		t.Errorf("the former identity must be recoverable for the operator, got %q", got)
	}
}

// The local config can be stale in a way the cluster-side values cannot, so an
// address the cluster is already registering with wins over it.
func TestUpgradeKeepsRecordedAddressOverStaleLocalConfig(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(map[string]any{
		"kipperRunDomain": "acme.kipper.run",
		"clusterHost":     "203.0.113.7",
	}))
	clientset := fake.NewSimpleClientset(liveConsoleAPI( //nolint:staticcheck
		corev1.EnvVar{Name: "CLUSTER_HOST", Value: "203.0.113.7"},
	))

	// A stale but perfectly parseable IP in ~/.kip/config.yaml.
	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "198.51.100.9"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if got := gatewayField(t, dyn, "clusterHost"); got != "203.0.113.7" {
		t.Errorf("a recorded address must not be replaced by the local config, got %q", got)
	}
}

// A "tmp" volume that is not the installer's writable one would leave the
// read-only root filesystem without anywhere to write.
func TestUpgradeHardeningReconcilesAConflictingTmpVolume(t *testing.T) {
	dep := liveConsoleAPI()
	dep.Spec.Template.Spec.Volumes = []corev1.Volume{{
		Name:         "tmp",
		VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/mnt/somewhere"}},
	}}
	dep.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "tmp", MountPath: "/somewhere-else"}}

	if _, err := applyConsoleAPIHardening(dep); err != nil {
		t.Fatalf("harden: %v", err)
	}
	vol := dep.Spec.Template.Spec.Volumes[volumeIndex(dep.Spec.Template.Spec.Volumes, "tmp")]
	if vol.EmptyDir == nil || vol.HostPath != nil {
		t.Errorf("the installer's tmp volume must win over a same-named one, got %+v", vol)
	}
	mounts := dep.Spec.Template.Spec.Containers[0].VolumeMounts
	if got := mounts[mountIndex(mounts, "tmp")].MountPath; got != "/tmp" {
		t.Errorf("the tmp mount must land on /tmp, got %q", got)
	}
}

// An opt-out decides whether to register, not whether the cluster is allowed to
// remember what it is. The address is recorded (the env is cleared while the
// opt-out stands, so the CR is the only copy left to turn registration back on
// with) and register itself is never touched.
func TestUpgradeRecordsIdentityButNeverReArmsAnOptOut(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(map[string]any{
		"kipperRunDomain": "acme.kipper.run",
		"register":        false,
	}))
	clientset := fake.NewSimpleClientset(liveConsoleAPI( //nolint:staticcheck
		corev1.EnvVar{Name: "CLUSTER_HOST", Value: "203.0.113.7"},
	))

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "203.0.113.7"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if got := gatewayField(t, dyn, "clusterHost"); got != "203.0.113.7" {
		t.Errorf("the address must survive an opt-out so it can be turned back on, got %q", got)
	}
	ci, err := dyn.Resource(clusterIdentityGVR).Get(context.Background(), "cluster", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get identity: %v", err)
	}
	if register, _, _ := unstructured.NestedBool(ci.Object, "spec", "gateway", "register"); register {
		t.Error("an upgrade must never re-arm a registration the operator turned off")
	}
}

// A cluster that has opted out and has nothing recorded is simply left alone.
func TestUpgradeSaysNothingToRecordForAnOptedOutCluster(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(map[string]any{"register": false}))
	clientset := fake.NewSimpleClientset(liveConsoleAPI()) //nolint:staticcheck

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "203.0.113.7"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if got := gatewayField(t, dyn, "kipperRunDomain"); got != "" {
		t.Errorf("nothing to record means nothing recorded, got %q", got)
	}
}

// captureStdout runs fn and returns what it printed.
func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stdout
	os.Stdout = w
	fn()
	os.Stdout = saved
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return string(out)
}

// A server that really moved keeps the recorded address in both the CR and the
// Deployment (steady reconciliation puts it back), so the address kip itself
// reached is the only signal anything changed. It has to be reported, or the
// operator has no way to notice.
func TestUpgradeReportsAnAddressDisagreementFromTheLocalHostAlone(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(map[string]any{
		"kipperRunDomain": "acme.kipper.run",
		"clusterHost":     "203.0.113.7",
	}))
	clientset := fake.NewSimpleClientset(liveConsoleAPI( //nolint:staticcheck
		corev1.EnvVar{Name: "CLUSTER_HOST", Value: "203.0.113.7"},
	))

	out := captureStdout(t, func() {
		if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "198.51.100.9"); err != nil {
			t.Fatalf("reconcile identity: %v", err)
		}
	})

	if !strings.Contains(out, "198.51.100.9") {
		t.Errorf("the differing address must be reported, got:\n%s", out)
	}
	if !strings.Contains(out, "203.0.113.7") {
		t.Errorf("the kept address must be named, got:\n%s", out)
	}
	if got := gatewayField(t, dyn, "clusterHost"); got != "203.0.113.7" {
		t.Errorf("the recorded address must still be kept, got %q", got)
	}
}

// Agreement is quiet: nothing to report when every source names one address —
// including when they spell that one address differently.
func TestUpgradeSaysNothingWhenAddressesAgree(t *testing.T) {
	for _, tc := range []struct{ name, cr, live, local string }{
		{"identical spellings", "203.0.113.7", "203.0.113.7", "203.0.113.7"},
		{"equivalent IPv6 spellings", "2001:db8::1", "2001:0db8:0:0:0:0:0:1", "2001:DB8::1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dyn := fakeIdentityDynamic(clusterIdentityObj(map[string]any{
				"kipperRunDomain": "acme.kipper.run",
				"clusterHost":     tc.cr,
			}))
			clientset := fake.NewSimpleClientset(liveConsoleAPI( //nolint:staticcheck
				corev1.EnvVar{Name: "CLUSTER_HOST", Value: tc.live},
			))

			out := captureStdout(t, func() {
				if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, tc.local); err != nil {
					t.Fatalf("reconcile identity: %v", err)
				}
			})
			if strings.Contains(out, "also answers at") {
				t.Errorf("one address spelled several ways must not warn, got:\n%s", out)
			}
		})
	}
}

// An old install can leave the cluster's own domain in KIPPER_RUN_DOMAIN even
// though the cluster never registered with the gateway. Adopting that would
// record a name the gateway can never serve.
func TestUpgradeIgnoresANonKipperRunDomainInTheLiveEnv(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(nil))
	clientset := fake.NewSimpleClientset(liveConsoleAPI( //nolint:staticcheck
		corev1.EnvVar{Name: "KIPPER_RUN_DOMAIN", Value: "acme.example.com"},
		corev1.EnvVar{Name: "CLUSTER_HOST", Value: "203.0.113.7"},
	))

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "203.0.113.7"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if got := gatewayField(t, dyn, "kipperRunDomain"); got != "" {
		t.Errorf("a domain that is not a kipper.run name must not be adopted, got %q", got)
	}
}

// rolledOut returns a Deployment whose status says its current generation is
// fully rolled out.
func rolledOut() *appsv1.Deployment {
	const gen = 3
	dep := liveConsoleAPI()
	dep.Generation = gen
	dep.Spec.Replicas = ptrInt32(1)
	dep.Status = appsv1.DeploymentStatus{
		ObservedGeneration: gen, UpdatedReplicas: 1, AvailableReplicas: 1, UnavailableReplicas: 0,
	}
	return dep
}

func ptrInt32(v int32) *int32 { return &v }

// An accepted template is not a running release. The upgrade has to wait for the
// pods, or a console-api that cannot start under the new hardening is reported as
// a success while the previous pod keeps serving.
func TestWaitForRolloutOutcomes(t *testing.T) {
	ctx := context.Background()

	t.Run("ready returns immediately", func(t *testing.T) {
		clientset := fake.NewSimpleClientset(rolledOut()) //nolint:staticcheck
		if err := waitForRollout(ctx, clientset, "kipper-system", "console-api", time.Second); err != nil {
			t.Errorf("a rolled-out deployment must pass: %v", err)
		}
	})

	t.Run("gives up when Kubernetes has", func(t *testing.T) {
		dep := rolledOut()
		dep.Status.UpdatedReplicas = 0
		dep.Status.AvailableReplicas = 1 // the old pod is still serving
		dep.Status.Conditions = []appsv1.DeploymentCondition{{
			Type:    appsv1.DeploymentProgressing,
			Status:  corev1.ConditionFalse,
			Reason:  "ProgressDeadlineExceeded",
			Message: "ReplicaSet \"console-api-abc\" has timed out progressing",
		}}
		clientset := fake.NewSimpleClientset(dep) //nolint:staticcheck

		err := waitForRollout(ctx, clientset, "kipper-system", "console-api", time.Minute)
		if err == nil {
			t.Fatal("a failed rollout must not pass")
		}
		if !strings.Contains(err.Error(), "timed out progressing") {
			t.Errorf("the reason Kubernetes gave must reach the operator, got %v", err)
		}
	})

	t.Run("reports what is stuck when the budget runs out", func(t *testing.T) {
		dep := rolledOut()
		dep.Status.UpdatedReplicas = 0            // never progresses, and never declared failed
		clientset := fake.NewSimpleClientset(dep) //nolint:staticcheck

		err := waitForRollout(ctx, clientset, "kipper-system", "console-api", 50*time.Millisecond)
		if err == nil {
			t.Fatal("a stalled rollout must not pass")
		}
		for _, want := range []string{"still rolling out", "0/1", "describe deploy"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the timeout message must contain %q, got %v", want, err)
			}
		}
	})
}

// The reconciler writes to the same Deployment under its own retry, so the CLI
// losing a race is routine and must not end an upgrade the operator would only
// have to run again.
func TestUpdateDeploymentWithRetrySurvivesAConflict(t *testing.T) {
	dep := rolledOut()
	clientset := fake.NewSimpleClientset(dep) //nolint:staticcheck

	conflicts := 1
	clientset.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "apps", Resource: "deployments"}, "console-api",
				errors.New("the object has been modified"))
		}
		return false, nil, nil
	})

	applied := 0
	err := updateDeploymentWithRetry(context.Background(), clientset, "kipper-system", "console-api", dep,
		func(d *appsv1.Deployment) error {
			applied++
			d.Spec.Template.Spec.Containers[0].Image = "ghcr.io/example/console-api:new"
			return nil
		})
	if err != nil {
		t.Fatalf("a single conflict must be retried, got %v", err)
	}
	if applied < 2 {
		t.Errorf("the mutation must be re-applied to the re-read object, applied %d time(s)", applied)
	}

	got, err := clientset.AppsV1().Deployments("kipper-system").Get(context.Background(), "console-api", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Spec.Template.Spec.Containers[0].Image != "ghcr.io/example/console-api:new" {
		t.Errorf("the update must land after the retry, image is %q", got.Spec.Template.Spec.Containers[0].Image)
	}
}

// The reconciler writes CR status on nearly every pass, so the backfill losing
// that race is routine. It must re-read and re-apply rather than ending an
// upgrade before a single component has been touched.
func TestUpgradeBackfillSurvivesAClusterIdentityConflict(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(map[string]any{"kipperRunDomain": "acme.kipper.run"}))
	clientset := fake.NewSimpleClientset(liveConsoleAPI( //nolint:staticcheck
		corev1.EnvVar{Name: "CLUSTER_HOST", Value: "203.0.113.7"},
	))

	conflicts := 1
	dyn.PrependReactor("update", "clusteridentities", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "kipper.run", Resource: "clusteridentities"}, "cluster",
				errors.New("the object has been modified"))
		}
		return false, nil, nil
	})

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "203.0.113.7"); err != nil {
		t.Fatalf("a single conflict must be retried, got %v", err)
	}
	if got := gatewayField(t, dyn, "clusterHost"); got != "203.0.113.7" {
		t.Errorf("the backfill must land after the retry, clusterHost is %q", got)
	}
}

// A concurrent writer that recorded an address first keeps it: the retry
// re-decides against the object it just read, and an address already recorded
// outranks the local config — so the retry stands down instead of overwriting it.
func TestUpgradeBackfillYieldsToAConcurrentlyRecordedAddress(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(map[string]any{"kipperRunDomain": "acme.kipper.run"}))
	clientset := fake.NewSimpleClientset(liveConsoleAPI()) //nolint:staticcheck

	// What another writer recorded while our first Update was in flight.
	concurrent := clusterIdentityObj(map[string]any{
		"kipperRunDomain": "acme.kipper.run",
		"clusterHost":     "198.51.100.9",
	})

	conflicted := false
	updates := 0
	dyn.PrependReactor("get", "clusteridentities", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return true, concurrent, nil
		}
		return false, nil, nil
	})
	dyn.PrependReactor("update", "clusteridentities", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if !conflicted {
			conflicted = true
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "kipper.run", Resource: "clusteridentities"}, "cluster",
				errors.New("the object has been modified"))
		}
		return false, nil, nil
	})

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "203.0.113.7"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if updates != 1 {
		t.Errorf("the retry must stand down once it reads a recorded address, but issued %d update(s)", updates)
	}
}

// A concurrent writer that recorded a different identity keeps it whole: the
// retry must not pair its fresh address with the domain this run captured before
// the conflict.
func TestUpgradeBackfillNeverSplitsAConcurrentlyRecordedPair(t *testing.T) {
	dyn := fakeIdentityDynamic(clusterIdentityObj(nil))
	clientset := fake.NewSimpleClientset(liveConsoleAPI( //nolint:staticcheck
		corev1.EnvVar{Name: "KIPPER_RUN_DOMAIN", Value: "acme.kipper.run"},
		corev1.EnvVar{Name: "CLUSTER_HOST", Value: "203.0.113.7"},
	))

	// What another writer recorded while our first Update was in flight: a
	// different domain, with its own address.
	concurrent := clusterIdentityObj(map[string]any{
		"kipperRunDomain": "moved.kipper.run",
		"clusterHost":     "198.51.100.9",
	})

	conflicted := false
	updates := 0
	dyn.PrependReactor("get", "clusteridentities", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return true, concurrent, nil
		}
		return false, nil, nil
	})
	dyn.PrependReactor("update", "clusteridentities", func(k8stesting.Action) (bool, runtime.Object, error) {
		updates++
		if !conflicted {
			conflicted = true
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "kipper.run", Resource: "clusteridentities"}, "cluster",
				errors.New("the object has been modified"))
		}
		return false, nil, nil
	})

	if err := reconcileGatewayIdentity(context.Background(), dyn, clientset, "203.0.113.7"); err != nil {
		t.Fatalf("reconcile identity: %v", err)
	}
	if updates != 1 {
		t.Errorf("a concurrently recorded identity must be left whole, but the retry issued %d update(s)", updates)
	}
}

// retiredSidecarRef stands for whatever registry a long-lived cluster was
// installed against before a first-party image moved. It must never match what
// this build ships, which TestRetiredSidecarRefIsNotTheShippedReference holds
// to.
const retiredSidecarRef = "ghcr.io/retired-registry/kipper-sidecar:latest"

// Every repoint test asserts that a stale value gets replaced, so all of them
// pass vacuously the moment the stale value stops being stale. An org rename
// applied across the tree is exactly how that happens, and it happened once.
func TestRetiredSidecarRefIsNotTheShippedReference(t *testing.T) {
	desired, err := installer.DesiredConsoleAPIDeployment()
	if err != nil {
		t.Fatalf("render the shipped manifest: %v", err)
	}
	for _, c := range desired.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if e.Value == retiredSidecarRef {
				t.Fatalf("the retired fixture %q is what this build ships, so every repoint test now passes on a no-op", retiredSidecarRef)
			}
		}
	}
}

// A cluster installed before a first-party image moved keeps naming the old
// location in the env other pods pull from, so every app pod that gets a
// sidecar fails to pull it while console-api itself is perfectly current.
// The fixture names a registry this build does not ship from rather than any
// org Kipper has actually used, so the next move cannot quietly turn the stale
// value into the current one and leave the test passing on a no-op.
func TestUpgradeRepointsPlatformImageEnv(t *testing.T) {
	dep := liveConsoleAPI()
	ctr := &dep.Spec.Template.Spec.Containers[0]
	ctr.Env = []corev1.EnvVar{
		{Name: "SIDECAR_IMAGE", Value: retiredSidecarRef},
		{Name: "CLUSTER_DOMAIN", Value: "acme.example.com"},
		{Name: "KIPPER_DISABLE_OUTBOUND_MIGRATION", Value: "1"},
		{Name: "SOME_OTHER_IMAGE", Value: "example.com/operator-owned:v1"},
	}

	moved, err := applyPlatformImageEnv(dep)
	if err != nil {
		t.Fatalf("repoint: %v", err)
	}
	if len(moved) == 0 {
		t.Fatal("a stale image reference must be reported as moved")
	}
	joined := strings.Join(moved, "; ")
	if !strings.Contains(joined, "SIDECAR_IMAGE") || !strings.Contains(joined, "→") {
		t.Errorf("the report must name the variable and both values, got %q", joined)
	}

	env := map[string]corev1.EnvVar{}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e
	}
	if got := env["SIDECAR_IMAGE"].Value; got == retiredSidecarRef || got == "" {
		t.Errorf("SIDECAR_IMAGE must be repointed at the shipped reference, got %q", got)
	}
	if env["DATAMOVER_IMAGE"].Value == "" {
		t.Error("an owned image var the cluster never had must be added")
	}
	// Cluster identity, operator controls, and any variable not on the owned
	// list are none of this function's business — the list is explicit precisely
	// so a name that merely looks like an image reference is left alone.
	if env["CLUSTER_DOMAIN"].Value != "acme.example.com" {
		t.Errorf("cluster identity env must be left alone, got %q", env["CLUSTER_DOMAIN"].Value)
	}
	if env["KIPPER_DISABLE_OUTBOUND_MIGRATION"].Value != "1" {
		t.Error("operator env must be left alone")
	}
	if env["SOME_OTHER_IMAGE"].Value != "example.com/operator-owned:v1" {
		t.Errorf("a variable not on the owned list must be left alone, got %q", env["SOME_OTHER_IMAGE"].Value)
	}

	again, err := applyPlatformImageEnv(dep)
	if err != nil {
		t.Fatalf("second repoint: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("repointing already-current references must be a no-op, got %v", again)
	}
}

// value and valueFrom are mutually exclusive. Setting one while the other stands
// makes the whole Deployment invalid, so the upgrade would fail on the API call
// rather than on anything the operator could read.
func TestUpgradeRepointClearsAValueFromReference(t *testing.T) {
	dep := liveConsoleAPI()
	dep.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{{
		Name: "SIDECAR_IMAGE",
		ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "somewhere"},
			Key:                  "sidecar",
		}},
	}}

	if _, err := applyPlatformImageEnv(dep); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name != "SIDECAR_IMAGE" {
			continue
		}
		if e.ValueFrom != nil {
			t.Error("valueFrom must be cleared when a literal value is written")
		}
		if e.Value == "" {
			t.Error("the shipped reference must be written")
		}
	}
}

// The kubelet serves the last entry for a duplicated name, so writing only the
// first would report a move the pod never sees.
func TestUpgradeRepointWritesEveryDuplicateEntry(t *testing.T) {
	dep := liveConsoleAPI()
	dep.Spec.Template.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "SIDECAR_IMAGE", Value: retiredSidecarRef},
		{Name: "SIDECAR_IMAGE", Value: retiredSidecarRef},
	}

	if _, err := applyPlatformImageEnv(dep); err != nil {
		t.Fatalf("repoint: %v", err)
	}
	for i, e := range dep.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "SIDECAR_IMAGE" && e.Value == retiredSidecarRef {
			t.Errorf("entry %d still carries the unpullable reference", i)
		}
	}
}
