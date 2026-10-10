package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// rebootingHost simulates SSH downtime and delayed cluster readiness after a reboot.
type rebootingHost struct {
	calls          []string
	bootID         string
	rebooted       bool
	downReads      int
	unhealthyReads int
	backupErr      error
	confirm        bool
	configs        []ssh.Config
	healthBootIDs  []string
	apps           []appRef
	appsErr        error
	startingReads  int
	servingSince   []time.Time
}

// serverClock differs from the local test clock to catch use of the wrong clock.
var serverClock = time.Date(2026, 10, 11, 0, 58, 0, 0, time.UTC)

func (h *rebootingHost) reboot(t *testing.T) (*nodeReboot, *bytes.Buffer) {
	t.Helper()
	var out bytes.Buffer
	clock := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	return &nodeReboot{
		Out: &out,
		Confirm: func(prompt string) (bool, error) {
			h.calls = append(h.calls, "confirm")
			return h.confirm, nil
		},
		Backup: func() (string, error) {
			h.calls = append(h.calls, "backup")
			return "before-reboot-20261011-010000", h.backupErr
		},
		SSH: ssh.Config{Host: "example.com", User: "root", KeyPath: "/keys/current"},
		BootID: func(cfg ssh.Config) (string, error) {
			h.configs = append(h.configs, cfg)
			if h.rebooted && h.downReads > 0 {
				h.downReads--
				return "", errors.New("connection refused")
			}
			return h.bootID, nil
		},
		Reboot: func(cfg ssh.Config) (time.Time, error) {
			h.configs = append(h.configs, cfg)
			h.calls = append(h.calls, "reboot")
			h.rebooted = true
			h.bootID = "new-boot"
			return serverClock, nil
		},
		Healthy: func(_ time.Duration, bootID string) (bool, string) {
			h.healthBootIDs = append(h.healthBootIDs, bootID)
			if h.unhealthyReads > 0 {
				h.unhealthyReads--
				return false, "Loki 0/1 ready"
			}
			return true, ""
		},
		Report: func(w io.Writer, cfg ssh.Config) {
			h.configs = append(h.configs, cfg)
			h.calls = append(h.calls, "report")
			_, _ = io.WriteString(w, "  Host checks\n")
		},
		ServingApps: func() ([]appRef, error) {
			h.calls = append(h.calls, "list apps")
			return h.apps, h.appsErr
		},
		NotServing: func(apps []appRef, since time.Time, _ time.Duration) ([]string, error) {
			h.servingSince = append(h.servingSince, since)
			if h.startingReads > 0 {
				h.startingReads--
				return []string{apps[0].String()}, nil
			}
			return nil, nil
		},
		Sleep:            func(d time.Duration) { clock = clock.Add(d) },
		Now:              func() time.Time { return clock },
		Probe:            30 * time.Second,
		NotRebootedAfter: 3 * time.Minute,
		Interval:         10 * time.Second,
		Timeout:          20 * time.Minute,
	}, &out
}

func TestNodeReboot(t *testing.T) {
	t.Run("backs up, reboots, waits for the new boot and a healthy cluster, then reports", func(t *testing.T) {
		h := &rebootingHost{bootID: "old-boot", downReads: 6, unhealthyReads: 3, confirm: true}
		r, out := h.reboot(t)

		require.NoError(t, r.run("example.com", false, false))
		assert.Equal(t, []string{"confirm", "backup", "list apps", "reboot", "report"}, h.calls)
		assert.Contains(t, out.String(), "✔  Backup before-reboot-20261011-010000 completed")
		assert.Contains(t, out.String(), "✔  example.com is back")
		assert.Contains(t, out.String(), "✔  Every node and component is ready")
	})

	t.Run("does nothing when the operator declines", func(t *testing.T) {
		h := &rebootingHost{bootID: "old-boot", confirm: false}
		r, _ := h.reboot(t)

		require.NoError(t, r.run("example.com", false, false))
		assert.Equal(t, []string{"confirm"}, h.calls)
	})

	t.Run("skips the prompt with --yes and the backup with --skip-backup", func(t *testing.T) {
		h := &rebootingHost{bootID: "old-boot"}
		r, _ := h.reboot(t)

		require.NoError(t, r.run("example.com", true, true))
		assert.Equal(t, []string{"list apps", "reboot", "report"}, h.calls)
	})

	t.Run("does not reboot when the backup fails", func(t *testing.T) {
		h := &rebootingHost{bootID: "old-boot", confirm: true, backupErr: errors.New("backup PartiallyFailed")}
		r, _ := h.reboot(t)

		err := r.run("example.com", false, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "--skip-backup")
		assert.NotContains(t, h.calls, "reboot")
	})

	t.Run("does not reboot a host it cannot reach", func(t *testing.T) {
		h := &rebootingHost{confirm: true}
		r, _ := h.reboot(t)
		r.BootID = func(ssh.Config) (string, error) { return "", errors.New("permission denied (publickey)") }

		err := r.run("example.com", true, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "publickey")
		assert.Empty(t, h.calls)
	})

	t.Run("fails when the host does not come back in time", func(t *testing.T) {
		h := &rebootingHost{bootID: "old-boot", downReads: 1000}
		r, _ := h.reboot(t)

		err := r.run("example.com", true, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not come back within 20m0s")
		assert.NotContains(t, h.calls, "report")
	})

	t.Run("names what is still not ready when the cluster does not recover in time", func(t *testing.T) {
		h := &rebootingHost{bootID: "old-boot", unhealthyReads: 1000}
		r, _ := h.reboot(t)

		err := r.run("example.com", true, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Loki 0/1 ready")
		assert.Contains(t, h.calls, "report", "the host checks still run so the operator sees the host's state")
	})

	t.Run("waits for a changed boot id, not just a reachable host", func(t *testing.T) {
		h := &rebootingHost{bootID: "old-boot"}
		r, out := h.reboot(t)
		r.Reboot = func(ssh.Config) (time.Time, error) { h.calls = append(h.calls, "reboot"); return serverClock, nil }

		err := r.run("example.com", true, true)
		require.Error(t, err, "a host that never rebooted must not count as back")
		assert.False(t, strings.Contains(out.String(), "is back"))
	})
}

func TestNodeRebootWaitsForTheAppsThatWereServing(t *testing.T) {
	t.Run("waits until every app serving before the reboot serves again", func(t *testing.T) {
		h := &rebootingHost{bootID: "old-boot", apps: []appRef{{Namespace: "shop", Name: "api"}}, startingReads: 3}
		r, out := h.reboot(t)

		require.NoError(t, r.run("example.com", true, true))
		assert.Len(t, h.servingSince, 4)
		assert.Contains(t, out.String(), "Waiting for 1 app to become ready again")
		assert.Contains(t, out.String(), "✔  All previously ready apps are ready again")
		for _, since := range h.servingSince {
			assert.Equal(t, serverClock, since, "readiness is judged against the server's clock at the reboot")
		}
	})

	t.Run("names the apps still starting when time runs out", func(t *testing.T) {
		h := &rebootingHost{bootID: "old-boot", apps: []appRef{{Namespace: "shop", Name: "api"}}, startingReads: 1000}
		r, _ := h.reboot(t)

		err := r.run("example.com", true, true)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "shop/api")
		assert.Contains(t, err.Error(), "kip app list")
		assert.Contains(t, h.calls, "report", "the host checks still run so the operator sees the host's state")
	})

	t.Run("reboots anyway, and says so, when the apps cannot be listed", func(t *testing.T) {
		h := &rebootingHost{bootID: "old-boot", appsErr: errors.New("forbidden")}
		r, out := h.reboot(t)

		require.NoError(t, r.run("example.com", true, true))
		assert.Contains(t, out.String(), "could not list apps (forbidden)")
		assert.Empty(t, h.servingSince)
	})
}

func TestNodeRebootWritesTheHostChecksToItsOutput(t *testing.T) {
	h := &rebootingHost{bootID: "old-boot"}
	r, out := h.reboot(t)

	require.NoError(t, r.run("example.com", true, true))
	assert.Contains(t, out.String(), "Host checks")
}

func TestAppsNotServingSince(t *testing.T) {
	since := time.Date(2026, 10, 11, 1, 0, 0, 0, time.UTC)
	pod := func(ns, app string, ready bool, at time.Time) corev1.Pod {
		status := corev1.ConditionFalse
		if ready {
			status = corev1.ConditionTrue
		}
		return corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Labels: map[string]string{"app": app}},
			Status: corev1.PodStatus{Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: status, LastTransitionTime: metav1.NewTime(at),
			}}},
		}
	}
	apps := []appRef{{Namespace: "shop", Name: "api"}, {Namespace: "shop", Name: "web"}, {Namespace: "blog", Name: "api"}}

	terminating := pod("shop", "cart", true, since.Add(time.Minute))
	terminating.DeletionTimestamp = &metav1.Time{Time: since.Add(2 * time.Minute)}
	apps = append(apps, appRef{Namespace: "shop", Name: "cart"})

	waiting := appsNotServingSince(apps, []corev1.Pod{
		pod("shop", "api", true, since.Add(time.Minute)),
		pod("shop", "web", true, since.Add(-time.Hour)),
		pod("blog", "api", false, since.Add(time.Minute)),
		terminating,
		pod("shop", "cart", false, since.Add(2*time.Minute)),
	}, since)

	assert.Equal(t, []string{"shop/web", "blog/api", "shop/cart"}, waiting,
		"readiness from before the reboot and a pod on its way out do not count, and namespaces stay apart")
}

func TestScheduledAt(t *testing.T) {
	at, err := scheduledAt("kip-server-time=1791680000\nRunning timer as unit: kip-node-reboot-1791680000.timer\n")
	require.NoError(t, err)
	assert.Equal(t, time.Unix(1791680000, 0), at)

	at, err = scheduledAt("Running timer as unit: kip-node-reboot-1791680000.timer\nkip-server-time=1791680000\n")
	require.NoError(t, err, "stderr can arrive before stdout")
	assert.Equal(t, time.Unix(1791680000, 0), at)

	_, err = scheduledAt("Failed to start transient timer unit\n")
	require.Error(t, err)
}

func TestServingApps(t *testing.T) {
	app := func(ns, name string, ready int64) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1", "kind": "App",
			"metadata": map[string]interface{}{"name": name, "namespace": ns},
			"status":   map[string]interface{}{"readyReplicas": ready},
		}}
		return u
	}
	scheme := runtime.NewScheme()
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(scheme,
		map[schema.GroupVersionResource]string{deployer.AppGVR: "AppList"},
		app("shop", "api", 2), app("shop", "stopped", 0), app("blog", "web", 1))

	apps, err := servingApps(context.Background(), dyn)
	require.NoError(t, err)
	assert.ElementsMatch(t, []appRef{{Namespace: "shop", Name: "api"}, {Namespace: "blog", Name: "web"}}, apps)
}

func TestNodeRebootUsesOneConnectionSetupThroughout(t *testing.T) {
	h := &rebootingHost{bootID: "old-boot", downReads: 2}
	r, _ := h.reboot(t)

	require.NoError(t, r.run("example.com", true, true))
	require.NotEmpty(t, h.configs)
	for _, cfg := range h.configs {
		assert.Equal(t, "/keys/current", cfg.KeyPath, "every connection, including the final report, uses the chosen key")
	}
	assert.Contains(t, h.calls, "report")
}

func TestWaitForBackupPhase(t *testing.T) {
	phases := func(seq ...string) func(context.Context) (string, error) {
		i := 0
		return func(context.Context) (string, error) {
			p := seq[min(i, len(seq)-1)]
			i++
			return p, nil
		}
	}

	t.Run("returns once the backup completes", func(t *testing.T) {
		require.NoError(t, waitForBackupPhase(context.Background(), "b", phases("New", "InProgress", "Completed"), time.Millisecond))
	})

	t.Run("names a failed phase", func(t *testing.T) {
		err := waitForBackupPhase(context.Background(), "b", phases("InProgress", "PartiallyFailed"), time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ended PartiallyFailed")
	})

	t.Run("gives up when a read stalls past the deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		stalled := func(ctx context.Context) (string, error) { <-ctx.Done(); return "", ctx.Err() }

		err := waitForBackupPhase(ctx, "b", stalled, time.Millisecond)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "did not complete in time")
		assert.Contains(t, err.Error(), "context deadline exceeded")
	})
}

func TestRebootBoundsEverySSHCommand(t *testing.T) {
	cfg := rebootSSHConfig(&config.Cluster{Host: "203.0.113.10"}, "/keys/override")
	assert.Equal(t, 30*time.Second, cfg.CommandTimeout, "a server still starting can accept a session and never answer it")
	assert.Equal(t, "/keys/override", cfg.KeyPath)
}

func TestHostSSHConfigHonoursTheKeyFlag(t *testing.T) {
	cluster := &config.Cluster{Host: "203.0.113.10", SSHKey: "/keys/configured"}
	cfg := hostSSHConfig(cluster, "/keys/override")
	assert.Equal(t, "/keys/override", cfg.KeyPath)
	assert.Equal(t, "root", cfg.User)
	assert.Contains(t, cfg.Options, "BatchMode=yes")
	assert.Equal(t, "/keys/configured", hostSSHConfig(cluster, "").KeyPath)
}

func TestNodeRebootChecksHealthAgainstTheNewBoot(t *testing.T) {
	h := &rebootingHost{bootID: "old-boot", downReads: 2}
	r, _ := h.reboot(t)

	require.NoError(t, r.run("example.com", true, true))
	require.NotEmpty(t, h.healthBootIDs)
	for _, id := range h.healthBootIDs {
		assert.Equal(t, "new-boot", id, "readiness must be judged against the boot the reboot produced")
	}
}

func TestNodeRebootNeedsHealthTwiceInARow(t *testing.T) {
	h := &rebootingHost{bootID: "old-boot"}
	r, _ := h.reboot(t)
	answers := []bool{true, false, true, true}
	r.Healthy = func(time.Duration, string) (bool, string) {
		ok := answers[0]
		answers = answers[1:]
		return ok, "Loki 0/1 ready"
	}

	require.NoError(t, r.run("example.com", true, true))
	assert.Empty(t, answers, "one healthy read followed by an unhealthy one must not count as recovered")
}

func TestNodeRebootStopsEarlyWhenTheServerDidNotReboot(t *testing.T) {
	h := &rebootingHost{bootID: "old-boot"}
	r, _ := h.reboot(t)
	r.Reboot = func(ssh.Config) (time.Time, error) { h.calls = append(h.calls, "reboot"); return serverClock, nil }
	start := r.Now()

	err := r.run("example.com", true, true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not reboot")
	assert.Less(t, r.Now().Sub(start), 5*time.Minute, "an unchanged boot id is reported within minutes, not after the full timeout")
}

func TestReadyAfterBoot(t *testing.T) {
	healthy := []k8s.ComponentStatus{{Name: "Traefik", Healthy: true}}

	ok, why := readyAfterBoot([]k8s.NodeInfo{{Name: "n1", Status: "Ready", BootID: "old-boot"}}, healthy, "new-boot")
	assert.False(t, ok, "a node still reporting the old boot shows status from before the reboot")
	assert.Contains(t, why, "has not reported the new boot")

	ok, why = readyAfterBoot([]k8s.NodeInfo{{Name: "n1", Status: "Ready", BootID: "new-boot"}}, []k8s.ComponentStatus{{Name: "Loki", Message: "0/1 ready"}}, "new-boot")
	assert.False(t, ok)
	assert.Equal(t, "Loki 0/1 ready", why)

	ok, _ = readyAfterBoot([]k8s.NodeInfo{{Name: "n1", Status: "Ready", BootID: "new-boot"}}, healthy, "new-boot")
	assert.True(t, ok)
}

func TestRebootCommandUsesAUnitNameOfItsOwn(t *testing.T) {
	first := rebootCommand(time.Unix(1000, 0))
	second := rebootCommand(time.Unix(2000, 0))
	assert.NotEqual(t, first, second, "a failed earlier unit must not block the next reboot")
	assert.Contains(t, first, "systemd-run --on-active=3 --unit=kip-node-reboot-1000 systemctl reboot")
}
