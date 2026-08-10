package git

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDetectGitHub(t *testing.T) {
	p := Detect("https://github.com/acme/api")
	assert.Equal(t, "github", p.Name())
}

func TestDetectGitLabCloud(t *testing.T) {
	p := Detect("https://gitlab.com/acme/api")
	assert.Equal(t, "gitlab", p.Name())
}

func TestDetectGitLabSelfHosted(t *testing.T) {
	p := Detect("https://gitlab.example.com/acme/api")
	assert.Equal(t, "gitlab", p.Name())
}

func TestGitHubCloneURLWithToken(t *testing.T) {
	gh := &GitHub{}
	url := gh.CloneURL("https://github.com/acme/api", Credentials{Token: "ghp_abc123"})
	assert.Equal(t, "https://x-access-token:ghp_abc123@github.com/acme/api", url)
}

func TestGitHubCloneURLPublic(t *testing.T) {
	gh := &GitHub{}
	url := gh.CloneURL("https://github.com/acme/api", Credentials{})
	assert.Equal(t, "https://github.com/acme/api", url)
}

func TestGitLabCloneURLWithToken(t *testing.T) {
	gl := &GitLab{}
	url := gl.CloneURL("https://gitlab.com/acme/api", Credentials{Token: "glpat-xyz"})
	assert.Equal(t, "https://oauth2:glpat-xyz@gitlab.com/acme/api", url)
}

func TestGitLabCloneURLSelfHosted(t *testing.T) {
	gl := &GitLab{}
	url := gl.CloneURL("https://git.example.com/org/repo", Credentials{Token: "tok"})
	assert.Equal(t, "https://oauth2:tok@git.example.com/org/repo", url)
}

func TestToSSH(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://github.com/acme/api", "git@github.com:acme/api.git"},
		{"https://gitlab.com/org/repo.git", "git@gitlab.com:org/repo.git"},
		{"https://git.example.com/acme/kipper", "git@git.example.com:acme/kipper.git"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.want, toSSH(tt.input))
		})
	}
}
