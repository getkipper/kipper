package doctor

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// CheckK3s passes when the k3s service is active, and says how long ago the
// server booted when uptime is available, to help distinguish a recent reboot
// from a long-running fault.
func CheckK3s(r Runner) Result {
	const id, label = "k3s", "k3s"
	booted := ""
	if out, err := r.Run("cat /proc/uptime"); err == nil {
		if fields := strings.Fields(out); len(fields) > 0 {
			if secs, err := strconv.ParseFloat(fields[0], 64); err == nil {
				booted = fmt.Sprintf(" (server booted %s ago)", ago(time.Duration(secs*float64(time.Second))))
			}
		}
	}
	out, err := r.Run("systemctl is-active k3s")
	if err == nil && strings.TrimSpace(out) == "active" {
		return Result{ID: id, Label: label, Status: Pass, Message: "k3s is running" + booted}
	}
	journal, _ := r.Run("journalctl -u k3s -n 40 --no-pager")
	return Result{ID: id, Label: label, Status: Fail, Message: "k3s is not running on the server" + booted, Detail: strings.TrimSpace(journal)}
}

// apiClient skips certificate verification for the reachability check and sends
// no credentials. Any HTTP response shows that the endpoint is listening.
var apiClient = &http.Client{
	Timeout:   10 * time.Second,
	Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}}, //nolint:gosec // reachability only, no credentials sent
}

// CheckAPI requests /livez at serverURL without credentials or certificate
// verification. Any HTTP response passes, including 401, because login is checked
// separately. A request error fails the check.
func CheckAPI(ctx context.Context, serverURL string) Result {
	const id, label = "api", "Kubernetes API"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(serverURL, "/")+"/livez", nil)
	if err == nil {
		var resp *http.Response
		if resp, err = apiClient.Do(req); err == nil {
			_ = resp.Body.Close()
			return Result{ID: id, Label: label, Status: Pass, Message: "the API server answers"}
		}
	}
	return Result{ID: id, Label: label, Status: Fail, Message: "the API server does not answer on its address", Detail: err.Error()}
}
