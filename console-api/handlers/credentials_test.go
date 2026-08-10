package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/console-api/internal/gitcred"
)

// TestMain swaps the credential-probe client for one that allows the loopback
// httptest servers these tests use. Production's probeClient refuses non-public
// addresses (the SSRF guard, tested in controller/pkg/netguard); the test
// client keeps the no-redirect policy those tests assert.
func TestMain(m *testing.M) {
	probeClient = &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	os.Exit(m.Run())
}

// === stripSchemeAndPath ===

func TestStripSchemeAndPath(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"registry.git.example.com", "registry.git.example.com"},
		{"https://registry.git.example.com", "registry.git.example.com"},
		{"registry.git.example.com:5005", "registry.git.example.com"},
		{"https://registry.git.example.com:5005", "registry.git.example.com"},
		{"https://registry.git.example.com:5005/v2/", "registry.git.example.com"},
		{"https://registry.git.example.com/v2/", "registry.git.example.com"},
		{"gitlab.com", "gitlab.com"},
		{"https://gitlab.com", "gitlab.com"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := stripSchemeAndPath(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

// === gitLabBaseURL ===

func TestGitLabBaseURL(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"registry.git.example.com", "https://git.example.com"},
		{"https://registry.git.example.com:5005", "https://git.example.com"},
		{"git.example.com", "https://git.example.com"},
		{"gitlab.com", "https://gitlab.com"},
		{"https://gitlab.com", "https://gitlab.com"},
		{"registry.gitlab.com", "https://gitlab.com"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := gitLabBaseURL(tt.input)
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

// === gitProbeTarget ===

func TestGitProbeTarget(t *testing.T) {
	// The provider is chosen only from the configured authority, never the token.
	tests := []struct {
		name       string
		server     string
		wantGitHub bool
		wantGitlab string
	}{
		{"github cloud by host", "github.com", true, ""},
		{"github cloud www subdomain", "https://www.github.com/org/repo.git", true, ""},
		{"github api subdomain", "api.github.com", true, ""},
		{"github cloud with a trailing-dot fqdn", "https://github.com./org/repo.git", true, ""},
		{"gitlab cloud", "gitlab.com", false, "https://gitlab.com"},
		{"self-hosted gitlab keeps port", "https://git.example.com:8443/team/repo.git", false, "https://git.example.com:8443"},
		{"host with github.com only in path is not github", "https://evil.example/github.com/x", false, "https://evil.example"},
		{"github.com on a non-default port is not github cloud", "https://github.com:8443/x", false, "https://github.com:8443"},
		{"github subdomain on a non-default port is not github cloud", "https://www.github.com:8443/x", false, "https://www.github.com:8443"},
		// A host that only ends in "github.com" without the dot is not a subdomain.
		{"lookalike host is not github", "https://evilgithub.com/x", false, "https://evilgithub.com"},
		// A GitHub Enterprise host is probed at its own authority, so a
		// GitHub-shaped token is never sent to public api.github.com.
		{"github enterprise host is not github cloud", "https://github.corp.example", false, "https://github.corp.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			isGitHub, gitlabBase, err := gitProbeTarget(tt.server)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if isGitHub != tt.wantGitHub {
				t.Errorf("isGitHub = %v, want %v", isGitHub, tt.wantGitHub)
			}
			if gitlabBase != tt.wantGitlab {
				t.Errorf("gitlabBase = %q, want %q", gitlabBase, tt.wantGitlab)
			}
		})
	}
}

// === daysUntil ===

func TestDaysUntil(t *testing.T) {
	t.Run("past date returns 0", func(t *testing.T) {
		if got := daysUntil("2020-01-01"); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
	t.Run("invalid format returns 0", func(t *testing.T) {
		if got := daysUntil("not-a-date"); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
	t.Run("empty string returns 0", func(t *testing.T) {
		if got := daysUntil(""); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
	t.Run("future date returns positive days", func(t *testing.T) {
		future := time.Now().AddDate(0, 0, 30).Format("2006-01-02")
		got := daysUntil(future)
		if got < 29 || got > 30 {
			t.Errorf("got %d, want ~30", got)
		}
	})
	t.Run("far future date returns many days", func(t *testing.T) {
		future := time.Now().AddDate(1, 0, 0).Format("2006-01-02")
		got := daysUntil(future)
		if got < 364 {
			t.Errorf("got %d, expected at least 364 days for a year out", got)
		}
	})
}

// === probeGitLabPAT ===

// A credential-bearing probe must not follow a 3xx to another authority — the
// token must never reach the redirect target.
func TestProbeGitLabPAT_DoesNotForwardTokenOnRedirect(t *testing.T) {
	captured := ""
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Header.Get("PRIVATE-TOKEN")
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/api/v4/personal_access_tokens/self", http.StatusFound)
	}))
	defer redirector.Close()

	result := probeGitLabPAT(context.Background(), redirector.URL, "glpat-secret")

	if captured != "" {
		t.Errorf("the token must not be forwarded to a redirect target, got %q", captured)
	}
	if result.Valid {
		t.Error("a redirect response must not be treated as a valid token")
	}
}

func TestProbeGitLabPAT_ActiveTokenWithExpiry(t *testing.T) {
	expiresAt := time.Now().AddDate(0, 0, 45).Format("2006-01-02")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"active":     true,
			"revoked":    false,
			"expires_at": expiresAt,
		})
	}))
	defer srv.Close()

	result := probeGitLabPAT(context.Background(), srv.URL, "glpat-test")

	if !result.Valid {
		t.Errorf("expected valid=true, got false; error: %s", result.Error)
	}
	if result.ExpiresAt != expiresAt {
		t.Errorf("expected expires_at %q, got %q", expiresAt, result.ExpiresAt)
	}
	if result.DaysRemaining < 44 || result.DaysRemaining > 45 {
		t.Errorf("expected ~45 days remaining, got %d", result.DaysRemaining)
	}
}

func TestProbeGitLabPAT_ActiveTokenNoExpiry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"active":  true,
			"revoked": false,
		})
	}))
	defer srv.Close()

	result := probeGitLabPAT(context.Background(), srv.URL, "glpat-never-expires")

	if !result.Valid {
		t.Errorf("expected valid=true for active token with no expiry, got false")
	}
	if result.ExpiresAt != "" {
		t.Errorf("expected empty ExpiresAt, got %q", result.ExpiresAt)
	}
}

func TestProbeGitLabPAT_InactiveToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"active":     false,
			"revoked":    false,
			"expires_at": "2020-01-01",
		})
	}))
	defer srv.Close()

	result := probeGitLabPAT(context.Background(), srv.URL, "glpat-expired")

	if result.Valid {
		t.Error("expected valid=false for inactive token")
	}
	if result.Error == "" {
		t.Error("expected non-empty error for inactive token")
	}
}

func TestProbeGitLabPAT_RevokedToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"active":  true,
			"revoked": true,
		})
	}))
	defer srv.Close()

	result := probeGitLabPAT(context.Background(), srv.URL, "glpat-revoked")

	if result.Valid {
		t.Error("expected valid=false for revoked token")
	}
}

func TestProbeGitLabPAT_Unauthorized(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	result := probeGitLabPAT(context.Background(), srv.URL, "glpat-wrong")

	if result.Valid {
		t.Error("expected valid=false for 401 response")
	}
	if result.Error == "" {
		t.Error("expected non-empty error for 401 response")
	}
}

func TestProbeGitLabPAT_ParseErrorOn200(t *testing.T) {
	// A 200 with unparseable body still means the token is valid (we can authenticate).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("not json"))
	}))
	defer srv.Close()

	result := probeGitLabPAT(context.Background(), srv.URL, "glpat-ok")

	if !result.Valid {
		t.Error("expected valid=true when server returns 200, even with unparseable body")
	}
}

// === probeRegistryBearer ===

func TestProbeRegistryBearer_ValidCreds(t *testing.T) {
	realmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, ok := r.BasicAuth()
		if !ok || password != "correct" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"issued-token"}`))
	}))
	defer realmSrv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	wwwAuth := `Bearer realm="` + realmSrv.URL + `",service="registry"`
	if !probeRegistryBearer(context.Background(), client, wwwAuth, "user", "correct") {
		t.Error("expected true for valid credentials at realm")
	}
}

func TestProbeRegistryBearer_InvalidCreds(t *testing.T) {
	realmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer realmSrv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	wwwAuth := `Bearer realm="` + realmSrv.URL + `",service="registry"`
	if probeRegistryBearer(context.Background(), client, wwwAuth, "user", "wrong") {
		t.Error("expected false for invalid credentials at realm")
	}
}

func TestProbeRegistryBearer_NoRealm(t *testing.T) {
	client := &http.Client{Timeout: 5 * time.Second}
	wwwAuth := `Bearer service="registry",scope="repository:pull"`
	if probeRegistryBearer(context.Background(), client, wwwAuth, "user", "pass") {
		t.Error("expected false when WWW-Authenticate has no realm")
	}
}

// Emulates GitLab Container Registry: /jwt/auth refuses to issue a token
// without service=container_registry, so the probe must forward it.
func TestProbeRegistryBearer_ForwardsServiceAndScope(t *testing.T) {
	realmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("service") != "container_registry" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if q.Get("scope") != "repository:foo/bar:pull" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, password, ok := r.BasicAuth()
		if !ok || password != "valid" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"issued"}`))
	}))
	defer realmSrv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	wwwAuth := `Bearer realm="` + realmSrv.URL + `",service="container_registry",scope="repository:foo/bar:pull"`
	if !probeRegistryBearer(context.Background(), client, wwwAuth, "user", "valid") {
		t.Error("expected true; probe must forward service and scope so realm can issue a token")
	}
}

// Some Bearer challenges only carry a realm. The probe must still work and
// must not append empty service/scope query params.
func TestProbeRegistryBearer_RealmOnly(t *testing.T) {
	realmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("did not expect query params on realm; got %q", r.URL.RawQuery)
		}
		_, password, ok := r.BasicAuth()
		if !ok || password != "valid" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer realmSrv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	wwwAuth := `Bearer realm="` + realmSrv.URL + `"`
	if !probeRegistryBearer(context.Background(), client, wwwAuth, "user", "valid") {
		t.Error("expected true for realm-only challenge with valid creds")
	}
}

// Challenge parameter order varies between registries. Parser must tolerate
// any order without losing fields.
func TestProbeRegistryBearer_ParamOrderTolerant(t *testing.T) {
	realmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("service") != "container_registry" || q.Get("scope") != "repository:foo:pull" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer realmSrv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	// service, then scope, then realm — realm last
	wwwAuth := `Bearer service="container_registry", scope="repository:foo:pull", realm="` + realmSrv.URL + `"`
	if !probeRegistryBearer(context.Background(), client, wwwAuth, "user", "valid") {
		t.Error("expected probe to handle challenge with realm last")
	}
}

// If the realm URL itself carries query parameters, forwarded service/scope
// must be appended without clobbering them.
func TestProbeRegistryBearer_PreservesRealmQuery(t *testing.T) {
	realmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("v") != "2" || q.Get("service") != "container_registry" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer realmSrv.Close()

	client := &http.Client{Timeout: 5 * time.Second}
	wwwAuth := `Bearer realm="` + realmSrv.URL + `?v=2",service="container_registry"`
	if !probeRegistryBearer(context.Background(), client, wwwAuth, "user", "valid") {
		t.Error("expected pre-existing realm query params to be preserved alongside forwarded service")
	}
}

// End-to-end through probeRegistry: a registry that issues a GitLab-style
// challenge must report Valid=true once the realm is called with service forwarded.
func TestProbeRegistry_GitLabStyleBearerChallenge(t *testing.T) {
	realmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("service") != "container_registry" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, password, ok := r.BasicAuth()
		if !ok || password != "valid" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"issued"}`))
	}))
	defer realmSrv.Close()

	registrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+realmSrv.URL+`",service="container_registry",scope="repository:any:pull"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registrySrv.Close()

	result := probeRegistry(context.Background(), registryEntry{
		Name: "gitlab", Server: registrySrv.URL, Username: "user", Password: "valid",
	})

	if !result.Valid {
		t.Errorf("expected valid=true for GitLab-style Bearer challenge, got false; error: %s", result.Error)
	}
}

// === parseBearerChallenge ===

func TestParseBearerChallenge(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected map[string]string
	}{
		{
			name:  "all three params",
			input: `Bearer realm="https://example.com/auth",service="container_registry",scope="repository:foo/bar:pull"`,
			expected: map[string]string{
				"realm":   "https://example.com/auth",
				"service": "container_registry",
				"scope":   "repository:foo/bar:pull",
			},
		},
		{
			name:  "realm only",
			input: `Bearer realm="https://example.com/auth"`,
			expected: map[string]string{
				"realm": "https://example.com/auth",
			},
		},
		{
			name:  "stray whitespace between params",
			input: `Bearer  realm="https://example.com/auth" ,  service="reg" ,  scope="repository:x:pull"`,
			expected: map[string]string{
				"realm":   "https://example.com/auth",
				"service": "reg",
				"scope":   "repository:x:pull",
			},
		},
		{
			name:  "service before realm",
			input: `Bearer service="reg",realm="https://example.com/auth"`,
			expected: map[string]string{
				"realm":   "https://example.com/auth",
				"service": "reg",
			},
		},
		{
			name:     "empty",
			input:    `Bearer `,
			expected: map[string]string{},
		},
		{
			name:  "whitespace around equals",
			input: `Bearer realm = "https://example.com/auth", service = "container_registry"`,
			expected: map[string]string{
				"realm":   "https://example.com/auth",
				"service": "container_registry",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseBearerChallenge(tt.input)
			if len(got) != len(tt.expected) {
				t.Errorf("got %d params, want %d: %v", len(got), len(tt.expected), got)
			}
			for k, v := range tt.expected {
				if got[k] != v {
					t.Errorf("key %q: got %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// === probeRegistry ===

func TestProbeRegistry_BasicAuthValid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, ok := r.BasicAuth()
		if !ok || password != "valid" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	result := probeRegistry(context.Background(), registryEntry{
		Name: "reg", Server: srv.URL, Username: "user", Password: "valid",
	})

	if !result.Valid {
		t.Errorf("expected valid=true, got false; error: %s", result.Error)
	}
}

func TestProbeRegistry_BasicAuthInvalid(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	result := probeRegistry(context.Background(), registryEntry{
		Name: "reg", Server: srv.URL, Username: "user", Password: "wrong",
	})

	if result.Valid {
		t.Error("expected valid=false for wrong credentials")
	}
	if result.Error == "" {
		t.Error("expected non-empty error field")
	}
}

func TestProbeRegistry_BearerExchangeValid(t *testing.T) {
	realmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, ok := r.BasicAuth()
		if !ok || password != "valid" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"token":"issued"}`))
	}))
	defer realmSrv.Close()

	registrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+realmSrv.URL+`",service="reg"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registrySrv.Close()

	result := probeRegistry(context.Background(), registryEntry{
		Name: "reg", Server: registrySrv.URL, Username: "user", Password: "valid",
	})

	if !result.Valid {
		t.Errorf("expected valid=true after Bearer exchange, got false; error: %s", result.Error)
	}
}

func TestProbeRegistry_BearerExchangeInvalid(t *testing.T) {
	realmSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer realmSrv.Close()

	registrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="`+realmSrv.URL+`",service="reg"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registrySrv.Close()

	result := probeRegistry(context.Background(), registryEntry{
		Name: "reg", Server: registrySrv.URL, Username: "user", Password: "wrong",
	})

	if result.Valid {
		t.Error("expected valid=false when Bearer exchange fails")
	}
}

func TestProbeRegistry_ValidWithGitLabExpiry(t *testing.T) {
	// A glpat- password triggers an expiry lookup against the provider API.
	// We use a single server that handles both the /v2/ probe and the PAT API.
	expiresAt := time.Now().AddDate(0, 0, 45).Format("2006-01-02")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/api/v4/personal_access_tokens/self":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"active":     true,
				"revoked":    false,
				"expires_at": expiresAt,
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// Server hint is the same URL, so gitLabBaseURL will derive https://127.0.0.1
	// (stripping the port). This means the expiry probe goes to a different host than
	// our test server, so ExpiresAt will be empty — but Valid should still be true from /v2/.
	result := probeRegistry(context.Background(), registryEntry{
		Name: "reg", Server: srv.URL, Username: "user", Password: "glpat-testtoken",
	})

	if !result.Valid {
		t.Errorf("expected valid=true from /v2/ probe, got false; error: %s", result.Error)
	}
}

// === Registry.Health handler ===

func TestRegistryHealth_EmptyResponse(t *testing.T) {
	client := fake.NewClientset()
	handler := &Registry{Client: client}

	r := chi.NewRouter()
	r.Get("/settings/registries/health", handler.Health)

	req := httptest.NewRequest("GET", "/settings/registries/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var resp struct {
		Health map[string]tokenHealth `json:"health"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Health) != 0 {
		t.Errorf("expected empty health map, got %v", resp.Health)
	}
}

func TestRegistryHealth_ValidCredentials(t *testing.T) {
	registrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, password, ok := r.BasicAuth()
		if ok && password == "good-token" {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusUnauthorized)
		}
	}))
	defer registrySrv.Close()

	entriesJSON, _ := json.Marshal([]registryEntry{{ //nolint:gosec // test fixture: serialising the registry entry shape is the whole point of the test
		Name:     "my-registry",
		Server:   registrySrv.URL,
		Username: "ci",
		Password: "good-token",
	}})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryConfigName, Namespace: registryNamespace},
		Data:       map[string][]byte{"registries": entriesJSON},
	}

	client := fake.NewClientset(secret)
	handler := &Registry{Client: client}

	r := chi.NewRouter()
	r.Get("/settings/registries/health", handler.Health)

	req := httptest.NewRequest("GET", "/settings/registries/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Health map[string]tokenHealth `json:"health"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	h, ok := resp.Health["my-registry"]
	if !ok {
		t.Fatalf("expected health entry for 'my-registry', got keys: %v", resp.Health)
	}
	if !h.Valid {
		t.Errorf("expected valid=true, got false; error: %s", h.Error)
	}
}

func TestRegistryHealth_InvalidCredentials(t *testing.T) {
	registrySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer registrySrv.Close()

	entriesJSON, _ := json.Marshal([]registryEntry{{ //nolint:gosec // test fixture: serialising the registry entry shape is the whole point of the test
		Name:     "expired-registry",
		Server:   registrySrv.URL,
		Username: "ci",
		Password: "expired-token",
	}})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryConfigName, Namespace: registryNamespace},
		Data:       map[string][]byte{"registries": entriesJSON},
	}

	client := fake.NewClientset(secret)
	handler := &Registry{Client: client}

	r := chi.NewRouter()
	r.Get("/settings/registries/health", handler.Health)

	req := httptest.NewRequest("GET", "/settings/registries/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp struct {
		Health map[string]tokenHealth `json:"health"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	h := resp.Health["expired-registry"]
	if h.Valid {
		t.Error("expected valid=false for expired credentials")
	}
	if h.Error == "" {
		t.Error("expected non-empty error message")
	}
}

func TestRegistryHealth_MultipleRegistries(t *testing.T) {
	goodSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer goodSrv.Close()

	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer badSrv.Close()

	entriesJSON, _ := json.Marshal([]registryEntry{ //nolint:gosec // test fixture: serialising the registry entry shape is the whole point of the test
		{Name: "good", Server: goodSrv.URL, Username: "u", Password: "p"},
		{Name: "bad", Server: badSrv.URL, Username: "u", Password: "p"},
	})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: registryConfigName, Namespace: registryNamespace},
		Data:       map[string][]byte{"registries": entriesJSON},
	}

	client := fake.NewClientset(secret)
	handler := &Registry{Client: client}

	r := chi.NewRouter()
	r.Get("/settings/registries/health", handler.Health)

	req := httptest.NewRequest("GET", "/settings/registries/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var resp struct {
		Health map[string]tokenHealth `json:"health"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(resp.Health) != 2 {
		t.Errorf("expected 2 health entries, got %d", len(resp.Health))
	}
	if !resp.Health["good"].Valid {
		t.Errorf("expected 'good' registry to be valid")
	}
	if resp.Health["bad"].Valid {
		t.Errorf("expected 'bad' registry to be invalid")
	}
}

// === GitCredentials.Health handler ===

func TestGitCredentialsHealth_EmptyResponse(t *testing.T) {
	client := fake.NewClientset()
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	r := chi.NewRouter()
	r.Get("/settings/git-credentials/health", handler.Health)

	req := httptest.NewRequest("GET", "/settings/git-credentials/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rec.Code)
	}

	var resp struct {
		Health map[string]tokenHealth `json:"health"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(resp.Health) != 0 {
		t.Errorf("expected empty health map, got %v", resp.Health)
	}
}

func TestGitCredentialsHealth_ReturnsEntryPerCredential(t *testing.T) {
	// probeGitCredential derives a base URL from the server field by stripping the port,
	// so an httptest server URL won't be reachable. The probe will fail (valid=false),
	// but the handler should still include a health entry for each configured credential.
	entriesJSON, _ := json.Marshal([]gitcred.Entry{
		{Name: "acme-gitlab", Server: "git.example.com", Token: "glpat-test"},
	})
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gitcred.ConfigSecretName,
			Namespace: gitcred.Namespace,
		},
		Data: map[string][]byte{"credentials": entriesJSON},
	}

	client := fake.NewClientset(secret)
	handler := &GitCredentials{Client: client, CRClient: testCRClient()}

	r := chi.NewRouter()
	r.Get("/settings/git-credentials/health", handler.Health)

	req := httptest.NewRequest("GET", "/settings/git-credentials/health", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d; body: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Health map[string]tokenHealth `json:"health"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if _, ok := resp.Health["acme-gitlab"]; !ok {
		t.Errorf("expected health entry for 'acme-gitlab', got keys: %v", resp.Health)
	}
}
