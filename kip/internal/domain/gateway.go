package domain

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

const defaultGatewayURL = "https://kipper.run"

// ErrNotRegistered reports that the gateway holds no registration for the
// token — the deregistration already happened. Callers treating removal as
// idempotent cleanup match on it with errors.Is.
var ErrNotRegistered = errors.New("no registration exists for this token")

// ErrNameTaken reports that the gateway refused a name because it belongs to
// somebody else. It is separated from every other failure so a caller can tell
// "choose a different name" from "the gateway is having trouble, try again" —
// advising a rename for what a retry would fix costs an operator a cluster
// identity change they never needed.
var ErrNameTaken = errors.New("the subdomain is already registered")

// GatewayClient communicates with the Kipper Gateway to register
// and manage *.kipper.run subdomains.
type GatewayClient struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewGatewayClient creates a client with sensible defaults.
// The base URL can be overridden via the gatewayURL parameter
// (empty string uses the default).
func NewGatewayClient(gatewayURL string) *GatewayClient {
	if gatewayURL == "" {
		gatewayURL = defaultGatewayURL
	}
	return &GatewayClient{
		BaseURL: gatewayURL,
		HTTPClient: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// Registration holds the result of a successful subdomain registration.
type Registration struct {
	Subdomain string `json:"subdomain"`
	Domain    string `json:"domain"`
	Token     string `json:"token"`
	// Challenge is a proof-of-possession nonce, and the gateway issues one only
	// to a caller whose token it recognised. That makes it the single signal
	// distinguishing a renewal from a request it turned away: both answer 201
	// with no token, and only this says the token was accepted. Empty from a
	// gateway old enough not to ask for proof.
	Challenge string `json:"challenge"`
}

// Register requests a *.kipper.run subdomain for the given IP.
// Register claims a subdomain for an IP. A token, when held, identifies the
// caller as the registration's existing owner: the gateway then renews it and
// discloses no new token, where an anonymous call against a claimed name is
// answered without one. Passing a token the caller already has is what lets a
// retry after a failed install continue on the same name.
func (c *GatewayClient) Register(subdomain, ip, token string) (*Registration, error) {
	payload := map[string]string{
		"subdomain": subdomain,
		"ip":        ip,
	}
	if token != "" {
		payload["token"] = token
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshalling request: %w", err)
	}

	resp, err := c.HTTPClient.Post(c.BaseURL+"/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("calling gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		return nil, parseError(resp)
	}

	var reg Registration
	if err := json.NewDecoder(resp.Body).Decode(&reg); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &reg, nil
}

// Deregister removes a subdomain using its management token. A gateway that
// no longer knows the token returns ErrNotRegistered, so retried cleanups can
// treat an already-removed registration as success.
func (c *GatewayClient) Deregister(token string) error {
	body, _ := json.Marshal(map[string]string{"token": token})

	req, err := http.NewRequest(http.MethodDelete, c.BaseURL+"/register", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return ErrNotRegistered
	}
	if resp.StatusCode != http.StatusNoContent {
		return parseError(resp)
	}

	return nil
}

// Ping renews a subdomain's last-seen timestamp to prevent expiry.
func (c *GatewayClient) Ping(token string) error {
	body, _ := json.Marshal(map[string]string{"token": token})

	resp, err := c.HTTPClient.Post(c.BaseURL+"/ping", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("calling gateway: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return parseError(resp)
	}

	return nil
}

func parseError(resp *http.Response) error {
	data, _ := io.ReadAll(resp.Body)

	var errResp struct {
		Error string `json:"error"`
	}
	detail := fmt.Errorf("gateway returned %d: %s", resp.StatusCode, string(data))
	if json.Unmarshal(data, &errResp) == nil && errResp.Error != "" {
		detail = fmt.Errorf("gateway: %s", errResp.Error)
	}
	// A conflict is the one refusal that means the name itself is unavailable;
	// every other status is the gateway failing to answer, which a retry may fix.
	if resp.StatusCode == http.StatusConflict {
		return fmt.Errorf("%w: %v", ErrNameTaken, detail)
	}
	return detail
}
