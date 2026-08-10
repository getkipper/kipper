package git

import (
	"fmt"
	"net/http"
	"strings"
)

// GitLab implements GitProvider for gitlab.com and self-hosted GitLab instances.
type GitLab struct{}

// CloneURL returns an HTTPS clone URL with token authentication.
func (g *GitLab) CloneURL(repo string, creds Credentials) string {
	if creds.SSHKey != "" {
		return toSSH(repo)
	}
	if creds.Token != "" {
		// https://oauth2:TOKEN@gitlab.com/org/repo.git
		cleaned := strings.TrimPrefix(repo, "https://")
		return fmt.Sprintf("https://oauth2:%s@%s", creds.Token, cleaned)
	}
	return repo
}

// ValidateWebhook verifies a GitLab webhook token.
func (g *GitLab) ValidateWebhook(r *http.Request, secret string) bool {
	// Stage 2: validate X-Gitlab-Token header
	return r.Header.Get("X-Gitlab-Token") == secret
}

// PostDeployStatus reports deployment status to GitLab via the Commit Status API.
func (g *GitLab) PostDeployStatus(repo string, sha string, status Status, creds Credentials) error {
	// Stage 2: POST /api/v4/projects/:id/statuses/:sha
	return nil
}

// Name returns the provider identifier.
func (g *GitLab) Name() string {
	return "gitlab"
}
