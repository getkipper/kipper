package handlers

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
	"github.com/getkipper/kipper/console-api/internal/giturl"
	"github.com/getkipper/kipper/controller/pkg/netguard"
)

// Apps provides handlers for application management within a project.
type Apps struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
	Domain   string
}

type appResponse struct {
	Name     string `json:"name"`
	Status   string `json:"status"`
	Image    string `json:"image"`
	Replicas int32  `json:"replicas"`
	Ready    int32  `json:"ready"`
}

type createAppRequest struct {
	Name            string            `json:"name"`
	Image           string            `json:"image"`
	Port            int32             `json:"port"`
	Replicas        int32             `json:"replicas"`
	Env             map[string]string `json:"env"`
	ResourceProfile string            `json:"resource_profile"`
	CPURequest      string            `json:"cpu_request,omitempty"`
	CPULimit        string            `json:"cpu_limit,omitempty"`
	MemoryRequest   string            `json:"memory_request,omitempty"`
	MemoryLimit     string            `json:"memory_limit,omitempty"`
	Git             *createGitSource  `json:"git,omitempty"`
	Route           *createRoute      `json:"route,omitempty"`
}

type createGitSource struct {
	URL            string `json:"url"`
	Branch         string `json:"branch,omitempty"`
	Token          string `json:"token,omitempty"`
	DockerfilePath string `json:"dockerfile_path,omitempty"`
	Context        string `json:"context,omitempty"`
}

type createRoute struct {
	Host         string   `json:"host,omitempty"`
	Path         string   `json:"path,omitempty"`
	RedirectFrom []string `json:"redirect_from,omitempty"`
}

// List returns all apps in a project.
// GET /api/v1/projects/{name}/apps
func (a *Apps) List(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appList kipperv1.AppList
	if err := a.CRClient.List(ctx, &appList, crclient.InNamespace(project)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list apps")
		return
	}

	apps := make([]appResponse, 0, len(appList.Items))
	for _, app := range appList.Items {
		apps = append(apps, appCRToResponse(app))
	}

	respondJSON(w, http.StatusOK, apps)
}

// Create deploys a new application in the project.
// POST /api/v1/projects/{name}/apps
func (a *Apps) Create(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	var req createAppRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		respondError(w, http.StatusBadRequest, "name is required")
		return
	}

	if req.Image == "" && req.Git == nil {
		respondError(w, http.StatusBadRequest, "either image or git source is required")
		return
	}

	if req.Image != "" && req.Git != nil {
		respondError(w, http.StatusBadRequest, "image and git source are mutually exclusive")
		return
	}

	// Auto-detect port from Dockerfile for git-based apps
	if req.Port == 0 && req.Git != nil && req.Git.URL != "" {
		req.Port = detectPortFromDockerfile(req.Git.URL, req.Git.Branch, req.Git.Token)
	}
	if req.Port == 0 {
		respondError(w, http.StatusBadRequest, "port is required (could not auto-detect from Dockerfile)")
		return
	}

	// Git-based apps use a placeholder image until the first build completes
	if req.Git != nil && req.Image == "" {
		req.Image = "busybox:latest"
	}

	if req.Replicas == 0 {
		req.Replicas = 1
	}

	// Every rejection the request can earn on its own is decided here, before
	// the name is reserved and before the git credential is written. A refusal
	// after either leaves state behind from a create that did not happen.
	if req.Git != nil {
		if req.Git.URL == "" {
			respondError(w, http.StatusBadRequest, "git url is required")
			return
		}
		// Reject a URL the build could not host-bind a credential to (http,
		// userinfo, fragment). A clean early error; the builder stays authoritative.
		if _, err := giturl.CanonicalAuthority(req.Git.URL); err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid git url: %v", err))
			return
		}
	}
	if req.Route != nil && len(req.Route.RedirectFrom) > kipperv1.MaxRedirectFromHosts {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("at most %d redirect domains are supported per route", kipperv1.MaxRedirectFromHosts))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Ahead of the git-credentials Secret below, because that write outlives a
	// refusal: a rejected create that had already stored a token would answer
	// "not created" while leaving the credential in the namespace.
	release, ok := reserveWorkloadName(ctx, w, a.CRClient, project, req.Name, "app")
	if !ok {
		return
	}

	// Mirror a single value to both request and limit so a single-field
	// payload still produces Guaranteed QoS.
	cpuReq, cpuLim := pairOrPassThrough(req.CPURequest, req.CPULimit)
	memReq, memLim := pairOrPassThrough(req.MemoryRequest, req.MemoryLimit)

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: project,
			Labels: map[string]string{
				"app":       req.Name,
				kipperLabel: kipperValue,
			},
		},
		Spec: kipperv1.AppSpec{
			Image:    req.Image,
			Port:     req.Port,
			Replicas: &req.Replicas,
			Resources: kipperv1.AppResources{
				Profile:       req.ResourceProfile,
				CPURequest:    cpuReq,
				CPULimit:      cpuLim,
				MemoryRequest: memReq,
				MemoryLimit:   memLim,
			},
			Env: req.Env,
		},
	}

	// Git source
	if req.Git != nil {
		app.Spec.Git = &kipperv1.AppGitSource{
			URL:            req.Git.URL,
			Branch:         req.Git.Branch,
			DockerfilePath: req.Git.DockerfilePath,
			Context:        req.Git.Context,
		}

		// Store git token as a Kubernetes Secret if provided
		if req.Git.Token != "" {
			secretName := req.Name + "-git-credentials"
			if err := a.createGitCredentialsSecret(ctx, project, secretName, req.Name, req.Git.Token); err != nil {
				release()
				respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to store git credentials: %v", err))
				return
			}
			app.Spec.Git.CredentialsSecret = secretName
		}
	}

	// Route
	if req.Route != nil {
		app.Spec.Route = &kipperv1.AppRoute{
			Host:         req.Route.Host,
			Path:         req.Route.Path,
			RedirectFrom: req.Route.RedirectFrom,
		}
	}

	if err := a.CRClient.Create(ctx, app); err != nil {
		// AlreadyExists is not a create that wrote nothing: it proves the
		// same-kind workload is there, and the reservation just made is that
		// workload's own first claim. Releasing it would undo the backfill and
		// leave an upgraded cluster's workload unreserved, since nothing else
		// enqueues it.
		if !errors.IsAlreadyExists(err) {
			release()
		}
		if errors.IsAlreadyExists(err) {
			respondError(w, http.StatusConflict, fmt.Sprintf("app %q already exists", req.Name))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to create app")
		return
	}

	// Trigger first build for git-based apps. The goroutine outlives the
	// request, so it must not inherit the request context (closing the
	// HTTP connection would otherwise cancel an in-flight first build).
	if req.Git != nil {
		go a.triggerFirstBuild(app) //nolint:gosec // intentional: build must outlive the request context
	}

	respondJSON(w, http.StatusCreated, appResponse{
		Name:     req.Name,
		Status:   "pending",
		Image:    req.Image,
		Replicas: req.Replicas,
		Ready:    0,
	})
}

func (a *Apps) createGitCredentialsSecret(ctx context.Context, namespace, secretName, appName, token string) error {
	secrets := a.Client.CoreV1().Secrets(namespace)
	wantLabels := map[string]string{
		"app.kubernetes.io/managed-by": "kipper",
		"kipper.run/app":               appName,
	}

	// Update path: read the existing Secret, mutate its data + labels,
	// write back. Calling Update with a fresh object that has no
	// resourceVersion can be rejected by the apiserver and silently
	// loses any user-attached metadata we don't echo back. Token
	// rotation goes through here on every retry, so the round-trip is
	// the safer pattern.
	existing, getErr := secrets.Get(ctx, secretName, metav1.GetOptions{})
	if getErr == nil {
		if existing.Labels == nil {
			existing.Labels = map[string]string{}
		}
		for k, v := range wantLabels {
			existing.Labels[k] = v
		}
		if existing.Data == nil {
			existing.Data = map[string][]byte{}
		}
		existing.Data["token"] = []byte(token)
		_, err := secrets.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	}
	if !errors.IsNotFound(getErr) {
		return getErr
	}

	// Create path: fresh Secret with the labels and token data.
	fresh := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: namespace,
			Labels:    wantLabels,
		},
		Data: map[string][]byte{
			"token": []byte(token),
		},
	}
	_, err := secrets.Create(ctx, fresh, metav1.CreateOptions{})
	return err
}

func (a *Apps) triggerFirstBuild(app *kipperv1.App) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	commit := fmt.Sprintf("initial-%d", time.Now().Unix())
	_, err := builder.CreateBuildJob(ctx, a.Client, app, commit)
	if err != nil {
		fmt.Printf("failed to trigger first build for %s/%s: %v\n", app.Namespace, app.Name, err)
	}
}

// Scale sets the replica count for an application.
// PUT /api/v1/projects/{name}/apps/{app}/scale
func (a *Apps) Scale(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	var req struct {
		Replicas int32 `json:"replicas"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Replicas < 0 {
		respondError(w, http.StatusBadRequest, "replicas must be a non-negative integer")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	app := &kipperv1.App{}
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, app); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	// With autoscaling enabled, the HPA owns the replica count and the
	// reconciler never applies spec.replicas to the Deployment. Accepting
	// the write would report a scale that never happens.
	if app.Spec.Autoscale != nil && app.Spec.Autoscale.Enabled {
		respondError(w, http.StatusConflict, fmt.Sprintf("autoscaling keeps %s running regardless of the replica count; disable autoscaling first, then scale", appName))
		return
	}

	app.Spec.Replicas = &req.Replicas
	if err := a.CRClient.Update(ctx, app); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to scale app")
		return
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"name":     appName,
		"replicas": req.Replicas,
	})
}

// Delete removes an application from the project.
// DELETE /api/v1/projects/{name}/apps/{app}
func (a *Apps) Delete(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{
			Name:      appName,
			Namespace: project,
		},
	}

	if err := a.CRClient.Delete(ctx, app); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to delete app")
		return
	}

	// Nothing is rewritten on other apps. An internal link stores no address, so
	// there is none to clean: the link stops resolving the moment this app is
	// gone and its callers report it as a LinksOpen condition until somebody
	// unlinks. A public link's URL is a plain environment variable the operator
	// asked for, and a variable spelled like this app's name is not proof the
	// platform wrote it — deleting other apps' env on a guess is how you lose
	// somebody's proxy address.

	w.WriteHeader(http.StatusNoContent)
}

// Restart triggers a rolling restart of an application.
// POST /api/v1/projects/{name}/apps/{app}/restart
func (a *Apps) Restart(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	app := &kipperv1.App{}
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, app); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if app.Annotations == nil {
		app.Annotations = make(map[string]string)
	}
	app.Annotations["kipper.run/restartedAt"] = time.Now().Format(time.RFC3339)

	if err := a.CRClient.Update(ctx, app); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to restart app")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "restarting"})
}

// UpdateImage changes the container image for an app and triggers a rollout.
// PUT /api/v1/projects/{name}/apps/{app}/image
func (a *Apps) UpdateImage(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	var req struct {
		Image string `json:"image"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Image == "" {
		respondError(w, http.StatusBadRequest, "image is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	app := &kipperv1.App{}
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, app); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	app.Spec.Image = req.Image

	if err := a.CRClient.Update(ctx, app); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update image")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated", "image": req.Image})
}

func appCRToResponse(app kipperv1.App) appResponse {
	// Use the actual replica count from the Deployment (via status) rather
	// than spec.replicas, which is stale when the HPA controls scaling.
	replicas := app.Status.Replicas
	if replicas == 0 && app.Spec.Replicas != nil {
		replicas = *app.Spec.Replicas
	}

	status := strings.ToLower(app.Status.Phase)
	if status == "" {
		status = "pending"
	}

	return appResponse{
		Name:     app.Name,
		Status:   status,
		Image:    app.Spec.Image,
		Replicas: replicas,
		Ready:    app.Status.ReadyReplicas,
	}
}

var exposePattern = regexp.MustCompile(`(?i)^EXPOSE\s+(\d+)`)

// detectPortFromDockerfile fetches the Dockerfile from a GitHub or GitLab
// repo and extracts the first EXPOSE port. Returns 0 if the Dockerfile
// can't be read or has no EXPOSE directive.
func detectPortFromDockerfile(gitURL, branch, token string) int32 {
	if branch == "" {
		branch = "main"
	}

	rawURL := dockerfileRawURL(gitURL, branch)
	if rawURL == "" {
		return 0
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0
	}
	if token != "" {
		req.Header.Set("Authorization", "token "+token)
	}

	// The host comes from the app's configured git URL, so fetch it through the
	// SSRF guard: no redirects (a 3xx must not carry the git token to another
	// host) and no connection to a non-public address (no internal SSRF).
	resp, err := netguard.Client(10 * time.Second).Do(req)
	if err != nil || resp.StatusCode != 200 {
		return 0
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return 0
	}

	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if m := exposePattern.FindStringSubmatch(line); m != nil {
			port, err := strconv.Atoi(m[1])
			if err == nil && port > 0 && port < 65536 {
				return int32(port) //nolint:gosec
			}
		}
	}
	return 0
}

// dockerfileRawURL returns the raw-Dockerfile URL for a git repo, but only for
// the recognised cloud hosts — GitHub and GitLab. The autodetect fetch carries
// the app's git token, so it must go to a trusted authority: matching a host
// merely because it contains "gitlab" would send the token to
// gitlab.attacker.example. A self-hosted host returns "" (no autodetect; the
// user sets the port explicitly), same as GitHub Enterprise already does.
func dockerfileRawURL(gitURL, branch string) string {
	parsed, err := url.Parse(gitURL)
	if err != nil || parsed.Scheme != "https" {
		return ""
	}

	path := strings.TrimSuffix(parsed.Path, ".git")
	path = strings.Trim(path, "/")
	host := parsed.Hostname()

	switch {
	case host == "github.com" || strings.HasSuffix(host, ".github.com"):
		return fmt.Sprintf("https://raw.githubusercontent.com/%s/%s/Dockerfile", path, branch)
	case host == "gitlab.com" || strings.HasSuffix(host, ".gitlab.com"):
		return fmt.Sprintf("https://gitlab.com/%s/-/raw/%s/Dockerfile", path, branch)
	default:
		return ""
	}
}
