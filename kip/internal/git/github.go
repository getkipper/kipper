package git

import (
	"fmt"
	"net/http"
	"strings"
)

// GitHub implements GitProvider for github.com repositories.
type GitHub struct{}

// CloneURL returns an HTTPS clone URL with token authentication.
func (g *GitHub) CloneURL(repo string, creds Credentials) string {
	if creds.SSHKey != "" {
		return toSSH(repo)
	}
	if creds.Token != "" {
		// https://x-access-token:TOKEN@github.com/org/repo.git
		cleaned := strings.TrimPrefix(repo, "https://")
		return fmt.Sprintf("https://x-access-token:%s@%s", creds.Token, cleaned)
	}
	return repo
}

// ValidateWebhook verifies a GitHub webhook signature.
func (g *GitHub) ValidateWebhook(r *http.Request, secret string) bool {
	// Stage 2: validate X-Hub-Signature-256 header
	return r.Header.Get("X-Hub-Signature-256") != ""
}

// PostDeployStatus reports deployment status to GitHub via the Statuses API.
func (g *GitHub) PostDeployStatus(repo string, sha string, status Status, creds Credentials) error {
	// Stage 2: POST /repos/{owner}/{repo}/statuses/{sha}
	return nil
}

// Name returns the provider identifier.
func (g *GitHub) Name() string {
	return "github"
}
