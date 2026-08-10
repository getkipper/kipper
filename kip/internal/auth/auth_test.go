package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// fakeIssuer is a minimal OIDC provider: discovery, JWKS, and a token
// endpoint, with its own RSA signing key. Tokens verify only when this
// issuer signed them, so the tests exercise the full verification path —
// signature and algorithm included — not just claim comparison.
type fakeIssuer struct {
	srv     *httptest.Server
	key     *rsa.PrivateKey
	idToken string
	gotForm map[string]string
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating signing key: %v", err)
	}
	fi := &fakeIssuer{key: key}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"issuer": %q, "jwks_uri": %q, "token_endpoint": %q}`,
			fi.srv.URL, fi.srv.URL+"/keys", fi.srv.URL+"/token")
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		keySet := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &fi.key.PublicKey, KeyID: "test-key", Algorithm: "RS256", Use: "sig"},
		}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(keySet)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parsing form: %v", err)
		}
		fi.gotForm = map[string]string{}
		for k := range r.PostForm {
			fi.gotForm[k] = r.PostForm.Get(k)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id_token": %q, "refresh_token": "rotated-refresh", "expires_in": 3600}`, fi.idToken)
	})

	fi.srv = httptest.NewServer(mux)
	t.Cleanup(fi.srv.Close)
	return fi
}

type tokenClaims struct {
	Issuer   string `json:"iss"`
	Audience any    `json:"aud"`
	Exp      int64  `json:"exp"`
	Email    string `json:"email"`
	Nonce    string `json:"nonce"`
	Azp      string `json:"azp,omitempty"`
}

func (fi *fakeIssuer) validClaims() tokenClaims {
	return tokenClaims{
		Issuer:   fi.srv.URL,
		Audience: clientID,
		Exp:      time.Now().Add(time.Hour).Truncate(time.Second).Unix(),
		Email:    "admin@kipper.test",
		Nonce:    "expected-nonce",
	}
}

// sign produces a compact JWT over claims with the given key and algorithm;
// the issuer's own key with RS256 yields a valid token.
func signToken(t *testing.T, key any, alg jose.SignatureAlgorithm, kid string, claims tokenClaims) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshalling claims: %v", err)
	}
	opts := (&jose.SignerOptions{}).WithType("JWT")
	if kid != "" {
		opts = opts.WithHeader("kid", kid)
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: key}, opts)
	if err != nil {
		t.Fatalf("creating signer: %v", err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("signing token: %v", err)
	}
	compact, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serializing token: %v", err)
	}
	return compact
}

func (fi *fakeIssuer) signed(t *testing.T, claims tokenClaims) string {
	return signToken(t, fi.key, jose.RS256, "test-key", claims)
}

func TestRandomTokenUniqueAndURLSafe(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := randomToken()
		if err != nil {
			t.Fatalf("randomToken: %v", err)
		}
		// 32 bytes → 43 chars, within RFC 7636's 43–128 verifier bounds.
		if len(tok) != 43 {
			t.Fatalf("token length = %d, want 43", len(tok))
		}
		if strings.ContainsAny(tok, "+/=") {
			t.Fatalf("token %q contains non-URL-safe characters", tok)
		}
		if seen[tok] {
			t.Fatal("randomToken returned a duplicate")
		}
		seen[tok] = true
	}
}

func TestChallengeMatchesRFC7636Vector(t *testing.T) {
	// Appendix B of RFC 7636.
	attempt := &loginAttempt{verifier: "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"}
	if got := attempt.challenge(); got != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Errorf("challenge = %q, want RFC 7636 test vector", got)
	}
}

func TestAuthURLCarriesPKCEStateAndNonce(t *testing.T) {
	attempt := &loginAttempt{verifier: "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk", state: "the-state", nonce: "the-nonce"}
	u, err := url.Parse(attempt.authURL("https://dex.test/dex", "http://localhost:18741/callback"))
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}
	q := u.Query()
	want := map[string]string{
		"client_id":             clientID,
		"redirect_uri":          "http://localhost:18741/callback",
		"response_type":         "code",
		"scope":                 "openid email profile groups offline_access",
		"code_challenge":        "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM",
		"code_challenge_method": "S256",
		"state":                 "the-state",
		"nonce":                 "the-nonce",
	}
	for key, wantVal := range want {
		if got := q.Get(key); got != wantVal {
			t.Errorf("auth URL %s = %q, want %q", key, got, wantVal)
		}
	}
}

func TestCallbackHandlerStateBinding(t *testing.T) {
	tests := []struct {
		name     string
		query    string
		wantCode string
		wantErr  bool
	}{
		{"matching state delivers code", "?state=the-state&code=auth-code", "auth-code", false},
		{"wrong state rejected", "?state=attacker-state&code=auth-code", "", true},
		{"missing state rejected", "?code=auth-code", "", true},
		{"missing code rejected", "?state=the-state&error=access_denied", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt := &loginAttempt{state: "the-state"}
			codeCh := make(chan string, 1)
			errCh := make(chan error, 1)

			r := httptest.NewRequest(http.MethodGet, "/callback"+tt.query, nil)
			attempt.callbackHandler(codeCh, errCh)(httptest.NewRecorder(), r)

			select {
			case code := <-codeCh:
				if tt.wantErr {
					t.Fatalf("expected an error, got code %q", code)
				}
				if code != tt.wantCode {
					t.Errorf("code = %q, want %q", code, tt.wantCode)
				}
			case err := <-errCh:
				if !tt.wantErr {
					t.Fatalf("expected code %q, got error %v", tt.wantCode, err)
				}
			default:
				t.Fatal("handler delivered neither code nor error")
			}
		})
	}
}

func TestVerifyIDToken(t *testing.T) {
	fi := newFakeIssuer(t)

	tests := []struct {
		name   string
		mutate func(*tokenClaims)
		nonce  string
		wantOK bool
	}{
		{"valid", func(*tokenClaims) {}, "expected-nonce", true},
		{"refresh skips nonce", func(c *tokenClaims) { c.Nonce = "stale" }, "", true},
		{"multiple audiences with our azp", func(c *tokenClaims) {
			c.Audience = []string{"other", clientID}
			c.Azp = clientID
		}, "expected-nonce", true},
		{"multiple audiences without azp", func(c *tokenClaims) { c.Audience = []string{"other", clientID} }, "expected-nonce", false},
		{"foreign azp", func(c *tokenClaims) { c.Azp = "kipper-console" }, "expected-nonce", false},
		{"foreign azp with multiple audiences", func(c *tokenClaims) {
			c.Audience = []string{"kipper-console", clientID}
			c.Azp = "kipper-console"
		}, "expected-nonce", false},
		{"our azp on a single audience", func(c *tokenClaims) { c.Azp = clientID }, "expected-nonce", true},
		{"wrong issuer", func(c *tokenClaims) { c.Issuer = "https://evil.test/dex" }, "expected-nonce", false},
		{"foreign audience", func(c *tokenClaims) { c.Audience = "kipper-console" }, "expected-nonce", false},
		{"missing audience", func(c *tokenClaims) { c.Audience = nil }, "expected-nonce", false},
		{"expired", func(c *tokenClaims) { c.Exp = time.Now().Add(-time.Hour).Unix() }, "expected-nonce", false},
		{"nonce mismatch", func(c *tokenClaims) { c.Nonce = "someone-elses-nonce" }, "expected-nonce", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := fi.validClaims()
			tt.mutate(&claims)
			_, err := verifyIDToken(context.Background(), fi.srv.URL, fi.signed(t, claims), tt.nonce)
			if (err == nil) != tt.wantOK {
				t.Errorf("verifyIDToken err = %v, want ok = %v", err, tt.wantOK)
			}
		})
	}
}

func TestVerifyIDTokenRejectsBadSignatures(t *testing.T) {
	fi := newFakeIssuer(t)
	claims := fi.validClaims()

	// A token signed by a key the issuer never published must fail even
	// though every claim is right — this is the check TLS cannot stand in
	// for.
	strangerKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating stranger key: %v", err)
	}
	forged := signToken(t, strangerKey, jose.RS256, "test-key", claims)
	if _, err := verifyIDToken(context.Background(), fi.srv.URL, forged, "expected-nonce"); err == nil {
		t.Fatal("expected a token signed by an unknown key to be rejected")
	}

	// A symmetric-signature token must be rejected by the algorithm pin,
	// never verified against a public key treated as an HMAC secret.
	hmacToken := signToken(t, []byte("0123456789abcdef0123456789abcdef"), jose.HS256, "", claims)
	if _, err := verifyIDToken(context.Background(), fi.srv.URL, hmacToken, "expected-nonce"); err == nil {
		t.Fatal("expected an HS256 token to be rejected")
	}

	// A tampered payload invalidates the signature.
	parts := strings.Split(fi.signed(t, claims), ".")
	tampered := parts[0] + "." + parts[1][:len(parts[1])-2] + "aa" + "." + parts[2]
	if _, err := verifyIDToken(context.Background(), fi.srv.URL, tampered, "expected-nonce"); err == nil {
		t.Fatal("expected a tampered token to be rejected")
	}
}

func TestVerifyIDTokenExpiryComesFromToken(t *testing.T) {
	fi := newFakeIssuer(t)
	claims := fi.validClaims()

	verified, err := verifyIDToken(context.Background(), fi.srv.URL, fi.signed(t, claims), "expected-nonce")
	if err != nil {
		t.Fatalf("verifyIDToken: %v", err)
	}
	if verified.Email != "admin@kipper.test" {
		t.Errorf("email = %q, want admin@kipper.test", verified.Email)
	}
	if got := verified.ExpiresAt.Unix(); got != claims.Exp {
		t.Errorf("ExpiresAt = %d, want the token's exp %d — the OAuth expires_in must not decide token lifetime", got, claims.Exp)
	}
}

func TestExchangeCodeSendsPKCEVerifierAndValidatesToken(t *testing.T) {
	fi := newFakeIssuer(t)
	fi.idToken = fi.signed(t, fi.validClaims())

	creds, err := exchangeCode(context.Background(), fi.srv.URL, "auth-code", "http://localhost:18741/callback", "the-verifier", "expected-nonce")
	if err != nil {
		t.Fatalf("exchangeCode: %v", err)
	}
	if creds.Email != "admin@kipper.test" {
		t.Errorf("email = %q, want admin@kipper.test", creds.Email)
	}
	if fi.gotForm["code_verifier"] != "the-verifier" {
		t.Errorf("code_verifier = %q, want the-verifier", fi.gotForm["code_verifier"])
	}
	if fi.gotForm["grant_type"] != "authorization_code" || fi.gotForm["code"] != "auth-code" {
		t.Errorf("unexpected exchange form: %v", fi.gotForm)
	}
}

func TestExchangeCodeRejectsInvalidIDToken(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*tokenClaims)
	}{
		{"nonce mismatch", func(c *tokenClaims) { c.Nonce = "someone-elses-nonce" }},
		{"wrong issuer", func(c *tokenClaims) { c.Issuer = "https://evil.test/dex" }},
		{"foreign audience", func(c *tokenClaims) { c.Audience = "kipper-console" }},
		{"expired", func(c *tokenClaims) { c.Exp = time.Now().Add(-time.Hour).Unix() }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fi := newFakeIssuer(t)
			claims := fi.validClaims()
			tt.mutate(&claims)
			fi.idToken = fi.signed(t, claims)

			if _, err := exchangeCode(context.Background(), fi.srv.URL, "auth-code", "http://localhost:18741/callback", "the-verifier", "expected-nonce"); err == nil {
				t.Fatal("expected exchangeCode to reject the token")
			}
		})
	}
}

func TestRefreshTokenVerifiesAndRotates(t *testing.T) {
	fi := newFakeIssuer(t)
	fi.idToken = fi.signed(t, fi.validClaims())

	creds, err := refreshToken(context.Background(), fi.srv.URL, "old-refresh")
	if err != nil {
		t.Fatalf("refreshToken: %v", err)
	}
	if creds.RefreshToken != "rotated-refresh" {
		t.Errorf("refresh token = %q, want the rotated one", creds.RefreshToken)
	}
	if fi.gotForm["grant_type"] != "refresh_token" || fi.gotForm["refresh_token"] != "old-refresh" {
		t.Errorf("unexpected refresh form: %v", fi.gotForm)
	}

	// A forged token on the refresh path must be rejected the same way.
	strangerKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating stranger key: %v", err)
	}
	fi.idToken = signToken(t, strangerKey, jose.RS256, "test-key", fi.validClaims())
	if _, err := refreshToken(context.Background(), fi.srv.URL, "old-refresh"); err == nil {
		t.Fatal("expected refreshToken to reject a forged token")
	}
}

func TestStoreSaveAtomicAndPrivate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	store := &Store{Clusters: map[string]*Credentials{
		"cluster.example.com": {IDToken: "tok", RefreshToken: "ref", Email: "admin@kipper.test", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	if err := store.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path, err := storePath()
	if err != nil {
		t.Fatalf("storePath: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat store: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("store mode = %o, want 0600", info.Mode().Perm())
	}

	// No temp file may survive a successful save.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("reading store dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".auth-") {
			t.Errorf("leftover temp file %s after save", e.Name())
		}
	}

	loaded, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Clusters["cluster.example.com"].RefreshToken != "ref" {
		t.Errorf("round-trip lost data: %+v", loaded.Clusters)
	}
}

func TestLockStoreSerializes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	unlock, err := lockStore()
	if err != nil {
		t.Fatalf("lockStore: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		unlock2, err := lockStore()
		if err != nil {
			t.Errorf("second lockStore: %v", err)
			close(acquired)
			return
		}
		close(acquired)
		unlock2()
	}()

	select {
	case <-acquired:
		t.Fatal("second lock acquired while the first was held")
	case <-time.After(100 * time.Millisecond):
	}

	unlock()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second lock never acquired after release")
	}
}

func writeStoreFile(t *testing.T, content string) {
	t.Helper()
	path, err := storePath()
	if err != nil {
		t.Fatalf("storePath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("writing store: %v", err)
	}
}

func TestLoadIgnoresLegacyStores(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// A store from the pre-verification implementation (no version marker)
	// may hold tokens nothing ever verified; it must load as empty so the
	// fast path cannot return them.
	writeStoreFile(t, `{"clusters":{"c1":{"id_token":"legacy","expires_at":"2999-01-01T00:00:00Z"}}}`)

	store, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(store.Clusters) != 0 {
		t.Fatalf("legacy store loaded %d clusters, want 0", len(store.Clusters))
	}
	if _, err := store.Token("c1", "dex.example.com"); err == nil {
		t.Fatal("expected a legacy token to be unavailable")
	}
}

func TestTokenHonoursDeletionUnderLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	expired := &Credentials{IDToken: "old", RefreshToken: "ref", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := Mutate(func(s *Store) { s.Clusters["c1"] = expired }); err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	stale, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	// Another process logs out between this process's read and its refresh.
	if err := Mutate(func(s *Store) { delete(s.Clusters, "c1") }); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	// No issuer is reachable here, so reaching the refresh would also fail —
	// the point is the error class: logged out, not a refresh failure.
	_, err = stale.Token("c1", "dex.example.com")
	if err == nil || !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("Token after concurrent logout = %v, want not authenticated", err)
	}
	if _, ok := stale.Clusters["c1"]; ok {
		t.Fatal("stale in-memory credentials survived the authoritative re-read")
	}
}

func TestTokenHonoursStrippedRefreshTokenUnderLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	expired := &Credentials{IDToken: "old", RefreshToken: "ref", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := Mutate(func(s *Store) { s.Clusters["c1"] = expired }); err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	stale, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if err := Mutate(func(s *Store) {
		s.Clusters["c1"] = &Credentials{IDToken: "old", ExpiresAt: time.Now().Add(-time.Minute)}
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	_, err = stale.Token("c1", "dex.example.com")
	if err == nil || !strings.Contains(err.Error(), "session expired") {
		t.Fatalf("Token after refresh token removal = %v, want session expired", err)
	}
}

func TestMutateStampsVersionAndPersists(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	if err := Mutate(func(s *Store) {
		s.Clusters["c1"] = &Credentials{IDToken: "tok", ExpiresAt: time.Now().Add(time.Hour)}
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	path, err := storePath()
	if err != nil {
		t.Fatalf("storePath: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading store: %v", err)
	}
	var raw struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parsing store: %v", err)
	}
	if raw.Version != storeVersion {
		t.Errorf("persisted version = %d, want %d", raw.Version, storeVersion)
	}
}

func TestRefreshTokenHonoursContextDeadline(t *testing.T) {
	// A token endpoint that accepts the connection and never answers must
	// not hold the caller (and with it the store lock) beyond the deadline.
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	t.Cleanup(func() { close(release); srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := refreshToken(ctx, srv.URL, "ref")
	if err == nil {
		t.Fatal("expected the stalled refresh to fail")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("refresh took %v, the deadline did not bound it", elapsed)
	}
}

func TestStoreUsableAfterFailedRefresh(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	// An unreachable issuer must fail the refresh and release the lock, so
	// login and logout on this machine are never wedged by one dead Dex.
	expired := &Credentials{IDToken: "old", RefreshToken: "ref", ExpiresAt: time.Now().Add(-time.Minute)}
	if err := Mutate(func(s *Store) { s.Clusters["c1"] = expired }); err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	store, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := store.Token("c1", "127.0.0.1:1"); err == nil {
		t.Fatal("expected refresh against an unreachable issuer to fail")
	}

	done := make(chan error, 1)
	go func() {
		done <- Mutate(func(s *Store) { delete(s.Clusters, "c1") })
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Mutate after failed refresh: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the store lock was not released after the failed refresh")
	}
}
