package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

func quotaTestProject(tier string, envs ...kipperv1.ProjectEnvironment) *kipperv1.Project {
	return &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "shop",
			Finalizers: []string{projectFinalizer},
		},
		Spec: kipperv1.ProjectSpec{Tier: tier, Environments: envs},
	}
}

func reconcileProject(t *testing.T, objs ...crclient.Object) crclient.Client {
	t.Helper()
	scheme := testScheme()
	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(append(objs, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9"))...).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.NoError(t, err)
	return fakeClient
}

func TestReconcileQuota_TierDefault(t *testing.T) {
	fakeClient := reconcileProject(t, quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"}))

	var quota corev1.ResourceQuota
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: projectQuotaName, Namespace: "shop-test"}, &quota)
	require.NoError(t, err)
	assert.Equal(t, "2", quota.Spec.Hard.Name(corev1.ResourceRequestsCPU, resource.DecimalSI).String())
	assert.Equal(t, "6", quota.Spec.Hard.Name(corev1.ResourceLimitsCPU, resource.DecimalSI).String())
	assert.Equal(t, "6Gi", quota.Spec.Hard.Name(corev1.ResourceRequestsMemory, resource.BinarySI).String())
	assert.Equal(t, "12Gi", quota.Spec.Hard.Name(corev1.ResourceLimitsMemory, resource.BinarySI).String())

	var lr corev1.LimitRange
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: projectLimitRangeName, Namespace: "shop-test"}, &lr)
	require.NoError(t, err)
	require.Len(t, lr.Spec.Limits, 1)
	assert.Equal(t, corev1.LimitTypeContainer, lr.Spec.Limits[0].Type)
	assert.Equal(t, "25m", lr.Spec.Limits[0].DefaultRequest.Cpu().String())
	assert.Equal(t, "128Mi", lr.Spec.Limits[0].Default.Memory().String())
}

func TestReconcileQuota_MediumTierAndDefaultEnvNamespace(t *testing.T) {
	// No explicit environments: the project runs on the defaulted "test" env.
	fakeClient := reconcileProject(t, quotaTestProject("medium"))

	var quota corev1.ResourceQuota
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: projectQuotaName, Namespace: "shop-test"}, &quota)
	require.NoError(t, err)
	assert.Equal(t, "4", quota.Spec.Hard.Name(corev1.ResourceRequestsCPU, resource.DecimalSI).String())
	assert.Equal(t, "24Gi", quota.Spec.Hard.Name(corev1.ResourceLimitsMemory, resource.BinarySI).String())
}

func TestReconcileQuota_EnvOverrideWins(t *testing.T) {
	project := quotaTestProject("small",
		kipperv1.ProjectEnvironment{Name: "test"},
		kipperv1.ProjectEnvironment{Name: "prod", Quota: &kipperv1.EnvQuota{
			CPURequest: "6", CPULimit: "12", MemoryRequest: "12Gi", MemoryLimit: "24Gi",
		}},
	)
	fakeClient := reconcileProject(t, project)

	var prodQuota corev1.ResourceQuota
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: projectQuotaName, Namespace: "shop-prod"}, &prodQuota)
	require.NoError(t, err)
	assert.Equal(t, "6", prodQuota.Spec.Hard.Name(corev1.ResourceRequestsCPU, resource.DecimalSI).String())
	assert.Equal(t, "24Gi", prodQuota.Spec.Hard.Name(corev1.ResourceLimitsMemory, resource.BinarySI).String())

	// The sibling environment still gets the tier default.
	var testQuota corev1.ResourceQuota
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: projectQuotaName, Namespace: "shop-test"}, &testQuota)
	require.NoError(t, err)
	assert.Equal(t, "2", testQuota.Spec.Hard.Name(corev1.ResourceRequestsCPU, resource.DecimalSI).String())
}

func TestReconcileQuota_InvalidOverrideFailsReconcile(t *testing.T) {
	project := quotaTestProject("small",
		kipperv1.ProjectEnvironment{Name: "test", Quota: &kipperv1.EnvQuota{
			CPURequest: "two cpus", CPULimit: "4", MemoryRequest: "4Gi", MemoryLimit: "8Gi",
		}},
	)
	scheme := testScheme()
	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cpuRequest")
}

func TestReconcileQuota_UpdatesExistingQuotaOnTierChange(t *testing.T) {
	scheme := testScheme()
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")).
		WithStatusSubresource(&kipperv1.Project{}).
		Build()

	r := &ProjectReconciler{Client: fakeClient, Scheme: scheme, APIReader: fakeClient}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "shop"}}
	_, err := r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var stored kipperv1.Project
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &stored))
	stored.Spec.Tier = "large"
	require.NoError(t, fakeClient.Update(context.Background(), &stored))

	_, err = r.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var quota corev1.ResourceQuota
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: projectQuotaName, Namespace: "shop-test"}, &quota)
	require.NoError(t, err)
	assert.Equal(t, "8", quota.Spec.Hard.Name(corev1.ResourceRequestsCPU, resource.DecimalSI).String())
	assert.Equal(t, "48Gi", quota.Spec.Hard.Name(corev1.ResourceLimitsMemory, resource.BinarySI).String())
}

func TestReconcileQuota_GrandfathersNamespaceOverTierDefault(t *testing.T) {
	// A pre-quota namespace already runs more than the small tier allows
	// (requests 3 CPU > 2 CPU cap). The first reconcile must not cap below
	// current usage: it raises the quota and records an explicit override.
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-test", Labels: map[string]string{kipperlabels.Project: "shop"}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "big-app", Namespace: "shop-test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "main", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("3"),
						corev1.ResourceMemory: resource.MustParse("2Gi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("3"),
						corev1.ResourceMemory: resource.MustParse("4Gi"),
					},
				}},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}

	fakeClient := reconcileProject(t, project, ns, pod)

	// 3 CPU + 25% headroom = 3.75, rounded up to the next 500m = 4.
	var quota corev1.ResourceQuota
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: projectQuotaName, Namespace: "shop-test"}, &quota)
	require.NoError(t, err)
	assert.Equal(t, "4", quota.Spec.Hard.Name(corev1.ResourceRequestsCPU, resource.DecimalSI).String())
	// Dimensions under the tier default keep the tier value.
	assert.Equal(t, "6Gi", quota.Spec.Hard.Name(corev1.ResourceRequestsMemory, resource.BinarySI).String())
	assert.Equal(t, "12Gi", quota.Spec.Hard.Name(corev1.ResourceLimitsMemory, resource.BinarySI).String())

	// The raised cap is recorded as an explicit override on the environment.
	var stored kipperv1.Project
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	require.Len(t, stored.Spec.Environments, 1)
	override := stored.Spec.Environments[0].Quota
	require.NotNil(t, override)
	assert.Equal(t, "4", override.CPURequest)
	assert.Equal(t, "6Gi", override.MemoryRequest)
}

func TestReconcileQuota_GrandfatherSkipsTerminalPods(t *testing.T) {
	project := quotaTestProject("small", kipperv1.ProjectEnvironment{Name: "test"})
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "shop-test", Labels: map[string]string{kipperlabels.Project: "shop"}}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "finished-build", Namespace: "shop-test"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "build", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("6")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("6")},
				}},
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
	}

	fakeClient := reconcileProject(t, project, ns, pod)

	var quota corev1.ResourceQuota
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: projectQuotaName, Namespace: "shop-test"}, &quota)
	require.NoError(t, err)
	assert.Equal(t, "2", quota.Spec.Hard.Name(corev1.ResourceRequestsCPU, resource.DecimalSI).String(),
		"terminal pods do not count towards usage, so the tier default stands")

	var stored kipperv1.Project
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "shop"}, &stored))
	assert.Nil(t, stored.Spec.Environments[0].Quota, "no override recorded when usage fits the tier")
}

func TestPodEffective_InitContainersUseMaxNotSum(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			InitContainers: []corev1.Container{
				{Name: "init-a", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				}},
				{Name: "init-b", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")},
				}},
			},
			Containers: []corev1.Container{
				{Name: "main", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")},
				}},
				{Name: "kipper-instance-proxy", Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m")},
				}},
			},
		},
	}

	// App containers sum to 150m; the largest init container needs 500m, so
	// the pod's effective request is 500m, not 150m and not 850m.
	got := podEffective(pod, corev1.ResourceCPU, false)
	assert.Equal(t, "500m", got.String())

	// With a heavier app-container sum the sum wins.
	pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU] = resource.MustParse("600m")
	got = podEffective(pod, corev1.ResourceCPU, false)
	assert.Equal(t, "650m", got.String())
}

func TestTierQuota_UnknownTierFallsBackToSmall(t *testing.T) {
	assert.Equal(t, kipperv1.TierQuota("small"), kipperv1.TierQuota(""))
	assert.Equal(t, kipperv1.TierQuota("small"), kipperv1.TierQuota("galactic"))
}

// The small tier must fit the full lifecycle of the heaviest built-in
// profile, not just its steady state: a jvm app rolling out runs old and new
// pods at once (each with the instance-header sidecar), while a Git build
// pod runs alongside. ResourceQuota rejects the surge or build pod when the
// tier is sized only for steady state, which surfaces as an opaque
// deploy/build failure. The usage is computed from the real reconciled
// Deployment and the real build Job, so a future profile or build-resource
// bump fails this test instead of production namespaces.
func TestTierQuota_SmallFitsJvmRolloutWithBuild(t *testing.T) {
	scheme := testScheme()
	app := newTestApp()
	app.Spec.Resources = kipperv1.AppResources{Profile: "jvm"}
	app.Spec.Route = &kipperv1.AppRoute{}
	app.Spec.Git = &kipperv1.AppGitSource{URL: "https://git.example.com/shop/web.git", Branch: "main"}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client:       fakeClient,
		Scheme:       scheme,
		Domain:       "test.kipper.run",
		SidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest",
	}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var deploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy))
	appPod := &corev1.Pod{Spec: deploy.Spec.Template.Spec}
	require.GreaterOrEqual(t, len(appPod.Spec.Containers), 2,
		"expected the instance-header sidecar to be injected; the test must count it")

	// CreateBuildJob reads the cluster registry's push credential and CA,
	// which kip installs into kipper-system on every cluster.
	buildClient := k8sfake.NewClientset(
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "zot-pull-credentials", Namespace: "kipper-system"},
			Data:       map[string][]byte{"password": []byte("pullpw")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "zot-push-credentials", Namespace: "kipper-system"},
			Data:       map[string][]byte{"password": []byte("pushpw")},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "zot-tls", Namespace: "kipper-system"},
			Data:       map[string][]byte{"ca.crt": []byte("-----BEGIN CERTIFICATE-----\nfake\n-----END CERTIFICATE-----\n")},
		},
	)
	buildJob, err := builder.CreateBuildJob(context.Background(), buildClient, fakeClient, app, "abc1234")
	require.NoError(t, err)
	buildPod := &corev1.Pod{Spec: buildJob.Spec.Template.Spec}

	small, err := kipperv1.TierQuota("small").Parsed()
	require.NoError(t, err)

	for _, dim := range []struct {
		name   string
		res    corev1.ResourceName
		limits bool
		hard   resource.Quantity
	}{
		{"requests.cpu", corev1.ResourceCPU, false, small.CPURequest},
		{"limits.cpu", corev1.ResourceCPU, true, small.CPULimit},
		{"requests.memory", corev1.ResourceMemory, false, small.MemoryRequest},
		{"limits.memory", corev1.ResourceMemory, true, small.MemoryLimit},
	} {
		var used resource.Quantity
		used.Add(podEffective(appPod, dim.res, dim.limits))
		used.Add(podEffective(appPod, dim.res, dim.limits)) // rollout surge: old + new
		used.Add(podEffective(buildPod, dim.res, dim.limits))
		assert.LessOrEqual(t, used.Cmp(dim.hard), 0,
			"%s: jvm rollout + build needs %s but small tier caps at %s", dim.name, used.String(), dim.hard.String())
	}
}
