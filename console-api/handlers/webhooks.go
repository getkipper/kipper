package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
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
	"github.com/getkipper/kipper/console-api/internal/registrycred"
	"github.com/getkipper/kipper/controller/pkg/giturl"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

const (
	webhookSecretSuffix = "-webhook"
	secretTokenField    = "token"
	historyAnnotation   = "kipper.run/deploy-history"
	maxHistoryEntries   = 10
)

// Webhooks provides handlers for CI/CD webhook-triggered deployments.
type Webhooks struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
	// ProbeGit authenticates a git credential against its host to report
	// whether it still works. Injected so a test can prove a token that must
	// not travel never reaches it.
	ProbeGit func(ctx context.Context, server, token string) tokenHealth
}

func (wh *Webhooks) probeGit(ctx context.Context, server, token string) tokenHealth {
	if wh.ProbeGit != nil {
		return wh.ProbeGit(ctx, server, token)
	}
	return probeGitCredential(ctx, server, token)
}

type webhookRequest struct {
	Image  string `json:"image"`
	Commit string `json:"commit"`
}

// deployEntry mirrors controllers.buildDeployEntry and webhook.DeployEntry in
// kip. All three marshal the same annotation and round-trip it, so a field
// missing here is stripped from every entry the moment somebody rolls back.
type deployEntry struct {
	Revision  int    `json:"revision"`
	Image     string `json:"image"`
	Commit    string `json:"commit,omitempty"`
	Trigger   string `json:"trigger"`
	Timestamp string `json:"timestamp"`
	Build     string `json:"build,omitempty"`
}

// Receive handles incoming webhook requests from GitLab/GitHub CI pipelines.
// POST /api/v1/webhook/{namespace}/{app}
// Authenticated via X-Kipper-Token header matching the app's webhook secret.
func (wh *Webhooks) Receive(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "namespace")
	app := chi.URLParam(r, "app")

	// Bound the body before any read: this endpoint authenticates by a token
	// carried in a header, so an unauthenticated caller can still reach the
	// body reads below. A webhook payload is a tiny JSON object; 1 MiB is far
	// above any real payload and caps an OOM attempt.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Bound the body read in time too, so a slow-drip body can't pin a
	// connection and goroutine indefinitely. A per-request deadline via the
	// ResponseController is scoped to this handler, so it never touches the
	// long-lived WebSocket streams the server also serves.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Second))

	// Read and validate token
	token := r.Header.Get("X-Kipper-Token")
	if token == "" {
		// Try GitLab/GitHub signature headers as fallback
		token = r.Header.Get("X-Gitlab-Token")
	}

	if token == "" {
		respondError(w, http.StatusUnauthorized, "missing webhook token")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Verify token against stored secret
	storedToken, err := wh.getWebhookToken(ctx, namespace, app)
	if err != nil {
		respondError(w, http.StatusUnauthorized, "webhook not configured for this app")
		return
	}

	if !hmac.Equal([]byte(token), []byte(storedToken)) {
		// Also try HMAC signature verification for GitHub
		body, _ := io.ReadAll(r.Body)
		sig := r.Header.Get("X-Hub-Signature-256")
		if sig == "" || !verifyHMAC(body, sig, storedToken) {
			respondError(w, http.StatusUnauthorized, "invalid webhook token")
			return
		}
		// Re-parse body
		var req webhookRequest
		if err := json.Unmarshal(body, &req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid request body")
			return
		}
		wh.processDeploy(ctx, w, namespace, app, req)
		return
	}

	var req webhookRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	wh.processDeploy(ctx, w, namespace, app, req)
}

func (wh *Webhooks) processDeploy(ctx context.Context, w http.ResponseWriter, namespace, app string, req webhookRequest) {
	var appCR kipperv1.App
	if err := wh.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: app}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found in %s", app, namespace))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Git != nil {
		// An image names an exact artefact the pipeline has already built and
		// pushed. Discarding it to build the repository again answers a
		// question nobody asked, and answering 200 for it let a pipeline stay
		// green while nothing deployed. The two ways of deploying cannot both
		// be in play, so say which one is configured and stop.
		if req.Image != "" {
			respondError(w, http.StatusConflict, fmt.Sprintf(
				"%s builds from %s, so the image you posted would be ignored. Remove the git source with 'kip app git remove %s' to deploy prebuilt images, or post only a commit to trigger a build",
				appCR.Name, appCR.Spec.Git.URL, appCR.Name))
			return
		}
		wh.triggerBuild(ctx, w, &appCR, req.Commit)
		return
	}

	// Image-based apps: direct deploy
	if req.Image == "" {
		respondError(w, http.StatusBadRequest, "image is required")
		return
	}

	wh.recordDeployAndUpdate(ctx, w, &appCR, req.Image, req.Commit, "webhook")
}

func (wh *Webhooks) triggerBuild(ctx context.Context, w http.ResponseWriter, appCR *kipperv1.App, commit string) {
	if commit == "" {
		respondError(w, http.StatusBadRequest, "commit is required for git-based apps")
		return
	}

	job, err := builder.CreateBuildJob(ctx, wh.Client, appCR, commit)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create build: %v", err))
		return
	}

	// Update build status on the App CR
	now := metav1.Now()
	appCR.Status.Build = &kipperv1.AppBuildStatus{
		Phase:     "Pending",
		Commit:    commit,
		StartedAt: &now,
		// Named from the start, so a status this write leaves behind is never
		// one the guards have to treat as predating the field.
		Build: job.Name,
	}
	if err := wh.CRClient.Status().Update(ctx, appCR); err != nil {
		log.Printf("webhook: failed to update build status for %s/%s: %v", appCR.Namespace, appCR.Name, err)
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status": "building",
		"app":    appCR.Name,
		"commit": commit,
		"job":    job.Name,
	})
}

func (wh *Webhooks) recordDeployAndUpdate(ctx context.Context, w http.ResponseWriter, appCR *kipperv1.App, image, commit, trigger string) {
	history := loadDeployHistory(appCR.Annotations)
	nextRevision := 1
	if len(history) > 0 {
		nextRevision = history[0].Revision + 1
	}

	entry := deployEntry{
		Revision:  nextRevision,
		Image:     image,
		Commit:    commit,
		Trigger:   trigger,
		Timestamp: time.Now().Format(time.RFC3339),
	}

	history = append([]deployEntry{entry}, history...)
	if len(history) > maxHistoryEntries {
		history = history[:maxHistoryEntries]
	}

	data, _ := json.Marshal(history)
	if appCR.Annotations == nil {
		appCR.Annotations = make(map[string]string)
	}
	appCR.Annotations[historyAnnotation] = string(data)
	appCR.Spec.Image = image

	if err := wh.CRClient.Update(ctx, appCR); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update app")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "deployed",
		"app":      appCR.Name,
		"image":    image,
		"commit":   commit,
		"revision": nextRevision,
	})
}

// Rebuild triggers a new build for a git-based app.
// POST /api/v1/projects/{name}/apps/{app}/rebuild
func (wh *Webhooks) Rebuild(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	var req struct {
		Commit string `json:"commit"`
	}
	_ = decodeJSON(r, &req)

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := wh.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", app))
		return
	}

	if appCR.Spec.Git == nil {
		respondError(w, http.StatusBadRequest, "app has no git source configured")
		return
	}

	commit := req.Commit
	if commit == "" {
		commit = fmt.Sprintf("manual-%d", time.Now().Unix())
	}

	wh.triggerBuild(ctx, w, &appCR, commit)
}

// describesTheSameBuild reports whether a stored verdict is about the job a
// status was derived from. Both name the job, so this needs no clock: comparing
// a job's apiserver-stamped creation time against a time the controller stamped
// itself assumed the two agreed.
//
// A verdict that names no job predates the field and is treated as describing
// whatever is reported, which keeps it preferred rather than hidden.
func describesTheSameBuild(stored, derived *kipperv1.AppBuildStatus) bool {
	if stored == nil {
		return false
	}
	if stored.Build == "" {
		return true
	}
	return derived == nil || derived.Build == "" || derived.Build == stored.Build
}

// BuildStatus returns the current build status and git source info for an app.
// GET /api/v1/projects/{name}/apps/{app}/build/status
func (wh *Webhooks) BuildStatus(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Get git source info from App CR
	var appCR kipperv1.App
	gitConfigured := false
	var gitURL, gitBranch, credentialsSecret string
	if err := wh.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err == nil {
		if appCR.Spec.Git != nil {
			gitConfigured = true
			gitURL = sanitizeGitURL(appCR.Spec.Git.URL)
			gitBranch = appCR.Spec.Git.Branch
			if gitBranch == "" {
				gitBranch = "main"
			}
			credentialsSecret = appCR.Spec.Git.CredentialsSecret
		}
	}

	resp := map[string]interface{}{
		"git_configured":     gitConfigured,
		"git_url":            gitURL,
		"git_branch":         gitBranch,
		"credentials_secret": credentialsSecret,
	}

	// Probe registry and git credential health in parallel.
	if gitConfigured {
		type healthResult struct {
			registryValid *bool
			gitValid      *bool
		}
		hr := make(chan healthResult, 1)

		go func() {
			probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
			defer probeCancel()

			var regValid, gitValid *bool

			var wg sync.WaitGroup

			// Registry pull secrets: probe all configured registries.
			wg.Add(1)
			go func() {
				defer wg.Done()
				entries, err := registrycred.Load(probeCtx, wh.Client)
				if err != nil {
					// An unreadable list stays "unknown" (nil), never "valid".
					return
				}
				if len(entries) == 0 {
					return
				}
				allValid := true
				for _, entry := range entries {
					h := probeRegistry(probeCtx, entry)
					if !h.Valid {
						allValid = false
						break
					}
				}
				regValid = &allValid
			}()

			// Git credential: probe only the app's OWN per-app credential. A
			// shared credential is administrator-managed, so a project member
			// must not make console-api authenticate its token (unauthorized use
			// + a validity oracle) by naming it on the App CR; its health lives on
			// the admin-only settings endpoint.
			if secretname.IsGitCredentialOf(appName, credentialsSecret) {
				wg.Add(1)
				go func() {
					defer wg.Done()
					secret, err := wh.Client.CoreV1().Secrets(project).Get(probeCtx, credentialsSecret, metav1.GetOptions{})
					if err != nil {
						return
					}
					token := secret.Data["token"]
					if len(token) == 0 {
						return
					}
					// The build refuses a credential whose recorded host is not
					// the one it clones from, and this probe authenticates
					// against exactly that host, so without the same check
					// opening the Deployments tab discloses the token the build
					// would not send.
					authority, authErr := giturl.CanonicalAuthority(appCR.Spec.Git.URL)
					if authErr != nil {
						return
					}
					if _, elsewhere := builder.CredentialBoundElsewhere(secret.Annotations, authority); elsewhere {
						unusable := false
						gitValid = &unusable
						return
					}
					h := wh.probeGit(probeCtx, appCR.Spec.Git.URL, string(token))
					gitValid = &h.Valid
				}()
			}

			wg.Wait()
			hr <- healthResult{registryValid: regValid, gitValid: gitValid}
		}()

		select {
		case result := <-hr:
			if result.registryValid != nil {
				resp["registry_valid"] = *result.registryValid
			}
			if result.gitValid != nil {
				resp["git_credential_valid"] = *result.gitValid
			}
		case <-ctx.Done():
			// Timed out — skip health fields rather than blocking the response.
		}
	}

	status, err := builder.GetBuildStatus(ctx, wh.Client, project, appName)
	// A discarded build is a job that succeeded, so the phase derived from the
	// job says Succeeded for the hour its TTL runs. The reconciler's verdict is
	// the one that matches what the app is actually running, and teaching the
	// job-derived path the fingerprint rule would be a second copy of a
	// comparison that has to stay single.
	//
	// The verdict only speaks for the job it was written about. A build started
	// afterwards may never reach Active — a pod refused by a quota does not —
	// so nothing would write over the verdict, and the operator would be shown
	// the previous build's outcome while waiting on the current one.
	if appCR.Status.Build != nil && appCR.Status.Build.Phase == "Discarded" && describesTheSameBuild(appCR.Status.Build, status) {
		status = nil
	}
	if err != nil || status == nil {
		// No active build jobs — fall back to the App CR's stored build
		// status. This covers the case where the build job has been
		// cleaned up but the result was recorded on the CR.
		if appCR.Status.Build != nil {
			b := appCR.Status.Build
			resp["phase"] = b.Phase
			resp["commit"] = b.Commit
			if b.StartedAt != nil {
				resp["startedAt"] = b.StartedAt.Format(time.RFC3339)
			}
			if b.CompletedAt != nil {
				resp["completedAt"] = b.CompletedAt.Format(time.RFC3339)
			}
			if b.Message != "" {
				resp["message"] = b.Message
			}
		} else {
			resp["phase"] = "none"
		}
		respondJSON(w, http.StatusOK, resp)
		return
	}

	resp["phase"] = status.Phase
	resp["commit"] = status.Commit
	if status.StartedAt != nil {
		resp["startedAt"] = status.StartedAt.Format(time.RFC3339)
	}
	if status.CompletedAt != nil {
		resp["completedAt"] = status.CompletedAt.Format(time.RFC3339)
	}
	if status.Message != "" {
		resp["message"] = status.Message
	}

	respondJSON(w, http.StatusOK, resp)
}

// CancelBuild cancels the active build for a git-based app.
// POST /api/v1/projects/{name}/apps/{app}/build/cancel
func (wh *Webhooks) CancelBuild(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := builder.CancelBuild(ctx, wh.Client, project, app); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to cancel build")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

// BuildLogs streams the build logs for the latest build pod.
// GET /api/v1/projects/{name}/apps/{app}/build/logs
func (wh *Webhooks) BuildLogs(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	ctx := r.Context()

	pod, err := builder.GetBuildPod(ctx, wh.Client, project, appName)
	if err != nil {
		respondError(w, http.StatusNotFound, "no build found")
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)

	// Only follow active pods. Completed pods have all their logs
	// available immediately — following them hangs the connection.
	podRunning := pod.Status.Phase == corev1.PodRunning || pod.Status.Phase == corev1.PodPending

	// Stream logs from all containers (init + main). Build a fresh slice
	// so we don't mutate pod.Spec.InitContainers' underlying array.
	allContainers := make([]corev1.Container, 0, len(pod.Spec.InitContainers)+len(pod.Spec.Containers))
	allContainers = append(allContainers, pod.Spec.InitContainers...)
	allContainers = append(allContainers, pod.Spec.Containers...)
	tailLines := int64(1000)

	for _, c := range allContainers {
		// The build pod lives in the isolated build namespace, not the tenant's
		// project namespace, so logs must be read from where GetBuildPod found it.
		logReq := wh.Client.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: c.Name,
			Follow:    podRunning,
			TailLines: &tailLines,
		})

		stream, streamErr := logReq.Stream(ctx)
		if streamErr != nil {
			continue
		}

		_, _ = fmt.Fprintf(w, "--- %s ---\n", c.Name)
		if ok {
			flusher.Flush()
		}

		buf := make([]byte, 4096)
		for {
			n, readErr := stream.Read(buf)
			if n > 0 {
				_, _ = w.Write(buf[:n])
				if ok {
					flusher.Flush()
				}
			}
			if readErr != nil {
				break
			}
		}
		_ = stream.Close()
	}
}

// History returns deploy history for an app.
// GET /api/v1/projects/{name}/apps/{app}/history
func (wh *Webhooks) History(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := wh.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, []deployEntry{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	history := loadDeployHistory(appCR.Annotations)
	respondJSON(w, http.StatusOK, history)
}

// Rollback reverts an app to a previous revision.
// POST /api/v1/projects/{name}/apps/{app}/rollback
func (wh *Webhooks) Rollback(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	var req struct {
		Revision int `json:"revision"`
	}
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	key := crclient.ObjectKey{Namespace: project, Name: app}

	// Validate and pick the target up front so a bad request returns a precise
	// error before any mutation.
	var appCR kipperv1.App
	if err := wh.CRClient.Get(ctx, key, &appCR); err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", app))
		return
	}
	target := selectRollbackTarget(loadDeployHistory(appCR.Annotations), req.Revision)
	if target == nil {
		if req.Revision > 0 {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("revision %d not found in deploy history", req.Revision))
		} else {
			respondError(w, http.StatusBadRequest, "no previous version to rollback to")
		}
		return
	}
	targetImage, targetCommit := target.Image, target.Commit

	// Remove every build Job for the app so a pending or just-succeeded build
	// can't reconcile later and overwrite the image this rollback sets.
	if err := builder.CancelBuilds(ctx, wh.Client, project, app); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to cancel active builds")
		return
	}

	// Apply under conflict retry: the reconciler and build controller also write
	// the App, so a benign concurrent update must not fail the rollback.
	var newRevision int
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var live kipperv1.App
		if err := wh.CRClient.Get(ctx, key, &live); err != nil {
			return err
		}
		history := loadDeployHistory(live.Annotations)
		newRevision = 1
		if len(history) > 0 {
			newRevision = history[0].Revision + 1
		}
		history = append([]deployEntry{{
			Revision:  newRevision,
			Image:     targetImage,
			Commit:    targetCommit,
			Trigger:   "rollback",
			Timestamp: time.Now().Format(time.RFC3339),
		}}, history...)
		if len(history) > maxHistoryEntries {
			history = history[:maxHistoryEntries]
		}
		data, _ := json.Marshal(history)
		if live.Annotations == nil {
			live.Annotations = make(map[string]string)
		}
		live.Annotations[historyAnnotation] = string(data)
		live.Spec.Image = targetImage
		return wh.CRClient.Update(ctx, &live)
	}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to rollback")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":   "rolled_back",
		"revision": newRevision,
		"image":    targetImage,
	})
}

// selectRollbackTarget returns the history entry to roll back to. An explicit
// revision must exist; a zero revision means the immediately previous version.
func selectRollbackTarget(history []deployEntry, revision int) *deployEntry {
	if revision > 0 {
		for i := range history {
			if history[i].Revision == revision {
				return &history[i]
			}
		}
		return nil
	}
	if len(history) >= 2 {
		return &history[1]
	}
	return nil
}

// GetConfig returns the webhook configuration for an app.
// GET /api/v1/projects/{name}/apps/{app}/webhook
func (wh *Webhooks) GetConfig(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	token, err := wh.getWebhookToken(ctx, project, app)
	if err != nil {
		respondJSON(w, http.StatusOK, map[string]interface{}{"enabled": false})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": true,
		"token":   token,
	})
}

// GenerateToken creates or regenerates a webhook token for an app.
// POST /api/v1/projects/{name}/apps/{app}/webhook
func (wh *Webhooks) GenerateToken(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to generate token")
		return
	}
	token := hex.EncodeToString(tokenBytes)

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	secretName := app + webhookSecretSuffix
	existing, err := wh.Client.CoreV1().Secrets(project).Get(ctx, secretName, metav1.GetOptions{})
	if err == nil {
		existing.Data = map[string][]byte{secretTokenField: []byte(token)}
		if _, err := wh.Client.CoreV1().Secrets(project).Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update webhook token")
			return
		}
	} else {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretName,
				Namespace: project,
				Labels: map[string]string{
					kipperLabel: kipperValue,
				},
			},
			Data: map[string][]byte{secretTokenField: []byte(token)},
		}
		if _, err := wh.Client.CoreV1().Secrets(project).Create(ctx, secret, metav1.CreateOptions{}); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to create webhook token")
			return
		}
	}

	respondJSON(w, http.StatusOK, map[string]string{"token": token})
}

// DeleteWebhook removes the webhook configuration for an app.
// DELETE /api/v1/projects/{name}/apps/{app}/webhook
func (wh *Webhooks) DeleteWebhook(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	err := wh.Client.CoreV1().Secrets(project).Delete(ctx, app+webhookSecretSuffix, metav1.DeleteOptions{})
	if err != nil && !errors.IsNotFound(err) {
		respondError(w, http.StatusInternalServerError, "failed to delete webhook")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (wh *Webhooks) getWebhookToken(ctx context.Context, namespace, app string) (string, error) {
	secret, err := wh.Client.CoreV1().Secrets(namespace).Get(ctx, app+webhookSecretSuffix, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	token, ok := secret.Data[secretTokenField]
	if !ok {
		return "", fmt.Errorf("token not found")
	}
	return string(token), nil
}

func verifyHMAC(payload []byte, signature, secret string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(payload)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(signature))
}

func sanitizeGitURL(rawURL string) string {
	// Strip inline credentials from HTTPS URLs to avoid leaking tokens.
	// https://oauth2:glpat-xxx@git.example.com/repo.git → https://git.example.com/repo.git
	if idx := strings.Index(rawURL, "://"); idx != -1 {
		rest := rawURL[idx+3:]
		if atIdx := strings.Index(rest, "@"); atIdx != -1 {
			return rawURL[:idx+3] + rest[atIdx+1:]
		}
	}
	return rawURL
}

func loadDeployHistory(annotations map[string]string) []deployEntry {
	if annotations == nil {
		return nil
	}
	raw, ok := annotations[historyAnnotation]
	if !ok || raw == "" {
		return nil
	}
	var history []deployEntry
	if err := json.Unmarshal([]byte(raw), &history); err != nil {
		return nil
	}
	return history
}
