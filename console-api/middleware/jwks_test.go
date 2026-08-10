package middleware

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
)

func TestJWKSKeyFuncRejectsNonRSA(t *testing.T) {
	// The keyfunc must reject a non-RSA signing method before it does any
	// network fetch or key lookup, so the algorithm guard holds even for
	// callers that forget WithValidMethods. The JWKS URL is unreachable on
	// purpose: if the method check were missing, the keyfunc would try to
	// fetch and fail differently (or return the RSA key for an HS256 token).
	kf := JWKSKeyFunc("http://127.0.0.1:1/keys")

	tok := &jwt.Token{
		Method: jwt.SigningMethodHS256,
		Header: map[string]any{"alg": "HS256", "kid": "any"},
	}

	if _, err := kf(tok); err == nil {
		t.Fatal("expected keyfunc to reject a non-RSA (HS256) token")
	}
}

func TestParseRSAPublicKeyRejectsUndersizedKey(t *testing.T) {
	small, err := rsa.GenerateKey(rand.Reader, 1024) //nolint:gosec // undersized on purpose: this is what the test rejects
	if err != nil {
		t.Fatalf("generating 1024-bit key: %v", err)
	}
	jwk := jwksKey{
		Kty: "RSA",
		N:   base64.RawURLEncoding.EncodeToString(small.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(small.E)).Bytes()),
	}
	if _, err := parseRSAPublicKey(jwk); err == nil {
		t.Fatal("expected a 1024-bit RSA key to be rejected")
	}

	big2048, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating 2048-bit key: %v", err)
	}
	jwk.N = base64.RawURLEncoding.EncodeToString(big2048.N.Bytes())
	jwk.E = base64.RawURLEncoding.EncodeToString(big.NewInt(int64(big2048.E)).Bytes())
	if _, err := parseRSAPublicKey(jwk); err != nil {
		t.Fatalf("expected a 2048-bit RSA key to be accepted, got %v", err)
	}
}

func TestJWKSRefetchThrottledOnUnknownKid(t *testing.T) {
	// A flood of tokens with unknown kids must not amplify into one outbound
	// fetch per request: the miss-triggered refetch is throttled.
	var fetches int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&fetches, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[]}`))
	}))
	defer srv.Close()

	keyfunc := JWKSKeyFunc(srv.URL)
	tok := &jwt.Token{Method: jwt.SigningMethodRS256, Header: map[string]any{"kid": "garbage", "alg": "RS256"}}

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = keyfunc(tok)
		}()
	}
	wg.Wait()

	// 50 concurrent unknown-kid requests must collapse to a small number of
	// fetches (the cold-cache fetch plus at most one throttled miss refetch),
	// never 50.
	got := atomic.LoadInt32(&fetches)
	assert.LessOrEqual(t, got, int32(2), "unknown-kid requests must not each trigger a fetch (got %d)", got)
}
