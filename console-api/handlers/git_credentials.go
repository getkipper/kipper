package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/controller/pkg/giturl"
	"github.com/getkipper/kipper/controller/pkg/sharedcred"
)

// GitCredentials provides handlers for shared git credential management. Shared
// credentials live only in the kipper-system list Secret and are resolved at
// build time; they are never copied into tenant namespaces.
type GitCredentials struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
}

type gitCredentialResponse struct {
	Name            string   `json:"name"`
	Server          string   `json:"server"`
	Username        string   `json:"username"`
	Token           string   `json:"token"`
	AllowedProjects []string `json:"allowedProjects"`
	AppCount        int      `json:"app_count"`
}

// List returns all configured git credentials with masked tokens.
// GET /api/v1/settings/git-credentials
func (gc *GitCredentials) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	entries, _ := sharedcred.Load(ctx, gc.Client)

	resp := make([]gitCredentialResponse, len(entries))
	for i, e := range entries {
		resp[i] = gitCredentialResponse{
			Name:            e.Name,
			Server:          e.Server,
			Username:        e.Username,
			Token:           maskValue(e.Token),
			AllowedProjects: e.AllowedProjects,
			AppCount:        gc.countAppsUsingCredential(ctx, e.Name),
		}
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"credentials": resp})
}

// gitCredentialRequest is what this endpoint may set. It carries the allow-list
// because the published shape always has, and it is read as raw JSON so that a
// field left out can be told from one sent as null: under the old behaviour null
// replaced the list, which is a revocation.
//
// What it may not do is change who may build with an existing credential. A
// caller holding a copy read before somebody else granted a project would
// revoke that project by rotating a token, and there is no server-side way to
// validate the projects named, since the organisation prefix a grant is stored
// under lives in kip's own configuration. So an existing credential takes the
// list it already has, and a request that would change it is refused rather than
// answered with "saved".
type gitCredentialRequest struct {
	Name            string          `json:"name"`
	Server          string          `json:"server"`
	Username        string          `json:"username"`
	Token           string          `json:"token"`
	AllowedProjects json.RawMessage `json:"allowedProjects"`
}

func (r gitCredentialRequest) entry(allowed []string) sharedcred.Entry {
	return sharedcred.Entry{
		Name:            r.Name,
		Server:          r.Server,
		Username:        r.Username,
		Token:           r.Token,
		AllowedProjects: allowed,
	}
}

// requestedProjects is the allow-list a request carries, and whether it carried
// one at all. An absent field asks for no change; null and [] both ask for a
// credential nobody may build with, which is what the old shape did with them.
func (r gitCredentialRequest) requestedProjects() ([]string, bool, error) {
	if len(r.AllowedProjects) == 0 {
		return nil, false, nil
	}
	var projects []string
	if err := json.Unmarshal(r.AllowedProjects, &projects); err != nil {
		return nil, false, err
	}
	if projects == nil {
		projects = []string{}
	}
	return projects, true, nil
}

// sameProjects compares two allow-lists as the sets they are, so a request that
// carries the list it read back, in whatever order or with a name repeated, is
// the no-op it means to be. Who may build is a question about membership, and a
// name listed twice authorises exactly what it authorises once.
func sameProjects(a, b []string) bool {
	in := make(map[string]bool, len(a))
	for _, p := range a {
		in[p] = true
	}
	for _, p := range b {
		if !in[p] {
			return false
		}
	}
	for _, p := range a {
		if !containsProject(b, p) {
			return false
		}
	}
	return true
}

func containsProject(projects []string, want string) bool {
	for _, p := range projects {
		if p == want {
			return true
		}
	}
	return false
}

// uniqueProjects drops a name repeated in a request, so what is stored is the
// set it means and a later request carrying either spelling is recognised as
// the same authorization.
func uniqueProjects(projects []string) []string {
	unique := make([]string, 0, len(projects))
	for _, p := range projects {
		// A blank name matches no project and no command can take it off again,
		// so it is dropped rather than stored as a grant that does nothing.
		if p != "" && !containsProject(unique, p) {
			unique = append(unique, p)
		}
	}
	return unique
}

// errChangesTheAllowList is a request that would rewrite an existing
// credential's grants, which this endpoint does not do.
var errChangesTheAllowList = errors.New("request changes the allow-list")

// Add creates or updates a shared git credential.
// POST /api/v1/settings/git-credentials
func (gc *GitCredentials) Add(w http.ResponseWriter, r *http.Request) {
	var req gitCredentialRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Server == "" || req.Token == "" {
		respondError(w, http.StatusBadRequest, "server and token are required")
		return
	}

	wanted, carried, err := req.requestedProjects()
	if err != nil {
		respondError(w, http.StatusBadRequest, "allowedProjects must be a list of project names")
		return
	}

	// Normalize and validate the server so it host-binds cleanly at build time.
	authority, err := giturl.CanonicalAuthority(req.Server)
	if err != nil {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid server: %v", err))
		return
	}
	req.Server = authority

	if req.Username == "" {
		req.Username = "oauth2"
	}

	if req.Name == "" {
		req.Name = sanitizeRegistryName(req.Server)
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	if err := sharedcred.Update(ctx, gc.Client, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		if live := sharedcred.Find(entries, req.Name); live != nil {
			if carried && !sameProjects(wanted, live.AllowedProjects) {
				return nil, errChangesTheAllowList
			}
			// A request that carries the list it read is asking for no change,
			// and one that would change it has already been refused, so what is
			// stored is the same set either way. The carried one is written
			// because it can still say something the stored one does not: an
			// empty list against a credential nobody has decided about is a
			// decision, and keeping the absent list would leave the next
			// upgrade free to grant it from the apps that reference it.
			stored := live.AllowedProjects
			if carried {
				stored = uniqueProjects(wanted)
			}
			*live = req.entry(stored)
			return entries, nil
		}
		// Nothing exists to overwrite, so the first list may be set here, which
		// is how the published shape has always created a granted credential.
		// A credential nobody has granted allows nobody, and recording that is
		// what tells an upgrade this one has been decided already.
		if !carried {
			wanted = []string{}
		}
		return append(entries, req.entry(uniqueProjects(wanted))), nil
	}); err != nil {
		if errors.Is(err, errChangesTheAllowList) {
			respondError(w, http.StatusBadRequest,
				"this endpoint cannot change who may build with an existing credential. Grant a project with 'kip credentials allow <name> --project <project>', which checks that the project exists, or take one away with 'kip credentials revoke'. Send the allow-list unchanged, or leave it out, to edit the rest")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to save git credentials")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "saved", "name": req.Name})
}

// Remove deletes a shared git credential.
// DELETE /api/v1/settings/git-credentials/{name}
func (gc *GitCredentials) Remove(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Check if any apps reference this credential
	appCount := gc.countAppsUsingCredential(ctx, name)
	if appCount > 0 {
		respondError(w, http.StatusConflict, fmt.Sprintf("credential %q is used by %d app(s). Remove the credential from those apps first", name, appCount))
		return
	}

	if err := sharedcred.Update(ctx, gc.Client, func(entries []sharedcred.Entry) ([]sharedcred.Entry, error) {
		kept := make([]sharedcred.Entry, 0, len(entries))
		for _, e := range entries {
			if e.Name != name {
				kept = append(kept, e)
			}
		}
		if len(kept) == len(entries) {
			return nil, &sharedcred.UnknownCredentialError{Name: name}
		}
		return kept, nil
	}); err != nil {
		var unknown *sharedcred.UnknownCredentialError
		if errors.As(err, &unknown) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("git credential %q not found", name))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to save git credentials")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// Health probes all configured git credentials and returns validity and expiry info.
// GET /api/v1/settings/git-credentials/health
func (gc *GitCredentials) Health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	entries, _ := sharedcred.Load(ctx, gc.Client)

	type result struct {
		name   string
		health tokenHealth
	}

	ch := make(chan result, len(entries))
	var wg sync.WaitGroup

	for _, entry := range entries {
		wg.Add(1)
		go func(e sharedcred.Entry) {
			defer wg.Done()
			probeCtx, probeCancel := context.WithTimeout(ctx, 5*time.Second)
			defer probeCancel()
			ch <- result{name: e.Name, health: probeGitCredential(probeCtx, e.Server, e.Token)}
		}(entry)
	}

	wg.Wait()
	close(ch)

	health := make(map[string]tokenHealth, len(entries))
	for r := range ch {
		health[r.name] = r.health
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"health": health})
}

// Reveal returns the plaintext token for a single git credential after
// re-verifying the caller's password against Dex. The admin-role check is
// enforced upstream by middleware.RequireRole; this handler adds the
// knowledge-factor gate on top.
// POST /api/v1/settings/git-credentials/{name}/reveal
func (gc *GitCredentials) Reveal(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if name == "" {
		respondError(w, http.StatusBadRequest, "credential name required")
		return
	}

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

	if err := VerifyUserPassword(ctx, gc.Client, claims.Email, req.Password); err != nil {
		if errors.Is(err, ErrInvalidPassword) {
			log.Printf("reveal git-credential=%s by user=%s: invalid password", name, claims.Email)
			respondError(w, http.StatusUnauthorized, "invalid password")
			return
		}
		log.Printf("reveal git-credential=%s by user=%s: %v", name, claims.Email, err)
		respondError(w, http.StatusInternalServerError, "password verification failed")
		return
	}

	entries, _ := sharedcred.Load(ctx, gc.Client)
	if e := sharedcred.Find(entries, name); e != nil {
		log.Printf("reveal git-credential=%s by user=%s: ok", name, claims.Email)
		respondJSON(w, http.StatusOK, map[string]string{"token": e.Token})
		return
	}

	respondError(w, http.StatusNotFound, fmt.Sprintf("git credential %q not found", name))
}

func (gc *GitCredentials) countAppsUsingCredential(ctx context.Context, credentialName string) int {
	var apps kipperv1.AppList
	if err := gc.CRClient.List(ctx, &apps); err != nil {
		return 0
	}

	count := 0
	for _, app := range apps.Items {
		if app.Spec.Git != nil && app.Spec.Git.CredentialsSecret == credentialName {
			count++
		}
	}
	return count
}
