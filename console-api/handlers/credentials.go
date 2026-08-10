package handlers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/getkipper/kipper/console-api/internal/giturl"
	"github.com/getkipper/kipper/controller/pkg/netguard"
)

// tokenHealth is the result of probing a credential for validity and expiry.
type tokenHealth struct {
	Valid         bool   `json:"valid"`
	ExpiresAt     string `json:"expires_at,omitempty"`
	DaysRemaining int    `json:"days_remaining,omitempty"`
	Error         string `json:"error,omitempty"`
}

// probeRegistry tests whether registry credentials are valid against the OCI
// /v2/ endpoint, and attempts to retrieve token expiry for known providers.
// Works with any OCI-compliant registry (GitLab, GHCR, Docker Hub, Quay, etc.).
func probeRegistry(ctx context.Context, entry registryEntry) tokenHealth {
	server := entry.Server
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}

	httpClient := probeClient

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/v2/", nil)
	if err != nil {
		return tokenHealth{Error: "failed to build request"}
	}
	auth := base64.StdEncoding.EncodeToString([]byte(entry.Username + ":" + entry.Password))
	req.Header.Set("Authorization", "Basic "+auth)

	resp, err := httpClient.Do(req)
	if err != nil {
		return tokenHealth{Valid: false, Error: "registry unreachable"}
	}
	defer func() { _ = resp.Body.Close() }()

	var valid bool
	switch resp.StatusCode {
	case http.StatusOK:
		valid = true
	case http.StatusUnauthorized:
		wwwAuth := resp.Header.Get("WWW-Authenticate")
		if strings.HasPrefix(wwwAuth, "Bearer ") {
			// Bearer token registries (GHCR, Docker Hub) need a token exchange
			// to verify credentials. Try the realm endpoint with basic auth.
			valid = probeRegistryBearer(ctx, httpClient, wwwAuth, entry.Username, entry.Password)
		}
		// Basic auth challenge with no valid creds → valid stays false
	}

	result := tokenHealth{Valid: valid}
	if !valid {
		result.Error = "authentication failed"
	}

	// Best-effort: try to get expiry info from the provider API.
	expiry := probeTokenExpiry(ctx, entry.Password, entry.Server)
	if expiry.ExpiresAt != "" {
		result.ExpiresAt = expiry.ExpiresAt
		result.DaysRemaining = expiry.DaysRemaining
	}

	return result
}

// probeRegistryBearer attempts a Bearer token exchange against the auth
// realm URL in the WWW-Authenticate header to verify credentials. Forwards
// the service and scope parameters from the challenge to the realm as query
// params, per the OCI distribution token auth spec — registries like GitLab
// will not issue a token without service=container_registry.
func probeRegistryBearer(ctx context.Context, client *http.Client, wwwAuth, username, password string) bool {
	params := parseBearerChallenge(wwwAuth)
	realm := params["realm"]
	if realm == "" {
		return false
	}

	u, err := url.Parse(realm)
	if err != nil {
		return false
	}
	q := u.Query()
	if svc := params["service"]; svc != "" {
		q.Set("service", svc)
	}
	if scope := params["scope"]; scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	req.SetBasicAuth(username, password)

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// parseBearerChallenge parses key=value parameters from a WWW-Authenticate
// Bearer challenge into a map. Tolerates parameters in any order, stray
// whitespace, and commas inside quoted values (multi-scope challenges).
func parseBearerChallenge(wwwAuth string) map[string]string {
	params := make(map[string]string)
	body := strings.TrimSpace(strings.TrimPrefix(wwwAuth, "Bearer "))
	for len(body) > 0 {
		body = strings.TrimLeft(body, ", \t")
		eq := strings.Index(body, "=")
		if eq <= 0 {
			break
		}
		key := strings.TrimSpace(body[:eq])
		body = strings.TrimLeft(body[eq+1:], " \t")
		var val string
		if strings.HasPrefix(body, `"`) {
			body = body[1:]
			end := strings.Index(body, `"`)
			if end < 0 {
				val = body
				body = ""
			} else {
				val = body[:end]
				body = body[end+1:]
			}
		} else {
			end := strings.Index(body, ",")
			if end < 0 {
				val = strings.TrimSpace(body)
				body = ""
			} else {
				val = strings.TrimSpace(body[:end])
				body = body[end:]
			}
		}
		if key != "" {
			params[key] = val
		}
	}
	return params
}

// probeGitCredential tests whether a git credential token is valid by probing
// the provider's API. Works with GitHub and GitLab (cloud and self-hosted).
func probeGitCredential(ctx context.Context, server, token string) tokenHealth {
	isGitHub, gitlabBase, err := gitProbeTarget(server)
	if err != nil {
		return tokenHealth{Valid: false, Error: "invalid git server"}
	}
	if isGitHub {
		return probeGitHub(ctx, token)
	}
	return probeGitLabPAT(ctx, gitlabBase, token)
}

// gitProbeTarget decides which provider API to probe for a git server, and the
// GitLab base URL to use. The provider is chosen only from the configured
// server's canonical authority — exactly github.com is GitHub cloud, anything
// else is probed at its own authority (preserving a non-default port). The token
// is never used to pick the provider: a GitHub-shaped token is not proof of
// authority (self-hosted GitHub issues the same shapes), and inferring GitHub
// from it would send the token to api.github.com instead of the configured host.
func gitProbeTarget(server string) (isGitHub bool, gitlabBase string, err error) {
	authority, err := giturl.CanonicalAuthority(server)
	if err != nil {
		return false, "", err
	}
	// github.com and its subdomains (www.github.com, api.github.com) are GitHub
	// cloud, whose token API is api.github.com. Only github owns *.github.com, so
	// the suffix match cannot be spoofed. A non-default port leaves it in the
	// authority (github.com:8443), which is a self-hosted install, not cloud.
	if authority == "github.com" || strings.HasSuffix(authority, ".github.com") {
		return true, "", nil
	}
	return false, "https://" + authority, nil
}

// probeClient never follows redirects, so a credential-bearing health probe
// cannot be bounced (via a 3xx) to a different authority than the one derived
// from the configured server, and it refuses to connect to a non-public
// address, so a configured server that resolves to an internal host cannot
// turn the probe into an SSRF. The provider API endpoints are fixed, so a
// redirect is never a legitimate part of a probe.
var probeClient = netguard.Client(5 * time.Second)

func probeGitHub(ctx context.Context, token string) tokenHealth {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return tokenHealth{Error: "failed to build request"}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "kipper")

	resp, err := probeClient.Do(req)
	if err != nil {
		return tokenHealth{Valid: false, Error: "GitHub API unreachable"}
	}
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return tokenHealth{Valid: false, Error: "authentication failed"}
	}
	// GitHub does not expose PAT expiry via API.
	return tokenHealth{Valid: true}
}

// probeTokenExpiry attempts to get expiry info for a token from its provider
// API, identified by token prefix. Returns empty result on any failure.
func probeTokenExpiry(ctx context.Context, token, serverHint string) tokenHealth {
	switch {
	case strings.HasPrefix(token, "glpat-"):
		base := gitLabBaseURL(serverHint)
		return probeGitLabPAT(ctx, base, token)
	default:
		return tokenHealth{}
	}
}

// probeGitLabPAT calls the GitLab personal_access_tokens/self API and returns
// validity and expiry info.
func probeGitLabPAT(ctx context.Context, baseURL, token string) tokenHealth {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v4/personal_access_tokens/self", nil)
	if err != nil {
		return tokenHealth{Valid: false, Error: "failed to build request"}
	}
	req.Header.Set("PRIVATE-TOKEN", token)

	resp, err := probeClient.Do(req)
	if err != nil {
		return tokenHealth{Valid: false, Error: "GitLab API unreachable"}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return tokenHealth{Valid: false, Error: "authentication failed"}
	}

	var pat struct {
		Active    bool   `json:"active"`
		Revoked   bool   `json:"revoked"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&pat); err != nil {
		return tokenHealth{Valid: true} // 200 means valid even if parse fails
	}

	result := tokenHealth{Valid: pat.Active && !pat.Revoked}
	if !result.Valid {
		result.Error = "token inactive or revoked"
	}
	if pat.ExpiresAt != "" {
		result.ExpiresAt = pat.ExpiresAt
		result.DaysRemaining = daysUntil(pat.ExpiresAt)
	}
	return result
}

// gitLabBaseURL derives the GitLab instance base URL from a registry server
// hostname. Strips the "registry." subdomain prefix and port number.
func gitLabBaseURL(hint string) string {
	host := stripSchemeAndPath(hint)
	host = strings.TrimPrefix(host, "registry.")
	return "https://" + host
}

// stripSchemeAndPath removes the scheme, path, and port from a URL or hostname.
func stripSchemeAndPath(s string) string {
	// Strip scheme
	if i := strings.Index(s, "://"); i != -1 {
		s = s[i+3:]
	}
	// Strip path
	if i := strings.Index(s, "/"); i != -1 {
		s = s[:i]
	}
	// Strip port
	if i := strings.LastIndex(s, ":"); i != -1 {
		s = s[:i]
	}
	return s
}

// daysUntil returns the number of whole days until an ISO date (YYYY-MM-DD).
// Returns 0 if the date is in the past or cannot be parsed.
func daysUntil(dateStr string) int {
	t, err := time.Parse("2006-01-02", dateStr)
	if err != nil {
		return 0
	}
	days := int(time.Until(t).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}
