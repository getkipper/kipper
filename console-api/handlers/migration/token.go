package migration

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

const (
	tokenSecretName      = "migration-token"
	tokenSecretNamespace = "kipper-system"
	tokenTTL             = 24 * time.Hour
)

// Token is the JSON payload encoded into the migration token string.
type Token struct {
	Endpoint string    `json:"endpoint"`
	Secret   string    `json:"secret"`
	Cluster  string    `json:"cluster"`
	Expires  time.Time `json:"expires"`
	// BaseDomain is the target's CLUSTER_DOMAIN, carried so the source can show
	// the coexist target URLs at plan time. It is attacker-authored like the
	// rest of the token, so the authoritative value is the one the target echoes
	// in the accept response; this is display-only until then.
	BaseDomain string `json:"base_domain,omitempty"`
}

// baseDomainPattern matches a plausible DNS domain of at least two lowercase
// labels. It guards the untrusted Token.BaseDomain so a value carrying a scheme,
// path, or other junk can never be spliced into a hostname.
var baseDomainPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// looksLikeDomain reports whether s is a plausible DNS domain name.
func looksLikeDomain(s string) bool {
	return len(s) <= 253 && baseDomainPattern.MatchString(s)
}

// GenerateToken creates a migration token and stores the secret on the
// target cluster for later validation. Returns the base64-encoded token
// string for the user to copy.
func GenerateToken(ctx context.Context, client kubernetes.Interface, endpoint, clusterDisplay, baseDomain string) (string, error) {
	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		return "", fmt.Errorf("generating secret: %w", err)
	}
	secretHex := hex.EncodeToString(secretBytes)

	token := Token{
		Endpoint:   endpoint,
		Secret:     secretHex,
		Cluster:    clusterDisplay,
		BaseDomain: baseDomain,
		Expires:    time.Now().Add(tokenTTL),
	}

	tokenJSON, err := json.Marshal(token) //nolint:gosec // serialising a freshly minted migration token for the source cluster is the intended use
	if err != nil {
		return "", fmt.Errorf("encoding token: %w", err)
	}

	// Store the secret in K8s for validation when the source connects
	k8sSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tokenSecretName,
			Namespace: tokenSecretNamespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				"kipper.run/migration":         "token",
			},
		},
		Data: map[string][]byte{
			"secret":  []byte(secretHex),
			"expires": []byte(token.Expires.Format(time.RFC3339)),
		},
	}

	_, err = client.CoreV1().Secrets(tokenSecretNamespace).Create(ctx, k8sSecret, metav1.CreateOptions{})
	if errors.IsAlreadyExists(err) {
		existing, getErr := client.CoreV1().Secrets(tokenSecretNamespace).Get(ctx, tokenSecretName, metav1.GetOptions{})
		if getErr != nil {
			return "", fmt.Errorf("updating migration token: %w", getErr)
		}
		existing.Data = k8sSecret.Data
		if _, updateErr := client.CoreV1().Secrets(tokenSecretNamespace).Update(ctx, existing, metav1.UpdateOptions{}); updateErr != nil {
			return "", fmt.Errorf("updating migration token: %w", updateErr)
		}
	} else if err != nil {
		return "", fmt.Errorf("storing migration token: %w", err)
	}

	return base64.StdEncoding.EncodeToString(tokenJSON), nil
}

// ValidateToken checks the provided secret against the stored token on
// this cluster. Returns nil if valid, error if invalid or expired.
func ValidateToken(ctx context.Context, client kubernetes.Interface, secret string) error {
	k8sSecret, err := client.CoreV1().Secrets(tokenSecretNamespace).Get(ctx, tokenSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("no migration token configured: generate one first")
		}
		return fmt.Errorf("reading migration token: %w", err)
	}

	storedSecret := string(k8sSecret.Data["secret"])
	expiresStr := string(k8sSecret.Data["expires"])

	// A blank stored secret must never validate: ConstantTimeCompare("","")
	// returns a match, so an empty presented token would be accepted against a
	// malformed secret. GenerateToken always writes 64 hex chars, so this is
	// defence in depth.
	if storedSecret == "" {
		return fmt.Errorf("migration token is not usable: generate a new one")
	}

	expires, err := time.Parse(time.RFC3339, expiresStr)
	if err != nil {
		return fmt.Errorf("invalid token expiry")
	}

	if time.Now().After(expires) {
		return fmt.Errorf("migration token has expired: generate a new one")
	}

	if subtle.ConstantTimeCompare([]byte(storedSecret), []byte(secret)) != 1 {
		return fmt.Errorf("invalid migration token")
	}

	return nil
}

// ConsumeToken atomically deletes the stored token so it can only be accepted
// once. It deletes under a UID/resourceVersion precondition, so two concurrent
// accepts cannot both consume the same token: the loser gets a not-found or
// conflict and this returns an error, letting the caller fail closed.
func ConsumeToken(ctx context.Context, client kubernetes.Interface) error {
	existing, err := client.CoreV1().Secrets(tokenSecretNamespace).Get(ctx, tokenSecretName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("migration token already consumed")
		}
		return fmt.Errorf("reading migration token: %w", err)
	}
	err = client.CoreV1().Secrets(tokenSecretNamespace).Delete(ctx, tokenSecretName, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &existing.UID, ResourceVersion: &existing.ResourceVersion},
	})
	if err != nil {
		if errors.IsNotFound(err) || errors.IsConflict(err) {
			return fmt.Errorf("migration token already consumed")
		}
		return fmt.Errorf("consuming migration token: %w", err)
	}
	return nil
}

// DecodeToken parses a base64-encoded migration token string.
func DecodeToken(tokenStr string) (*Token, error) {
	data, err := base64.StdEncoding.DecodeString(tokenStr)
	if err != nil {
		return nil, fmt.Errorf("invalid migration token format")
	}

	var token Token
	if err := json.Unmarshal(data, &token); err != nil {
		return nil, fmt.Errorf("invalid migration token payload")
	}

	// Transport hygiene, nothing more: the endpoint is attacker-authored
	// data, and https only keeps the projects and secrets off the wire in
	// plaintext — it does not authenticate the target as the displayed
	// cluster. Loopback hosts are exempt for local development, where no
	// network is crossed.
	u, err := url.Parse(token.Endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("migration token carries an invalid endpoint")
	}
	if u.Scheme != "https" && !isLoopbackHost(u.Hostname()) {
		return nil, fmt.Errorf("migration token endpoint must use https: plaintext transport would expose every transferred secret")
	}

	if time.Now().After(token.Expires) {
		return nil, fmt.Errorf("migration token has expired: generate a new one on the target cluster")
	}

	// The base domain drives the coexist URLs and the env/secret rewrite, and an
	// empty one would silently disable the target-domain mismatch guard. A
	// freshly generated token always carries it, so require a canonical value
	// rather than tolerate an "older target".
	if !looksLikeDomain(token.BaseDomain) {
		return nil, fmt.Errorf("migration token is missing a valid target base domain: generate a new one on the target cluster")
	}

	return &token, nil
}

// isLoopbackHost reports whether the hostname stays on this machine.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
