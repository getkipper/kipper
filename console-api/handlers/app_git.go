package handlers

import (
	"context"
	stderrors "errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
	"github.com/getkipper/kipper/console-api/internal/gitreach"
	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/controller/pkg/gitcred"
	"github.com/getkipper/kipper/controller/pkg/giturl"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/controller/pkg/sharedcred"
)

// gitResponse is what the UI reads when populating the Git source
// panel. The token field is never echoed back — the API treats it
// strictly as write-only so a compromised read endpoint can't exfil
// credentials.
type gitResponse struct {
	Configured     bool   `json:"configured"`
	URL            string `json:"url,omitempty"`
	Branch         string `json:"branch,omitempty"`
	DockerfilePath string `json:"dockerfile_path,omitempty"`
	Context        string `json:"context,omitempty"`
	HasToken       bool   `json:"has_token"`
}

type setGitRequest struct {
	URL            string `json:"url,omitempty"`
	Branch         string `json:"branch,omitempty"`
	DockerfilePath string `json:"dockerfile_path,omitempty"`
	Context        string `json:"context,omitempty"`
	// Token is optional. Empty string means "leave the existing
	// credentials Secret alone". Non-empty rotates the token in place.
	Token string `json:"token,omitempty"`
}

// GetGit returns the App's current Git source for the Settings UI. The
// token is never returned — only a `has_token` boolean indicating
// whether a credentials Secret is wired up.
// GET /api/v1/projects/{name}/apps/{app}/git
func (a *Apps) GetGit(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, gitResponse{})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Git == nil {
		respondJSON(w, http.StatusOK, gitResponse{})
		return
	}

	resp := gitResponse{
		Configured:     true,
		URL:            sanitizeGitURL(appCR.Spec.Git.URL),
		Branch:         appCR.Spec.Git.Branch,
		DockerfilePath: appCR.Spec.Git.DockerfilePath,
		Context:        appCR.Spec.Git.Context,
		HasToken:       appCR.Spec.Git.CredentialsSecret != "",
	}
	respondJSON(w, http.StatusOK, resp)
}

// SetGit updates the App's Git source. Empty string fields are NOT
// applied — the request acts as a partial update so the UI can rotate
// just the token, or just the branch, without re-supplying every field.
// A non-empty token is stored as a new credential object named after the pair
// it holds, and the App CR is moved onto it. Nothing writes to the credential
// the app is currently cloning with, so a failed update leaves that pair
// untouched and the next build is unaffected.
//
// PUT /api/v1/projects/{name}/apps/{app}/git
func (a *Apps) SetGit(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	var req setGitRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Git == nil {
		// Operators can wire a git source onto an existing image-based
		// App; the URL is the only field required to bootstrap that.
		if req.URL == "" {
			respondError(w, http.StatusBadRequest, "git.url is required to attach a git source to an app that does not have one")
			return
		}
		appCR.Spec.Git = &kipperv1.AppGitSource{}
	}

	// Reject a URL the build could not host-bind a credential to; a clean early
	// error rather than a build-time failure.
	if req.URL != "" {
		if _, err := giturl.CanonicalAuthority(req.URL); err != nil {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid git url: %v", err))
			return
		}
	}

	// The source this app cloned from before this request, which is the
	// authority its stored credential was given for.
	previousURL := appCR.Spec.Git.URL

	// Moving an app to another host while its own credential stays attached is
	// refused outright, because accepting it stores the dangerous state rather
	// than merely declining to act on it: the CR would name the new host and
	// still reference the old token, and every later build and status probe
	// resolves that token against whatever host the CR now names.
	//
	// Any deployer may edit this URL and none of them needs to know the token,
	// while reading it back needs a password re-entry — so accepting the pair
	// would turn a routine source edit into a way to post a private repository
	// credential to an arbitrary host. A replacement token means the caller has
	// one for the new host, and is allowed.
	if req.URL != "" && req.Token == "" && appCR.Spec.Git.CredentialsSecret != "" &&
		usesPerAppCredential(appCR.Name, appCR.Spec.Git.CredentialsSecret) &&
		!sameGitAuthority(req.URL, previousURL) {
		respondError(w, http.StatusBadRequest, fmt.Sprintf(
			"%s is on a different host from the repository this app clones, and its stored access token belongs to the old one. Send a token for the new host with this change, or remove the source first with 'kip app git remove %s'",
			sanitizeGitURL(req.URL), appName))
		return
	}

	// Partial update: only overwrite fields the request actually set.
	// This lets the UI ship a token-only payload without clearing the
	// other fields.
	if req.URL != "" {
		appCR.Spec.Git.URL = req.URL
	}
	if req.Branch != "" {
		appCR.Spec.Git.Branch = req.Branch
	}
	if req.DockerfilePath != "" {
		appCR.Spec.Git.DockerfilePath = req.DockerfilePath
	}
	if req.Context != "" {
		appCR.Spec.Git.Context = req.Context
	}

	// Token rotation. A rotation is the App moving onto a different credential
	// object, so the CR update below is what carries it. Only a non-empty token
	// writes anything.
	// Prove the source can be read before storing anything. The credential
	// checked is the effective one — the token in this request when there is
	// one, the stored token otherwise — so rotating to a stale token is caught
	// here rather than by the next build.
	//
	// Only a definite refusal blocks the write. A host this cluster cannot
	// reach has told us nothing about the repository, and refusing on that
	// would make the console stop accepting work whenever its own egress is
	// unhappy.
	// What can be judged without a network is judged first, so a timeout never
	// waves through input the builder already knows is impossible.
	branchToCheck := appCR.Spec.Git.Branch
	if branchToCheck == "" {
		branchToCheck = "main"
	}
	if err := builder.ValidateGitSource(appCR.Spec.Git.URL, branchToCheck); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	effectiveToken, resolved := a.effectiveGitToken(ctx, project, appName, appCR.Spec.Git.CredentialsSecret, req.Token, appCR.Spec.Git.URL, previousURL)
	if resolved {
		result, detail := a.reachGit()(ctx, appCR.Spec.Git.URL, branchToCheck, gitCredentialUsername, effectiveToken)
		if result == gitreach.NeedsCredential || result == gitreach.Unsafe {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("%s cannot be cloned: %s", sanitizeGitURL(appCR.Spec.Git.URL), detail))
			return
		}
		// A host the preflight could not reach does not block the write, even
		// when a token is being attached. It looks like the cautious choice and
		// is not: the token the build uses is released by a credential helper
		// scoped to the clone URL's own host and to https, so a redirect
		// elsewhere gets nothing. Refusing here would instead mean a cluster
		// whose console egress differs from the build namespace's — an ordinary
		// per-namespace network policy — could never store or rotate a token,
		// while its builds worked perfectly.
	}

	// The credential is a new object per pair, and the CR update is what
	// commits it. Nothing here writes to the Secret the app currently names, so
	// a failed update leaves the committed pair exactly as it was and this
	// attempt has nothing to undo but the object it just made. The sweep in the
	// App reconciler removes the one the app has moved off.
	if req.Token != "" {
		name, err := a.writeGitCredential(ctx, project, appName, req.Token, appCR.Spec.Git.URL, &appCR)
		if err != nil {
			// A refusal is cluster state disagreeing with the request rather
			// than the platform failing, and the caller can act on it: nothing
			// was written, so the app still names the credential it did.
			status := http.StatusInternalServerError
			var refused *gitcred.Refusal
			if stderrors.As(err, &refused) {
				status = http.StatusConflict
			}
			respondError(w, status, fmt.Sprintf("failed to store git credentials: %v", err))
			return
		}
		appCR.Spec.Git.CredentialsSecret = name
	}

	if err := a.CRClient.Update(ctx, &appCR); err != nil {
		// Nothing is undone here. Two writers of the same pair share one
		// object, so winning its Create is not owning it, and no read can
		// establish that nobody is about to commit the App onto it. An object
		// this write leaves behind is a token the caller supplied, in their own
		// namespace, which the App reconciler's sweep collects; deleting one
		// another writer has committed cannot be recovered without the git
		// host.
		//
		// The sweep needs an App to reconcile, so a write racing the App's own
		// deletion would leave the object until the namespace went. The
		// credential carries an owner reference to that App, so Kubernetes
		// collects it instead.
		respondError(w, http.StatusInternalServerError, "failed to update git source")
		return
	}

	respondJSON(w, http.StatusOK, gitResponse{
		Configured:     true,
		URL:            sanitizeGitURL(appCR.Spec.Git.URL),
		Branch:         appCR.Spec.Git.Branch,
		DockerfilePath: appCR.Spec.Git.DockerfilePath,
		Context:        appCR.Spec.Git.Context,
		HasToken:       appCR.Spec.Git.CredentialsSecret != "",
	})
}

// RevealGitToken returns the plaintext token an app uses to clone its source
// repository, after re-verifying the caller's password against Dex. This
// breaks the write-only invariant of GetGit, so it sits behind two gates:
// the deployer role (enforced upstream by middleware.RequireRole) and the
// knowledge-factor password check here.
//
// Reveal is deployer-accessible, looser than the admin-only global credential
// reveal. A deployer already owns an app's git source and can rotate its token
// via SetGit, so recovering it stays within their existing scope; the password
// re-entry is the second factor. Operators who treat app tokens as broad,
// cross-repo PATs should scope them per repository.
// POST /api/v1/projects/{name}/apps/{app}/git/reveal
func (a *Apps) RevealGitToken(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	var req struct {
		Password string `json:"password"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Password == "" {
		respondError(w, http.StatusBadRequest, "password is required")
		return
	}

	claims := middleware.UserFromContext(r.Context())
	if claims == nil {
		respondError(w, http.StatusUnauthorized, "not authenticated")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := VerifyUserPassword(ctx, a.Client, claims.Email, req.Password); err != nil {
		if stderrors.Is(err, ErrInvalidPassword) {
			log.Printf("reveal git-token app=%s/%s by user=%s: invalid password", project, appName, claims.Email)
			respondError(w, http.StatusUnauthorized, "invalid password")
			return
		}
		log.Printf("reveal git-token app=%s/%s by user=%s: %v", project, appName, claims.Email, err)
		respondError(w, http.StatusInternalServerError, "password verification failed")
		return
	}

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	if appCR.Spec.Git == nil || appCR.Spec.Git.CredentialsSecret == "" {
		respondError(w, http.StatusNotFound, fmt.Sprintf("app %q has no git credential configured", appName))
		return
	}

	// Reveal only the app's OWN per-app credential. A shared credential (or a
	// leftover fan-out copy named after one) is administrator-managed and must
	// not be disclosed to a deployer through the per-app reveal — otherwise a
	// deployer could point CredentialsSecret at a shared token their project is
	// not allow-listed for and read it in plaintext, bypassing the builder's
	// classification. This applies the same contract the builder enforces.
	if !secretname.IsGitCredentialOf(appName, appCR.Spec.Git.CredentialsSecret) {
		respondError(w, http.StatusForbidden, "this app uses an administrator-managed shared git credential, which cannot be revealed here")
		return
	}

	secret, err := a.Client.CoreV1().Secrets(project).Get(ctx, appCR.Spec.Git.CredentialsSecret, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "git credentials secret not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to read git credentials")
		return
	}

	token, ok := secret.Data["token"]
	if !ok || len(token) == 0 {
		respondError(w, http.StatusNotFound, "git credentials secret has no token")
		return
	}

	log.Printf("reveal git-token app=%s/%s by user=%s: ok", project, appName, claims.Email)
	respondJSON(w, http.StatusOK, map[string]string{"token": string(token)})
}

// DeleteGit detaches an app's git source, so it deploys prebuilt images
// instead of building them.
//
// Only the source is cleared here. The token the source used and the build
// status it left behind are the controller's to remove, because a handler that
// did both would leave a half-detached app behind whenever it failed between
// the two, and nothing would come back to finish it.
//
// DELETE /api/v1/projects/{name}/apps/{app}/git
func (a *Apps) DeleteGit(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	appName := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var appCR kipperv1.App
		if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: appName}, &appCR); err != nil {
			return err
		}
		if appCR.Spec.Git == nil {
			return nil
		}
		appCR.Spec.Git = nil
		return a.CRClient.Update(ctx, &appCR)
	}); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", appName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to remove the git source")
		return
	}

	respondJSON(w, http.StatusOK, map[string]any{"configured": false})
}

// reachGit returns the preflight to run. A seam rather than a direct call,
// because every test of this handler would otherwise reach the real internet,
// and what those tests are about is which fields get written.
func (a *Apps) reachGit() GitReachFunc {
	if a.GitReach != nil {
		return a.GitReach
	}
	return gitreach.Check
}

// gitCredentialUsername is what a token is sent as. Git hosts ignore the
// username on a personal access token but reject a request that sends none.
const gitCredentialUsername = "kipper"

// effectiveGitToken returns the credential a build would really use, and
// whether it could be determined at all.
//
// An app's credential is either a Secret in its own namespace or an entry in
// the cluster's shared list, and the builder resolves both. Reading only the
// first and then probing anonymously is worse than not probing: a private
// repository cloned successfully by a shared credential answers 401 to an
// anonymous request, so a working app would be refused an unrelated edit with
// "this repository is private", which is both wrong and unactionable.
//
// So a credential named but not resolvable here reports false, and the caller
// skips the check. The build still enforces the real rules — allow-list, host
// binding, the lot — and this makes no attempt to second-guess them.
func (a *Apps) effectiveGitToken(ctx context.Context, namespace, appName, credentialsSecret, requested, cloneURL, previousURL string) (string, bool) {
	if requested != "" {
		return requested, true
	}
	if credentialsSecret == "" {
		return "", true
	}
	// Resolution order is the builder's, because anything this sends is
	// something the build would send: the administrator's shared list first,
	// and a namespaced Secret only under the app's own conventional name.
	// Reading the namespace first let a project's own Secret of that name
	// shadow a shared entry, and Secret names are namespace-local, so that
	// collision is ordinary rather than contrived.
	shared, err := sharedcred.Load(ctx, a.Client)
	if err != nil {
		return "", false
	}
	if entry := sharedcred.Find(shared, credentialsSecret); entry != nil {
		if entry.Token == "" {
			return "", false
		}
		if !a.sharedCredentialApplies(ctx, namespace, entry, cloneURL) {
			return "", false
		}
		return entry.Token, true
	}

	// The builder reads no other name, so neither does this.
	if !secretname.IsGitCredentialOf(appName, credentialsSecret) {
		return "", false
	}
	secret, err := a.Client.CoreV1().Secrets(namespace).Get(ctx, credentialsSecret, metav1.GetOptions{})
	if err != nil {
		return "", false
	}
	// A per-app token was given for one repository, and this API treats it as
	// write-only: it is never read back without a password re-entry. Sending it
	// to a host the caller has just named would hand that second factor away,
	// because any deployer may change the URL and none of them needs to know
	// the token to do it. So a credential follows its own authority, or it does
	// not travel at all.
	if !sameGitAuthority(cloneURL, previousURL) {
		return "", false
	}
	// A URL that never moved still proves nothing about the token beside it: a
	// rolled-back move can strand one host's credential on another host's app.
	authority, err := giturl.CanonicalAuthority(cloneURL)
	if err != nil {
		return "", false
	}
	if _, elsewhere := builder.CredentialBoundElsewhere(secret.Annotations, authority); elsewhere {
		return "", false
	}
	return string(secret.Data["token"]), true
}

// sharedCredentialApplies re-applies the builder's gates before a shared token
// is used for anything.
//
// The clone URL on an app is writable by any deployer in the project, and this
// check sends the token to whatever host it names. Without the gates a
// deployer could point an app at a host they control, leave the token field
// empty, and have the preflight hand them an admin-managed credential
// belonging to another project — a disclosure the builder's own binding
// (resolveGitToken) exists to prevent, and one RevealGitToken already refuses
// to make.
//
// The two gates are the builder's: the credential is bound to one host, and it
// is allow-listed to particular projects. Anything unresolvable fails closed,
// because the caller then skips the check rather than probing with a token it
// should not hold.
func (a *Apps) sharedCredentialApplies(ctx context.Context, namespace string, entry *sharedcred.Entry, cloneURL string) bool {
	credentialAuthority, err := giturl.CanonicalAuthority(entry.Server)
	if err != nil {
		return false
	}
	cloneAuthority, err := giturl.CanonicalAuthority(cloneURL)
	if err != nil || cloneAuthority != credentialAuthority {
		return false
	}
	ns, err := a.Client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil || ns.Labels["app.kubernetes.io/managed-by"] != "kipper" {
		return false
	}
	project := ns.Labels["kipper.run/project"]
	return project != "" && entry.AllowsProject(project)
}

// sameGitAuthority reports whether two clone URLs name the same host in the
// sense the builder binds a credential to.
//
// Anything unparseable is not the same authority, because the question being
// asked is whether a stored credential may travel to a new address, and an
// address that cannot be read is not one it may travel to.
func sameGitAuthority(a, b string) bool {
	if a == b {
		return true
	}
	left, err := giturl.CanonicalAuthority(a)
	if err != nil {
		return false
	}
	right, err := giturl.CanonicalAuthority(b)
	if err != nil {
		return false
	}
	return left == right
}

// usesPerAppCredential reports whether an app's credential is its own rather
// than one of the cluster's shared entries.
//
// The two are bound differently. A shared credential names the host it may be
// used against in the administrator's own list, so moving an app does not put
// it at risk: the builder refuses it anywhere else. A per-app credential
// records the host it was stored for on the object itself, which is
// best-effort and absent on anything written before that existed, so the move
// itself is what has to be refused.
func usesPerAppCredential(appName, credentialsSecret string) bool {
	return secretname.IsGitCredentialOf(appName, credentialsSecret)
}
