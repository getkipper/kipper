package migration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/sharedcred"
)

func migrationTokenSecret(secret string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenSecretName, Namespace: tokenSecretNamespace},
		Data:       map[string][]byte{"secret": []byte(secret), "expires": []byte(time.Now().Add(time.Hour).Format(time.RFC3339))},
	}
}

func sharedCredentials(t *testing.T, entries ...sharedcred.Entry) *corev1.Secret {
	t.Helper()
	data, err := json.Marshal(entries)
	require.NoError(t, err)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: sharedcred.ConfigSecretName, Namespace: sharedcred.Namespace},
		Data:       map[string][]byte{"credentials": data},
	}
}

func askTargetGitCredentials(t *testing.T, h *Handler, secret string, checks []gitCredentialCheck) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{"checks": checks})
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migrate-target/git-credentials", bytes.NewReader(body))
	req.Header.Set("X-Migration-Secret", secret)
	rec := httptest.NewRecorder()
	h.TargetGitCredentialsHandler(rec, req)
	return rec
}

func TestTargetGitCredentials_ReportsWhetherEachProjectMayBuild(t *testing.T) {
	h := &Handler{Client: fake.NewSimpleClientset(
		migrationTokenSecret("s"),
		sharedCredentials(t, sharedcred.Entry{Name: "forge", Server: "https://git.example.com", Token: "tok-not-for-the-source", AllowedProjects: []string{"shop"}}),
	)}

	rec := askTargetGitCredentials(t, h, "s", []gitCredentialCheck{
		{Name: "forge", Project: "shop", Server: "git.example.com"},
		{Name: "forge", Project: "blog", Server: "git.example.com"},
		{Name: "forge", Project: "shop", Server: "other.example.com"},
		{Name: "gone", Project: "shop", Server: "git.example.com"},
	})

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "tok-not-for-the-source", "the answer must never carry the token")
	var resp struct {
		Results []gitCredentialCheck `json:"results"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	states := make([]string, 0, len(resp.Results))
	for _, r := range resp.Results {
		states = append(states, r.State)
	}
	assert.Equal(t, []string{credentialAllowed, credentialNotAllowed, credentialOtherServer, credentialMissing}, states)
}

func TestTargetGitCredentials_RefusesAWrongMigrationSecret(t *testing.T) {
	h := &Handler{Client: fake.NewSimpleClientset(migrationTokenSecret("s"), sharedCredentials(t, sharedcred.Entry{Name: "forge"}))}

	rec := askTargetGitCredentials(t, h, "wrong", []gitCredentialCheck{{Name: "forge", Project: "shop"}})

	assert.Equal(t, http.StatusUnauthorized, rec.Code)
}

func TestTargetGitCredentials_RefusesAnOversizedRequest(t *testing.T) {
	h := &Handler{Client: fake.NewSimpleClientset(migrationTokenSecret("s"))}
	checks := make([]gitCredentialCheck, maxGitCredentialChecks+1)
	for i := range checks {
		checks[i] = gitCredentialCheck{Name: "forge", Project: "shop"}
	}

	rec := askTargetGitCredentials(t, h, "s", checks)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
}

// Migration checks grants before creating projects so the first target build
// can use them. The plan names a command that preserves the project name and
// allows a grant before the project exists.
func TestCheckTargetGitCredentials_NamesTheGrantEachAppNeeds(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Checks []gitCredentialCheck `json:"checks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		states := map[string]string{"shop": credentialNotAllowed, "blog": credentialNotAllowed, "docs": credentialMissing, "wiki": credentialOtherServer, "ok": credentialAllowed}
		for i := range req.Checks {
			req.Checks[i].State = states[req.Checks[i].Project]
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"results": req.Checks})
	}))
	defer target.Close()

	resp := &planResponse{Conflicts: []string{"blog"}, sharedGitCredentials: []gitCredentialUse{
		{App: "shop-prod/web", gitCredentialCheck: gitCredentialCheck{Name: "forge", Project: "shop", Server: "git.example.com"}},
		{App: "blog-prod/site", gitCredentialCheck: gitCredentialCheck{Name: "forge", Project: "blog", Server: "git.example.com"}},
		{App: "docs-prod/site", gitCredentialCheck: gitCredentialCheck{Name: "forge", Project: "docs", Server: "git.example.com"}},
		{App: "wiki-prod/site", gitCredentialCheck: gitCredentialCheck{Name: "forge", Project: "wiki", Server: "git.example.com"}},
		{App: "ok-prod/site", gitCredentialCheck: gitCredentialCheck{Name: "forge", Project: "ok", Server: "git.example.com"}},
	}}
	(&Handler{}).checkTargetGitCredentials(&Token{Endpoint: target.URL}, resp)

	blockers := strings.Join(resp.Blockers, "\n")
	assert.Len(t, resp.Blockers, 4, blockers)
	assert.Contains(t, blockers, "kip credentials allow --project shop --allow-missing-project -- forge",
		"a project that does not exist on the target yet needs the flag")
	assert.Contains(t, blockers, "kip credentials allow --project blog --allow-missing-project -- forge",
		"the flag keeps an existing project's name exact too, where an org prefix would grant another project")
	assert.Contains(t, blockers, `has no shared git credential "forge"`)
	assert.Contains(t, blockers, "git.example.com")
	assert.NotContains(t, blockers, "ok-prod/site")
}

func TestCheckTargetGitCredentials_WarnsWhenTheTargetIsTooOld(t *testing.T) {
	target := httptest.NewServer(http.NotFoundHandler())
	defer target.Close()

	resp := &planResponse{sharedGitCredentials: []gitCredentialUse{
		{App: "shop-prod/web", gitCredentialCheck: gitCredentialCheck{Name: "forge", Project: "shop", Server: "git.example.com"}},
	}}
	(&Handler{}).checkTargetGitCredentials(&Token{Endpoint: target.URL}, resp)

	assert.Empty(t, resp.Blockers)
	require.Len(t, resp.Warnings, 1)
	assert.Contains(t, resp.Warnings[0], "kip credentials allow --project shop --allow-missing-project -- forge")
}

func TestCheckTargetGitCredentials_BlocksWhenTheTargetFails(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer target.Close()

	resp := &planResponse{sharedGitCredentials: []gitCredentialUse{
		{App: "shop-prod/web", gitCredentialCheck: gitCredentialCheck{Name: "forge", Project: "shop", Server: "git.example.com"}},
	}}
	(&Handler{}).checkTargetGitCredentials(&Token{Endpoint: target.URL}, resp)

	assert.Empty(t, resp.Warnings)
	require.Len(t, resp.Blockers, 1)
	assert.Contains(t, resp.Blockers[0], "Could not check the target's shared git credentials")
}

// Many apps share a few credentials, so the check asks once per distinct
// question, and in batches the target accepts, however many apps move.
func TestCheckTargetGitCredentials_StaysWithinTheTargetsLimit(t *testing.T) {
	var asked int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Checks []gitCredentialCheck `json:"checks"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if len(req.Checks) > maxGitCredentialChecks {
			http.Error(w, "too many", http.StatusBadRequest)
			return
		}
		asked += len(req.Checks)
		for i := range req.Checks {
			req.Checks[i].State = credentialNotAllowed
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"results": req.Checks})
	}))
	defer target.Close()

	resp := &planResponse{}
	for i := 0; i < maxGitCredentialChecks+20; i++ {
		project := fmt.Sprintf("p%d", i)
		for _, app := range []string{"web", "api"} {
			resp.sharedGitCredentials = append(resp.sharedGitCredentials, gitCredentialUse{
				App:                project + "-prod/" + app,
				gitCredentialCheck: gitCredentialCheck{Name: "forge", Project: project, Server: "git.example.com"},
			})
		}
	}
	(&Handler{}).checkTargetGitCredentials(&Token{Endpoint: target.URL}, resp)

	assert.Equal(t, maxGitCredentialChecks+20, asked, "each distinct credential and project is asked about once")
	assert.Len(t, resp.Blockers, 2*(maxGitCredentialChecks+20), "every app whose build would be refused is named")
}

// The target checks credentials against the app's clone host, so the request
// must use that host even if the source credential names a different server.
func TestSharedGitCredentialUse_AsksAboutTheHostTheAppClonesFrom(t *testing.T) {
	shared := []sharedcred.Entry{{Name: "forge", Server: "https://other.example.com"}}
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
		Spec:       kipperv1.AppSpec{Git: &kipperv1.AppGitSource{URL: "https://git.example.com/shop/web.git", CredentialsSecret: "forge"}},
	}

	use, ok, err := sharedGitCredentialUse(app, "shop", shared)

	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, gitCredentialCheck{Name: "forge", Project: "shop", Server: "git.example.com"}, use.gitCredentialCheck)
	assert.Equal(t, "shop-prod/web", use.App)

	app.Spec.Git.CredentialsSecret = "web-git-own"
	_, ok, err = sharedGitCredentialUse(app, "shop", shared)
	require.NoError(t, err)
	assert.False(t, ok, "an app's own credential travels with it and needs no check")
}

// The operator pastes the grant into a shell, and the credential's name comes
// from the source cluster, so no name may run anything or split into words.
func TestGrantCommand_IsOneLiteralCommandForAnyName(t *testing.T) {
	for _, name := range []string{"forge$(touch pwned)", "two words", "it's", "-rf", "`id`"} {
		t.Run(name, func(t *testing.T) {
			bin, work := t.TempDir(), t.TempDir()
			script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n"
			require.NoError(t, os.WriteFile(filepath.Join(bin, "kip"), []byte(script), 0o755)) //nolint:gosec // test-local executable

			cmd := exec.Command("sh", "-c", grantCommand(gitCredentialCheck{Name: name, Project: "shop"})) //nolint:gosec // the command under test
			cmd.Dir = work
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
			out, err := cmd.Output()
			require.NoError(t, err)

			args := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
			assert.Equal(t, []string{"credentials", "allow", "--project", "shop", "--allow-missing-project", "--", name}, args)
			_, statErr := os.Stat(filepath.Join(work, "pwned"))
			assert.True(t, os.IsNotExist(statErr), "the name ran a command")
		})
	}
}
