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
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
	"github.com/getkipper/kipper/console-api/internal/gitreach"
	"github.com/getkipper/kipper/controller/pkg/appowner"
	"github.com/getkipper/kipper/controller/pkg/gitcred"
	"github.com/getkipper/kipper/controller/pkg/giturl"
	"github.com/getkipper/kipper/controller/pkg/netguard"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// Apps provides handlers for application management within a project.
type Apps struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
	Domain   string
	// GitReach checks whether a git source can be cloned with the credential
	// it is being given, before either is stored. Nil uses the real one.
	GitReach GitReachFunc
}

// GitReachFunc reports whether a repository answers to a credential, and why
// not when it does not.
type GitReachFunc func(ctx context.Context, repoURL, branch, username, token string) (gitreach.Result, string)

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
		// The same clone preflight SetGit runs. Create is the path most apps
		// arrive by, so checking only the edit path would leave the original
		// failure — a private repository with no token, whose every build dies
		// at clone — reachable through the front door.
		branchToCheck := req.Git.Branch
		if branchToCheck == "" {
			branchToCheck = "main"
		}
		if err := builder.ValidateGitSource(req.Git.URL, branchToCheck); err != nil {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		result, detail := a.reachGit()(r.Context(), req.Git.URL, branchToCheck, gitCredentialUsername, req.Git.Token)
		if result == gitreach.NeedsCredential || result == gitreach.Unsafe {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("%s cannot be cloned: %s", sanitizeGitURL(req.Git.URL), detail))
			return
		}
		// Unknown does not block, for the reason set out in SetGit: the build's
		// credential helper is bound to the clone URL's host, so a token stored
		// against a host this cluster could not reach is not a token that can
		// go anywhere.
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

	// The reservation succeeds when an App of this name is already there, since
	// the claim it makes is that App's own backfill. So the conflict has to be
	// answered here, before the credential write: a duplicate create is an
	// ordinary stale form or a retry, and letting it reach that write would
	// replace the live App's token and then answer that nothing was created. A
	// create that loses a race after this still lands on AlreadyExists below.
	var live kipperv1.App
	switch err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: req.Name}, &live); {
	case err == nil:
		respondError(w, http.StatusConflict, fmt.Sprintf("app %q already exists", req.Name))
		return
	case !errors.IsNotFound(err):
		release()
		respondError(w, http.StatusInternalServerError, "failed to check for an existing app")
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

		// The App names its credential here; the Secret itself is written
		// after the create succeeds. Two requests creating the same name both
		// pass the check above, because only the create tells them apart, so
		// writing first meant the loser replaced the winner's token under the
		// one fixed name and then answered 409, having changed a workload it
		// did not create. Nothing builds before the create returns, so there
		// is no window where the reference is dangling and used.
		if req.Git.Token != "" {
			authority, _ := giturl.CanonicalAuthority(req.Git.URL)
			app.Spec.Git.CredentialsSecret = secretname.GitCredential(
				req.Name, secretname.GitCredentialDigest(req.Git.Token, authority))
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
		// AlreadyExists means the workload is there and owns the credential.
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

	if req.Git != nil && req.Git.Token != "" {
		if _, err := a.writeGitCredential(ctx, project, req.Name, req.Git.Token, req.Git.URL, app); err != nil {
			// The App exists and names a credential that is not there. Nothing
			// builds until the next push or rebuild, and that build reports the
			// credential as missing, so saying it here is what stops the
			// operator looking for the cause in the repository.
			//
			// The namespace this handler has cannot be decomposed back into the
			// project and environment kip composes it from, so the command names
			// the app and leaves the project for the operator rather than
			// guessing it.
			respondError(w, http.StatusInternalServerError, fmt.Sprintf(
				"%s was created but its git token could not be stored (%v). Add it again on the app's Source tab, or with 'kip app deploy --name %s --project <project> --port %d --git %s --git-token <token>'",
				req.Name, err, req.Name, app.Spec.Port, req.Git.URL))
			return
		}
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

// writeGitCredential stores a token for one clone host and returns the Secret
// that holds it.
//
// The name is a digest of the pair, so the object is immutable: a rotation
// writes a new one and the App moves by naming it, which is one atomic update.
// Nothing here ever writes to the Secret the App currently names, so two
// rotations cannot cross and a failed one has nothing to undo. Writing the same
// pair twice converges on the same object, so winning its Create says nothing
// about who owns it and no caller is told.
func (a *Apps) writeGitCredential(ctx context.Context, namespace, appName, token, cloneURL string, owner *kipperv1.App) (string, error) {
	// An unparseable URL records nothing rather than a wrong binding; the
	// source validation upstream is what rejects it.
	authority, _ := giturl.CanonicalAuthority(cloneURL)
	name := secretname.GitCredential(appName, secretname.GitCredentialDigest(token, authority))

	fresh := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				"kipper.run/app":               appName,
			},
		},
		Data: map[string][]byte{"token": []byte(token)},
	}
	if authority != "" {
		fresh.Annotations = map[string]string{builder.GitAuthorityAnnotation: authority}
	}
	// Bound to the App that is being pointed at it, so Kubernetes collects it
	// when that App goes. The sweep needs a live App to reconcile, so a write
	// racing a deletion would otherwise leave the token for the life of the
	// namespace, and no writer can safely delete an object another writer may
	// have committed.
	if owner != nil && owner.UID != "" {
		fresh.OwnerReferences = []metav1.OwnerReference{
			appowner.Reference(kipperv1.GroupVersion.String(), owner.Name, owner.UID),
		}
	}

	_, err := a.Client.CoreV1().Secrets(namespace).Create(ctx, fresh, metav1.CreateOptions{})
	if err == nil {
		return name, nil
	}
	if !errors.IsAlreadyExists(err) {
		return "", err
	}
	// AlreadyExists says the name is taken, not that the object holds the pair
	// the name stands for: the digest is sixteen hex characters, and anything
	// that can write a Secret can put something else at the address. The claim
	// checks, because committing an App onto an object it has not verified
	// would have it clone with a token nobody supplied.
	if err := a.claimGitCredential(ctx, namespace, name, appName, owner, token, authority); err != nil {
		return "", err
	}
	return name, nil
}

// claimGitCredential marks a credential as being committed onto now, and binds
// it to the App doing the committing.
//
// The object outlives the App that first wrote it, because a name is a digest
// of the pair rather than of who asked. An App deleted and recreated under the
// same name converges on the same object, and leaving the dead App's controller
// reference on it means garbage collection removes the credential the live App
// has just been pointed at.
func (a *Apps) claimGitCredential(ctx context.Context, namespace, name, appName string, owner *kipperv1.App, token, authority string) error {
	var ref *metav1.OwnerReference
	if owner != nil && owner.UID != "" {
		bound := appowner.Reference(kipperv1.GroupVersion.String(), owner.Name, owner.UID)
		ref = &bound
	}
	secrets := a.Client.CoreV1().Secrets(namespace)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := secrets.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if err := gitcred.Claim(live, appName, token, authority, ref, time.Now()); err != nil {
			return err
		}
		_, err = secrets.Update(ctx, live, metav1.UpdateOptions{})
		return err
	})
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

	// The same rule the webhook and both CLI writers apply. Setting an image on
	// an app that builds from git writes something the next build overwrites.
	// Promotion in projects.go is the remaining writer that does it silently.
	if app.Spec.Git != nil {
		respondError(w, http.StatusConflict, fmt.Sprintf(
			"%s builds its image from %s, so the image set here would be overwritten by the next build. Remove the git source first if it should deploy prebuilt images instead",
			appName, app.Spec.Git.URL))
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
