package controllers

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// adoptionWorld pairs a legacy Deployment and serving pod with the upgraded
// reconciler. Its client simulates API server defaulting.
type adoptionWorld struct {
	t      *testing.T
	ctx    context.Context
	c      crclient.WithWatch
	r      *AppReconciler
	app    *kipperv1.App
	dialer *fakeDialer
	writes int
}

func newAdoptionWorld(t *testing.T, routed bool, opts ...func(*interceptor.Funcs)) *adoptionWorld {
	t.Helper()
	w := &adoptionWorld{t: t, ctx: context.Background()}
	app := newTestApp()
	app.UID = "uid-my-app"
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	replicas := int32(1)
	app.Spec.Replicas = &replicas
	if routed {
		app.Spec.Route = &kipperv1.AppRoute{}
	}
	scheme := testScheme()
	funcs := interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if d, ok := obj.(*appsv1.Deployment); ok {
				defaultPodTemplate(&d.Spec.Template)
				d.UID = "uid-my-app-deploy"
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
			d, ok := obj.(*appsv1.Deployment)
			if !ok {
				return c.Update(ctx, obj, opts...)
			}
			defaultPodTemplate(&d.Spec.Template)
			if isDryRun(opts) {
				return nil
			}
			w.writes++
			return c.Update(ctx, obj, opts...)
		},
	}
	for _, o := range opts {
		o(&funcs)
	}
	w.c = crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).WithStatusSubresource(app).
		WithInterceptorFuncs(funcs).Build()
	w.app = app

	before := &AppReconciler{Client: w.c, Scheme: scheme, SidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest"}
	w.reconcile(before)
	dep := w.deployment()
	for i := range dep.Spec.Template.Spec.Containers {
		dep.Spec.Template.Spec.Containers[i].ReadinessProbe = nil
	}
	dep.Spec.Template.Spec.TerminationGracePeriodSeconds = nil
	require.NoError(t, w.c.Update(w.ctx, dep))
	w.addPod("10.0.0.1", time.Hour)

	w.dialer = &fakeDialer{answers: map[string]error{}}
	w.r = &AppReconciler{
		Client: w.c, Scheme: scheme, SidecarImage: before.SidecarImage,
		PreStopSleep: true, Dial: w.dialer.dial, Recorder: record.NewFakeRecorder(10),
	}
	w.writes = 0
	return w
}

func (w *adoptionWorld) reconcile(r *AppReconciler) {
	w.t.Helper()
	_, err := r.Reconcile(w.ctx, ctrl.Request{NamespacedName: types.NamespacedName{Name: w.app.Name, Namespace: w.app.Namespace}})
	require.NoError(w.t, err)
}

func (w *adoptionWorld) deployment() *appsv1.Deployment {
	w.t.Helper()
	var dep appsv1.Deployment
	require.NoError(w.t, w.c.Get(w.ctx, types.NamespacedName{Name: w.app.Name, Namespace: w.app.Namespace}, &dep))
	return &dep
}

func (w *adoptionWorld) editApp(edit func(*kipperv1.App)) {
	w.t.Helper()
	require.NoError(w.t, w.c.Get(w.ctx, types.NamespacedName{Name: w.app.Name, Namespace: w.app.Namespace}, w.app))
	edit(w.app)
	require.NoError(w.t, w.c.Update(w.ctx, w.app))
}

// addPod gives the Deployment a ReplicaSet and a Running, Ready pod in it.
func (w *adoptionWorld) addPod(ip string, age time.Duration) {
	w.t.Helper()
	dep := w.deployment()
	controller := true
	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{
		Name: dep.Name + "-" + ip, Namespace: dep.Namespace, UID: types.UID("rs-" + ip), Labels: map[string]string{"app": dep.Name},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: dep.Name, UID: dep.UID, Controller: &controller}},
	}}
	require.NoError(w.t, w.c.Create(w.ctx, rs))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: rs.Name + "-pod", Namespace: dep.Namespace, Labels: map[string]string{"app": dep.Name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.Name, UID: rs.UID, Controller: &controller}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: dep.Name, Image: w.app.Spec.Image}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning, PodIP: ip,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: dep.Name, State: corev1.ContainerState{
				Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(time.Now().Add(-age))},
			}}},
		},
	}
	require.NoError(w.t, w.c.Create(w.ctx, pod))
}

func assertShaped(t *testing.T, tpl corev1.PodTemplateSpec, wantProbe bool) {
	t.Helper()
	for _, c := range tpl.Spec.Containers {
		require.NotNil(t, c.Lifecycle, "container %s drains", c.Name)
		assert.Equal(t, int64(preStopSleepSeconds), c.Lifecycle.PreStop.Sleep.Seconds)
		if c.Name == sidecarContainerName {
			assert.NotNil(t, c.ReadinessProbe, "the sidecar is checked")
		}
	}
	require.NotNil(t, tpl.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, int64(shapedGracePeriodSeconds), *tpl.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, wantProbe, tpl.Spec.Containers[0].ReadinessProbe != nil)
}

func TestAdoption_AnUpgradeRestartsNothing(t *testing.T) {
	w := newAdoptionWorld(t, true)
	before := w.deployment().Spec.Template

	w.reconcile(w.r)
	w.reconcile(w.r)

	assert.Equal(t, before, w.deployment().Spec.Template, "an unchanged app keeps the template it runs")
	assert.Empty(t, w.dialer.dialled, "evidence is only read on a roll")
}

func TestAdoption_AnImageChangeTakesTheShapeAndTheCheck(t *testing.T) {
	w := newAdoptionWorld(t, true)
	w.editApp(func(a *kipperv1.App) { a.Spec.Image = "myimage:2" })

	w.reconcile(w.r)

	tpl := w.deployment().Spec.Template
	assert.Equal(t, "myimage:2", tpl.Spec.Containers[0].Image)
	assertShaped(t, tpl, true)
	assert.Equal(t, "tcp", tpl.Annotations[inferredReadinessAnnotation])
	assert.Equal(t, []string{"10.0.0.1:8080"}, w.dialer.dialled)

	before := w.deployment().Spec.Template
	w.dialer.dialled = nil
	w.reconcile(w.r)
	assert.Equal(t, before, w.deployment().Spec.Template, "and it settles there")
	assert.Empty(t, w.dialer.dialled, "tcp is kept, not asked again")
}

func TestAdoption_AnEnvEditOnALegacyAppStillHolds(t *testing.T) {
	w := newAdoptionWorld(t, true)
	before := w.deployment().Spec.Template

	w.editApp(func(a *kipperv1.App) { a.Spec.Env = map[string]string{"LOG_LEVEL": "debug"} })
	w.reconcile(w.r)

	assert.Equal(t, before, w.deployment().Spec.Template, "an env edit does not restart a running app, upgraded or not")
	assert.Empty(t, w.dialer.dialled)
}

func TestAdoption_ARestartAdopts(t *testing.T) {
	w := newAdoptionWorld(t, false)
	w.editApp(func(a *kipperv1.App) {
		a.Annotations = map[string]string{"kipper.run/restartedAt": "2026-10-01T09:00:00Z"}
	})

	w.reconcile(w.r)

	assertShaped(t, w.deployment().Spec.Template, true)
}

func TestAdoption_ARefusedDryRunDeploysWithoutAdopting(t *testing.T) {
	w := newAdoptionWorld(t, true, func(f *interceptor.Funcs) {
		next := f.Update
		f.Update = func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.UpdateOption) error {
			if _, ok := obj.(*appsv1.Deployment); ok && isDryRun(opts) {
				return errors.NewBadRequest("the server does not support dry run")
			}
			return next(ctx, c, obj, opts...)
		}
	})
	recorder := w.r.Recorder.(*record.FakeRecorder)

	w.editApp(func(a *kipperv1.App) { a.Spec.Image = "myimage:2" })
	w.reconcile(w.r)
	w.editApp(func(a *kipperv1.App) { a.Spec.Image = "myimage:3" })
	w.reconcile(w.r)

	tpl := w.deployment().Spec.Template
	assert.Equal(t, "myimage:3", tpl.Spec.Containers[0].Image, "deploys still land")
	assert.Nil(t, tpl.Spec.Containers[0].Lifecycle, "but the shape is not taken on")
	assert.Nil(t, tpl.Spec.Containers[0].ReadinessProbe)
	assert.Empty(t, w.dialer.dialled)

	var adoption int
	for len(recorder.Events) > 0 {
		if e := <-recorder.Events; strings.Contains(e, "AdoptionUnavailable") {
			adoption++
		}
	}
	assert.Equal(t, 1, adoption, "said once, not on every pass")
}

func TestAdoption_AFailedVersionReadThatRecoversRollsNothing(t *testing.T) {
	w := newAdoptionWorld(t, true)
	w.r.PreStopSleep = false
	w.editApp(func(a *kipperv1.App) { a.Spec.Image = "myimage:2" })
	w.reconcile(w.r)
	require.Nil(t, w.deployment().Spec.Template.Spec.Containers[0].Lifecycle)
	before := w.deployment().Spec.Template

	w.r.PreStopSleep = true
	w.reconcile(w.r)

	assert.Equal(t, before, w.deployment().Spec.Template)
}

// Retry inconclusive inference on later rollouts, even after the app has
// adopted the platform probe and shutdown settings.
func TestAdoption_APendingAppIsAskedAtItsNextRoll(t *testing.T) {
	for _, tc := range []struct {
		stored inference
		routed bool
	}{{inferencePending, false}, {inferredNone, false}, {inferencePending, true}, {inferredNone, true}} {
		stored := tc.stored
		t.Run(fmt.Sprintf("stored %q routed %v", stored, tc.routed), func(t *testing.T) {
			w := newAdoptionWorld(t, tc.routed)
			w.dialer.answers["10.0.0.1"] = refused
			w.editApp(func(a *kipperv1.App) { a.Spec.Image = "myimage:2" })
			w.reconcile(w.r)
			dep := w.deployment()
			assertShaped(t, dep.Spec.Template, false)
			applyPlatformShape(&dep.Spec.Template, w.app, stored, true)
			require.NoError(t, w.c.Update(w.ctx, dep))

			w.dialer.dialled = nil
			w.writes = 0
			w.reconcile(w.r)
			assert.Zero(t, w.writes, "an unchanged app is not written")
			assert.Empty(t, w.dialer.dialled, "and not asked")

			w.editApp(func(a *kipperv1.App) { a.Spec.Env = map[string]string{"LOG_LEVEL": "debug"} })
			w.reconcile(w.r)
			assert.Empty(t, w.dialer.dialled, "a held env edit is not a roll")

			delete(w.dialer.answers, "10.0.0.1")
			w.editApp(func(a *kipperv1.App) { a.Spec.Image = "myimage:3" })
			w.reconcile(w.r)
			tpl := w.deployment().Spec.Template
			assert.NotEmpty(t, w.dialer.dialled, "a roll asks again")
			assert.NotNil(t, tpl.Spec.Containers[0].ReadinessProbe, "and the new pods are gated")
			assert.Equal(t, "tcp", tpl.Annotations[inferredReadinessAnnotation])
		})
	}
}

func TestAdoption_DeclaringAndClearingACheckRolls(t *testing.T) {
	w := newAdoptionWorld(t, false)
	w.editApp(func(a *kipperv1.App) { a.Spec.Health = &kipperv1.AppHealth{Type: "http", Path: "/ready"} })
	w.reconcile(w.r)

	tpl := w.deployment().Spec.Template
	require.NotNil(t, tpl.Spec.Containers[0].ReadinessProbe)
	assert.Equal(t, "/ready", tpl.Spec.Containers[0].ReadinessProbe.HTTPGet.Path)
	assert.NotContains(t, tpl.Annotations, inferredReadinessAnnotation)
	assertShaped(t, tpl, true)
	assert.Empty(t, w.dialer.dialled, "a declared check is not inferred")

	w.editApp(func(a *kipperv1.App) { a.Spec.Health = nil })
	w.reconcile(w.r)

	tpl = w.deployment().Spec.Template
	require.NotNil(t, tpl.Spec.Containers[0].ReadinessProbe, "automatic again: the port accepts")
	assert.NotNil(t, tpl.Spec.Containers[0].ReadinessProbe.TCPSocket)
	assert.Equal(t, "tcp", tpl.Annotations[inferredReadinessAnnotation])
}

func TestAdoption_ANewAppIsCreatedShaped(t *testing.T) {
	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{}
	c := defaultingClient(testScheme(), app)
	r := &AppReconciler{Client: c, Scheme: testScheme(), SidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest", PreStopSleep: true}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}})
	require.NoError(t, err)

	var dep appsv1.Deployment
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: app.Name, Namespace: app.Namespace}, &dep))
	assertShaped(t, dep.Spec.Template, false)
}

// A ready pod accepting connections is sufficient evidence even during
// a rapid second deployment.
func TestAdoption_ARapidSecondDeployIsGated(t *testing.T) {
	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{}
	c := defaultingClient(testScheme(), app)
	d := &fakeDialer{answers: map[string]error{}}
	r := &AppReconciler{Client: c, Scheme: testScheme(), SidecarImage: "ghcr.io/getkipper/kipper-sidecar:latest", PreStopSleep: true, Dial: d.dial}
	w := &adoptionWorld{t: t, ctx: context.Background(), c: c, r: r, app: app, dialer: d}
	w.reconcile(r)
	require.Nil(t, w.deployment().Spec.Template.Spec.Containers[0].ReadinessProbe, "a brand-new app has nothing to infer from")
	w.addPod("10.0.0.7", time.Minute)

	w.editApp(func(a *kipperv1.App) { a.Spec.Image = "myimage:2" })
	w.reconcile(r)

	tpl := w.deployment().Spec.Template
	require.NotNil(t, tpl.Spec.Containers[0].ReadinessProbe, "the second deploy's new pods are gated")
	assert.Equal(t, "tcp", tpl.Annotations[inferredReadinessAnnotation])
}

// Declaring an inferred TCP check removes the inference annotation,
// triggering a rollout even when the probe itself is unchanged.
func TestAdoption_DeclaringTheInferredCheckRolls(t *testing.T) {
	w := newAdoptionWorld(t, false)
	w.editApp(func(a *kipperv1.App) { a.Spec.Image = "myimage:2" })
	w.reconcile(w.r)
	require.Equal(t, "tcp", w.deployment().Spec.Template.Annotations[inferredReadinessAnnotation])

	w.editApp(func(a *kipperv1.App) { a.Spec.Health = &kipperv1.AppHealth{Type: "tcp"} })
	w.reconcile(w.r)

	assert.NotContains(t, w.deployment().Spec.Template.Annotations, inferredReadinessAnnotation)
}
