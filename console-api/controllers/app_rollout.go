package controllers

import (
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/version"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

const (
	defaultStartupTimeoutSeconds = 300
	minProgressDeadlineSeconds   = 600
	progressDeadlineSlackSeconds = 300
)

// appStartupTimeoutSeconds sets the age after which a running, unready pod
// is reported as stuck.
func appStartupTimeoutSeconds(app *kipperv1.App) int32 {
	if app.Spec.Health != nil && app.Spec.Health.StartupTimeoutSeconds != nil {
		return *app.Spec.Health.StartupTimeoutSeconds
	}
	return defaultStartupTimeoutSeconds
}

func appStartupTimeout(app *kipperv1.App) time.Duration {
	return time.Duration(appStartupTimeoutSeconds(app)) * time.Second
}

// appProgressDeadline allows five minutes beyond the startup timeout, with
// a minimum of ten minutes, so startup diagnostics can appear first.
func appProgressDeadline(app *kipperv1.App) int32 {
	return max(appStartupTimeoutSeconds(app)+progressDeadlineSlackSeconds, minProgressDeadlineSeconds)
}

// ensureProgressDeadline reports whether it changed the deadline. This field
// is outside the pod template, so changing it does not trigger a rollout.
func ensureProgressDeadline(dep *appsv1.Deployment, deadline int32) bool {
	if dep.Spec.ProgressDeadlineSeconds != nil && *dep.Spec.ProgressDeadlineSeconds == deadline {
		return false
	}
	dep.Spec.ProgressDeadlineSeconds = &deadline
	return true
}

// SupportsPreStopSleep uses the server version to decide whether to enable
// the preStop sleep action. An unreadable version leaves the action disabled.
func SupportsPreStopSleep(info *version.Info, err error) bool {
	if err != nil || info == nil {
		return false
	}
	major, errMajor := strconv.Atoi(info.Major)
	minor, errMinor := strconv.Atoi(strings.TrimSuffix(info.Minor, "+"))
	if errMajor != nil || errMinor != nil {
		return false
	}
	return major > 1 || (major == 1 && minor >= 30)
}
