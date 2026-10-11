package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func basicAuthUsersName(app string) string { return app + "-basic-auth" }

func basicAuthLockName(app string) string { return app + "-basic-auth-locked" }

// basicAuthSecret selects the users Secret or creates a lock Secret with a
// discarded random password to keep the route protected until users are added.
func (r *AppReconciler) basicAuthSecret(ctx context.Context, app *kipperv1.App) (string, error) {
	var users corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: basicAuthUsersName(app.Name), Namespace: app.Namespace}, &users)
	switch {
	case err == nil && usableBasicAuthUsers(&users):
		apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
			Type: kipperv1.ConditionBasicAuthReady, Status: metav1.ConditionTrue, Reason: "UsersConfigured",
			Message: "basic auth users are configured", ObservedGeneration: app.Generation,
		})
		return users.Name, r.deleteBasicAuthLock(ctx, app)
	case err != nil && !errors.IsNotFound(err):
		return "", fmt.Errorf("reading the basic-auth users: %w", err)
	}

	if err := r.ensureBasicAuthLock(ctx, app); err != nil {
		return "", err
	}
	apimeta.SetStatusCondition(&app.Status.Conditions, metav1.Condition{
		Type: kipperv1.ConditionBasicAuthReady, Status: metav1.ConditionFalse, Reason: "NoUsers",
		Message:            "basic auth is enabled; add a user to allow access",
		ObservedGeneration: app.Generation,
	})
	return basicAuthLockName(app.Name), nil
}

// usableBasicAuthUsers checks credential structure, not password hash validity.
func usableBasicAuthUsers(s *corev1.Secret) bool {
	if s.Type == corev1.SecretTypeBasicAuth {
		return len(s.Data[corev1.BasicAuthUsernameKey]) > 0 && len(s.Data[corev1.BasicAuthPasswordKey]) > 0
	}
	if len(s.Data) != 1 {
		return false
	}
	for _, v := range s.Data {
		return hasHtpasswdLine(v)
	}
	return false
}

func hasHtpasswdLine(v []byte) bool {
	for _, line := range strings.Split(string(v), "\n") {
		user, hash, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && user != "" && hash != "" {
			return true
		}
	}
	return false
}

// ensureBasicAuthLock creates or repairs the app's lock Secret.
// Only app-owned Secrets are trusted to hold lock credentials.
func (r *AppReconciler) ensureBasicAuthLock(ctx context.Context, app *kipperv1.App) error {
	var existing corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: basicAuthLockName(app.Name), Namespace: app.Namespace}, &existing)
	found := err == nil
	switch {
	case found && !metav1.IsControlledBy(&existing, app):
		return fmt.Errorf("cannot lock basic auth: secret %s/%s is not controlled by this app; rename or delete it", app.Namespace, existing.Name)
	case found && usableBasicAuthUsers(&existing):
		return nil
	case !found && !errors.IsNotFound(err):
		return err
	}
	users, err := lockEntry()
	if err != nil {
		return err
	}
	if found {
		existing.Type = corev1.SecretTypeOpaque
		existing.Data = map[string][]byte{"users": users}
		return r.Update(ctx, &existing)
	}
	lock := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: basicAuthLockName(app.Name), Namespace: app.Namespace,
			Labels: map[string]string{"app": app.Name, kipperLabel: kipperValue},
		},
		Data: map[string][]byte{"users": users},
	}
	if err := controllerutil.SetControllerReference(app, lock, r.Scheme); err != nil {
		return err
	}
	// Losing a create race is retried by the next pass, which checks the
	// winner's owner like any other existing Secret.
	return r.Create(ctx, lock)
}

// lockEntry hashes a random password and discards the plaintext to prevent login.
func lockEntry() ([]byte, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(hex.EncodeToString(secret)), bcrypt.DefaultCost)
	if err != nil {
		return nil, err
	}
	return []byte("locked:" + string(hash) + "\n"), nil
}

func (r *AppReconciler) deleteBasicAuthLock(ctx context.Context, app *kipperv1.App) error {
	var lock corev1.Secret
	err := r.Get(ctx, types.NamespacedName{Name: basicAuthLockName(app.Name), Namespace: app.Namespace}, &lock)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(&lock, app) {
		return nil
	}
	uid := lock.UID
	if err := r.Delete(ctx, &lock, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
		return err
	}
	return nil
}

// enqueueForBasicAuthUsers maps Secret changes to apps by the middleware's
// naming convention. The API creates users Secrets without owner references,
// so they need an explicit watch.
func (r *AppReconciler) enqueueForBasicAuthUsers(_ context.Context, obj client.Object) []ctrl.Request {
	app, ok := strings.CutSuffix(obj.GetName(), "-basic-auth")
	if !ok || app == "" {
		return nil
	}
	return []ctrl.Request{{NamespacedName: types.NamespacedName{Name: app, Namespace: obj.GetNamespace()}}}
}
