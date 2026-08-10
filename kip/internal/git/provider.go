package git

import (
	"net/http"
	"strings"
)

// Status represents a deployment status to report back to the git provider.
type Status string

const (
	StatusPending Status = "pending"
	StatusSuccess Status = "success"
	StatusFailure Status = "failure"
)

// Credentials holds authentication details for accessing a git repository.
type Credentials struct {
	Token  string
	SSHKey string
	CACert string
}

// GitProvider abstracts git hosting platforms for clone URL generation,
// webhook validation, and deploy status reporting.
type GitProvider interface {
	// CloneURL returns the full clone URL with embedded credentials.
	CloneURL(repo string, creds Credentials) string

	// ValidateWebhook checks that an incoming webhook request is authentic.
	ValidateWebhook(r *http.Request, secret string) bool

	// PostDeployStatus reports a deployment status back to the git provider.
	PostDeployStatus(repo string, sha string, status Status, creds Credentials) error

	// Name returns the provider identifier.
	Name() string
}

// Detect returns the appropriate GitProvider for a repository URL.
func Detect(repoURL string) GitProvider {
	if strings.Contains(repoURL, "github.com") {
		return &GitHub{}
	}
	// Default to GitLab for everything else, including self-hosted instances
	return &GitLab{}
}
