package controllers

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/registrycred"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = kipperv1.AddToScheme(s)
	return s
}

// buildAppResources follows the AppResources contract: a one-sided override
// mirrors to the other side (Guaranteed QoS), so a partial override can't
// leave request above limit; no override keeps the profile baseline, which is
// where burstable profiles like jvm get their high ceiling.
func TestBuildResources_PartialOverrideMirrors(t *testing.T) {
	// Override only the request: the limit mirrors it, per the documented
	// "if only one side is set, the other defaults to it" contract.
	got := buildAppResources(kipperv1.AppResources{Profile: "jvm", CPURequest: "200m"})
	assert.Equal(t, "200m", got.Requests.Cpu().String(), "explicit request applied")
	assert.Equal(t, "200m", got.Limits.Cpu().String(), "unset limit mirrors the request")

	// Override only a low limit: the request mirrors down to it, so request
	// never exceeds limit.
	got = buildAppResources(kipperv1.AppResources{Profile: "standard", MemoryLimit: "64Mi"})
	assert.Equal(t, "64Mi", got.Limits.Memory().String(), "explicit limit applied")
	assert.Equal(t, "64Mi", got.Requests.Memory().String(), "unset request mirrors the limit")

	// Both sides explicit: honoured as given, enabling a burstable pair.
	got = buildAppResources(kipperv1.AppResources{Profile: "custom", CPURequest: "200m", CPULimit: "1500m"})
	assert.Equal(t, "200m", got.Requests.Cpu().String())
	assert.Equal(t, "1500m", got.Limits.Cpu().String(), "explicit burstable pair preserved")

	// No overrides: the profile baseline comes through, keeping jvm's burst
	// ceiling.
	got = buildAppResources(kipperv1.AppResources{Profile: "jvm"})
	assert.Equal(t, "100m", got.Requests.Cpu().String())
	assert.Equal(t, "1", got.Limits.Cpu().String(), "jvm profile limit (1000m) preserved when nothing is overridden")
}

func newTestApp() *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "project-test",
		},
		Spec: kipperv1.AppSpec{
			Image: "myimage:latest",
			Port:  8080,
		},
	}
}

func TestAdoptWriterSecrets(t *testing.T) {
	ctx := context.Background()

	t.Run("adopts an unowned kipper-labelled app secrets Secret", func(t *testing.T) {
		scheme := testScheme()
		app := newTestApp()
		orphan := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretname.Secrets(secretname.KindApp, app.Name),
				Namespace: app.Namespace,
				Labels:    map[string]string{"app": app.Name, kipperLabel: kipperValue},
			},
			Data: map[string][]byte{"API_KEY": []byte("abc")},
		}
		fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, orphan).Build()
		r := &AppReconciler{Client: fakeClient, Scheme: scheme}

		require.NoError(t, r.adoptWriterSecrets(ctx, app))

		var secret corev1.Secret
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: secretname.Secrets(secretname.KindApp, app.Name), Namespace: app.Namespace}, &secret))
		owner := metav1.GetControllerOf(&secret)
		require.NotNil(t, owner, "the app secrets Secret must be garbage-collected with the App")
		assert.Equal(t, app.Name, owner.Name)
		assert.Equal(t, []byte("abc"), secret.Data["API_KEY"], "adoption must not touch the data")
	})

	t.Run("leaves a name-colliding foreign Secret alone", func(t *testing.T) {
		scheme := testScheme()
		app := newTestApp()
		// Same conventional name, but created by another system: no Kipper
		// writer labels. Adopting it would let an App delete data Kipper does
		// not own.
		foreign := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretname.Secrets(secretname.KindApp, app.Name),
				Namespace: app.Namespace,
				Labels:    map[string]string{"app.kubernetes.io/managed-by": "argocd"},
			},
			Data: map[string][]byte{"THEIRS": []byte("x")},
		}
		fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, foreign).Build()
		r := &AppReconciler{Client: fakeClient, Scheme: scheme}

		require.NoError(t, r.adoptWriterSecrets(ctx, app))

		var secret corev1.Secret
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: secretname.Secrets(secretname.KindApp, app.Name), Namespace: app.Namespace}, &secret))
		assert.Nil(t, metav1.GetControllerOf(&secret), "a Secret without Kipper writer labels must never become an App child")
	})

	t.Run("leaves a Secret labelled for a different app alone", func(t *testing.T) {
		scheme := testScheme()
		app := newTestApp()
		mislabelled := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretname.Secrets(secretname.KindApp, app.Name),
				Namespace: app.Namespace,
				Labels:    map[string]string{"app": "someone-else", kipperLabel: kipperValue},
			},
		}
		fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, mislabelled).Build()
		r := &AppReconciler{Client: fakeClient, Scheme: scheme}

		require.NoError(t, r.adoptWriterSecrets(ctx, app))

		var secret corev1.Secret
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: secretname.Secrets(secretname.KindApp, app.Name), Namespace: app.Namespace}, &secret))
		assert.Nil(t, metav1.GetControllerOf(&secret))
	})

	t.Run("adopts the conventional git credentials Secret", func(t *testing.T) {
		scheme := testScheme()
		app := newTestApp()
		app.Spec.Git = &kipperv1.AppGitSource{URL: "https://github.com/acme/api.git", CredentialsSecret: app.Name + "-git-credentials"}
		creds := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      app.Name + "-git-credentials",
				Namespace: app.Namespace,
				Labels:    map[string]string{"kipper.run/app": app.Name, kipperLabel: kipperValue},
			},
			Data: map[string][]byte{"token": []byte("tok")},
		}
		fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, creds).Build()
		r := &AppReconciler{Client: fakeClient, Scheme: scheme}

		require.NoError(t, r.adoptWriterSecrets(ctx, app))

		var secret corev1.Secret
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: app.Name + "-git-credentials", Namespace: app.Namespace}, &secret))
		require.NotNil(t, metav1.GetControllerOf(&secret))
	})

	t.Run("leaves a shared git credentials Secret unowned", func(t *testing.T) {
		scheme := testScheme()
		app := newTestApp()
		app.Spec.Git = &kipperv1.AppGitSource{URL: "https://github.com/acme/api.git", CredentialsSecret: "org-shared-deploy-key"} //nolint:gosec // G101: a Secret's name, not a credential
		creds := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "org-shared-deploy-key", Namespace: app.Namespace},
			Data:       map[string][]byte{"token": []byte("tok")},
		}
		fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, creds).Build()
		r := &AppReconciler{Client: fakeClient, Scheme: scheme}

		require.NoError(t, r.adoptWriterSecrets(ctx, app))

		var secret corev1.Secret
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "org-shared-deploy-key", Namespace: app.Namespace}, &secret))
		assert.Nil(t, metav1.GetControllerOf(&secret), "a non-conventional credentials Secret may be shared and must not be adopted")
	})

	t.Run("does not fight an existing controller", func(t *testing.T) {
		scheme := testScheme()
		app := newTestApp()
		other := newTestApp()
		other.Name = "other-app"
		other.UID = "other-uid"
		owned := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: secretname.Secrets(secretname.KindApp, app.Name), Namespace: app.Namespace},
		}
		require.NoError(t, ctrl.SetControllerReference(other, owned, scheme))
		fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, owned).Build()
		r := &AppReconciler{Client: fakeClient, Scheme: scheme}

		require.NoError(t, r.adoptWriterSecrets(ctx, app))

		var secret corev1.Secret
		require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: secretname.Secrets(secretname.KindApp, app.Name), Namespace: app.Namespace}, &secret))
		assert.Equal(t, "other-app", metav1.GetControllerOf(&secret).Name)
	})

	t.Run("missing secrets are fine", func(t *testing.T) {
		scheme := testScheme()
		app := newTestApp()
		fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()
		r := &AppReconciler{Client: fakeClient, Scheme: scheme}
		require.NoError(t, r.adoptWriterSecrets(ctx, app))
	})
}

func TestReconcileEnvSecret_ClearsSecretWhenEnvEmptied(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Env = nil // every env var removed

	staleSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretname.Env(secretname.KindApp, app.Name), Namespace: app.Namespace},
		Data:       map[string][]byte{"OLD_KEY": []byte("stale")},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, staleSecret).
		Build()
	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme}

	mustPublishAppEnv(t, reconciler, context.Background(), app, mustAppEnvSources(t, fakeClient, app))

	var secret corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: secretname.Env(secretname.KindApp, app.Name), Namespace: app.Namespace}, &secret))
	assert.Empty(t, secret.Data, "emptying spec.env must clear the env Secret, not leave stale keys")
}

func TestReconcileEnvSecret_MirrorsSpecEnvExactly(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Env = map[string]string{"KEEP": "yes"}

	staleSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretname.Env(secretname.KindApp, app.Name), Namespace: app.Namespace},
		Data:       map[string][]byte{"KEEP": []byte("yes"), "REMOVED": []byte("old")},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, staleSecret).
		Build()
	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme}

	mustPublishAppEnv(t, reconciler, context.Background(), app, mustAppEnvSources(t, fakeClient, app))

	var secret corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: secretname.Env(secretname.KindApp, app.Name), Namespace: app.Namespace}, &secret))
	assert.Equal(t, map[string][]byte{"KEEP": []byte("yes")}, secret.Data, "the Secret must mirror spec.env, dropping keys no longer present")
}

func TestReconcileEnvSecret_StampsOnChangeNotOnNoOp(t *testing.T) {
	scheme := testScheme()
	app := newTestApp()
	app.Spec.Env = map[string]string{"LOG": "info"}

	// Pre-existing Secret whose data already matches spec.env, with no stamp.
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretname.Env(secretname.KindApp, app.Name), Namespace: app.Namespace},
		Data:       map[string][]byte{"LOG": []byte("info")},
	}
	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, existing).Build()
	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme}

	// A reconcile with no data change must not stamp a change (no false banner).
	mustPublishAppEnv(t, reconciler, context.Background(), app, mustAppEnvSources(t, fakeClient, app))
	var secret corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: secretname.Env(secretname.KindApp, app.Name), Namespace: app.Namespace}, &secret))
	assert.Empty(t, secret.Annotations[kipperv1.DataUpdatedAtAnnotation], "a no-op reconcile must not stamp a change")

	// Changing spec.env stamps the Secret so the console can flag a pending restart.
	app.Spec.Env = map[string]string{"LOG": "debug"}
	mustPublishAppEnv(t, reconciler, context.Background(), app, mustAppEnvSources(t, fakeClient, app))
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: secretname.Env(secretname.KindApp, app.Name), Namespace: app.Namespace}, &secret))
	assert.NotEmpty(t, secret.Annotations[kipperv1.DataUpdatedAtAnnotation], "a real env change must stamp the Secret")
	assert.Equal(t, []byte("debug"), secret.Data["LOG"])
}

// The pre-generation env Secret is kept current while pods from before the move
// still name it, and created for nothing new. Recreating it would fight the
// retirement that removes it once nothing reads it.
func TestReconcileEnvSecret_UpdatesThePreGenerationSecretWithoutCreatingOne(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()
	app := newTestApp()
	app.Spec.Env = map[string]string{"LOG": "info"}

	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()
	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme}

	legacy := types.NamespacedName{
		Name: secretname.Env(secretname.KindApp, app.Name), Namespace: app.Namespace,
	}
	mustPublishAppEnv(t, reconciler, ctx, app, mustAppEnvSources(t, fakeClient, app))
	assert.True(t, errors.IsNotFound(fakeClient.Get(ctx, legacy, &corev1.Secret{})),
		"a workload created now has no pre-generation Secret to retire later")

	// One that already exists is a workload from before the move, whose pods
	// still name it.
	require.NoError(t, fakeClient.Create(ctx, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: legacy.Name, Namespace: legacy.Namespace},
		Data:       map[string][]byte{"LOG": []byte("stale")},
	}))
	app.Spec.Env = map[string]string{"LOG": "debug"}
	mustPublishAppEnv(t, reconciler, ctx, app, mustAppEnvSources(t, fakeClient, app))

	var kept corev1.Secret
	require.NoError(t, fakeClient.Get(ctx, legacy, &kept))
	assert.Equal(t, []byte("debug"), kept.Data["LOG"],
		"a pod still reading it must not be left on a value the CR no longer has")
	assert.NotEmpty(t, kept.Annotations[kipperv1.DataUpdatedAtAnnotation])
}

// Production and the tests around it build these names through the same helper,
// so an assertion that calls secretname.Env proves only that the helper agrees
// with itself — a consistently wrong name keeps both green. This pins the
// literal at the seam that decides whether the environment reaches the process:
// the pod asks for a name, and something has to have written an object under it.
func TestReconcile_AppPodReadsTheEnvSecretTheControllerWrote(t *testing.T) {
	scheme := testScheme()
	app := newTestApp()
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}

	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app).WithStatusSubresource(app).Build()
	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var deploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy))
	secret := podEnvGeneration(t, context.Background(), fakeClient,
		deploy.Spec.Template.Spec, "app-my-app-env-", "project-test")
	assert.Equal(t, []byte("info"), secret.Data["LOG_LEVEL"])
}

// The same seam for Functions, and the reason the kind is in the name at all: an
// App and a Function called my-app in one namespace must not resolve to one
// Secret, because it carries their resolved service credentials.
func TestReconcile_FunctionPodReadsItsOwnEnvSecret(t *testing.T) {
	scheme := testScheme()
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test"},
		Spec: kipperv1.FunctionSpec{
			Runtime: "node20",
			Source:  &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			Env:     map[string]string{"LOG_LEVEL": "debug"},
		},
	}

	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn).WithStatusSubresource(fn).Build()
	reconciler := &FunctionReconciler{Client: fakeClient, Scheme: scheme}

	sources := mustFunctionEnvSources(t, fakeClient, fn)
	gen := mustPublishFunctionEnv(t, reconciler, context.Background(), fn, sources)
	require.NoError(t, reconciler.reconcileDeployment(context.Background(), fn, "", sources, gen, nil))

	var deploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy))
	secret := podEnvGeneration(t, context.Background(), fakeClient,
		deploy.Spec.Template.Spec, "function-my-app-env-", "project-test")
	assert.Equal(t, []byte("debug"), secret.Data["LOG_LEVEL"])

	var appSecrets corev1.SecretList
	require.NoError(t, fakeClient.List(context.Background(), &appSecrets,
		crclient.InNamespace("project-test")))
	for _, sec := range appSecrets.Items {
		assert.False(t, strings.HasPrefix(sec.Name, "app-my-app-env"),
			"a Function must not write the App's environment, got %q", sec.Name)
	}
}

// An env change rewrites the Secret without touching the pod template, so a
// running function would go on serving the old values. `kip function env set`
// stamps the CR, and this is the projection that turns that into a new pod.
func TestReconcileDeployment_FunctionRestartStampReachesThePodTemplate(t *testing.T) {
	scheme := testScheme()
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "resize",
			Namespace:   "project-test",
			Annotations: map[string]string{"kipper.run/restartedAt": "2026-07-30T09:00:00Z"},
		},
		Spec: kipperv1.FunctionSpec{
			Runtime: "node20",
			Source:  &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
		},
	}

	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn).WithStatusSubresource(fn).Build()
	reconciler := &FunctionReconciler{Client: fakeClient, Scheme: scheme}
	require.NoError(t, reconciler.reconcileDeployment(context.Background(), fn, "", mustFunctionEnvSources(t, fakeClient, fn), "", nil))

	var deploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "resize", Namespace: "project-test"}, &deploy))
	assert.Equal(t, "2026-07-30T09:00:00Z", deploy.Spec.Template.Annotations["kipper.run/restartedAt"],
		"without the stamp on the pod template the Deployment is unchanged and no pod is replaced")
}

func TestReconcileDeployment_WithVolumeMounts(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Volumes = []kipperv1.AppVolumeMount{
		{Name: "uploads", MountPath: "/data/uploads"},
		{Name: "cache", MountPath: "/tmp/cache"},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	// Fetch the created Deployment
	var deploy appsv1.Deployment
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy)
	require.NoError(t, err)

	// Verify pod volumes reference the correct PVCs
	assert.Len(t, deploy.Spec.Template.Spec.Volumes, 2)

	volMap := make(map[string]string)
	for _, v := range deploy.Spec.Template.Spec.Volumes {
		require.NotNil(t, v.PersistentVolumeClaim)
		volMap[v.Name] = v.PersistentVolumeClaim.ClaimName
	}
	assert.Equal(t, "shared-uploads", volMap["uploads"])
	assert.Equal(t, "shared-cache", volMap["cache"])

	// Verify container volume mounts
	container := deploy.Spec.Template.Spec.Containers[0]
	assert.Len(t, container.VolumeMounts, 2)

	mountMap := make(map[string]string)
	for _, m := range container.VolumeMounts {
		mountMap[m.Name] = m.MountPath
	}
	assert.Equal(t, "/data/uploads", mountMap["uploads"])
	assert.Equal(t, "/tmp/cache", mountMap["cache"])
}

func TestReconcileDeployment_NoVolumes(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var deploy appsv1.Deployment
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy)
	require.NoError(t, err)

	assert.Empty(t, deploy.Spec.Template.Spec.Volumes)
	assert.Empty(t, deploy.Spec.Template.Spec.Containers[0].VolumeMounts)
}

func TestReconcileDeployment_VolumeAddedOnUpdate(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	// First reconcile: no volumes
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	// Add a volume to the App CR
	var updated kipperv1.App
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated)
	require.NoError(t, err)

	updated.Spec.Volumes = []kipperv1.AppVolumeMount{
		{Name: "media", MountPath: "/data/media"},
	}
	require.NoError(t, fakeClient.Update(context.Background(), &updated))

	// Second reconcile: volume should appear
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var deploy appsv1.Deployment
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy)
	require.NoError(t, err)

	assert.Len(t, deploy.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "media", deploy.Spec.Template.Spec.Volumes[0].Name)
	assert.Equal(t, "shared-media", deploy.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)

	assert.Len(t, deploy.Spec.Template.Spec.Containers[0].VolumeMounts, 1)
	assert.Equal(t, "/data/media", deploy.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath)
}

// TestReconcileDeployment_VolumeRemovedOnUpdate is the reconciler half of
// kip volume unmount: dropping the entry from spec.volumes must render the
// next Deployment without the volume and its mount.
func TestReconcileDeployment_VolumeRemovedOnUpdate(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Volumes = []kipperv1.AppVolumeMount{
		{Name: "media", MountPath: "/data/media"},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	// First reconcile: the volume is rendered.
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	// Unmount: clear spec.volumes.
	var updated kipperv1.App
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated)
	require.NoError(t, err)
	updated.Spec.Volumes = nil
	require.NoError(t, fakeClient.Update(context.Background(), &updated))

	// Second reconcile: the volume and its mount are gone.
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var deploy appsv1.Deployment
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy)
	require.NoError(t, err)

	assert.Empty(t, deploy.Spec.Template.Spec.Volumes)
	assert.Empty(t, deploy.Spec.Template.Spec.Containers[0].VolumeMounts)
}

// A replicas-only App edit carries no template or label change, so the
// replica comparison alone must trigger the Deployment update. This covers
// scale-to-zero, which migration freezes and kip app scale rely on.
func TestReconcileDeployment_ReplicasOnlyChange(t *testing.T) {
	for _, target := range []int32{3, 0} {
		scheme := testScheme()
		app := newTestApp()

		fakeClient := crfake.NewClientBuilder().
			WithScheme(scheme).
			WithObjects(app).
			WithStatusSubresource(app).
			Build()

		reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme}

		_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
		})
		require.NoError(t, err)

		var updated kipperv1.App
		require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated))
		replicas := target
		updated.Spec.Replicas = &replicas
		require.NoError(t, fakeClient.Update(context.Background(), &updated))

		_, err = reconciler.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
		})
		require.NoError(t, err)

		var deploy appsv1.Deployment
		require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy))
		require.NotNil(t, deploy.Spec.Replicas)
		assert.Equal(t, target, *deploy.Spec.Replicas, "replicas-only change must reach the Deployment")
	}
}

// With autoscaling enabled the HPA owns the replica count: a reconcile must
// not reset the Deployment's replicas back to spec.replicas.
func TestReconcileDeployment_AutoscaleLeavesReplicasToHPA(t *testing.T) {
	scheme := testScheme()
	app := newTestApp()
	app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MinReplicas: 1, MaxReplicas: 5, CPUTarget: 80}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	// Simulate the HPA scaling the Deployment out to 4.
	var deploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy))
	hpaReplicas := int32(4)
	deploy.Spec.Replicas = &hpaReplicas
	require.NoError(t, fakeClient.Update(context.Background(), &deploy))

	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy))
	require.NotNil(t, deploy.Spec.Replicas)
	assert.Equal(t, int32(4), *deploy.Spec.Replicas, "HPA-owned replica count must survive the reconcile")
}

// A template change while autoscaling is enabled must roll the Deployment
// without resetting the HPA-owned replica count.
func TestReconcileDeployment_TemplateChangeKeepsHPAReplicas(t *testing.T) {
	scheme := testScheme()
	app := newTestApp()
	app.Spec.Autoscale = &kipperv1.AppAutoscale{Enabled: true, MinReplicas: 1, MaxReplicas: 5, CPUTarget: 80}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}}

	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	// The HPA scales out, then the operator changes the image.
	var deploy appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy))
	hpaReplicas := int32(4)
	deploy.Spec.Replicas = &hpaReplicas
	require.NoError(t, fakeClient.Update(context.Background(), &deploy))

	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, app))
	app.Spec.Image = "registry.example.com/shop/my-app:v2"
	require.NoError(t, fakeClient.Update(context.Background(), app))

	_, err = reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy))
	assert.Equal(t, "registry.example.com/shop/my-app:v2", deploy.Spec.Template.Spec.Containers[0].Image, "template change must reach the Deployment")
	require.NotNil(t, deploy.Spec.Replicas)
	assert.Equal(t, int32(4), *deploy.Spec.Replicas, "HPA-owned replica count must survive the template update")
}

// An app deleted before adoption ever ran must still take its writer
// Secrets with it, and a Function of the same name keeps its own.
func TestSweepWriterSecrets(t *testing.T) {
	scheme := testScheme()

	secretFor := func(kind secretname.Kind, name string) *corev1.Secret {
		return &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name: secretname.Secrets(kind, name), Namespace: "project-test",
				Labels: map[string]string{"app": name, kipperLabel: kipperValue},
			},
			Data: map[string][]byte{"DB_PASSWORD": []byte("x")},
		}
	}

	app := newTestApp()
	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, secretFor(secretname.KindApp, "my-app")).
		WithStatusSubresource(app).
		Build()
	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme}

	require.NoError(t, reconciler.sweepWriterSecrets(context.Background(), app))
	var swept corev1.Secret
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "app-my-app-secrets", Namespace: "project-test"}, &swept)
	assert.True(t, errors.IsNotFound(err), "unadopted writer secret must be swept on deletion")

	// A Function of the same name owns a different object, and deleting the App
	// must not reach it. Before the names carried the kind these were one Secret,
	// and the App's sweep took the Function's credentials with it.
	fn := &kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test"}}
	fakeClient = crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, fn, secretFor(secretname.KindApp, "my-app"), secretFor(secretname.KindFunction, "my-app")).
		WithStatusSubresource(app).
		Build()
	reconciler = &AppReconciler{Client: fakeClient, Scheme: scheme}

	require.NoError(t, reconciler.sweepWriterSecrets(context.Background(), app))
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "function-my-app-secrets", Namespace: "project-test"}, &swept),
		"the function's credentials are the function's")
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "app-my-app-secrets", Namespace: "project-test"}, &swept)
	assert.True(t, errors.IsNotFound(err), "and the app's own still goes with it")
}

// Deleting a Function garbage-collects its writer Secret the same way
// deleting an App does.
func TestFunctionSweepWriterSecret(t *testing.T) {
	scheme := testScheme()
	fn := &kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "resize", Namespace: "project-test"}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "function-resize-secrets", Namespace: "project-test",
			Labels: map[string]string{"app": "resize", kipperLabel: kipperValue},
		},
		Data: map[string][]byte{"API_KEY": []byte("x")},
	}
	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, secret).Build()
	reconciler := &FunctionReconciler{Client: fakeClient, Scheme: scheme}

	require.NoError(t, reconciler.adoptWriterSecret(context.Background(), fn))
	var adopted corev1.Secret
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "function-resize-secrets", Namespace: "project-test"}, &adopted))
	owner := metav1.GetControllerOf(&adopted)
	require.NotNil(t, owner, "function must adopt its writer secret")
	assert.Equal(t, "Function", owner.Kind)

	require.NoError(t, reconciler.sweepWriterSecret(context.Background(), fn))
	err := fakeClient.Get(context.Background(), types.NamespacedName{Name: "function-resize-secrets", Namespace: "project-test"}, &adopted)
	assert.True(t, errors.IsNotFound(err), "sweep must remove the function's writer secret")
}

func TestShouldInjectSidecar(t *testing.T) {
	tests := []struct {
		name         string
		sidecarImage string
		route        *kipperv1.AppRoute
		want         bool
	}{
		{
			name:         "enabled with route and image",
			sidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest",
			route:        &kipperv1.AppRoute{},
			want:         true,
		},
		{
			name:         "disabled when NoInstanceHeader is true",
			sidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest",
			route:        &kipperv1.AppRoute{NoInstanceHeader: true},
			want:         false,
		},
		{
			name:         "disabled when no route",
			sidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest",
			route:        nil,
			want:         false,
		},
		{
			name:         "disabled when no sidecar image configured",
			sidecarImage: "",
			route:        &kipperv1.AppRoute{},
			want:         false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &AppReconciler{SidecarImage: tt.sidecarImage}
			app := &kipperv1.App{
				Spec: kipperv1.AppSpec{Route: tt.route},
			}
			assert.Equal(t, tt.want, r.shouldInjectSidecar(app))
		})
	}
}

func TestReconcileDeployment_WithSidecar(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client:       fakeClient,
		Scheme:       scheme,
		SidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest",
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var deploy appsv1.Deployment
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy)
	require.NoError(t, err)

	// Should have 2 containers: app + sidecar
	assert.Len(t, deploy.Spec.Template.Spec.Containers, 2)

	appContainer := deploy.Spec.Template.Spec.Containers[0]
	sidecar := deploy.Spec.Template.Spec.Containers[1]

	// App container keeps its original port
	assert.Equal(t, int32(8080), appContainer.Ports[0].ContainerPort)

	// Sidecar should listen on offset port and forward to app port
	assert.Equal(t, "kipper-instance-proxy", sidecar.Name)
	assert.Equal(t, int32(18080), sidecar.Ports[0].ContainerPort)

	// Sidecar should have correct env vars
	envMap := make(map[string]string)
	for _, e := range sidecar.Env {
		if e.Value != "" {
			envMap[e.Name] = e.Value
		} else if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
			envMap[e.Name] = e.ValueFrom.FieldRef.FieldPath
		}
	}
	assert.Equal(t, "18080", envMap["LISTEN_PORT"])
	assert.Equal(t, "8080", envMap["UPSTREAM_PORT"])
	assert.Equal(t, "metadata.name", envMap["POD_NAME"])
}

func TestReconcileDeployment_WithoutSidecar(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{NoInstanceHeader: true}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client:       fakeClient,
		Scheme:       scheme,
		SidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest",
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var deploy appsv1.Deployment
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy)
	require.NoError(t, err)

	// Should have only 1 container
	assert.Len(t, deploy.Spec.Template.Spec.Containers, 1)
	assert.Equal(t, int32(8080), deploy.Spec.Template.Spec.Containers[0].Ports[0].ContainerPort)
}

func TestReconcileDeployment_NoRouteNoSidecar(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	// No route set

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client:       fakeClient,
		Scheme:       scheme,
		SidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest",
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var deploy appsv1.Deployment
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy)
	require.NoError(t, err)

	assert.Len(t, deploy.Spec.Template.Spec.Containers, 1)
}

func TestReconcileIngress_WithRedirectsAndBasicAuth(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{
		Redirects: []kipperv1.RedirectRule{
			{Source: "^/$", Target: "/en/", Permanent: true},
			{Source: "^/old$", Target: "/new", Permanent: false},
		},
		BasicAuth: true,
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Domain: "test.kipper.run",
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var ingress networkingv1.Ingress
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress)
	require.NoError(t, err)

	mw := ingress.Annotations["traefik.ingress.kubernetes.io/router.middlewares"]
	assert.Contains(t, mw, "project-test-my-app-redirect-0@kubernetescrd")
	assert.Contains(t, mw, "project-test-my-app-redirect-1@kubernetescrd")
	assert.Contains(t, mw, "project-test-my-app-basic-auth@kubernetescrd")
}

func TestReconcileRateLimitMiddleware_CreatesPerAppMiddleware(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{RateLimit: 50}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Domain: "test.kipper.run",
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	mw := getMiddleware(t, fakeClient, "my-app-rate-limit")
	average, _, err := unstructured.NestedInt64(mw.Object, "spec", "rateLimit", "average")
	require.NoError(t, err)
	assert.Equal(t, int64(50), average)
	burst, _, err := unstructured.NestedInt64(mw.Object, "spec", "rateLimit", "burst")
	require.NoError(t, err)
	assert.Equal(t, int64(100), burst, "burst is twice the average, matching the cluster default ratio")

	var ingress networkingv1.Ingress
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress)
	require.NoError(t, err)
	assert.Contains(t, ingress.Annotations["traefik.ingress.kubernetes.io/router.middlewares"],
		"project-test-my-app-rate-limit@kubernetescrd")
}

func TestReconcileRateLimitMiddleware_UpdatesOnSpecChange(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{RateLimit: 50}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme, Domain: "test.kipper.run"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}}

	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	var stored kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &stored))
	stored.Spec.Route.RateLimit = 200
	require.NoError(t, fakeClient.Update(context.Background(), &stored))

	_, err = reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	mw := getMiddleware(t, fakeClient, "my-app-rate-limit")
	average, _, err := unstructured.NestedInt64(mw.Object, "spec", "rateLimit", "average")
	require.NoError(t, err)
	assert.Equal(t, int64(200), average)
}

func TestReconcileRateLimitMiddleware_DeletedWhenRateLimitCleared(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{RateLimit: 50}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme, Domain: "test.kipper.run"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}}

	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	getMiddleware(t, fakeClient, "my-app-rate-limit")

	var stored kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &stored))
	stored.Spec.Route.RateLimit = 0
	require.NoError(t, fakeClient.Update(context.Background(), &stored))

	_, err = reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	existing := middlewareStub("my-app-rate-limit")
	err = fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app-rate-limit", Namespace: "project-test"}, existing)
	assert.True(t, errors.IsNotFound(err), "per-app middleware removed once the app falls back to the cluster default")

	var ingress networkingv1.Ingress
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &ingress))
	assert.Contains(t, ingress.Annotations["traefik.ingress.kubernetes.io/router.middlewares"],
		"traefik-rate-limit@kubernetescrd")
}

func TestReconcileAPIKeyMiddlewares_CreatesForwardAuthAndStrip(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{RequireAPIKey: true}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme, Domain: "test.kipper.run"}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	auth := getMiddleware(t, fakeClient, "my-app-apikey")
	address, _, err := unstructured.NestedString(auth.Object, "spec", "forwardAuth", "address")
	require.NoError(t, err)
	assert.Equal(t,
		"http://kipper-authz.kipper-system.svc.cluster.local:8080/authorize?namespace=project-test&app=my-app",
		address, "app identity travels as trusted middleware config, not client headers")

	// The gate Middlewares must be owned by the App so an out-of-band deletion
	// triggers a reconcile that restores the gate.
	owners := auth.GetOwnerReferences()
	require.Len(t, owners, 1, "forwardAuth middleware must have an owner reference")
	assert.Equal(t, "App", owners[0].Kind)
	assert.Equal(t, "my-app", owners[0].Name)
	require.NotNil(t, owners[0].Controller)
	assert.True(t, *owners[0].Controller, "the App must be the controller owner")
	reqHeaders, _, err := unstructured.NestedStringSlice(auth.Object, "spec", "forwardAuth", "authRequestHeaders")
	require.NoError(t, err)
	assert.Equal(t, []string{"X-API-Key", "Access-Control-Request-Method", "X-Forwarded-For"}, reqHeaders,
		"authz sees the key, the preflight marker, and the client IP for forensics")
	respHeaders, _, err := unstructured.NestedStringSlice(auth.Object, "spec", "forwardAuth", "authResponseHeaders")
	require.NoError(t, err)
	assert.Equal(t, []string{"X-Kipper-Key-Prefix", "X-Kipper-Key-Name"}, respHeaders,
		"the consumer identity authz returns is copied onto the upstream request")

	strip := getMiddleware(t, fakeClient, "my-app-apikey-strip")
	headers, _, err := unstructured.NestedStringMap(strip.Object, "spec", "headers", "customRequestHeaders")
	require.NoError(t, err)
	assert.Equal(t, "", headers["X-API-Key"], "empty value removes the key before it reaches the backend")

	identityStrip := getMiddleware(t, fakeClient, "my-app-apikey-identity-strip")
	idHeaders, _, err := unstructured.NestedStringMap(identityStrip.Object, "spec", "headers", "customRequestHeaders")
	require.NoError(t, err)
	assert.Equal(t, "", idHeaders["X-Kipper-Key-Prefix"], "a forged prefix is cleared before injection")
	assert.Equal(t, "", idHeaders["X-Kipper-Key-Name"], "a forged name is cleared before injection")

	var ingress networkingv1.Ingress
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress))
	mw := ingress.Annotations["traefik.ingress.kubernetes.io/router.middlewares"]
	assert.Contains(t, mw, "project-test-my-app-apikey@kubernetescrd")
	assert.Contains(t, mw, "project-test-my-app-apikey-strip@kubernetescrd")
	// The identity strip must run before forwardAuth (clear forged headers)
	// and forwardAuth before the key strip (authz still needs the key).
	identityPos := strings.Index(mw, "my-app-apikey-identity-strip@kubernetescrd")
	authPos := strings.Index(mw, "my-app-apikey@kubernetescrd")
	keyStripPos := strings.Index(mw, "my-app-apikey-strip@kubernetescrd")
	require.NotEqual(t, -1, identityPos)
	assert.Less(t, identityPos, authPos, "identity strip runs before forwardAuth")
	assert.Less(t, authPos, keyStripPos, "forwardAuth runs before the key strip")
}

func TestReconcileAPIKeyMiddlewares_DeletedWhenDisabled(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{RequireAPIKey: true}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme, Domain: "test.kipper.run"}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}}
	_, err := reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)
	getMiddleware(t, fakeClient, "my-app-apikey")

	var stored kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &stored))
	stored.Spec.Route.RequireAPIKey = false
	require.NoError(t, fakeClient.Update(context.Background(), &stored))

	_, err = reconciler.Reconcile(context.Background(), req)
	require.NoError(t, err)

	for _, name := range []string{"my-app-apikey", "my-app-apikey-strip", "my-app-apikey-identity-strip"} {
		stub := middlewareStub(name)
		err = fakeClient.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "project-test"}, stub)
		assert.True(t, errors.IsNotFound(err), "%s must be deleted when the gate is turned off", name)
	}

	var ingress networkingv1.Ingress
	require.NoError(t, fakeClient.Get(context.Background(), req.NamespacedName, &ingress))
	assert.NotContains(t, ingress.Annotations["traefik.ingress.kubernetes.io/router.middlewares"], "apikey")
}

func middlewareStub(name string) *unstructured.Unstructured {
	mw := &unstructured.Unstructured{}
	mw.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})
	mw.SetName(name)
	mw.SetNamespace("project-test")
	return mw
}

func getMiddleware(t *testing.T, c crclient.Client, name string) *unstructured.Unstructured {
	t.Helper()
	mw := middlewareStub(name)
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: "project-test"}, mw)
	require.NoError(t, err)
	return mw
}

func TestReconcileIngress_NoRedirectsNoBasicAuth(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Domain: "test.kipper.run",
	}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var ingress networkingv1.Ingress
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress)
	require.NoError(t, err)

	mw := ingress.Annotations["traefik.ingress.kubernetes.io/router.middlewares"]
	assert.NotContains(t, mw, "redirect")
	assert.NotContains(t, mw, "basic-auth")
}

func TestReconcileIngress_KipperRunHostSkipsCertManager(t *testing.T) {
	// kipper.run subdomains are served by the gateway, which terminates
	// TLS for the whole `*.kipper.run` wildcard with its own cert. The
	// cluster-side Ingress must NOT carry a cert-manager annotation or
	// a TLS block — challenges would 404 at the gateway and the
	// Certificate would sit pending forever (witnessed on acme-tools
	// 2026-05-17, cleaned up manually).
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{
		Host: "deck-acme-tools.kipper.run",
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme, Domain: "acme-tools.kipper.run"}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var ingress networkingv1.Ingress
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress))

	_, hasCertMgrAnnotation := ingress.Annotations["cert-manager.io/cluster-issuer"]
	assert.False(t, hasCertMgrAnnotation, "kipper.run hostname must not carry the cert-manager annotation; gateway handles the public cert")

	// spec.tls.hosts stays populated so Traefik has a TLS router for
	// the SNI the gateway sends. secretName is empty — without a
	// matching Secret Traefik falls back to its default cert, which
	// is fine because the gateway hop uses InsecureSkipVerify.
	require.Len(t, ingress.Spec.TLS, 1)
	assert.Equal(t, []string{"deck-acme-tools.kipper.run"}, ingress.Spec.TLS[0].Hosts)
	assert.Empty(t, ingress.Spec.TLS[0].SecretName, "gateway-fronted ingress must not request a per-host secret")

	require.Len(t, ingress.Spec.Rules, 1)
	assert.Equal(t, "deck-acme-tools.kipper.run", ingress.Spec.Rules[0].Host, "rules.host still points at the kipper.run subdomain")
}

func TestReconcileIngress_CustomDomainKeepsCertManager(t *testing.T) {
	// Custom-domain Ingresses still need cluster-side TLS — cert-manager
	// issues per-host LE certs because the gateway's wildcard does not
	// cover arbitrary user domains.
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{
		Host: "parceltrack.example.com",
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme, Domain: "console.example.com"}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var ingress networkingv1.Ingress
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress))

	assert.Equal(t, "letsencrypt-prod", ingress.Annotations["cert-manager.io/cluster-issuer"])
	require.Len(t, ingress.Spec.TLS, 1)
	assert.Equal(t, []string{"parceltrack.example.com"}, ingress.Spec.TLS[0].Hosts)
	assert.Equal(t, "my-app-tls", ingress.Spec.TLS[0].SecretName)
}

func TestReconcileDeployment_AutoscalePreservesReplicas(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client: fakeClient,
		Scheme: scheme,
	}

	// First reconcile: creates deployment with 1 replica
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	// Simulate HPA scaling deployment to 3 replicas
	var deploy appsv1.Deployment
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy)
	require.NoError(t, err)
	three := int32(3)
	deploy.Spec.Replicas = &three
	require.NoError(t, fakeClient.Update(context.Background(), &deploy))

	// Enable autoscaling on the App CR
	var updated kipperv1.App
	err = fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated)
	require.NoError(t, err)
	updated.Spec.Autoscale = &kipperv1.AppAutoscale{
		Enabled:     true,
		MinReplicas: 2,
		MaxReplicas: 5,
		CPUTarget:   70,
	}
	require.NoError(t, fakeClient.Update(context.Background(), &updated))

	// Reconcile again: should NOT reset replicas to 1
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &deploy)
	require.NoError(t, err)
	assert.Equal(t, int32(3), *deploy.Spec.Replicas)
}

func TestReconcileIngress_DeletedWhenRouteCleared(t *testing.T) {
	scheme := testScheme()

	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{
		Client: fakeClient,
		Scheme: scheme,
		Domain: "test.kipper.run",
	}

	// First reconcile creates the Ingress.
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var ingress networkingv1.Ingress
	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress)
	require.NoError(t, err, "ingress should exist after first reconcile")

	// Clear the route on the App CR.
	var updated kipperv1.App
	require.NoError(t, fakeClient.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "my-app"}, &updated))
	updated.Spec.Route = nil
	require.NoError(t, fakeClient.Update(context.Background(), &updated))

	// Reconcile again — the Ingress must be deleted.
	_, err = reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	err = fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &ingress)
	assert.True(t, errors.IsNotFound(err), "ingress should be deleted after Spec.Route is cleared, got err=%v", err)
}

// serviceOwnedSecret builds a credentials Secret carrying the controller owner
// reference the Service reconciler stamps on the creds it creates.
func serviceOwnedSecret(name, serviceName string) *corev1.Secret {
	controller := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "project-test",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(),
				Kind:       "Service",
				Name:       serviceName,
				UID:        types.UID("uid-" + serviceName),
				Controller: &controller,
			}},
		},
		Data: map[string][]byte{"PASSWORD": []byte("shared")},
	}
}

func TestReconcileDeployment_RefusesBindingToForeignSecret(t *testing.T) {
	// A direct App-CR write names a binding "victim-git" so the reconciler
	// would inject "victim-git-credentials" (another app's git token). The
	// existence of that Secret — and even a deployer creating a colliding
	// Service CR of the same name — must not get it injected, because the
	// Secret carries no Service owner reference and no binding label.
	scheme := testScheme()

	app := newTestApp()
	app.Spec.ServiceBindings = []kipperv1.ServiceBinding{{Name: "victim-git", Prefix: "X_"}}

	gitSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "victim-git-credentials", Namespace: "project-test"},
		Data:       map[string][]byte{"token": []byte("ghp_secret")},
	}
	// The attacker also stands up a colliding Service CR; adoption never stamps
	// the owner reference onto the pre-existing foreign secret.
	collidingSvc := &kipperv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "victim-git", Namespace: "project-test"}, Spec: kipperv1.ServiceSpec{Type: "postgres"}}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, gitSecret, collidingSvc).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme}
	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	// The binding is declared and cannot be proven, so the reconcile stops
	// rather than rolling the workload without it. Carrying on would have
	// started the pod with the env silently absent.
	require.Error(t, err, "a refused binding must fail the reconcile, not deploy without the binding")

	var got appsv1.Deployment
	if getErr := fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got); getErr == nil {
		for _, ef := range got.Spec.Template.Spec.Containers[0].EnvFrom {
			if ef.SecretRef != nil {
				assert.NotEqual(t, "victim-git-credentials", ef.SecretRef.Name,
					"a foreign secret must not be injected even when a colliding Service CR exists")
			}
		}
	}
}

func TestFunctionEnvSources_ProvenanceGate(t *testing.T) {
	// The Function path applies the same provenance gate: a Service-owned
	// shared credential is injected; a foreign same-named secret is refused.
	scheme := testScheme()
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "project-test"},
		Spec: kipperv1.FunctionSpec{
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "cache"}, {Name: "victim-git"}},
		},
	}
	cacheSvc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "project-test", UID: types.UID("uid-cache")},
		Spec:       kipperv1.ServiceSpec{Type: "redis"},
	}
	cacheCreds := serviceOwnedSecret("cache-credentials", "cache")
	gitSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "victim-git-credentials", Namespace: "project-test"},
		Data:       map[string][]byte{"token": []byte("ghp_secret")},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, cacheSvc, cacheCreds, gitSecret).Build()

	sources, _, err := functionEnvSources(context.Background(), c, fn, nil)
	require.NoError(t, err)
	names := contributingSecretNames(sources)
	assert.True(t, names["cache-credentials"], "a Service-owned credential must be injected")
	assert.False(t, names["victim-git-credentials"], "a foreign secret must not be injected")
}

func TestBindingEnvSources_PropagatesLookupError(t *testing.T) {
	// A transient secret-lookup failure must surface so the reconcile retries,
	// rather than silently dropping a binding and stripping credentials.
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, isSecret := obj.(*corev1.Secret); isSecret {
					return context.DeadlineExceeded
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	_, _, err := bindingEnvSources(context.Background(), c,
		&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test"}},
		secretname.KindApp,
		[]kipperv1.ServiceBinding{{Name: "cache"}}, nil)
	require.Error(t, err, "a non-NotFound lookup error must propagate")
}

// A binding whose Secret carries no proof Kipper created it is refused, and the
// app then starts without env it was written to expect — on a live cluster a
// pre-rename label meant a Postgres binding vanished and the pod died on a
// literal ${DB_HOST} in its JDBC URL, with nothing naming the binding. The
// refusal has to be reported, not just applied.
func TestBindingEnvSourcesNameWhatTheyRefused(t *testing.T) {
	scheme := testScheme()
	mailhog := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "mailhog", Namespace: "project-test", UID: types.UID("uid-mailhog")},
		Spec:       kipperv1.ServiceSpec{Type: "mailhog"},
	}
	usable := serviceOwnedSecret("mailhog-credentials", "mailhog")
	// Present under the derived name, but owned by nothing this workload can
	// claim, so it is not a credential the App may be handed.
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "db-app-my-app-credentials",
			Namespace: "project-test",
			Labels:    map[string]string{"kipper.run/binding": "true"},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(mailhog, usable, stale).Build()

	valid, refused, err := bindingEnvSources(context.Background(), c,
		&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test"}},
		secretname.KindApp,
		[]kipperv1.ServiceBinding{
			{Name: "db", Prefix: "DB_", Database: "my_app"},
			{Name: "mailhog", Prefix: "MAIL_"},
		}, nil)
	require.NoError(t, err)

	require.Len(t, valid, 1, "the usable binding must still be injected")
	require.Equal(t, "mailhog", valid[0].service)

	require.Len(t, refused, 1, "the refused binding must be reported, not dropped in silence")
	require.Contains(t, refused[0], "db", "the report must name the binding")
	require.Contains(t, refused[0], "db-app-my-app-credentials", "the report must name the Secret it looked for")
}

// The provenance gate is now an exact UID match against the Service the binding
// names. Matching the owner's kind and API group accepted any Service CR in the
// namespace, so a binding could name one service and be handed another's
// credentials — including a service in the same project the caller was never
// meant to reach through that workload.
func TestBindingEnvSources_RefuseAnotherServicesCredentials(t *testing.T) {
	scheme := testScheme()
	db := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "project-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	// db-credentials exists, but it belongs to a different Service CR.
	misowned := serviceOwnedSecret("db-credentials", "billing-db")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(db, misowned).Build()

	valid, refused, err := bindingEnvSources(context.Background(), c,
		&kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test"}},
		secretname.KindApp,
		[]kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_"}}, nil)
	require.NoError(t, err)
	assert.Empty(t, valid, "credentials owned by another Service must not be injected")
	require.Len(t, refused, 1)
	assert.Contains(t, refused[0], "db")
}

func TestReconcileDeployment_DetectsBindingDriftOnRestoredDeployment(t *testing.T) {
	// Regression for the acme-tools migration: a velero-restored
	// Deployment can come back with an envFrom list that looks similar
	// enough to satisfy `equality.Semantic.DeepEqual` but is missing
	// the per-binding credential secrets the new App CR requires.
	// Without an explicit binding-coverage check, the reconciler would
	// skip the Update and leave the pod running against stale or
	// default credentials. Operator had to manually delete those
	// Deployments so the reconciler could recreate them.
	scheme := testScheme()

	app := newTestApp()
	app.Spec.ServiceBindings = []kipperv1.ServiceBinding{
		{Name: "db", Prefix: "DB_", Database: "myapp_test"},
		{Name: "cache", Prefix: "REDIS_"},
	}

	// The reconciler only injects a binding whose credential Secret it can tie
	// to the Service the binding names. The db binding pins a database, so its
	// Secret is derived here rather than seeded: the reconciler renders
	// db-app-my-app-credentials from the service's shared credentials.
	app.UID = types.UID("uid-my-app")
	cacheSvc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "project-test", UID: types.UID("uid-cache")},
		Spec:       kipperv1.ServiceSpec{Type: "redis"},
	}
	cacheCreds := serviceOwnedSecret("cache-credentials", "cache")
	dbSvc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "project-test", UID: types.UID("uid-db")},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	dbCreds := serviceOwnedSecret("db-credentials", "db")

	// Pre-seed a "restored" Deployment whose container has *no*
	// EnvFrom entries for the bindings — the broken state we hit on
	// acme-tools. The Deployment metadata matches what the reconciler
	// would produce for this app, so it looks owned but the env is
	// stale.
	stale := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "project-test",
			Labels:    map[string]string{"app": "my-app", "app.kubernetes.io/managed-by": "kipper"},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "my-app"}},
			Template: brokenDeploymentTemplate(),
		},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(app, stale, cacheSvc, cacheCreds, dbSvc, dbCreds).
		WithStatusSubresource(app).
		Build()

	reconciler := &AppReconciler{Client: fakeClient, Scheme: scheme}

	_, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var got appsv1.Deployment
	require.NoError(t, fakeClient.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))

	// The reconciler must have republished an environment carrying both
	// bindings. Asserting on the published values rather than on the Secret
	// names the pod lists is what the restore case actually cares about: the
	// credentials reaching the process, not the objects they came from.
	require.NotEmpty(t, got.Spec.Template.Spec.Containers)
	published := podEnvGeneration(t, context.Background(), fakeClient,
		got.Spec.Template.Spec, "app-my-app-env-", "project-test")

	assert.Equal(t, []byte("shared"), published.Data["DB_PASSWORD"],
		"the derived per-binding credentials must be republished when a binding pins a database; got %v", published.Data)
	assert.Equal(t, []byte("shared"), published.Data["REDIS_PASSWORD"],
		"shared credentials must be republished when a binding has no .database; got %v", published.Data)
}

func registryListSecret(t *testing.T, entries ...registrycred.Entry) *corev1.Secret {
	t.Helper()
	data, err := json.Marshal(entries) //nolint:gosec // test fixture: serialising the registry entry shape is the whole point of the test
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registrycred.ConfigSecretName, Namespace: registrycred.Namespace},
		Data:       map[string][]byte{"registries": data},
	}
}

func appWithImage(image string) *kipperv1.App {
	return &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "project-test", UID: types.UID("uid-web")},
		Spec:       kipperv1.AppSpec{Image: image, Port: 80},
	}
}

// managedNamespace is the workloads' project-test namespace the way the
// project reconciler labels it (project "acme") — the authoritative source
// ensureImagePullSecret resolves a workload's project from.
func managedNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "project-test",
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "kipper",
			"kipper.run/project":           "acme",
		},
	}}
}

func TestEnsureImagePullSecret_PrivateThirdPartyImage(t *testing.T) {
	// An app image from a configured private registry gets a scoped, app-owned
	// pull Secret staged in its own namespace, and a ref to it — when the
	// app's project is on the credential's allow-list.
	scheme := testScheme()
	app := appWithImage("ghcr.io/org/app:v1")
	list := registryListSecret(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "bob", Password: "s3cr3t",
		AllowedProjects: []string{"acme"},
	})
	ns := managedNamespace()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, list, ns).Build()

	refs, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "web-app-registry-pull", refs[0].Name)

	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "web-app-registry-pull"}, &secret))
	assert.Equal(t, corev1.SecretTypeDockerConfigJson, secret.Type)
	assert.Equal(t, "true", secret.Labels["kipper.run/registry"], "the pull secret must be labelled so env-copy/export skip it")
	assert.Contains(t, string(secret.Data[".dockerconfigjson"]), "ghcr.io")
	assert.Contains(t, string(secret.Data[".dockerconfigjson"]), "s3cr3t")
	require.Len(t, secret.OwnerReferences, 1, "the pull secret is owned by the app so it GC's with it")
	assert.Equal(t, "App", secret.OwnerReferences[0].Kind)
}

func TestEnsureImagePullSecret_PublicImageNoSecret(t *testing.T) {
	// A public image with no configured credential gets no pull secret.
	scheme := testScheme()
	app := appWithImage("nginx:1.25")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).Build()

	refs, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.NoError(t, err)
	assert.Nil(t, refs)
	var secret corev1.Secret
	err = c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "web-app-registry-pull"}, &secret)
	assert.True(t, errors.IsNotFound(err))
}

func TestEnsureImagePullSecret_ClusterRegistryNeedsNoSecret(t *testing.T) {
	// The cluster registry is trusted node-locally (k3s registries mirror), so a
	// git-built app's image needs no pull secret even if registries are configured.
	scheme := testScheme()
	app := appWithImage("zot.kipper-system.svc.cluster.local:5000/project-test/web:abc")
	list := registryListSecret(t, registrycred.Entry{Name: "ghcr", Server: "ghcr.io", Username: "u", Password: "p"})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, list).Build()

	refs, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.NoError(t, err)
	assert.Nil(t, refs)
}

func TestEnsureImagePullSecret_RemovesStaleWhenImageGoesPublic(t *testing.T) {
	// When an app's image changes to one needing no credential, its stale pull
	// secret is removed.
	scheme := testScheme()
	app := appWithImage("nginx:1.25")
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-app-registry-pull",
			Namespace: "project-test",
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
		},
		Type: corev1.SecretTypeDockerConfigJson,
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, stale).Build()

	refs, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.NoError(t, err)
	assert.Nil(t, refs)
	var secret corev1.Secret
	err = c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "web-app-registry-pull"}, &secret)
	assert.True(t, errors.IsNotFound(err), "the stale pull secret must be removed when the image no longer needs a credential")
}

func TestEnsureImagePullSecret_LeavesForeignSecretUntouched(t *testing.T) {
	// A same-named Secret that Kipper does not own must not be deleted.
	scheme := testScheme()
	app := appWithImage("nginx:1.25")
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "web-app-registry-pull", Namespace: "project-test"},
		Type:       corev1.SecretTypeDockerConfigJson,
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, foreign).Build()

	_, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.NoError(t, err)
	var secret corev1.Secret
	assert.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "web-app-registry-pull"}, &secret),
		"a Secret Kipper does not own must be left untouched")
}

func TestEnsureImagePullSecret_DeniesUnlistedProject(t *testing.T) {
	// A configured credential is staged only for projects on its allow-list; an
	// unlisted project gets no secret, and a previously staged one is removed.
	scheme := testScheme()
	app := appWithImage("ghcr.io/org/app:v1")
	list := registryListSecret(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "bob", Password: "s3cr3t",
		AllowedProjects: []string{"other"},
	})
	ns := managedNamespace()
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-app-registry-pull",
			Namespace: "project-test",
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
		},
		Type: corev1.SecretTypeDockerConfigJson,
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, list, ns, stale).Build()

	refs, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.NoError(t, err)
	assert.Nil(t, refs)
	var secret corev1.Secret
	err = c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "web-app-registry-pull"}, &secret)
	assert.True(t, errors.IsNotFound(err), "a pull secret staged for a project no longer on the allow-list must be removed")
}

func TestEnsureImagePullSecret_DeniesWithoutGrant(t *testing.T) {
	// An empty allow-list denies every project (fail closed), so a credential
	// is never staged until an admin explicitly grants a project.
	scheme := testScheme()
	app := appWithImage("ghcr.io/org/app:v1")
	list := registryListSecret(t, registrycred.Entry{Name: "ghcr", Server: "ghcr.io", Username: "bob", Password: "s3cr3t"})
	ns := managedNamespace()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, list, ns).Build()

	refs, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.NoError(t, err)
	assert.Nil(t, refs)
	var secret corev1.Secret
	err = c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "web-app-registry-pull"}, &secret)
	assert.True(t, errors.IsNotFound(err))
}

func TestEnsureImagePullSecret_DeniesUnmanagedNamespace(t *testing.T) {
	// A namespace without the controller-owned project label is definitively
	// not a project namespace, so no credential is staged in it.
	scheme := testScheme()
	app := appWithImage("ghcr.io/org/app:v1")
	list := registryListSecret(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "bob", Password: "s3cr3t",
		AllowedProjects: []string{"acme"},
	})
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "project-test"}}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, list, ns).Build()

	refs, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.NoError(t, err)
	assert.Nil(t, refs)
}

func TestEnsureImagePullSecret_ErrorOnUnreadableCredentials(t *testing.T) {
	// A transient read failure must surface as an error so the reconcile is
	// retried — never as "no credential", which would strip a working pull
	// secret from the pod template.
	scheme := testScheme()
	app := appWithImage("ghcr.io/org/app:v1")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if key.Name == registrycred.ConfigSecretName {
					return context.DeadlineExceeded
				}
				return cl.Get(ctx, key, obj, opts...)
			},
		}).Build()

	_, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.Error(t, err)
}

func TestEnsureImagePullSecret_ErrorOnMissingNamespace(t *testing.T) {
	// A credential match without a readable namespace is an unknown state, not
	// a deny — the reconcile must error and retry rather than guess.
	scheme := testScheme()
	app := appWithImage("ghcr.io/org/app:v1")
	list := registryListSecret(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "bob", Password: "s3cr3t",
		AllowedProjects: []string{"acme"},
	})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, list).Build()

	_, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.Error(t, err)
}

func TestEnsureImagePullSecret_RejectsForeignClusterImage(t *testing.T) {
	// A hand-written CR pointing at another project's cluster-registry image
	// must be refused — the nodes pull the cluster registry with a node-wide
	// identity, so this is the only gate against cross-project image reads.
	scheme := testScheme()
	app := appWithImage("zot.kipper-system.svc.cluster.local:5000/victim-prod/payments:v3")
	victim := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "victim-prod",
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "kipper",
			"kipper.run/project":           "victim",
		},
	}}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, managedNamespace(), victim).Build()

	_, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not belong")
}

func TestEnsureImagePullSecret_AllowsSameProjectClusterImage(t *testing.T) {
	// A cluster-registry image from a sibling namespace of the SAME project
	// (an image promoted across environments) is allowed and needs no secret.
	scheme := testScheme()
	app := appWithImage("zot.kipper-system.svc.cluster.local:5000/project-staging/web:v3")
	staging := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "project-staging",
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "kipper",
			"kipper.run/project":           "acme",
		},
	}}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, managedNamespace(), staging).Build()

	refs, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.NoError(t, err)
	assert.Nil(t, refs)
}

func TestEnsureImagePullSecret_ErrorOnUnknownClusterImageNamespace(t *testing.T) {
	// A cluster-registry image whose namespace cannot be resolved is an
	// unknown state and must fail closed.
	scheme := testScheme()
	app := appWithImage("zot.kipper-system.svc.cluster.local:5000/no-such-ns/app:v1")
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, managedNamespace()).Build()

	_, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.Error(t, err)
}

func TestJobReconciler_RejectsForeignClusterImage(t *testing.T) {
	// Kipper Job CRs pass through the same chokepoint: a cross-project
	// cluster-registry image must never produce a batch Job or CronJob pod.
	scheme := testScheme()
	victim := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "victim-prod",
		Labels: map[string]string{
			"app.kubernetes.io/managed-by": "kipper",
			"kipper.run/project":           "victim",
		},
	}}
	for _, schedule := range []string{"", "*/5 * * * *"} {
		job := &kipperv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "digest", Namespace: "project-test", UID: types.UID("uid-digest")},
			Spec: kipperv1.JobSpec{
				Image:    "zot.kipper-system.svc.cluster.local:5000/victim-prod/payments:v3",
				Schedule: schedule,
			},
		}
		c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(job, managedNamespace(), victim).Build()
		r := &JobReconciler{Client: c, Scheme: scheme}

		var err error
		if schedule == "" {
			err = r.reconcileOneOffJob(context.Background(), job, "")
		} else {
			err = r.reconcileCronJob(context.Background(), job, "")
		}
		require.Error(t, err, "schedule=%q", schedule)

		var nj batchv1.Job
		getErr := c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "digest"}, &nj)
		assert.True(t, errors.IsNotFound(getErr), "no batch Job may exist for a rejected image (schedule=%q)", schedule)
		var cj batchv1.CronJob
		getErr = c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "digest"}, &cj)
		assert.True(t, errors.IsNotFound(getErr), "no CronJob may exist for a rejected image (schedule=%q)", schedule)
	}
}

func TestJobReconciler_StagesPullSecretForPrivateImage(t *testing.T) {
	// A Job running a granted private third-party image gets the scoped pull
	// Secret staged and referenced, same as Apps and Functions.
	scheme := testScheme()
	job := &kipperv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "digest", Namespace: "project-test", UID: types.UID("uid-digest")},
		Spec: kipperv1.JobSpec{
			Image:    "ghcr.io/org/digest:v1",
			Schedule: "*/5 * * * *",
		},
	}
	list := registryListSecret(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "bob", Password: "s3cr3t",
		AllowedProjects: []string{"acme"},
	})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(job, list, managedNamespace()).Build()
	r := &JobReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileCronJob(context.Background(), job, ""))

	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "digest-job-registry-pull"}, &secret))
	var cj batchv1.CronJob
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "digest"}, &cj))
	require.Len(t, cj.Spec.JobTemplate.Spec.Template.Spec.ImagePullSecrets, 1)
	assert.Equal(t, "digest-job-registry-pull", cj.Spec.JobTemplate.Spec.Template.Spec.ImagePullSecrets[0].Name)
}

func TestEnsureImagePullSecret_ErrorOnFailedCleanup(t *testing.T) {
	// A revoked credential's staged Secret must be confirmed gone: a failed
	// delete surfaces as an error so the reconcile retries the cleanup.
	scheme := testScheme()
	app := appWithImage("ghcr.io/org/app:v1")
	list := registryListSecret(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "bob", Password: "s3cr3t",
		AllowedProjects: []string{"other"},
	})
	ns := managedNamespace()
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web-app-registry-pull",
			Namespace: "project-test",
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
		},
		Type: corev1.SecretTypeDockerConfigJson,
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, list, ns, stale).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, opts ...crclient.DeleteOption) error {
				return context.DeadlineExceeded
			},
		}).Build()

	_, err := ensureImagePullSecret(context.Background(), c, scheme, app, app.Spec.Image)
	require.Error(t, err)
}

func TestEnqueueForRegistryCredentials_MapsListSecretToAllWorkloads(t *testing.T) {
	// A change of the central credential list must reconcile every App and
	// Function, so a rotation or revocation reaches staged pull Secrets
	// without waiting for an unrelated workload event.
	scheme := testScheme()
	appA := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "project-a"}}
	appB := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "project-b"}}
	fn := &kipperv1.Function{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "project-a"}}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(appA, appB, fn).Build()

	listSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: registrycred.ConfigSecretName, Namespace: registrycred.Namespace,
	}}
	other := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "web-env", Namespace: "project-a"}}

	ar := &AppReconciler{Client: c, Scheme: scheme}
	reqs := ar.enqueueForRegistryCredentials(context.Background(), listSecret)
	require.Len(t, reqs, 2, "every App must be enqueued for the central list Secret")
	assert.Nil(t, ar.enqueueForRegistryCredentials(context.Background(), other),
		"an unrelated Secret must not fan out")

	fr := &FunctionReconciler{Client: c, Scheme: scheme}
	freqs := fr.enqueueForRegistryCredentials(context.Background(), listSecret)
	require.Len(t, freqs, 1, "every Function must be enqueued for the central list Secret")
	assert.Equal(t, "worker", freqs[0].Name)

	jr := &JobReconciler{Client: c, Scheme: scheme}
	jreqs := jr.enqueueForRegistryCredentials(context.Background(), listSecret)
	assert.Empty(t, jreqs, "no Jobs exist yet")
	job := &kipperv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "digest", Namespace: "project-a"}}
	require.NoError(t, c.Create(context.Background(), job))
	jreqs = jr.enqueueForRegistryCredentials(context.Background(), listSecret)
	require.Len(t, jreqs, 1, "every Job must be enqueued for the central list Secret")
	assert.Equal(t, "digest", jreqs[0].Name)
}

func TestStageFunctionPullSecret_StagesForTestRun(t *testing.T) {
	// The test-run handler stages through this before referencing the Secret,
	// so a test run cannot race the Function's first reconcile.
	scheme := testScheme()
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "project-test", UID: types.UID("uid-worker")},
		Spec: kipperv1.FunctionSpec{
			Image:    "ghcr.io/org/worker:v1",
			Triggers: []kipperv1.FunctionTrigger{{Type: "cron", Schedule: "*/5 * * * *"}},
		},
	}
	list := registryListSecret(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "bob", Password: "s3cr3t",
		AllowedProjects: []string{"acme"},
	})
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, list, managedNamespace()).Build()

	refs, err := StageFunctionPullSecret(context.Background(), c, fn)
	require.NoError(t, err)
	require.Len(t, refs, 1)
	assert.Equal(t, "worker-function-registry-pull", refs[0].Name)
	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "worker-function-registry-pull"}, &secret))
}

func TestReconcileCronJob_StagesPullSecret(t *testing.T) {
	// A cron-only Function never runs reconcileDeployment, so the cron path
	// must stage the pull Secret itself before referencing it.
	scheme := testScheme()
	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "project-test", UID: types.UID("uid-worker")},
		Spec: kipperv1.FunctionSpec{
			Image:    "ghcr.io/org/worker:v1",
			Triggers: []kipperv1.FunctionTrigger{{Type: "cron", Schedule: "*/5 * * * *"}},
		},
	}
	list := registryListSecret(t, registrycred.Entry{
		Name: "ghcr", Server: "ghcr.io", Username: "bob", Password: "s3cr3t",
		AllowedProjects: []string{"acme"},
	})
	ns := managedNamespace()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(fn, list, ns).Build()

	r := &FunctionReconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.reconcileCronJob(context.Background(), fn, &fn.Spec.Triggers[0], nil, ""))

	var secret corev1.Secret
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "worker-function-registry-pull"}, &secret),
		"the cron reconcile must stage the pull Secret the CronJob pod references")

	var cron batchv1.CronJob
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "project-test", Name: "worker-cron"}, &cron))
	require.Len(t, cron.Spec.JobTemplate.Spec.Template.Spec.ImagePullSecrets, 1)
	assert.Equal(t, "worker-function-registry-pull", cron.Spec.JobTemplate.Spec.Template.Spec.ImagePullSecrets[0].Name)
}

// brokenDeploymentTemplate returns a minimal PodTemplateSpec for the
// "my-app" fixture — same image and label selector the reconciler would
// create, but with zero EnvFrom entries. Used by the binding-drift test
// to set up the velero-restored state.
func brokenDeploymentTemplate() corev1.PodTemplateSpec {
	const appName = "my-app"

	return corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": appName}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    appName,
					Image:   "myimage:latest",
					Ports:   []corev1.ContainerPort{{ContainerPort: 8080}},
					EnvFrom: nil, // <- the load-bearing emptiness
				},
			},
		},
	}
}

// The condition has to track the current state, not accumulate history: a
// refusal that has been fixed must go True, and removing the binding entirely
// must remove the condition rather than leave a failure whose cause is gone.
func TestServiceBindingConditionTracksCurrentState(t *testing.T) {
	app := newTestApp()
	app.Generation = 7
	app.Spec.ServiceBindings = []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_", Database: "my_app"}}

	applyServiceBindingCondition(&app.Status.Conditions, app.Generation, len(app.Spec.ServiceBindings), []string{"db (no usable db-my-app-credentials Secret)"})
	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionServiceBindingsReady)
	require.NotNil(t, cond, "a refusal must be reported")
	require.Equal(t, metav1.ConditionFalse, cond.Status)
	require.Equal(t, "BindingSecretUnusable", cond.Reason)
	require.Contains(t, cond.Message, "db-my-app-credentials")
	require.Equal(t, int64(7), cond.ObservedGeneration)

	// The Secret is fixed, the binding stays declared.
	applyServiceBindingCondition(&app.Status.Conditions, app.Generation, len(app.Spec.ServiceBindings), nil)
	cond = apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionServiceBindingsReady)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, "BindingsInjected", cond.Reason)

	// The binding is removed altogether.
	app.Spec.ServiceBindings = nil
	applyServiceBindingCondition(&app.Status.Conditions, app.Generation, len(app.Spec.ServiceBindings), nil)
	require.Nil(t, apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionServiceBindingsReady),
		"no bindings means no condition, not a stale one")
}

// A CR written directly can declare any number of bindings with names of any
// length. An oversized message makes the whole status update invalid, which would
// suppress the very condition that exists to make the failure visible.
func TestServiceBindingConditionMessageStaysWithinTheSchemaLimit(t *testing.T) {
	app := newTestApp()
	app.Spec.ServiceBindings = []kipperv1.ServiceBinding{{Name: "db"}}

	refused := make([]string, 200)
	for i := range refused {
		refused[i] = strings.Repeat("n", 300)
	}
	applyServiceBindingCondition(&app.Status.Conditions, app.Generation, len(app.Spec.ServiceBindings), refused)

	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionServiceBindingsReady)
	require.NotNil(t, cond)
	require.Less(t, len(cond.Message), 2048, "the message must be bounded")
	require.Contains(t, cond.Message, "more", "the count of omitted entries must survive truncation")
}

// A Deployment that already existed when its App was created was adopted for
// reconciliation but never for garbage collection: only the Create path carried
// the controller reference. So `kip app delete` removed the CR and left the
// workload serving — which is what happened to one app in production, found
// while deleting it.
// reconcileDerivedEnvSecret has re-asserted ownership on every pass since wave
// 1, for the same reason: a reference lost to a direct write or a restore has
// to be repaired rather than waiting for something to recreate the object.
func TestReconcileDeployment_AdoptsADeploymentThatArrivedFirst(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")

	// The shape deck was in: correct name and labels, owned by nobody.
	orphan := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "project-test",
			Labels:    map[string]string{"app": "my-app", kipperLabel: kipperValue},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "my-app"}},
			Template: brokenDeploymentTemplate(),
		},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, orphan).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.NoError(t, err)

	var got appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	ref := metav1.GetControllerOf(&got)
	require.NotNil(t, ref, "an adopted Deployment must carry the App's controller reference, or deleting the App leaves it running")
	assert.Equal(t, "App", ref.Kind)
	assert.Equal(t, app.UID, ref.UID)
}

// And a Deployment whose template already matches must still be repaired — the
// no-change early return would otherwise skip the write that adopts it, which
// is precisely the steady state a long-running app is in.
func TestReconcileDeployment_AdoptsEvenWhenNothingElseChanged(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}}

	// First pass creates it, owned.
	_, err := r.Reconcile(ctx, req)
	require.NoError(t, err)

	// Strip the reference the way a direct write or a restore would.
	var live appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &live))
	live.OwnerReferences = nil
	require.NoError(t, c.Update(ctx, &live))

	// A pass with nothing else to do must still put it back.
	_, err = r.Reconcile(ctx, req)
	require.NoError(t, err)

	var got appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	require.NotNil(t, metav1.GetControllerOf(&got),
		"a lost owner reference must be repaired on the next pass, not on the next template change")
}

// Adopting on name alone would let an App capture an object it did not create
// and then delete it with itself. adoptWriterSecrets has required Kipper's
// markers for Secrets since it was written; the child objects did not, so a
// Deployment, Service, Ingress or HPA that GitOps, an operator or a human left
// under the app's name was fair game.
func TestReconcileDeployment_RefusesAForeignDeploymentUnderTheSameName(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")

	// Somebody else's Deployment, sharing only the name.
	foreign := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "project-test",
			Labels:    map[string]string{"team": "platform"},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "my-app"}},
			Template: brokenDeploymentTemplate(),
		},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, foreign).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.Error(t, err, "an object Kipper did not create must not be adopted")
	assert.Contains(t, err.Error(), "not created by Kipper")

	var got appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	assert.Nil(t, metav1.GetControllerOf(&got), "and must not have been claimed")
	assert.Equal(t, "platform", got.Labels["team"], "nor had its labels overwritten")
}

// Turning autoscaling off deletes this app's autoscaler, not one that merely
// shares its name.
func TestReconcileHPA_DisablingDoesNotDeleteAForeignAutoscaler(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Autoscale = nil

	foreign := &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, foreign).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileHPA(ctx, app))
	assert.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"},
		&autoscalingv2.HorizontalPodAutoscaler{}),
		"an autoscaler this app does not own must survive autoscaling being switched off")
}

// Deletion asks exactly what adoption asks. A Middleware nothing controls but
// which Kipper's labels say it created for this app is this app's to remove —
// otherwise switching a feature off would strand the very object switching it
// on would have adopted.
func TestReconcileAPIKeyMiddlewares_DisablingDeletesAnUnownedMiddlewareKipperMade(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Route = &kipperv1.AppRoute{Host: "app.example.com", RequireAPIKey: false}

	ours := &unstructured.Unstructured{}
	ours.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})
	ours.SetName("my-app-apikey-strip")
	ours.SetNamespace("project-test")
	ours.SetLabels(map[string]string{"app": "my-app", kipperLabel: kipperValue})

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, ours).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileAPIKeyMiddlewares(ctx, app))

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})
	err := c.Get(ctx, types.NamespacedName{
		Name: "my-app-apikey-strip", Namespace: "project-test"}, got)
	assert.True(t, errors.IsNotFound(err),
		"a middleware Kipper created for this app must be cleaned up even without an owner reference")
}

// A route refused for a host conflict tears down the app's own Ingress. An
// Ingress under the same name that this app does not own is somebody else's.
func TestReconcileIngress_RemovingTheRouteLeavesAForeignIngress(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Route = nil

	foreign := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app", Namespace: "project-test"},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, foreign).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileIngress(ctx, app))
	assert.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"},
		&networkingv1.Ingress{}),
		"an ingress this app does not own must survive its route being removed")
}

// Switching the API-key gate off used to delete all three middleware names
// outright. A foreign Middleware colliding with one of them was protected while
// the gate was on and destroyed the moment it was switched off.
func TestReconcileAPIKeyMiddlewares_DisablingDoesNotDeleteAForeignMiddleware(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Route = &kipperv1.AppRoute{Host: "app.example.com", RequireAPIKey: false}

	foreign := &unstructured.Unstructured{}
	foreign.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})
	foreign.SetName("my-app-apikey-strip")
	foreign.SetNamespace("project-test")

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, foreign).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.reconcileAPIKeyMiddlewares(ctx, app))

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "traefik.io", Version: "v1alpha1", Kind: "Middleware",
	})
	assert.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: "my-app-apikey-strip", Namespace: "project-test"}, got),
		"a middleware this app does not own must survive the api-key gate being switched off")
}

// An App and a Function may share a name in one namespace, and a Function's
// children carry the same managed-by and app labels. The resource-type label is
// what tells them apart — the same test the CLI's workloadDeployment applies —
// so an orphaned Function child is not this App's to claim and later delete.
func TestReconcileDeployment_RefusesAnOrphanedFunctionsDeployment(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")

	fnDeploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-app",
			Namespace: "project-test",
			Labels: map[string]string{
				"app": "my-app", kipperLabel: kipperValue,
				"kipper.run/resource-type": "function",
			},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "my-app"}},
			Template: brokenDeploymentTemplate(),
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, fnDeploy).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"},
	})
	require.Error(t, err, "a Function's Deployment must not be adopted by a same-named App")
	assert.Contains(t, err.Error(), "belongs to a function")

	var got appsv1.Deployment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	assert.Nil(t, metav1.GetControllerOf(&got), "and must not have been claimed")
}

// mustPublishAppEnv runs the App's env publication and returns the generation
// the pod template should name.
func mustPublishAppEnv(t *testing.T, r *AppReconciler, ctx context.Context, app *kipperv1.App, sources []envSource) string {
	t.Helper()
	gen, err := r.reconcileEnvSecret(ctx, app, sources)
	require.NoError(t, err)
	return gen
}

// mustPublishFunctionEnv is the same for a Function.
func mustPublishFunctionEnv(t *testing.T, r *FunctionReconciler, ctx context.Context, fn *kipperv1.Function, sources []envSource) string {
	t.Helper()
	gen, err := r.reconcileEnvSecret(ctx, fn, sources)
	require.NoError(t, err)
	return gen
}

// mustPublishJobEnv is the same for a Job.
func mustPublishJobEnv(t *testing.T, r *JobReconciler, ctx context.Context, job *kipperv1.Job) string {
	t.Helper()
	gen, err := r.reconcileEnvSecret(ctx, job)
	require.NoError(t, err)
	return gen
}

// podEnvGeneration reads the environment a pod template names and returns the
// object behind it.
// The name is read off the pod rather than constructed from secretname, for the
// reason these tests existed before generations: an assertion that builds the
// name through the same helper production uses proves only that the helper
// agrees with itself, and a consistently wrong name keeps both green. The
// prefix is asserted as a literal for the same reason.
func podEnvGeneration(t *testing.T, ctx context.Context, c crclient.Client,
	spec corev1.PodSpec, wantPrefix, namespace string) corev1.Secret {
	t.Helper()
	require.NotEmpty(t, spec.Containers[0].EnvFrom, "the pod must read a published environment")
	require.Len(t, spec.Containers[0].EnvFrom, 1,
		"one generation carries the whole environment, so there is one entry")

	ref := spec.Containers[0].EnvFrom[0].SecretRef
	require.NotNil(t, ref)
	assert.True(t, strings.HasPrefix(ref.Name, wantPrefix),
		"expected a %s generation, got %q", wantPrefix, ref.Name)
	require.NotNil(t, ref.Optional)
	assert.False(t, *ref.Optional,
		"a pod that cannot read its published environment must fail to start rather than start without it")

	var secret corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, &secret),
		"the name the pod reads must be the name the controller writes")
	return secret
}

// The operator's own kip app update, the console and a scale all write the same
// Deployment this controller writes, so a conflict here is ordinary. Returning
// it works — controller-runtime requeues a returned error — but it logs a
// failure for something that did not fail, and those are the lines that were in
// front of the operator while promote quietly did nothing.
func TestReconcileDeployment_RetriesAConflictingWrite(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "hrportal-prod"},
		Spec:       kipperv1.AppSpec{Image: "ghcr.io/acme/backend:2026-08-02", Port: 8080},
	}
	existing := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backend",
			Namespace: "hrportal-prod",
			Labels:    map[string]string{"app": "backend", kipperLabel: kipperValue},
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "backend"}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{
					{Name: "backend", Image: "ghcr.io/acme/backend:2026-06-29"},
				}},
			},
		},
	}

	updates := 0
	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, existing).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				if _, isDeployment := obj.(*appsv1.Deployment); isDeployment {
					updates++
					if updates == 1 {
						return errors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "backend", context.DeadlineExceeded)
					}
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: fakeClient, Scheme: scheme}

	require.NoError(t, r.reconcileDeployment(ctx, app, nil, "gen-1", ""),
		"a conflict is what this object does all day, not a reconcile failure")
	assert.Equal(t, 2, updates, "and it is retried rather than returned")

	var got appsv1.Deployment
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "backend", Namespace: "hrportal-prod"}, &got))
	assert.Equal(t, "ghcr.io/acme/backend:2026-08-02", got.Spec.Template.Spec.Containers[0].Image,
		"and the write it was carrying still lands")
}

// applyDeployment decides from the live object and writes what it decides into
// desired — holding the environment generation the pods are running, keeping an
// unpinned resource value. An attempt that loses a conflict leaves those
// decisions behind, and the next attempt reads a different live object, so
// reusing the same desired lets one attempt's conclusions be applied to another
// attempt's world.
func TestReconcileDeployment_EachRetryDecidesFromAFreshDesired(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "backend", Namespace: "hrportal-prod"},
		Spec:       kipperv1.AppSpec{Image: "ghcr.io/acme/backend:2026-08-02", Port: 8080},
	}
	live := func(image string) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "backend",
				Namespace: "hrportal-prod",
				Labels:    map[string]string{"app": "backend", kipperLabel: kipperValue},
			},
			Spec: appsv1.DeploymentSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "backend"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "backend"}},
					Spec: corev1.PodSpec{Containers: []corev1.Container{
						{Name: "backend", Image: image},
					}},
				},
			},
		}
	}

	updates := 0
	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, live("ghcr.io/acme/backend:2026-06-29")).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
				d, isDeployment := obj.(*appsv1.Deployment)
				if !isDeployment {
					return c.Update(ctx, obj, opts...)
				}
				updates++
				if updates == 1 {
					// The first attempt's conclusion, thrown away by a conflict.
					// If desired is shared, its container is the one the second
					// attempt will write.
					d.Spec.Template.Spec.Containers[0].Image = "ghcr.io/acme/backend:FIRST-ATTEMPT"
					return errors.NewConflict(schema.GroupResource{Group: "apps", Resource: "deployments"}, "backend", context.DeadlineExceeded)
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()
	r := &AppReconciler{Client: fakeClient, Scheme: scheme}

	require.NoError(t, r.reconcileDeployment(ctx, app, nil, "gen-1", ""))

	var got appsv1.Deployment
	require.NoError(t, fakeClient.Get(ctx, types.NamespacedName{Name: "backend", Namespace: "hrportal-prod"}, &got))
	assert.Equal(t, "ghcr.io/acme/backend:2026-08-02", got.Spec.Template.Spec.Containers[0].Image,
		"the second attempt must decide from the app, not from what the first attempt left behind")
}

// A rotation moves the app onto a new credential object and leaves the previous
// one behind, so something has to remove it or every rotation strands a
// plaintext token in the namespace. Only what the live spec does not name goes,
// and only once it is old enough that a write still on its way to the CR is not
// mistaken for an orphan.
func TestSweepGitCredentials_RemovesTheGenerationsTheAppHasMovedOff(t *testing.T) {
	current := secretname.GitCredential("web", secretname.GitCredentialDigest("current", "github.com"))
	previous := secretname.GitCredential("web", secretname.GitCredentialDigest("previous", "github.com"))
	legacy := secretname.LegacyGitCredential("web")
	old := metav1.NewTime(metav1.Now().Add(-time.Hour))

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-test", UID: "app-uid"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			URL: "https://github.com/acme/web.git", CredentialsSecret: current,
		}},
	}
	mine := func(name string, created metav1.Time) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop-test", CreationTimestamp: created,
			Labels: map[string]string{kipperLabel: kipperValue, "kipper.run/app": "web"},
		}}
	}
	// Moved off, labelled, past grace — collectable but for one thing: another
	// actor co-owns it, and an owner that does not control still governs
	// collection, so it is not this app's to remove.
	coOwned := mine(secretname.GitCredential("web", secretname.GitCredentialDigest("shared", "github.com")), old)
	coOwned.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "example.com/v1", Kind: "Backup", Name: "nightly", UID: "a-backup",
	}}

	// A stranger's object that happens to sit under the app's prefix.
	foreign := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      secretname.GitCredential("web", secretname.GitCredentialDigest("theirs", "example.com")),
		Namespace: "shop-test", CreationTimestamp: old,
	}}
	justWritten := mine(secretname.GitCredential("web", secretname.GitCredentialDigest("inflight", "github.com")), metav1.Now())
	// Carries this app's writer labels but is not a credential. Nothing stamps
	// both labels on anything else today, so the sweep's name check is what
	// keeps that true when something does.
	notACredential := mine("web-something-else", old)

	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(app, mine(current, old), mine(previous, old), mine(legacy, old), foreign, justWritten, notACredential, coOwned).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	_, sweepErr := r.sweepGitCredentials(context.Background(), app)
	require.NoError(t, sweepErr)

	remaining := map[string]bool{}
	var list corev1.SecretList
	require.NoError(t, c.List(context.Background(), &list, crclient.InNamespace("shop-test")))
	for i := range list.Items {
		remaining[list.Items[i].Name] = true
	}

	assert.True(t, remaining[current], "the credential the app names was deleted")
	assert.False(t, remaining[previous], "a generation the app moved off was left behind")
	assert.False(t, remaining[legacy], "the pre-generation credential was left behind after a rotation")
	assert.True(t, remaining[foreign.Name], "a Secret without the writer labels was deleted")
	assert.True(t, remaining[justWritten.Name], "a credential still on its way to the CR was deleted")
	assert.True(t, remaining[notACredential.Name],
		"a labelled Secret that is not a credential was deleted by the credential sweep")
	assert.True(t, remaining[coOwned.Name],
		"a credential another actor co-owns was deleted, though its lifetime is not this app's to end")
}

// Deleting an App has to take every generation of its credential with it, not
// only the name credentials used to have. Each one is a plaintext token, and
// once the App is gone nothing else knows they were its.
func TestSweepWriterSecrets_TakesEveryGenerationOfTheCredential(t *testing.T) {
	current := secretname.GitCredential("web", secretname.GitCredentialDigest("current", "github.com"))
	previous := secretname.GitCredential("web", secretname.GitCredentialDigest("previous", "github.com"))
	legacy := secretname.LegacyGitCredential("web")

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-test", UID: "app-uid"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			URL: "https://github.com/acme/web.git", CredentialsSecret: current,
		}},
	}
	mine := func(name string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop-test",
			Labels: map[string]string{kipperLabel: kipperValue, "kipper.run/app": "web"},
		}}
	}
	// Co-owned by another actor: deleting the App does not entitle this sweep
	// to end a lifetime something else also holds.
	coOwned := mine(secretname.GitCredential("web", secretname.GitCredentialDigest("shared", "github.com")))
	coOwned.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "example.com/v1", Kind: "Backup", Name: "nightly", UID: "a-backup",
	}}

	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(app, mine(current), mine(previous), mine(legacy), coOwned).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	require.NoError(t, r.sweepWriterSecrets(context.Background(), app))

	var list corev1.SecretList
	require.NoError(t, c.List(context.Background(), &list, crclient.InNamespace("shop-test")))
	var left []string
	for i := range list.Items {
		left = append(left, list.Items[i].Name)
	}
	assert.Equal(t, []string{coOwned.Name}, left,
		"deleting the app either left its own credentials behind or took one another actor co-owns: %v", left)
}

// Skipping a credential inside the grace period only defers the work if
// something comes back for it. Nothing maps a Secret event to its App, and a
// stable app produces no further events of its own, so without a requeue the
// plaintext token the app rotated off stays in the namespace for as long as the
// app is quiet. The env sweep beside this one already reports its remaining
// time for the same reason.
func TestSweepGitCredentials_AsksToBeCalledBackWhileSomethingIsInsideTheGrace(t *testing.T) {
	current := secretname.GitCredential("web", secretname.GitCredentialDigest("current", "github.com"))
	justRotatedOff := secretname.GitCredential("web", secretname.GitCredentialDigest("previous", "github.com"))

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-test", UID: "app-uid"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			URL: "https://github.com/acme/web.git", CredentialsSecret: current,
		}},
	}
	mine := func(name string, created metav1.Time) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: "shop-test", CreationTimestamp: created,
			Labels: map[string]string{kipperLabel: kipperValue, "kipper.run/app": "web"},
		}}
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(app, mine(current, metav1.Now()), mine(justRotatedOff, metav1.Now())).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	retryIn, err := r.sweepGitCredentials(context.Background(), app)
	require.NoError(t, err)

	assert.Greater(t, retryIn, time.Duration(0),
		"a credential held back by the grace period was left with nothing to collect it")

	var list corev1.SecretList
	require.NoError(t, c.List(context.Background(), &list, crclient.InNamespace("shop-test")))
	assert.Len(t, list.Items, 2, "the grace period did not hold the young credential back")
}

// Nothing mapped an unowned credential Secret back to its
// App: `Owns` covers only children carrying an owner reference, and the two
// Secret watches map the registry and service credentials. So a credential left
// behind by a failed write sat in the namespace until something unrelated
// touched the App, which for a stable app is the resync at best. Removing the
// eager delete without this trades a race for an unbounded leak.
func TestEnqueueAppsForGitCredential_ReachesTheAppThatOwnsIt(t *testing.T) {
	r := &AppReconciler{}

	mine := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      secretname.GitCredential("web", secretname.GitCredentialDigest("t", "github.com")),
		Namespace: "shop-test",
		Labels:    map[string]string{kipperLabel: kipperValue, "kipper.run/app": "web"},
	}}
	assert.Equal(t,
		[]reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: "shop-test", Name: "web"}}},
		r.enqueueAppsForGitCredential(context.Background(), mine),
		"an orphaned credential could not reach the app whose sweep collects it")

	// A Secret carrying the labels but not a credential name belongs to some
	// other writer, and a foreign object must not enqueue anything.
	notACredential := mine.DeepCopy()
	notACredential.Name = "web-something-else"
	assert.Empty(t, r.enqueueAppsForGitCredential(context.Background(), notACredential))

	unlabelled := mine.DeepCopy()
	unlabelled.Labels = nil
	assert.Empty(t, r.enqueueAppsForGitCredential(context.Background(), unlabelled))
}

// Creation time protects a newly created pair, not a
// pre-existing one being committed again. Rotate away from B and back to it:
// the writer finds B already there, the reconcile still sees the old spec and B
// past its grace, and B is deleted just as the App commits onto it. The API
// answers 200 and the credential is gone.
// So a writer that reuses an object says so, and the sweep takes the later of
// the two times.
func TestSweepGitCredentials_KeepsACredentialAWriterHasJustClaimed(t *testing.T) {
	current := secretname.GitCredential("web", secretname.GitCredentialDigest("current", "github.com"))
	rotatingBackTo := secretname.GitCredential("web", secretname.GitCredentialDigest("previous", "github.com"))
	old := metav1.NewTime(metav1.Now().Add(-time.Hour))

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-test", UID: "app-uid"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			URL: "https://github.com/acme/web.git", CredentialsSecret: current,
		}},
	}
	claimed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: rotatingBackTo, Namespace: "shop-test", CreationTimestamp: old,
		Labels:      map[string]string{kipperLabel: kipperValue, "kipper.run/app": "web"},
		Annotations: map[string]string{labels.AnnoGitCredentialClaimed: metav1.Now().Format(time.RFC3339)},
	}}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app, claimed).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	retryIn, err := r.sweepGitCredentials(context.Background(), app)
	require.NoError(t, err)
	assert.Greater(t, retryIn, time.Duration(0))

	var still corev1.Secret
	assert.NoError(t, c.Get(context.Background(),
		crclient.ObjectKey{Namespace: "shop-test", Name: rotatingBackTo}, &still),
		"a credential a writer was committing onto was swept out from under it")
}

// A claim from the future was "clamped" to now on every
// pass, which recomputes the full grace each time: the token was never
// collected and the App requeued every two minutes for as long as the claim
// said. The clamp changed retention in no case at all.
// A claim can only be about a commit happening now, so one further ahead than
// the grace is not a claim. Falling back to creation time bounds the hold.
func TestSweepGitCredentials_DoesNotTrustAClaimFromTheFuture(t *testing.T) {
	current := secretname.GitCredential("web", secretname.GitCredentialDigest("current", "github.com"))
	stranded := secretname.GitCredential("web", secretname.GitCredentialDigest("previous", "github.com"))

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-test", UID: "app-uid"},
		Spec: kipperv1.AppSpec{Git: &kipperv1.AppGitSource{
			URL: "https://github.com/acme/web.git", CredentialsSecret: current,
		}},
	}
	// Written long ago, claimed by a machine whose clock is a year out.
	wild := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: stranded, Namespace: "shop-test",
		CreationTimestamp: metav1.NewTime(metav1.Now().Add(-time.Hour)),
		Labels:            map[string]string{kipperLabel: kipperValue, "kipper.run/app": "web"},
		Annotations: map[string]string{
			labels.AnnoGitCredentialClaimed: metav1.Now().Add(365 * 24 * time.Hour).Format(time.RFC3339),
		},
	}}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(app, wild).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	retryIn, err := r.sweepGitCredentials(context.Background(), app)
	require.NoError(t, err)
	assert.Zero(t, retryIn, "the sweep asked to be woken again for a credential it should have collected")

	var gone corev1.Secret
	err = c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-test", Name: stranded}, &gone)
	assert.True(t, errors.IsNotFound(err),
		"a claim from the future held a plaintext token past its grace")
}

// Adoption takes an object nothing owns, and leaves everything else. A Secret
// still owned by an App of this name that is gone is one garbage collection is
// already entitled to remove, and installing a live owner does not recall a
// deletion it may have issued, so adopting it would tie the live App to an
// object that can vanish.
func TestAdoptWriterSecrets_TakesWhatNothingOwnsAndLeavesTheRest(t *testing.T) {
	name := secretname.GitCredential("web", secretname.GitCredentialDigest("t", "github.com"))
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-test", UID: "the-app-that-exists-now"},
		Spec:       kipperv1.AppSpec{Git: &kipperv1.AppGitSource{URL: "https://github.com/acme/web.git", CredentialsSecret: name}},
	}
	dead := true
	orphaned := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "shop-test",
		Labels: map[string]string{kipperLabel: kipperValue, "kipper.run/app": "web"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
			Name: "web", UID: "the-app-that-was-deleted", Controller: &dead,
		}},
	}}
	// A stranger's object at the name this app's spec points at, so adoption
	// genuinely reaches it. Naming a different generation would put it outside
	// the candidate set and prove nothing.
	foreignApp := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "shop-test", UID: "the-other-app"},
		Spec:       kipperv1.AppSpec{Git: &kipperv1.AppGitSource{URL: "https://github.com/acme/other.git", CredentialsSecret: secretname.GitCredential("other", secretname.GitCredentialDigest("t", "github.com"))}},
	}
	foreignOwned := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: foreignApp.Spec.Git.CredentialsSecret, Namespace: "shop-test",
		Labels: map[string]string{kipperLabel: kipperValue, "kipper.run/app": "other"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "serving.example.com/v1", Kind: "Service",
			Name: "something-else", UID: "a-service", Controller: &dead,
		}},
	}}

	unowned := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name:      secretname.GitCredential("web", secretname.GitCredentialDigest("free", "github.com")),
		Namespace: "shop-test",
		Labels:    map[string]string{kipperLabel: kipperValue, "kipper.run/app": "web"},
	}}

	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(app, foreignApp, orphaned, foreignOwned, unowned).Build()
	r := &AppReconciler{Client: c, Scheme: testScheme()}

	// Still owned by the incarnation before a delete and recreate, so garbage
	// collection is already entitled to remove it: adoption leaves it.
	require.NoError(t, r.adoptWriterSecrets(context.Background(), app))
	var left corev1.Secret
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-test", Name: name}, &left))
	require.Len(t, left.OwnerReferences, 1)
	assert.Equal(t, "the-app-that-was-deleted", string(left.OwnerReferences[0].UID),
		"an object garbage collection may already be removing was adopted by the live app")

	// Nothing owns this one, which is what adoption is for.
	app.Spec.Git.CredentialsSecret = unowned.Name
	require.NoError(t, r.adoptWriterSecrets(context.Background(), app))
	var free corev1.Secret
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-test", Name: unowned.Name}, &free))
	require.Len(t, free.OwnerReferences, 1)
	assert.Equal(t, "the-app-that-exists-now", string(free.OwnerReferences[0].UID),
		"a credential nothing owned was not adopted")

	// A stranger's object is left however often it is reconciled.
	require.NoError(t, r.adoptWriterSecrets(context.Background(), foreignApp))
	var foreign corev1.Secret
	require.NoError(t, c.Get(context.Background(), crclient.ObjectKey{Namespace: "shop-test", Name: foreignOwned.Name}, &foreign))
	require.Len(t, foreign.OwnerReferences, 1)
	assert.Equal(t, "something-else", foreign.OwnerReferences[0].Name,
		"a Secret another object controls was taken over")
}
