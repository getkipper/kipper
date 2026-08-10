package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const (
	clientID     = "kipper-cli"
	callbackPort = "18741"
	callbackPath = "/callback"

	// tokenExchangeTimeout bounds one whole token operation: the endpoint
	// request plus the discovery and JWKS fetches verification needs. The
	// refresh path runs under the exclusive store lock, so an unbounded
	// stall there would block every kip invocation on this machine, login
	// and logout included.
	tokenExchangeTimeout = 30 * time.Second
)

// authHTTPClient bounds every individual token-flow request; the per-call
// context bounds the operation end to end.
var authHTTPClient = &http.Client{Timeout: tokenExchangeTimeout}

// Credentials holds the stored auth state for a cluster.
type Credentials struct {
	IDToken      string    `json:"id_token"`
	RefreshToken string    `json:"refresh_token"`
	Email        string    `json:"email"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// storeVersion marks stores whose every entry was minted by the verifying
// flow. Loading any other version yields an empty store, so the fast path in
// Token only ever returns tokens this code verified when storing them; the
// only migration is logging in again.
const storeVersion = 2

// Store persists auth credentials per cluster.
type Store struct {
	Version  int                     `json:"version"`
	Clusters map[string]*Credentials `json:"clusters"`
}

func storePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".kip", "auth.json"), nil
}

// Load reads the auth store from disk. A store written by an older kip (or
// anything else) is treated as empty: its tokens were never verified by this
// code, so they must be re-obtained through a login.
func Load() (*Store, error) {
	path, err := storePath()
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Store{Version: storeVersion, Clusters: make(map[string]*Credentials)}, nil
		}
		return nil, err
	}

	var store Store
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("parsing auth store %s: %w", path, err)
	}
	if store.Version != storeVersion || store.Clusters == nil {
		return &Store{Version: storeVersion, Clusters: make(map[string]*Credentials)}, nil
	}
	return &store, nil
}

// Mutate applies fn to the on-disk store as one locked transaction: the
// store is re-read under the exclusive lock, changed, and saved before the
// lock is released. Every writer must go through this (or hold the same
// lock, as the refresh path in Token does) — an unlocked load-modify-save
// would silently overwrite a refresh token another process rotated in
// between.
func Mutate(fn func(*Store)) error {
	unlock, err := lockStore()
	if err != nil {
		return err
	}
	defer unlock()

	store, err := Load()
	if err != nil {
		return err
	}
	fn(store)
	return store.Save()
}

// Save writes the auth store to disk atomically and durably: the content is
// synced to a temp file, renamed over the store, and the directory is synced
// so the rename itself survives a crash. A rotated refresh token often
// exists nowhere else, so both halves matter — atomicity for concurrent
// readers, durability for the token.
func (s *Store) Save() error {
	path, err := storePath()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	s.Version = storeVersion
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".auth-*.json")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}

	// Flush the directory entry the rename created, so the store survives a
	// power cut rather than reappearing empty with a refresh token that was
	// rotated away. Windows has no equivalent — see syncDir.
	if err := syncDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("syncing auth store directory: %w", err)
	}
	return nil
}

// Token returns a valid ID token for the cluster, refreshing if expired.
//
// clusterID is the stable opaque key the Store uses to look up stored
// credentials (typically the cluster's Domain field). dexHost is the
// hostname Dex serves on, resolved from the Cluster (admin overrides
// take precedence over the convention).
//
// The refresh path serializes against other kip processes: Dex rotates the
// refresh token on use, so two racing refreshes would leave one process
// holding a revoked token. Under the lock the store is re-read, because the
// race being guarded against is another process having refreshed already.
func (s *Store) Token(clusterID, dexHost string) (string, error) {
	creds, ok := s.Clusters[clusterID]
	if !ok {
		return "", fmt.Errorf("not authenticated. Run: kip auth login")
	}

	if time.Now().Before(creds.ExpiresAt) {
		return creds.IDToken, nil
	}

	if creds.RefreshToken == "" {
		return "", fmt.Errorf("session expired. Run: kip auth login")
	}

	unlock, err := lockStore()
	if err != nil {
		return "", fmt.Errorf("locking auth store for refresh: %w", err)
	}
	defer unlock()

	// The fresh read is authoritative: falling back to the pre-lock snapshot
	// would resurrect credentials another process deleted (logout) or
	// stripped while this one waited for the lock.
	fresh, err := Load()
	if err != nil {
		return "", fmt.Errorf("re-reading auth store: %w", err)
	}
	current, ok := fresh.Clusters[clusterID]
	if !ok {
		delete(s.Clusters, clusterID)
		return "", fmt.Errorf("not authenticated. Run: kip auth login")
	}
	if time.Now().Before(current.ExpiresAt) {
		s.Clusters[clusterID] = current
		return current.IDToken, nil
	}
	if current.RefreshToken == "" {
		return "", fmt.Errorf("session expired. Run: kip auth login")
	}
	creds = current

	issuer := fmt.Sprintf("https://%s/dex", dexHost)
	ctx, cancel := context.WithTimeout(context.Background(), tokenExchangeTimeout)
	defer cancel()
	newCreds, err := refreshToken(ctx, issuer, creds.RefreshToken)
	if err != nil {
		return "", fmt.Errorf("session expired. Run: kip auth login")
	}

	fresh.Clusters[clusterID] = newCreds
	s.Clusters[clusterID] = newCreds
	if err := fresh.Save(); err != nil {
		// The rotated refresh token exists only in this process now; the
		// token still works, but the next invocation will fail its refresh,
		// so the operator needs to see why.
		fmt.Fprintf(os.Stderr, "  ⚠  could not persist refreshed credentials (next refresh will need a new login): %v\n", err)
	}
	return newCreds.IDToken, nil
}

// Credential returns the stored credentials for a cluster after Token has
// ensured they are current; nil when none exist. The ExpiresAt inside is
// what the exec-credential protocol reports to kubectl, so client-go stops
// caching the token exactly when the API server stops accepting it.
func (s *Store) Credential(clusterID string) *Credentials {
	return s.Clusters[clusterID]
}

// randomToken returns a URL-safe random string used for the PKCE
// verifier, state, and nonce values (32 bytes of entropy, RFC 7636 §4.1
// compliant as a verifier).
func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// loginAttempt carries the random values that bind one OAuth flow
// together: the PKCE verifier for the token exchange, the state for
// the loopback callback, and the nonce for the returned ID token.
type loginAttempt struct {
	verifier string
	state    string
	nonce    string
}

func newLoginAttempt() (*loginAttempt, error) {
	verifier, err := randomToken()
	if err != nil {
		return nil, err
	}
	state, err := randomToken()
	if err != nil {
		return nil, err
	}
	nonce, err := randomToken()
	if err != nil {
		return nil, err
	}
	return &loginAttempt{verifier: verifier, state: state, nonce: nonce}, nil
}

// challenge derives the S256 code challenge from the verifier
// (RFC 7636 §4.2).
func (l *loginAttempt) challenge() string {
	sum := sha256.Sum256([]byte(l.verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// authURL builds the authorization request carrying the PKCE
// challenge, state, and nonce.
func (l *loginAttempt) authURL(issuer, redirectURI string) string {
	// `groups` is requested so connectors that supply group membership emit
	// it — group-based RBAC on the Kubernetes API server keys off it. Dex's
	// static password users carry no groups, and the claim is then simply
	// absent.
	return fmt.Sprintf("%s/auth?client_id=%s&redirect_uri=%s&response_type=code&scope=%s&code_challenge=%s&code_challenge_method=S256&state=%s&nonce=%s",
		issuer,
		url.QueryEscape(clientID),
		url.QueryEscape(redirectURI),
		url.QueryEscape("openid email profile groups offline_access"),
		url.QueryEscape(l.challenge()),
		url.QueryEscape(l.state),
		url.QueryEscape(l.nonce),
	)
}

// callbackHandler validates the loopback callback against this login
// attempt and delivers the authorization code. A state mismatch is
// rejected before the code is trusted (login CSRF / response swapping).
func (l *loginAttempt) callbackHandler(codeCh chan<- string, errCh chan<- error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != l.state {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprint(w, callbackPage("Authentication Failed", "The response did not match this login attempt.", true))
			errCh <- fmt.Errorf("authentication failed: state mismatch: the callback did not originate from this login attempt")
			return
		}
		code := r.URL.Query().Get("code")
		if code == "" {
			errMsg := r.URL.Query().Get("error_description")
			if errMsg == "" {
				errMsg = r.URL.Query().Get("error")
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = fmt.Fprint(w, callbackPage("Authentication Failed", errMsg, true))
			errCh <- fmt.Errorf("authentication failed: %s", errMsg)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = fmt.Fprint(w, callbackPage("Authenticated", "You can close this window and return to the terminal.", false))
		codeCh <- code
	}
}

// Login performs the browser-based OAuth flow and returns credentials.
//
// kipper-cli is a public client (no secret), so the flow carries the
// full native-app protections: PKCE S256 binds the token exchange to
// this process, state binds the callback to this login attempt, and
// nonce binds the ID token to this request.
//
// dexHost is the hostname Dex serves on, resolved from the Cluster
// (admin overrides take precedence over the convention).
func Login(dexHost string) (*Credentials, error) {
	issuer := fmt.Sprintf("https://%s/dex", dexHost)
	redirectURI := fmt.Sprintf("http://localhost:%s%s", callbackPort, callbackPath)

	attempt, err := newLoginAttempt()
	if err != nil {
		return nil, err
	}

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)

	mux := http.NewServeMux()
	mux.HandleFunc(callbackPath, attempt.callbackHandler(codeCh, errCh))

	server := &http.Server{
		Addr:              "localhost:" + callbackPort,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if err := server.ListenAndServe(); err != http.ErrServerClosed {
			errCh <- fmt.Errorf("starting callback server: %w", err)
		}
	}()

	authURL := attempt.authURL(issuer, redirectURI)

	fmt.Printf("  Opening browser for authentication...\n")
	fmt.Printf("  If the browser doesn't open, visit:\n  %s\n\n", authURL)

	openBrowser(authURL)

	var code string
	select {
	case code = <-codeCh:
	case err := <-errCh:
		_ = server.Shutdown(context.Background())
		return nil, err
	case <-time.After(5 * time.Minute):
		_ = server.Shutdown(context.Background())
		return nil, fmt.Errorf("authentication timed out")
	}

	_ = server.Shutdown(context.Background())

	ctx, cancel := context.WithTimeout(context.Background(), tokenExchangeTimeout)
	defer cancel()
	creds, err := exchangeCode(ctx, issuer, code, redirectURI, attempt.verifier, attempt.nonce)
	if err != nil {
		return nil, err
	}

	return creds, nil
}

func exchangeCode(ctx context.Context, issuer, code, redirectURI, codeVerifier, expectedNonce string) (*Credentials, error) {
	tokenURL := issuer + "/token"
	data := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {clientID},
		"code_verifier": {codeVerifier},
	}

	resp, err := postForm(ctx, tokenURL, data)
	if err != nil {
		return nil, fmt.Errorf("exchanging auth code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token exchange failed (status %d)", resp.StatusCode)
	}

	var tokenResp struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, fmt.Errorf("parsing token response: %w", err)
	}

	verified, err := verifyIDToken(ctx, issuer, tokenResp.IDToken, expectedNonce)
	if err != nil {
		return nil, fmt.Errorf("token exchange failed: %w", err)
	}

	return &Credentials{
		IDToken:      tokenResp.IDToken,
		RefreshToken: tokenResp.RefreshToken,
		Email:        verified.Email,
		ExpiresAt:    verified.ExpiresAt,
	}, nil
}

func refreshToken(ctx context.Context, issuer, refreshTok string) (*Credentials, error) {
	tokenURL := issuer + "/token"
	data := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshTok},
		"client_id":     {clientID},
	}

	resp, err := postForm(ctx, tokenURL, data)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("refresh failed (status %d)", resp.StatusCode)
	}

	var tokenResp struct {
		IDToken      string `json:"id_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, err
	}

	verified, err := verifyIDToken(ctx, issuer, tokenResp.IDToken, "")
	if err != nil {
		return nil, fmt.Errorf("refresh failed: %w", err)
	}

	return &Credentials{
		IDToken:      tokenResp.IDToken,
		RefreshToken: tokenResp.RefreshToken,
		Email:        verified.Email,
		ExpiresAt:    verified.ExpiresAt,
	}, nil
}

// postForm submits one token-endpoint request bounded by ctx and the
// client timeout.
func postForm(ctx context.Context, tokenURL string, data url.Values) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return authHTTPClient.Do(req)
}

func openBrowser(url string) {
	// Try the platform openers in turn; the printed URL is always the
	// reliable fallback when none is present (headless, minimal Linux).
	for _, opener := range []string{"open", "xdg-open"} {
		if _, err := exec.LookPath(opener); err == nil {
			//nolint:gosec // url comes from the OAuth flow we constructed in this process; not user-tainted input
			_ = exec.Command(opener, url).Start()
			return
		}
	}
}

func callbackPage(title, message string, isError bool) string {
	iconColor := "#0284c7"
	iconPath := `<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M5 13l4 4L19 7" />`
	if isError {
		iconColor = "#dc2626"
		iconPath = `<path stroke-linecap="round" stroke-linejoin="round" stroke-width="2" d="M6 18L18 6M6 6l12 12" />`
	}

	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>%s, Kipper</title>
  <link rel="preconnect" href="https://fonts.googleapis.com" />
  <link href="https://fonts.googleapis.com/css2?family=DM+Sans:wght@400;500;600&display=swap" rel="stylesheet" />
  <style>
    * { margin: 0; padding: 0; box-sizing: border-box; }
    body {
      font-family: 'DM Sans', system-ui, sans-serif;
      background: #f8fafc;
      color: #0f172a;
      min-height: 100vh;
      display: flex;
      align-items: center;
      justify-content: center;
    }
    .card {
      text-align: center;
      max-width: 400px;
      padding: 48px 32px;
      animation: fadeIn 0.4s ease-out;
    }
    .icon {
      width: 64px;
      height: 64px;
      border-radius: 50%%;
      background: %s12;
      display: flex;
      align-items: center;
      justify-content: center;
      margin: 0 auto 24px;
    }
    .icon svg {
      width: 32px;
      height: 32px;
      stroke: %s;
      fill: none;
    }
    h1 {
      font-size: 24px;
      font-weight: 600;
      letter-spacing: -0.025em;
      margin-bottom: 8px;
    }
    p {
      font-size: 15px;
      color: #64748b;
      line-height: 1.5;
    }
    .brand {
      margin-top: 48px;
      display: flex;
      justify-content: center;
    }
    .brand svg {
      width: 28px;
      height: auto;
    }
    @keyframes fadeIn {
      from { opacity: 0; transform: translateY(8px); }
      to { opacity: 1; transform: translateY(0); }
    }
  </style>
</head>
<body>
  <div class="card">
    <div class="icon">
      <svg viewBox="0 0 24 24">%s</svg>
    </div>
    <h1>%s</h1>
    <p>%s</p>
    <div class="brand" aria-label="Kipper">
      <svg viewBox="0 0 576.71 676.67" xmlns="http://www.w3.org/2000/svg">
        <path fill="#BDE4FF" d="M106.36 445.6l106.58 0 87.72 -107.26 62.99 -76.32 -88.13 -106.77 -62.59 75.83 87.72 107.26 -105.76 0 -88.54 107.26z"/>
        <path fill="#0EA5E9" d="M106.36 231.07l-106.36 0 88.54 107.26 -88.54 107.26 106.36 0 88.54 -107.26 -88.54 -107.26z"/>
        <path fill="#0EA5E9" d="M296.34 0l-74.66 90.45 141.97 171.56 -62.99 76.32 62.99 76.32 -141.97 171.57 74.66 90.45c93.65,-112.68 187.1,-225.35 280.37,-338.34 -93.27,-112.98 -186.72,-225.66 -280.37,-338.34z"/>
        <circle fill="#fff" cx="440.9" cy="343.39" r="34.88"/>
        <path fill="#fff" d="M106.36 231.08l106.58 0 87.72 107.26 -105.76 0 -88.54 -107.26zm194.3 107.26l-87.72 107.26 62.59 75.82 88.13 -106.77 -62.99 -76.32z"/>
        <path fill="#7DC3F5" d="M286.59 534.83l-1.29 0 0 -1.56 1.29 1.56zm0 -392.99l-1.29 0 0 1.56 1.29 -1.56zm-180.99 89.23l107.33 0 62.59 -75.83 -107.33 0 -47.4 57.43 -15.19 18.4zm0 214.52l107.33 0 62.59 75.82 -107.33 0 -47.4 -57.43 -15.19 -18.39z"/>
      </svg>
    </div>
  </div>
</body>
</html>`, title, iconColor, iconColor, iconPath, title, message)
}
