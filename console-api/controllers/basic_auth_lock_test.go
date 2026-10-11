package controllers

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func reconcileBasicAuthApp(t *testing.T, objs ...*corev1.Secret) (*AppReconciler, *kipperv1.App) {
	t.Helper()
	scheme := testScheme()
	app := newTestApp()
	app.Spec.Route = &kipperv1.AppRoute{BasicAuth: true}
	b := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app).WithStatusSubresource(app)
	for _, s := range objs {
		b = b.WithObjects(s)
	}
	r := &AppReconciler{Client: b.Build(), Scheme: scheme, Domain: "test.kipper.run"}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}})
	require.NoError(t, err)
	var got kipperv1.App
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, &got))
	return r, &got
}

func basicAuthSecretOf(t *testing.T, r *AppReconciler) string {
	t.Helper()
	mw := getMiddleware(t, r.Client, "my-app-basic-auth")
	name, _, _ := unstructured.NestedString(mw.Object, "spec", "basicAuth", "secret")
	return name
}

func TestBasicAuthWithoutUsersLocksTheApp(t *testing.T) {
	r, app := reconcileBasicAuthApp(t)

	lock := basicAuthSecretOf(t, r)
	assert.NotEqual(t, "my-app-basic-auth", lock, "a missing users Secret must not be referenced")
	var secret corev1.Secret
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: lock, Namespace: "project-test"}, &secret))
	users := strings.TrimSpace(string(secret.Data["users"]))
	require.NotEmpty(t, users, "Traefik refuses an empty credential Secret")
	assert.True(t, strings.Contains(users, ":$2"), "a bcrypt entry Traefik can load: %q", users)
	assert.True(t, metav1.IsControlledBy(&secret, app), "the lock is the app's and goes with it")

	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionBasicAuthReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "NoUsers", cond.Reason)
}

func TestBasicAuthWithUsersUsesThem(t *testing.T) {
	users := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app-basic-auth", Namespace: "project-test"},
		Data:       map[string][]byte{"users": []byte("alice:$2a$10$abcdefghijklmnopqrstuuOtXm3P1Q0vG2s0eJ8YpF1hJxQy2wq6\n")},
	}
	r, app := reconcileBasicAuthApp(t, users)

	assert.Equal(t, "my-app-basic-auth", basicAuthSecretOf(t, r))
	cond := apimeta.FindStatusCondition(app.Status.Conditions, kipperv1.ConditionBasicAuthReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestAnEmptyUsersSecretAlsoLocksTheApp(t *testing.T) {
	empty := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app-basic-auth", Namespace: "project-test"},
		Data:       map[string][]byte{"users": []byte("\n")},
	}
	r, _ := reconcileBasicAuthApp(t, empty)

	assert.NotEqual(t, "my-app-basic-auth", basicAuthSecretOf(t, r))
}

func TestDeletingTheUsersSecretReachesTheApp(t *testing.T) {
	r := &AppReconciler{}
	users := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "my-app-basic-auth", Namespace: "project-test", Labels: map[string]string{"app": "my-app"},
	}}

	reqs := r.enqueueForBasicAuthUsers(context.Background(), users)

	assert.Equal(t, []ctrl.Request{{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}}}, reqs)
	assert.Empty(t, r.enqueueForBasicAuthUsers(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "project-test"}}))
}

func TestASecretAtTheLockNameThatIsNotTheAppsIsLeftAlone(t *testing.T) {
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "my-app-basic-auth-locked", Namespace: "project-test"},
		Data:       map[string][]byte{"users": []byte("guest:$2a$10$known")},
	}
	scheme := testScheme()
	app := newTestApp()
	c := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(app, foreign).WithStatusSubresource(app).Build()
	r := &AppReconciler{Client: c, Scheme: scheme, Domain: "test.kipper.run"}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}})
	require.NoError(t, err)

	var still corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "my-app-basic-auth-locked", Namespace: "project-test"}, &still),
		"basic auth off must not delete a Secret the app does not own")

	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "my-app", Namespace: "project-test"}, app))
	app.Spec.Route = &kipperv1.AppRoute{BasicAuth: true}
	require.NoError(t, c.Update(context.Background(), app))
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}})
	require.Error(t, err, "a lock whose name is taken by another Secret cannot be trusted")
}

func TestAnOwnedLockThatWasEmptiedIsRepaired(t *testing.T) {
	r, app := reconcileBasicAuthApp(t)
	var lock corev1.Secret
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-app-basic-auth-locked", Namespace: "project-test"}, &lock))
	lock.Data = map[string][]byte{"users": []byte("")}
	require.NoError(t, r.Update(context.Background(), &lock))

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: app.Name, Namespace: app.Namespace}})
	require.NoError(t, err)

	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Name: "my-app-basic-auth-locked", Namespace: "project-test"}, &lock))
	assert.Contains(t, string(lock.Data["users"]), ":$2", "Traefik refuses an empty credential Secret")
}

func TestUsersAreReadTheWayTraefikReadsThem(t *testing.T) {
	typed := &corev1.Secret{Type: corev1.SecretTypeBasicAuth, Data: map[string][]byte{"username": []byte("alice"), "password": []byte("pw")}}
	assert.True(t, usableBasicAuthUsers(typed))

	twoEntries := &corev1.Secret{Data: map[string][]byte{"users": []byte("alice:$2a$10$x"), "extra": []byte("y")}}
	assert.False(t, usableBasicAuthUsers(twoEntries), "Traefik rejects an Opaque Secret with more than one entry")

	one := &corev1.Secret{Data: map[string][]byte{"users": []byte("alice:$2a$10$x\n")}}
	assert.True(t, usableBasicAuthUsers(one))
}

func TestAUsersSecretWithoutLabelsStillReachesTheApp(t *testing.T) {
	r := &AppReconciler{}
	unlabelled := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "my-app-basic-auth", Namespace: "project-test"}}

	assert.Equal(t, []ctrl.Request{{NamespacedName: types.NamespacedName{Name: "my-app", Namespace: "project-test"}}},
		r.enqueueForBasicAuthUsers(context.Background(), unlabelled))
}
