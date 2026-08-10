package handlers

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// dataUpdatedAtAnnotation is stamped on the app-<app>-env and app-<app>-secrets
// Secrets whenever their data changes. See kipperv1.DataUpdatedAtAnnotation.
const dataUpdatedAtAnnotation = kipperv1.DataUpdatedAtAnnotation

// Env provides handlers for managing application environment variables.
type Env struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
}

// Get returns the environment variables for an app.
// GET /api/v1/projects/{name}/apps/{app}/env
func (e *Env) Get(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := e.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, map[string]string{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get env vars")
		return
	}

	env := appCR.Spec.Env
	if env == nil {
		env = map[string]string{}
	}

	respondJSON(w, http.StatusOK, env)
}

// Preview resolves the app's environment templates and reports what each value
// becomes, with every secret-derived substitution masked.
//
// Deployer-gated at the route. Env GET is viewer-readable because it returns
// the templates as written, which hold no credential; this returns what they
// resolve to, so it must not reach a role that cannot write them (D13).
// GET /api/v1/projects/{name}/apps/{app}/env/preview
func (e *Env) Preview(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := e.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "app not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to read app")
		return
	}

	preview, err := controllers.BuildEnvPreview(ctx, e.CRClient, &appCR)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to resolve environment")
		return
	}

	respondJSON(w, http.StatusOK, preview)
}

// DirectEnvConflicts returns any direct env: entries on the deployment that
// would override envFrom values. Kubernetes gives direct env: precedence over
// envFrom, so these entries silently prevent updates via the env secret.
// GET /api/v1/projects/{name}/apps/{app}/env/conflicts
func (e *Env) DirectEnvConflicts(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	deploy, err := e.Client.AppsV1().Deployments(project).Get(ctx, app, metav1.GetOptions{})
	if err != nil {
		respondJSON(w, http.StatusOK, []string{})
		return
	}

	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		respondJSON(w, http.StatusOK, []string{})
		return
	}

	// The addresses of this app's links are direct env: entries too, and they are
	// meant to be. They are derived from spec.links every pass and deliberately
	// take precedence, so reporting them as conflicts would mark every linked app
	// as broken and offer to remove the thing making its links work.
	derived := e.linkDerivedKeys(ctx, project, app)

	conflicts := []string{}
	for _, ev := range deploy.Spec.Template.Spec.Containers[0].Env {
		if derived[ev.Name] {
			continue
		}
		conflicts = append(conflicts, ev.Name)
	}

	respondJSON(w, http.StatusOK, conflicts)
}

// linkOwnedKeyError names a variable an env update tried to set that one of the
// app's links already owns. Carried out of the retry rather than answered
// inside it, so the response is written once the loop has finished.
type linkOwnedKeyError struct{ key, target string }

// linkDerivedKeys is the set of env var names this app's links inject. The
// reconciler owns them; nothing else may report or remove them.
func (e *Env) linkDerivedKeys(ctx context.Context, namespace, app string) map[string]bool {
	var appCR kipperv1.App
	if err := e.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: app}, &appCR); err != nil {
		return nil
	}
	keys := make(map[string]bool, len(appCR.Spec.Links))
	for _, link := range appCR.Spec.Links {
		keys[controllers.AppEnvKey(link.App)] = true
	}
	return keys
}

// RemoveDirectEnvConflicts removes all direct env: entries from the deployment
// so that envFrom takes effect.
// DELETE /api/v1/projects/{name}/apps/{app}/env/conflicts
func (e *Env) RemoveDirectEnvConflicts(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	deploy, err := e.Client.AppsV1().Deployments(project).Get(ctx, app, metav1.GetOptions{})
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("deployment %q not found", app))
		return
	}

	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		respondJSON(w, http.StatusOK, map[string]string{"status": "no containers"})
		return
	}

	// Everything except the addresses this app's links inject. Those are the
	// reconciler's, it would put them straight back, and removing them rolls the
	// pods for nothing.
	derived := e.linkDerivedKeys(ctx, project, app)
	kept := make([]corev1.EnvVar, 0, len(deploy.Spec.Template.Spec.Containers[0].Env))
	for _, ev := range deploy.Spec.Template.Spec.Containers[0].Env {
		if derived[ev.Name] {
			kept = append(kept, ev)
		}
	}
	removed := len(deploy.Spec.Template.Spec.Containers[0].Env) - len(kept)
	deploy.Spec.Template.Spec.Containers[0].Env = kept

	if _, err := e.Client.AppsV1().Deployments(project).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update deployment")
		return
	}

	respondJSON(w, http.StatusOK, map[string]int{"removed": removed})
}

// Update replaces all environment variables for an app. The reconciler mirrors
// spec.env into the app-<app>-env Secret; the running pods keep the old values
// until they restart, which the console surfaces via a "restart to apply"
// banner rather than restarting automatically.
// PUT /api/v1/projects/{name}/apps/{app}/env
func (e *Env) Update(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	var env map[string]string
	if err := decodeJSON(r, &env); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Retry on conflict: the reconciler and build controller also write the App,
	// so a benign concurrent update should not surface as a 500. The reconciler
	// stamps the app-<app>-env Secret when the rendered data changes, which is what
	// drives the restart banner — so every writer of spec.env is covered, not
	// just this handler.
	var owned *linkOwnedKeyError
	var written map[string]string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var appCR kipperv1.App
		if err := e.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
			return err
		}
		// Checked against the object about to be written, on every attempt. A
		// link created between the first read and a retry would otherwise slip
		// past a check made once, and the app would end up holding both a stored
		// value and a link — the state this refusal exists to prevent, reachable
		// only by losing a race.
		//
		// A variable a link owns cannot be set here at all: the reconciler emits
		// it as an explicit container env entry, which wins over the Secret this
		// map renders into, so accepting it would store a value the app never
		// sees while showing it back as though it were in force.
		writing := make(map[string]string, len(env))
		for k, v := range env {
			writing[k] = v
		}
		for _, link := range appCR.Spec.Links {
			key := controllers.AppEnvKey(link.App)
			value, set := writing[key]
			if !set {
				continue
			}
			// A stored value the app already had, resent unchanged, is not
			// somebody setting this variable — it is the editor posting the map
			// back, and the value has had no effect since the link started
			// providing one. Drop it rather than refusing an edit that was about
			// something else entirely. An app linked before addresses were
			// derived carries exactly this, and nothing migrates it, so the
			// alternative is an operator who cannot change any variable at all
			// until they work out which one the error means.
			if stored, had := appCR.Spec.Env[key]; had && stored == value {
				delete(writing, key)
				continue
			}
			owned = &linkOwnedKeyError{key: key, target: link.App}
			return nil
		}
		appCR.Spec.Env = writing
		written = writing
		return e.CRClient.Update(ctx, &appCR)
	})
	if owned != nil {
		respondError(w, http.StatusConflict, fmt.Sprintf(
			"%s is the address of this app's link to %q, so the link sets it. "+
				"Remove it here to use the link's address, or unlink %s if this app should set it itself.",
			owned.key, owned.target, owned.target))
		return
	}
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q has no env configured", app))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to update env vars")
		return
	}

	// What was stored, not what was sent. A superseded address is dropped on the
	// way in, and echoing the request back would leave the caller holding a key
	// the app no longer has — which it would send again on the next edit, and be
	// refused for, having done nothing wrong.
	respondJSON(w, http.StatusOK, written)
}

// RestartStatus reports whether the app's running pods predate its last env or
// secret change, i.e. a restart is needed for the current values to take
// effect. The console shows a "restart to apply" banner while this is true.
// GET /api/v1/projects/{name}/apps/{app}/env/status
func (e *Env) RestartStatus(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	pending, err := e.envRestartPending(ctx, project, app)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to check restart status")
		return
	}
	respondJSON(w, http.StatusOK, map[string]bool{"restartPending": pending})
}

// envRestartPending is true when the app's pods are not on the environment the
// controller last published.
//
// A workload's whole environment is one immutable object whose name is a
// fingerprint of its contents, so this is two names compared: the one the last
// pass published, and the one the running pod template asks for. Anything that
// changes what the app should read — spec.env, its own secrets, a binding's
// credentials — changes the first without changing the second, because they are
// all inside the published object.
//
// It used to compare a data-updated-at stamp across several Secrets against each
// pod's start time. That was the only question available when a pod's
// environment came from several mutable objects, and it had to reason about when
// a stamp was written relative to when a kubelet started a container.
func (e *Env) envRestartPending(ctx context.Context, namespace, app string) (bool, error) {
	var workload kipperv1.App
	if err := e.CRClient.Get(ctx, crclient.ObjectKey{Name: app, Namespace: namespace}, &workload); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	// Nothing published yet: the window between creating a workload and its
	// first pass, where there is no environment to be behind.
	published := workload.Status.PublishedEnv
	if published == "" {
		return false, nil
	}

	deploy, err := e.Client.AppsV1().Deployments(namespace).Get(ctx, app, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return false, nil
	}
	prefix := secretname.EnvGenerationPrefix(secretname.KindApp, app)
	for _, ef := range containers[0].EnvFrom {
		if ef.SecretRef != nil && strings.HasPrefix(ef.SecretRef.Name, prefix) {
			return ef.SecretRef.Name != published, nil
		}
	}
	// The pod template names no generation at all, which is a workload that has
	// not been through a pass since generations shipped. Restarting it would
	// move it onto one.
	return true, nil
}
