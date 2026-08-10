package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// retiredGeneration is an old published environment of the given app, owned by
// it and no longer named by anything the app writes.
func retiredGeneration(app *kipperv1.App, digest string) *corev1.Secret {
	immutable := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretname.EnvGeneration(secretname.KindApp, app.Name, digest),
			Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(app, app.GroupVersionKind()),
			},
		},
		Immutable: &immutable,
		Data:      map[string][]byte{"LOG_LEVEL": []byte("old")},
	}
}

func envPodSpec(secretName string) corev1.PodSpec {
	return corev1.PodSpec{Containers: []corev1.Container{{
		Name:  "app",
		Image: "app:1",
		EnvFrom: []corev1.EnvFromSource{{
			SecretRef: &corev1.SecretEnvSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
			},
		}},
	}}}
}

func mustRetire(t *testing.T, ctx context.Context, c crclient.Client, app *kipperv1.App, current string) time.Duration {
	t.Helper()
	wait, _, err := retireEnvSecrets(ctx, c, c, app, secretname.KindApp, current, nil)
	require.NoError(t, err)
	return wait
}

func secretExists(t *testing.T, ctx context.Context, c crclient.Client, name, namespace string) bool {
	t.Helper()
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &corev1.Secret{})
	return err == nil
}

// One pass marks, a later one deletes. A pass that could do both would delete an
// environment on the strength of a single look, and a CronJob controller can
// create a native Job from a template between the look and the delete.
func TestRetireEnvSecrets_NeedsAMaturedMarkBeforeDeleting(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	old := retiredGeneration(app, "aaaaaaaaaaaa")

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, old).Build()

	wait := mustRetire(t, ctx, c, app, "app-my-app-env-current")
	assert.Equal(t, envRetirementGrace, wait, "the pass that marks must ask to be woken for the grace")
	require.True(t, secretExists(t, ctx, c, old.Name, app.Namespace),
		"a first look must not delete anything")

	var marked corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: old.Name, Namespace: app.Namespace}, &marked))
	require.NotEmpty(t, marked.Annotations[unreferencedSinceAnnotation])

	// A second pass, still inside the grace.
	wait = mustRetire(t, ctx, c, app, "app-my-app-env-current")
	assert.Greater(t, wait, time.Duration(0), "an immature mark must still be waited on")
	assert.True(t, secretExists(t, ctx, c, old.Name, app.Namespace),
		"the grace is the protection, so it has to be waited out")

	// The mark matures.
	marked.Annotations[unreferencedSinceAnnotation] =
		time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano)
	require.NoError(t, c.Update(ctx, &marked))

	mustRetire(t, ctx, c, app, "app-my-app-env-current")
	assert.False(t, secretExists(t, ctx, c, old.Name, app.Namespace),
		"an environment nothing has read for the whole grace is swept")
}

// A reference that comes back resets the clock rather than resuming it. A
// rollback puts an old ReplicaSet back in charge, and its pods read the
// environment it names.
func TestRetireEnvSecrets_AReturningReferenceClearsTheMark(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	old := retiredGeneration(app, "aaaaaaaaaaaa")

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, old).Build()
	mustRetire(t, ctx, c, app, "app-my-app-env-current")

	var marked corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: old.Name, Namespace: app.Namespace}, &marked))
	require.NotEmpty(t, marked.Annotations[unreferencedSinceAnnotation])

	// Something reads it again, and the mark is already old enough to delete on.
	marked.Annotations[unreferencedSinceAnnotation] =
		time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano)
	require.NoError(t, c.Update(ctx, &marked))
	require.NoError(t, c.Create(ctx, &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app-rollback", Namespace: app.Namespace},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "my-app"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "my-app"}},
				Spec:       envPodSpec(old.Name),
			},
		},
	}))

	mustRetire(t, ctx, c, app, "app-my-app-env-current")
	require.True(t, secretExists(t, ctx, c, old.Name, app.Namespace),
		"an environment something reads again must survive its own matured mark")

	var cleared corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: old.Name, Namespace: app.Namespace}, &cleared))
	assert.Empty(t, cleared.Annotations[unreferencedSinceAnnotation],
		"and the clock must start over rather than resume")
}

// Every shape that can still produce a pod counts, including the native Job a
// CronJob creates, which an ownership walk would have to be taught about.
func TestRetireEnvSecrets_KeepsWhatAnyPodProducingTemplateReads(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")

	cases := []struct {
		name   string
		object crclient.Object
	}{
		{"a live pod", &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "reader", Namespace: app.Namespace},
			Spec:       envPodSpec(secretname.EnvGeneration(secretname.KindApp, app.Name, "aaaaaaaaaaaa")),
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		}},
		{"a retained replicaset", &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: "reader", Namespace: app.Namespace},
			Spec: appsv1.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "x"}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "x"}},
					Spec:       envPodSpec(secretname.EnvGeneration(secretname.KindApp, app.Name, "aaaaaaaaaaaa")),
				},
			},
		}},
		{"a native job a cronjob created", &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "reader-28900", Namespace: app.Namespace},
			Spec: batchv1.JobSpec{Template: corev1.PodTemplateSpec{
				Spec: envPodSpec(secretname.EnvGeneration(secretname.KindApp, app.Name, "aaaaaaaaaaaa")),
			}},
		}},
		{"a cronjob template", &batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: "reader", Namespace: app.Namespace},
			Spec: batchv1.CronJobSpec{
				Schedule: "0 2 * * *",
				JobTemplate: batchv1.JobTemplateSpec{Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: envPodSpec(secretname.EnvGeneration(secretname.KindApp, app.Name, "aaaaaaaaaaaa")),
					},
				}},
			},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old := retiredGeneration(app, "aaaaaaaaaaaa")
			c := crfake.NewClientBuilder().WithScheme(scheme).
				WithObjects(app, old, tc.object).Build()

			// Marked long ago, so only the consumer stands between it and deletion.
			old.Annotations = map[string]string{
				unreferencedSinceAnnotation: time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano),
			}
			require.NoError(t, c.Update(ctx, old))

			mustRetire(t, ctx, c, app, "app-my-app-env-current")
			assert.True(t, secretExists(t, ctx, c, old.Name, app.Namespace),
				"%s still reads this environment", tc.name)
		})
	}
}

// A finished pod cannot restart, so it cannot re-read anything.
func TestRetireEnvSecrets_AFinishedPodIsNotAConsumer(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	old := retiredGeneration(app, "aaaaaaaaaaaa")
	old.Annotations = map[string]string{
		unreferencedSinceAnnotation: time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano),
	}

	done := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "finished", Namespace: app.Namespace},
		Spec:       envPodSpec(old.Name),
		Status:     corev1.PodStatus{Phase: corev1.PodSucceeded},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, old, done).Build()

	mustRetire(t, ctx, c, app, "app-my-app-env-current")
	assert.False(t, secretExists(t, ctx, c, old.Name, app.Namespace),
		"a pod that has finished holds nothing open")
}

// The environment this pass published is never a candidate, whatever the scan
// says: a pod template written moments ago may not have reached the reader.
func TestRetireEnvSecrets_NeverSweepsTheCurrentEnvironment(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	current := retiredGeneration(app, "bbbbbbbbbbbb")
	current.Annotations = map[string]string{
		unreferencedSinceAnnotation: time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano),
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, current).Build()

	mustRetire(t, ctx, c, app, current.Name)
	assert.True(t, secretExists(t, ctx, c, current.Name, app.Namespace),
		"the environment just published must survive even an old mark")
}

// Somebody else's Secret at a name that looks like ours is not ours to delete.
func TestRetireEnvSecrets_LeavesAForeignSecretAlone(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")

	foreign := retiredGeneration(app, "aaaaaaaaaaaa")
	foreign.OwnerReferences = nil
	foreign.Annotations = map[string]string{
		unreferencedSinceAnnotation: time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano),
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, foreign).Build()

	mustRetire(t, ctx, c, app, "app-my-app-env-current")
	assert.True(t, secretExists(t, ctx, c, foreign.Name, app.Namespace),
		"a name this app did not write is not this app's to sweep")
}

// The environment this pass published is the strongest reference there is, so a
// mark left on it from before a rollback must be cleared. Otherwise the next
// roll-forward deletes it on a stale mark with no grace at all.
func TestRetireEnvSecrets_ClearsTheMarkOnTheCurrentEnvironment(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	current := retiredGeneration(app, "bbbbbbbbbbbb")
	current.Annotations = map[string]string{
		unreferencedSinceAnnotation: time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano),
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, current).Build()
	mustRetire(t, ctx, c, app, current.Name)

	var got corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: current.Name, Namespace: app.Namespace}, &got))
	assert.Empty(t, got.Annotations[unreferencedSinceAnnotation],
		"the environment in use must not keep a mark that would delete it the moment it is superseded")
}

// A StatefulSet scaled to zero still has a template that will produce a pod.
func TestRetireEnvSecrets_KeepsWhatAStatefulSetTemplateReads(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	old := retiredGeneration(app, "aaaaaaaaaaaa")
	old.Annotations = map[string]string{
		unreferencedSinceAnnotation: time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano),
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "reader", Namespace: app.Namespace},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "reader"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "reader"}},
				Spec:       envPodSpec(old.Name),
			},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, old, sts).Build()

	mustRetire(t, ctx, c, app, "app-my-app-env-current")
	assert.True(t, secretExists(t, ctx, c, old.Name, app.Namespace),
		"a statefulset template still reads this environment")
}

// The pre-generation Secret retires like a generation now that publication
// updates it where it exists and creates it nowhere. While it was recreated
// every pass, retiring it would have deleted what the next reconcile wrote back.
func TestRetireEnvSecrets_RetiresThePreGenerationSecret(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretname.Env(secretname.KindApp, app.Name),
			Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(app, app.GroupVersionKind()),
			},
			Annotations: map[string]string{
				unreferencedSinceAnnotation: time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano),
			},
		},
		Data: map[string][]byte{"LOG_LEVEL": []byte("info")},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, legacy).Build()

	mustRetire(t, ctx, c, app, "app-my-app-env-current")
	assert.False(t, secretExists(t, ctx, c, legacy.Name, app.Namespace),
		"nothing reads it and nothing recreates it, so it goes")
}

// The last look before deleting. An hour of absence proves an earlier pass
// found no reference; it does not make a set of LIST calls and a DELETE one
// operation. Looking again narrows that window to the gap between the last read
// and the write, and anything that turned up in the meantime gets its full
// grace back rather than being deleted on the next look.
func TestDeleteAfterRecheck_SparesSomethingThatTurnedUpDuringThePass(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	doomed := retiredGeneration(app, "aaaaaaaaaaaa")
	doomed.Annotations = map[string]string{
		unreferencedSinceAnnotation: time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano),
	}

	// The Job a CronJob controller created from a template it had already read.
	latecomer := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly-28900", Namespace: app.Namespace},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: envPodSpec(doomed.Name)}},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, doomed, latecomer).Build()

	spared, err := deleteAfterRecheck(ctx, c, c, app.Namespace, []*corev1.Secret{doomed})
	require.NoError(t, err)
	assert.True(t, spared, "sparing a candidate must be reported, or nothing schedules another look")
	require.True(t, secretExists(t, ctx, c, doomed.Name, app.Namespace),
		"a job created between the scan and the delete must not lose its environment")

	var got corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: doomed.Name, Namespace: app.Namespace}, &got))
	assert.Empty(t, got.Annotations[unreferencedSinceAnnotation],
		"and it must get the whole grace again rather than dying on the next look")
}

// A generation kept alive by something outside this workload has no other timer.
// The StatefulSet is not owned and not watched, so nothing makes this controller
// look again when it goes away.
func TestRetireEnvSecrets_KeepsLookingWhileAnOutsiderHoldsAGenerationOpen(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	old := retiredGeneration(app, "aaaaaaaaaaaa")

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "outsider", Namespace: app.Namespace},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "outsider"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "outsider"}},
				Spec:       envPodSpec(old.Name),
			},
		},
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, old, sts).Build()

	wait := mustRetire(t, ctx, c, app, "app-my-app-env-current")
	assert.Greater(t, wait, time.Duration(0),
		"an environment held open from outside must still be revisited")
}

// One scan for a batch puts the whole of every earlier deletion inside the
// window of every later one.
func TestDeleteAfterRecheck_ScansAgainForEachCandidate(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	first := retiredGeneration(app, "aaaaaaaaaaaa")
	second := retiredGeneration(app, "bbbbbbbbbbbb")

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, first, second).Build()

	// A consumer of the second candidate appears while the first is being
	// deleted. Interception is the only way to land inside that window.
	guard := &latecomerOnDelete{Client: c, after: first.Name, job: &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly-28900", Namespace: app.Namespace},
		Spec:       batchv1.JobSpec{Template: corev1.PodTemplateSpec{Spec: envPodSpec(second.Name)}},
	}}

	_, err := deleteAfterRecheck(ctx, guard, c, app.Namespace, []*corev1.Secret{first, second})
	require.NoError(t, err)

	assert.False(t, secretExists(t, ctx, c, first.Name, app.Namespace),
		"the first candidate had nothing reading it")
	assert.True(t, secretExists(t, ctx, c, second.Name, app.Namespace),
		"a consumer that appeared during the batch must be seen before the next delete")
}

// latecomerOnDelete creates an object the moment a named Secret is deleted, so a
// test can land something inside the batch window.
type latecomerOnDelete struct {
	crclient.Client
	after string
	job   *batchv1.Job
	done  bool
}

func (l *latecomerOnDelete) Delete(ctx context.Context, obj crclient.Object, opts ...crclient.DeleteOption) error {
	if err := l.Client.Delete(ctx, obj, opts...); err != nil {
		return err
	}
	if !l.done && obj.GetName() == l.after {
		l.done = true
		return l.Create(ctx, l.job)
	}
	return nil
}

// A consumer that appears inside the recheck window is the same situation the
// initial scan handles, reached a moment later, and it needs the same timer.
func TestRetireEnvSecrets_SchedulesARescanForALateConsumer(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	doomed := retiredGeneration(app, "aaaaaaaaaaaa")
	doomed.Annotations = map[string]string{
		unreferencedSinceAnnotation: time.Now().Add(-2 * envRetirementGrace).UTC().Format(time.RFC3339Nano),
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, doomed).Build()

	// Nothing reads it during the first scan; a StatefulSet appears before the
	// recheck. Landing there is what makes this different from the case the
	// first scan sees.
	guard := &latecomerOnList{Client: c, sts: &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "outsider", Namespace: app.Namespace},
		Spec: appsv1.StatefulSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "outsider"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "outsider"}},
				Spec:       envPodSpec(doomed.Name),
			},
		},
	}}

	wait, _, err := retireEnvSecrets(ctx, c, guard, app, secretname.KindApp, "app-my-app-env-current", nil)
	require.NoError(t, err)

	require.True(t, secretExists(t, ctx, c, doomed.Name, app.Namespace),
		"the late consumer must have spared it")
	assert.Greater(t, wait, time.Duration(0),
		"and nothing else will bring this controller back for it")
}

// latecomerOnList creates its StatefulSet after the first Secret list, so it is
// absent from the initial consumer scan and present in the recheck.
type latecomerOnList struct {
	crclient.Client
	sts  *appsv1.StatefulSet
	done bool
}

func (l *latecomerOnList) List(ctx context.Context, list crclient.ObjectList, opts ...crclient.ListOption) error {
	if err := l.Client.List(ctx, list, opts...); err != nil {
		return err
	}
	if _, isSecrets := list.(*corev1.SecretList); isSecrets && !l.done {
		l.done = true
		return l.Create(ctx, l.sts)
	}
	return nil
}

// A pass that stops early left a projection whose binding was removed with
// nothing scheduled to look at it again, because the only sweep was at the end
// of a pass that never got there.
func TestReconcile_SweepsEvenWhenThePassStopsEarly(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	// The binding names a Service that does not exist, so the render refuses
	// before anything downstream runs.
	app.Spec.ServiceBindings = []kipperv1.ServiceBinding{{Name: "missing", Database: "x"}}

	// A projection from a binding that has already been removed, read by nobody.
	orphan := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-app-my-app-credentials", Namespace: app.Namespace,
			Labels: map[string]string{derivedBindingLabel: "true", "app": app.Name},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(app, app.GroupVersionKind()),
			},
		},
		Data: map[string][]byte{"PASSWORD": []byte("s3cret")},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, orphan).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.Error(t, err, "the pass stops on the binding it cannot resolve")

	var marked corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: orphan.Name, Namespace: app.Namespace}, &marked))
	assert.NotEmpty(t, marked.Annotations[unreferencedSinceAnnotation],
		"a projection nothing wants must still be picked up by a pass that failed elsewhere")
}

// The fallback keep-set is a superset of what the render would have said, so a
// failed pass never sweeps a projection the workload still declares.
func TestReconcile_AFailedPassKeepsADeclaredProjection(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.ServiceBindings = []kipperv1.ServiceBinding{{Name: "db", Database: "x"}}

	declared := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretname.Binding("db", secretname.KindApp, app.Name), Namespace: app.Namespace,
			Labels: map[string]string{derivedBindingLabel: "true", "app": app.Name},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(app, app.GroupVersionKind()),
			},
		},
		Data: map[string][]byte{"PASSWORD": []byte("s3cret")},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, declared).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.Error(t, err, "the Service is missing, so the render refuses")

	var kept corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: declared.Name, Namespace: app.Namespace}, &kept))
	assert.Empty(t, kept.Annotations[unreferencedSinceAnnotation],
		"a projection the workload still declares must not be marked by a pass that could not check")
}

// The conversion gate counts objects from before the move, and a retained
// ReplicaSet naming the generation a workload has rolled off is ordinary
// deployment history. Counting it would leave the gate permanently open and the
// message permanently wrong.
func TestRetireEnvSecrets_CountsOnlyPreGenerationReaders(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")

	old := retiredGeneration(app, "aaaaaaaaaaaa")
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretname.Env(secretname.KindApp, app.Name),
			Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(app, app.GroupVersionKind()),
			},
		},
		Data: map[string][]byte{"LOG_LEVEL": []byte("info")},
	}

	// One retained revision reads both: the generation it rolled off, and the
	// pre-generation Secret from before the move.
	revision := func(name, secret string) *appsv1.ReplicaSet {
		return &appsv1.ReplicaSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: app.Namespace},
			Spec: appsv1.ReplicaSetSpec{
				Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app.Name}},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app.Name}},
					Spec:       envPodSpec(secret),
				},
			},
		}
	}
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(
		app, old, legacy,
		revision("my-app-old", old.Name),
		revision("my-app-older", legacy.Name),
	).Build()

	_, readers, err := retireEnvSecrets(ctx, c, c, app, secretname.KindApp, "app-my-app-env-current", nil)
	require.NoError(t, err)
	assert.Equal(t, 1, readers,
		"only the pre-generation Secret counts; the retained generation is ordinary history")
}

// The same guarantee as the App's, for the workload kind that shares its
// binding shapes. Fixing one and claiming both is how the Function was left
// behind the first time.
func TestFunctionReconcile_SweepsEvenWhenThePassStopsEarly(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize"),
		},
		Spec: kipperv1.FunctionSpec{
			Runtime:         "node",
			Source:          &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			ServiceBindings: []kipperv1.ServiceBinding{{Name: "missing", Database: "x"}},
		},
	}

	orphan := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretname.Binding("gone", secretname.KindFunction, fn.Name), Namespace: fn.Namespace,
			Labels: map[string]string{derivedBindingLabel: "true", "app": fn.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Function",
				Name: fn.Name, UID: fn.UID, Controller: func() *bool { b := true; return &b }(),
			}},
		},
		Data: map[string][]byte{"PASSWORD": []byte("s3cret")},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn, orphan).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: fn.Name, Namespace: fn.Namespace,
	}})
	require.Error(t, err, "the pass stops on the binding it cannot resolve")

	var marked corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: orphan.Name, Namespace: fn.Namespace}, &marked))
	assert.NotEmpty(t, marked.Annotations[unreferencedSinceAnnotation],
		"a function's orphaned projection must be picked up by a pass that failed elsewhere")
}

// And the conversion sentence has to survive the status write for every kind,
// not only the App.
func TestFunctionReconcile_PersistsTheConversionGate(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name: "resize", Namespace: "media-test", UID: types.UID("uid-resize"),
		},
		Spec: kipperv1.FunctionSpec{
			Runtime: "node",
			Source:  &kipperv1.FunctionSource{Code: "module.exports = () => 'ok'"},
			Env:     map[string]string{"LOG_LEVEL": "info"},
		},
	}
	// A pre-generation Secret, read by a retained revision.
	controller := true
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretname.Env(secretname.KindFunction, fn.Name), Namespace: fn.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Function",
				Name: fn.Name, UID: fn.UID, Controller: &controller,
			}},
		},
		Data: map[string][]byte{"LOG_LEVEL": []byte("info")},
	}
	revision := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "resize-old", Namespace: fn.Namespace},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": fn.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": fn.Name}},
				Spec:       envPodSpec(legacy.Name),
			},
		},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(fn, legacy, revision).WithStatusSubresource(fn).Build()
	r := &FunctionReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: fn.Name, Namespace: fn.Namespace,
	}})
	require.NoError(t, err)

	var got kipperv1.Function
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: fn.Name, Namespace: fn.Namespace}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionEnvPublished)
	require.NotNil(t, cond)
	assert.Contains(t, cond.Message, "from before generations",
		"the conversion sentence must reach the API for a Function too")
}

// A binding can stay broken indefinitely, so the conversion sentence has to
// reach the API on the failure paths too. Sweeping after the status write those
// paths already do left it in a copy the pass discarded, every pass.
func TestReconcile_ConversionGateReachesTheAPIOnAFailedPass(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	// Declared, and unresolvable: every pass exits through the binding path.
	app.Spec.ServiceBindings = []kipperv1.ServiceBinding{{Name: "missing", Database: "x"}}

	controller := true
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretname.Env(secretname.KindApp, app.Name), Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
				Name: app.Name, UID: app.UID, Controller: &controller,
			}},
		},
		Data: map[string][]byte{"LOG_LEVEL": []byte("info")},
	}
	revision := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app-old", Namespace: app.Namespace},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app.Name}},
				Spec:       envPodSpec(legacy.Name),
			},
		},
	}

	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, legacy, revision).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.Error(t, err, "the binding cannot be resolved, so the pass fails")

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionEnvPublished)
	require.NotNil(t, cond, "the pass persists status on this path, so the condition must be on it")
	assert.Contains(t, cond.Message, "from before generations",
		"what the sweep learned must be written with the refusal, not after it")
}

// The third refusal path: the bindings render, and the injection gate then
// refuses one. That returns a refusal list rather than an error, so it is a
// different branch from the one a missing Service takes — and it lost its sweep
// entirely when the other two were moved above their status write.
func TestReconcile_ConversionGateReachesTheAPIWhenABindingIsRefused(t *testing.T) {
	ctx := context.Background()
	scheme := testScheme()

	app := newTestApp()
	app.UID = types.UID("uid-my-app")
	app.Spec.Env = map[string]string{"LOG_LEVEL": "info"}
	app.Spec.ServiceBindings = []kipperv1.ServiceBinding{{Name: "db", Prefix: "DB_", Database: "x"}}

	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db", Namespace: app.Namespace, UID: types.UID("uid-db"),
		},
		Spec: kipperv1.ServiceSpec{Type: "mailhog"},
	}
	controller := true
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: "db-credentials", Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "db", UID: types.UID("uid-db"), Controller: &controller,
			}},
		},
		Data: map[string][]byte{"HOST": []byte("db"), "PORT": []byte("1025")},
	}

	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: secretname.Env(secretname.KindApp, app.Name), Namespace: app.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
				Name: app.Name, UID: app.UID, Controller: &controller,
			}},
		},
		Data: map[string][]byte{"LOG_LEVEL": []byte("info")},
	}
	revision := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app-old", Namespace: app.Namespace},
		Spec: appsv1.ReplicaSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": app.Name}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": app.Name}},
				Spec:       envPodSpec(legacy.Name),
			},
		},
	}

	// The service's type changes between the render and the injection gate. The
	// render saw a type that derives nothing and kept the shared credentials;
	// the gate computes the derived name for a type that does, finds nothing
	// rendered under it and nothing to attest, and refuses.
	reads := 0
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(app, svc, creds, legacy, revision).WithStatusSubresource(app).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, cl crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if err := cl.Get(ctx, key, obj, opts...); err != nil {
					return err
				}
				if s, isSvc := obj.(*kipperv1.Service); isSvc && s.Name == "db" {
					reads++
					if reads > 1 {
						s.Spec.Type = "postgres"
					}
				}
				return nil
			},
		}).Build()
	r := &AppReconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace,
	}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "refused", "this must be the refusal branch, not an error branch")

	var got kipperv1.App
	require.NoError(t, c.Get(ctx, types.NamespacedName{
		Name: app.Name, Namespace: app.Namespace}, &got))
	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionEnvPublished)
	require.NotNil(t, cond, "the refusal path persists status, so the condition must be on it")
	assert.Contains(t, cond.Message, "from before generations",
		"the refusal branch must sweep before its write like the other two")
}
