package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/giturl"
	"github.com/getkipper/kipper/controller/pkg/sharedcred"
)

const (
	credentialAllowed     = "allowed"
	credentialNotAllowed  = "not_allowed"
	credentialMissing     = "missing"
	credentialOtherServer = "other_server"
)

// maxGitCredentialChecks bounds one request to the target's credential check.
const maxGitCredentialChecks = 500

// gitCredentialCheck asks whether the target's shared git credential Name lets
// Project build from Server, a canonical git authority. State carries the
// target's answer.
type gitCredentialCheck struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Server  string `json:"server,omitempty"`
	State   string `json:"state,omitempty"`
}

// gitCredentialUse is a moving git app that builds with a shared credential.
type gitCredentialUse struct {
	App string
	gitCredentialCheck
}

// sharedGitCredentialUse returns the check a git app needs on the target when
// it builds with one of this cluster's shared credentials, or false when it
// uses its own credential, which travels with the app. The server is the
// authority the app clones from, which is what the target's build compares the
// credential against.
func sharedGitCredentialUse(app *kipperv1.App, project string, shared []sharedcred.Entry) (gitCredentialUse, bool, error) {
	if app.Spec.Git == nil || app.Spec.Git.CredentialsSecret == "" {
		return gitCredentialUse{}, false, nil
	}
	entry := sharedcred.Find(shared, app.Spec.Git.CredentialsSecret)
	if entry == nil {
		return gitCredentialUse{}, false, nil
	}
	server, err := giturl.CanonicalAuthority(app.Spec.Git.URL)
	if err != nil {
		return gitCredentialUse{}, false, fmt.Errorf("reading the git URL of %s/%s: %w", app.Namespace, app.Name, err)
	}
	return gitCredentialUse{
		App:                app.Namespace + "/" + app.Name,
		gitCredentialCheck: gitCredentialCheck{Name: entry.Name, Project: project, Server: server},
	}, true, nil
}

// TargetGitCredentialsHandler tells a source cluster whether each of its git
// apps' shared credentials exists here and lets the app's project build. It
// returns states without exposing tokens. By supplying credential and project
// names, a migration token holder can learn whether credentials exist, match
// a supplied server and allow a given project.
// POST /api/v1/migrate-target/git-credentials
func (h *Handler) TargetGitCredentialsHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := ValidateToken(ctx, h.Client, r.Header.Get("X-Migration-Secret")); err != nil {
		respondError(w, http.StatusUnauthorized, "invalid migration secret")
		return
	}

	var req struct {
		Checks []gitCredentialCheck `json:"checks"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if len(req.Checks) > maxGitCredentialChecks {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("at most %d checks per request", maxGitCredentialChecks))
		return
	}

	shared, err := sharedcred.Load(ctx, h.Client)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "reading shared git credentials failed")
		return
	}
	results := make([]gitCredentialCheck, 0, len(req.Checks))
	for _, check := range req.Checks {
		check.State = credentialState(shared, check)
		results = append(results, check)
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"results": results})
}

// credentialState checks that the credential exists and allows the project.
// When a server is supplied, it must match the credential's canonical authority.
func credentialState(shared []sharedcred.Entry, check gitCredentialCheck) string {
	entry := sharedcred.Find(shared, check.Name)
	if entry == nil {
		return credentialMissing
	}
	if check.Server != "" {
		if server, err := giturl.CanonicalAuthority(entry.Server); err != nil || server != check.Server {
			return credentialOtherServer
		}
	}
	if !entry.AllowsProject(check.Project) {
		return credentialNotAllowed
	}
	return credentialAllowed
}

// checkTargetGitCredentials asks the target about every shared git credential
// the moving apps build with, and turns each one that would refuse the first
// build into a blocker naming its fix. A target too old to have the check gets
// a warning listing the grants to check manually. Any other failure blocks.
func (h *Handler) checkTargetGitCredentials(token *Token, resp *planResponse) {
	if len(resp.sharedGitCredentials) == 0 {
		return
	}
	var checks []gitCredentialCheck
	for _, use := range resp.sharedGitCredentials {
		if !slices.Contains(checks, use.gitCredentialCheck) {
			checks = append(checks, use.gitCredentialCheck)
		}
	}

	states := make(map[gitCredentialCheck]string, len(checks))
	for batch := range slices.Chunk(checks, maxGitCredentialChecks) {
		answered, unsupported, err := h.askTargetGitCredentials(token, batch)
		if unsupported {
			var grants []string
			for _, check := range checks {
				grants = append(grants, grantCommand(check))
			}
			resp.Warnings = append(resp.Warnings, fmt.Sprintf(
				"The target's Kipper is too old to report on shared git credentials, so a git app's first build there may be refused. On the target, check that each credential exists and run: %s",
				strings.Join(grants, "; ")))
			return
		}
		if err != nil {
			resp.Blockers = append(resp.Blockers, fmt.Sprintf("Could not check the target's shared git credentials: %v", err))
			return
		}
		for i, check := range batch {
			states[check] = answered[i]
		}
	}

	for _, use := range resp.sharedGitCredentials {
		switch states[use.gitCredentialCheck] {
		case credentialAllowed:
		case credentialMissing:
			resp.Blockers = append(resp.Blockers, fmt.Sprintf(
				"App %s builds with shared git credential %q. The target has no shared git credential %q. Add it for %s in the target's credential settings, then run '%s' there.",
				use.App, use.Name, use.Name, use.Server, grantCommand(use.gitCredentialCheck)))
		case credentialOtherServer:
			resp.Blockers = append(resp.Blockers, fmt.Sprintf(
				"App %s clones from %s with the shared git credential %q, and the target's credential of that name is for a different server. Point it at %s in the target's credential settings.",
				use.App, use.Server, use.Name, use.Server))
		default:
			resp.Blockers = append(resp.Blockers, fmt.Sprintf(
				"App %s builds with the shared git credential %q, which the target does not allow for project %s. Run '%s' on the target.",
				use.App, use.Name, use.Project, grantCommand(use.gitCredentialCheck)))
		}
	}
}

// askTargetGitCredentials sends one batch of checks and returns the target's
// state for each, in order. unsupported reports a target without the check.
func (h *Handler) askTargetGitCredentials(token *Token, checks []gitCredentialCheck) (states []string, unsupported bool, err error) {
	status, body, err := h.callTargetRaw(token, http.MethodPost, "/api/v1/migrate-target/git-credentials",
		map[string]interface{}{"checks": checks})
	if err != nil {
		return nil, false, err
	}
	if status == http.StatusNotFound {
		return nil, true, nil
	}
	if status != http.StatusOK {
		return nil, false, fmt.Errorf("target returned %d", status)
	}
	var answer struct {
		Results []gitCredentialCheck `json:"results"`
	}
	if err := json.Unmarshal(body, &answer); err != nil || len(answer.Results) != len(checks) {
		return nil, false, fmt.Errorf("unreadable answer from the target")
	}
	states = make([]string, len(checks))
	for i, result := range answer.Results {
		states[i] = result.State
	}
	return states, false, nil
}

// grantCommand is the kip command that allows a credential for a project on
// the target. The flag keeps the project's name exactly as the migration
// creates it, without the org prefix kip would otherwise add, and lets the
// grant come before the project exists. The operator pastes it into a shell,
// and a credential name is whatever the source's administrator chose, so the
// arguments are quoted and a name that looks like a flag follows "--".
func grantCommand(check gitCredentialCheck) string {
	return fmt.Sprintf("kip credentials allow --project %s --allow-missing-project -- %s",
		shellQuote(check.Project), shellQuote(check.Name))
}

// plainShellWord matches a word every POSIX shell reads literally.
var plainShellWord = regexp.MustCompile(`^[A-Za-z0-9._/@:+=-]+$`)

// shellQuote renders s as one literal shell word.
func shellQuote(s string) string {
	if plainShellWord.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
