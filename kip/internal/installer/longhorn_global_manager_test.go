package installer

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// globalLeaseRunner answers the global manager's Lease and pod reads. Each Lease
// read returns the next entry of leases, repeating the last one.
type globalLeaseRunner struct {
	leases   []string
	leaseErr error
	pods     string
	reads    int
}

func (r *globalLeaseRunner) Run(cmd string) (string, error) {
	switch cmd {
	case globalManagerLeaseQuery:
		if r.leaseErr != nil {
			return "", r.leaseErr
		}
		out := r.leases[min(r.reads, len(r.leases)-1)]
		r.reads++
		return out, nil
	case globalManagerPodQuery:
		return r.pods, nil
	}
	return "", fmt.Errorf("unexpected command %q", cmd)
}

func (r *globalLeaseRunner) RunStdin(cmd string, _ io.Reader) (string, error) {
	return r.Run(cmd)
}

func globalLease(holder, renew string) string {
	return fmt.Sprintf(`{"spec":{"holderIdentity":%q,"renewTime":%q}}`, holder, renew)
}

func globalManagerPod(name, phase, ready string) string {
	return fmt.Sprintf(`{"metadata":{"name":%q},"status":{"phase":%q,"conditions":[{"type":"Ready","status":%q}]}}`, name, phase, ready)
}

func globalManagerPods(pods ...string) string {
	return `{"items":[` + strings.Join(pods, ",") + `]}`
}

func leaderWait() LonghornUpgrade {
	return LonghornUpgrade{Out: &strings.Builder{}, StepTimeout: 200 * time.Millisecond, Interval: time.Microsecond, now: time.Now}
}

func TestGlobalLeaderRenewingItsLeaseIsSettled(t *testing.T) {
	r := &globalLeaseRunner{
		leases: []string{globalLease("gm-a", "2026-11-09T18:00:00.000000Z"), globalLease("gm-a", "2026-11-09T18:00:02.000000Z")},
		pods:   globalManagerPods(globalManagerPod("gm-a", "Running", "True"), globalManagerPod("gm-b", "Running", "True")),
	}
	require.NoError(t, leaderWait().waitForGlobalLeader(r))
	assert.Equal(t, 2, r.reads, "the renewal is seen between two reads")
}

// The health endpoint responds before election and on standby pods, so
// readiness alone cannot establish leadership.
func TestGlobalLeaderNeedsALease(t *testing.T) {
	r := &globalLeaseRunner{leaseErr: errors.New(`leases.coordination.k8s.io "longhorn-global-manager" not found`),
		pods: globalManagerPods(globalManagerPod("gm-a", "Running", "True"))}
	err := leaderWait().waitForGlobalLeader(r)
	assert.ErrorContains(t, err, "reading the global manager's lease")
	assert.ErrorContains(t, err, LonghornUpgradeCommand)
}

func TestGlobalLeaderProblems(t *testing.T) {
	ready := globalManagerPods(globalManagerPod("gm-a", "Running", "True"))
	for name, tc := range map[string]struct {
		leases []string
		pods   string
		want   string
	}{
		"stale": {[]string{globalLease("gm-a", "2026-11-09T18:00:00.000000Z")}, ready,
			"has not renewed"},
		"no holder": {[]string{globalLease("", "2026-11-09T18:00:00.000000Z")}, ready,
			"no global manager holds"},
		"holder gone": {[]string{globalLease("gm-old", "2026-11-09T18:00:00.000000Z"), globalLease("gm-old", "2026-11-09T18:00:02.000000Z")}, ready,
			"gm-old, which is not a running global manager pod"},
		"holder pending": {[]string{globalLease("gm-a", "2026-11-09T18:00:00.000000Z"), globalLease("gm-a", "2026-11-09T18:00:02.000000Z")},
			globalManagerPods(globalManagerPod("gm-a", "Pending", "False")), "gm-a, which is not a running global manager pod"},
		"holder not ready": {[]string{globalLease("gm-a", "2026-11-09T18:00:00.000000Z"), globalLease("gm-a", "2026-11-09T18:00:02.000000Z")},
			globalManagerPods(globalManagerPod("gm-a", "Running", "False")), "gm-a, which is not a running global manager pod"},
		"unreadable": {[]string{"not json"}, ready, "reading the global manager's lease"},
	} {
		r := &globalLeaseRunner{leases: tc.leases, pods: tc.pods}
		err := leaderWait().waitForGlobalLeader(r)
		assert.ErrorContains(t, err, tc.want, name)
	}
}
