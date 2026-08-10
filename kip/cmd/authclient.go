package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/getkipper/kipper/kip/internal/config"
)

// healthClient fetches the console-api /health endpoint. It is a package
// variable so tests can point it at a test server without mutating the shared
// http.DefaultClient.
var healthClient = http.DefaultClient

// cliVersion returns the kip build version, falling back to "dev" when the
// binary was built without an injected version.
func cliVersion() string {
	if rootCmd.Version == "" {
		return "dev"
	}
	return rootCmd.Version
}

// consoleAPIVersion fetches the cluster's console-api build version from its
// unauthenticated /health endpoint. It returns "" when the version can't be
// determined — the endpoint is unreachable, or an older console-api that
// predates the version field.
func consoleAPIVersion(ctx context.Context, cluster *config.Cluster) string {
	url := fmt.Sprintf("https://%s/health", cluster.ConsoleAPIHost())
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return ""
	}
	resp, err := healthClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var body struct {
		Version string `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return ""
	}
	return body.Version
}

// authRejectedError builds the error returned when console-api answers 401 for
// a request that already carried a locally-valid token. kip refreshes the token
// before every call, so a server 401 is not a plain session expiry. It fetches
// the cluster's version so the message can name a version skew instead of
// sending the user in circles through `kip auth login`.
func authRejectedError(ctx context.Context, cluster *config.Cluster) error {
	return fmt.Errorf("%s", authRejectedMessage(cliVersion(), consoleAPIVersion(ctx, cluster)))
}

// authRejectedMessage is the pure message-building half of authRejectedError,
// split out so the wording is testable without a live server. It presents both
// versions as diagnostic facts and does not claim a skew: kip and console-api
// stamp their versions from different schemes (a release tag vs a commit sha),
// so it cannot tell "same release, different string" from a real mismatch. It
// leaves that judgement to the operator rather than steering them to update.
func authRejectedMessage(cliVer, serverVer string) string {
	if serverVer != "" && serverVer != cliVer {
		return fmt.Sprintf("console-api rejected your credentials (HTTP 401), but your session is "+
			"not expired, so kip auth login alone may not fix it. Versions, kip: %s, "+
			"console-api: %s. A CLI/cluster version mismatch or a change to the cluster's login "+
			"are the usual causes; update kip if it is behind the cluster, otherwise run: "+
			"kip auth login", cliVer, serverVer)
	}
	return fmt.Sprintf("console-api rejected your credentials (HTTP 401), but your session is not "+
		"expired. This usually means a version mismatch (kip %s) or the cluster's login was "+
		"reconfigured. Try: kip auth login", cliVer)
}
