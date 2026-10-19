package doctor

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckK3s_ReportsUptime(t *testing.T) {
	r := (&routedRunner{}).on("systemctl is-active k3s", "active\n", nil).on("/proc/uptime", "540.12 1000.0\n", nil)

	res := CheckK3s(r)
	assert.Equal(t, Pass, res.Status)
	assert.Contains(t, res.Message, "server booted 9 minutes ago")
}

func TestCheckK3s_FailsWithTheJournalTailWhenStopped(t *testing.T) {
	r := (&routedRunner{}).
		on("systemctl is-active k3s", "inactive\n", errors.New("exit status 3")).
		on("/proc/uptime", "120.0 1.0\n", nil).
		on("journalctl -u k3s", "Oct 06 00:17:01 k3s[812]: level=fatal msg=\"starting kubernetes: preparing server\"\n", nil)

	res := CheckK3s(r)
	assert.Equal(t, Fail, res.Status)
	assert.Contains(t, res.Message, "k3s is not running")
	assert.Contains(t, res.Detail, "preparing server")
}

func TestCheckAPI_CountsAnyHTTPAnswerAsUp(t *testing.T) {
	for _, code := range []int{http.StatusOK, http.StatusUnauthorized, http.StatusForbidden} {
		assert.Equal(t, Pass, CheckAPI(context.Background(), statusServer(t, code)).Status, "status %d", code)
	}
	down := CheckAPI(context.Background(), "https://127.0.0.1:1")
	assert.Equal(t, Fail, down.Status)
	assert.Contains(t, down.Message, "does not answer")
}
