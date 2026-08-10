package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// BasicAuth provides handlers for HTTP basic authentication management.
type BasicAuth struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
}

type basicAuthResponse struct {
	Enabled bool     `json:"enabled"`
	Users   []string `json:"users"`
}

type basicAuthRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// Get returns the basic auth status and list of usernames.
// GET /api/v1/projects/{name}/apps/{app}/basic-auth
func (ba *BasicAuth) Get(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app, _ := url.PathUnescape(chi.URLParam(r, "app"))

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := ba.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		respondJSON(w, http.StatusOK, basicAuthResponse{Users: []string{}})
		return
	}

	enabled := appCR.Spec.Route != nil && appCR.Spec.Route.BasicAuth

	users := parseHtpasswdUsernames(ctx, ba.Client, project, app)

	respondJSON(w, http.StatusOK, basicAuthResponse{Enabled: enabled, Users: users})
}

// Set adds or updates a basic auth user.
// PUT /api/v1/projects/{name}/apps/{app}/basic-auth
func (ba *BasicAuth) Set(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app, _ := url.PathUnescape(chi.URLParam(r, "app"))

	var req basicAuthRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Username == "" || req.Password == "" {
		respondError(w, http.StatusBadRequest, "username and password are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to hash password")
		return
	}

	htpasswdLine := fmt.Sprintf("%s:%s", req.Username, string(hash))

	secretName := app + "-basic-auth"
	secret, err := ba.Client.CoreV1().Secrets(project).Get(ctx, secretName, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: project,
				Labels: map[string]string{
					"app":                          app,
					"app.kubernetes.io/managed-by": "kipper",
				},
			},
			Data: map[string][]byte{
				"users": []byte(htpasswdLine + "\n"),
			},
		}
		if _, err := ba.Client.CoreV1().Secrets(project).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to create basic auth secret")
			return
		}
	case err != nil:
		respondError(w, http.StatusInternalServerError, "failed to read basic auth secret")
		return
	default:
		existing := string(secret.Data["users"])
		updated := updateHtpasswd(existing, req.Username, htpasswdLine)
		secret.Data["users"] = []byte(updated)
		if _, err := ba.Client.CoreV1().Secrets(project).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update basic auth secret")
			return
		}
	}

	// Enable BasicAuth on the App CR
	var appCR kipperv1.App
	if err := ba.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err == nil {
		if appCR.Spec.Route == nil {
			appCR.Spec.Route = &kipperv1.AppRoute{}
		}
		if !appCR.Spec.Route.BasicAuth {
			appCR.Spec.Route.BasicAuth = true
			_ = ba.CRClient.Update(ctx, &appCR)
		}
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "saved", "username": req.Username})
}

// Delete removes basic auth entirely (Secret + flag).
// DELETE /api/v1/projects/{name}/apps/{app}/basic-auth
func (ba *BasicAuth) Delete(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app, _ := url.PathUnescape(chi.URLParam(r, "app"))

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	secretName := app + "-basic-auth"
	_ = ba.Client.CoreV1().Secrets(project).Delete(ctx, secretName, metav1.DeleteOptions{})

	var appCR kipperv1.App
	if err := ba.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err == nil {
		if appCR.Spec.Route != nil && appCR.Spec.Route.BasicAuth {
			appCR.Spec.Route.BasicAuth = false
			_ = ba.CRClient.Update(ctx, &appCR)
		}
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// DeleteUser removes a single user from basic auth.
// DELETE /api/v1/projects/{name}/apps/{app}/basic-auth/{username}
func (ba *BasicAuth) DeleteUser(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app, _ := url.PathUnescape(chi.URLParam(r, "app"))
	username, _ := url.PathUnescape(chi.URLParam(r, "username"))

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	secretName := app + "-basic-auth"
	secret, err := ba.Client.CoreV1().Secrets(project).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		respondError(w, http.StatusNotFound, "no basic auth configured")
		return
	}

	existing := string(secret.Data["users"])
	var lines []string
	for _, line := range strings.Split(existing, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && parts[0] == username {
			continue
		}
		lines = append(lines, line)
	}

	if len(lines) == 0 {
		// Last user removed, disable basic auth entirely
		_ = ba.Client.CoreV1().Secrets(project).Delete(ctx, secretName, metav1.DeleteOptions{})
		var appCR kipperv1.App
		if err := ba.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err == nil {
			if appCR.Spec.Route != nil {
				appCR.Spec.Route.BasicAuth = false
				_ = ba.CRClient.Update(ctx, &appCR)
			}
		}
	} else {
		secret.Data["users"] = []byte(strings.Join(lines, "\n") + "\n")
		_, _ = ba.Client.CoreV1().Secrets(project).Update(ctx, secret, metav1.UpdateOptions{})
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "removed", "username": username})
}

func parseHtpasswdUsernames(ctx context.Context, client kubernetes.Interface, namespace, app string) []string {
	secret, err := client.CoreV1().Secrets(namespace).Get(ctx, app+"-basic-auth", metav1.GetOptions{})
	if err != nil {
		return []string{}
	}

	var users []string
	for _, line := range strings.Split(string(secret.Data["users"]), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			users = append(users, parts[0])
		}
	}
	return users
}

func updateHtpasswd(existing, username, newLine string) string {
	var lines []string
	replaced := false
	for _, line := range strings.Split(existing, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && parts[0] == username {
			lines = append(lines, newLine)
			replaced = true
		} else {
			lines = append(lines, line)
		}
	}
	if !replaced {
		lines = append(lines, newLine)
	}
	return strings.Join(lines, "\n") + "\n"
}
