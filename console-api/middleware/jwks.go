package middleware

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// jwksKey represents a single key from a JWKS endpoint.
type jwksKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
	Alg string `json:"alg"`
}

type jwksResponse struct {
	Keys []jwksKey `json:"keys"`
}

// JWKSKeyFunc returns a jwt.Keyfunc that fetches and caches public keys
// from a Dex JWKS endpoint. Keys are cached and refreshed periodically.
func JWKSKeyFunc(jwksURL string) jwt.Keyfunc {
	var (
		mu        sync.RWMutex
		keys      map[string]*rsa.PublicKey
		fetchedAt time.Time
		cacheTTL  = 5 * time.Minute

		// fetchMu serializes fetches so a burst of concurrent misses collapses
		// into one outbound request instead of one per request.
		// nextRefetchAllowedAt throttles the miss-triggered refetch: an unknown
		// kid (a rotated key or an attacker spraying garbage kids) may force at
		// most one refetch per window, so unauthenticated requests cannot be
		// amplified into a self-inflicted DoS on Dex. A *successful* fetch arms
		// the full window — we have Dex's current key set and should not hammer
		// it. A *failed* fetch arms only a short backoff, so a transient Dex
		// blip does not lock out a genuinely rotated key for the full window
		// while the apiserver (its own JWKS machinery) already accepts it.
		fetchMu              sync.Mutex
		nextRefetchAllowedAt time.Time
		refetchCooldown      = 30 * time.Second
		refetchFailBackoff   = 3 * time.Second
	)

	client := &http.Client{Timeout: 10 * time.Second}

	// refetch performs at most one fetch per throttle window across all
	// callers: fetchMu serializes them and the window makes a concurrent burst
	// (a cold cache warmed by many requests, or a spray of unknown kids)
	// collapse to a single outbound request. A throttled caller falls through
	// to consult the cache rather than erroring.
	refetch := func() {
		fetchMu.Lock()
		defer fetchMu.Unlock()
		if time.Now().Before(nextRefetchAllowedAt) {
			return
		}

		resp, err := client.Get(jwksURL) //nolint:gosec // URL from trusted Dex config
		if err != nil {
			nextRefetchAllowedAt = time.Now().Add(refetchFailBackoff)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		var jwks jwksResponse
		if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
			nextRefetchAllowedAt = time.Now().Add(refetchFailBackoff)
			return
		}

		newKeys := make(map[string]*rsa.PublicKey)
		for _, k := range jwks.Keys {
			if k.Kty != "RSA" {
				continue
			}
			pub, err := parseRSAPublicKey(k)
			if err != nil {
				continue
			}
			newKeys[k.Kid] = pub
		}

		mu.Lock()
		keys = newKeys
		fetchedAt = time.Now()
		mu.Unlock()
		nextRefetchAllowedAt = time.Now().Add(refetchCooldown)
	}

	return func(token *jwt.Token) (any, error) {
		// Pin RS256 before we hand back an RSA public key. Without this an
		// attacker could present an HS256 token and have the verifier treat
		// the (public, known-from-JWKS) RSA key as an HMAC secret. The parse
		// sites also pin RS256 via WithValidMethods; asserting it here too
		// keeps the guarantee at the key layer, so the keyfunc is safe even
		// for a future caller that forgets WithValidMethods.
		if token.Method != jwt.SigningMethodRS256 {
			return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
		}

		mu.RLock()
		age := time.Since(fetchedAt)
		empty := keys == nil
		mu.RUnlock()

		// A stale or cold cache is refreshed on the normal TTL; the cooldown
		// only collapses a concurrent warm-up burst, never the 5-minute
		// scheduled refresh.
		if age > cacheTTL || empty {
			refetch()
		}

		kid, ok := token.Header["kid"].(string)
		if !ok {
			return nil, fmt.Errorf("missing kid in token header")
		}

		mu.RLock()
		key, exists := keys[kid]
		mu.RUnlock()

		if !exists {
			// An unknown kid may be a genuine rotation or an attacker spraying
			// garbage. The shared cooldown means the miss cannot be amplified
			// into a per-request outbound fetch.
			refetch()
			mu.RLock()
			key, exists = keys[kid]
			mu.RUnlock()
			if !exists {
				return nil, fmt.Errorf("unknown key ID: %s", kid)
			}
		}

		return key, nil
	}
}

func parseRSAPublicKey(k jwksKey) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, err
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, err
	}

	n := new(big.Int).SetBytes(nBytes)
	e := int(new(big.Int).SetBytes(eBytes).Int64())

	// Reject undersized moduli: a short RSA key is cheap enough to factor that
	// accepting one would let a forged token pass verification.
	if n.BitLen() < 2048 {
		return nil, fmt.Errorf("RSA key too small: %d bits", n.BitLen())
	}

	return &rsa.PublicKey{N: n, E: e}, nil
}
