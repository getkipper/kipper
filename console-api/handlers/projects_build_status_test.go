package handlers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func summaryFor(t *testing.T, app *kipperv1.App) appSummary {
	t.Helper()
	p := &Projects{CRClient: testCRClient(app)}
	apps := p.getAppSummaries(context.Background(), app.Namespace, "prod")
	require.Len(t, apps, 1)
	return apps[0]
}

func TestGetAppSummaries_BuildStatus(t *testing.T) {
	base := func(build *kipperv1.AppBuildStatus) *kipperv1.App {
		return &kipperv1.App{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "blog"},
			Spec:       kipperv1.AppSpec{Image: "api:v1"},
			Status:     kipperv1.AppStatus{Phase: "Pending", Build: build},
		}
	}

	t.Run("an in-flight build surfaces as building, not the placeholder pending", func(t *testing.T) {
		assert.Equal(t, "building", summaryFor(t, base(&kipperv1.AppBuildStatus{Phase: "Building"})).Status)
	})

	t.Run("a queued build (Pending) also surfaces as building", func(t *testing.T) {
		assert.Equal(t, "building", summaryFor(t, base(&kipperv1.AppBuildStatus{Phase: "Pending"})).Status)
	})

	t.Run("a rebuild of a running app shows building, not running", func(t *testing.T) {
		app := base(&kipperv1.AppBuildStatus{Phase: "Building"})
		app.Status.Phase = "Running" // old pods still serving during the rebuild
		assert.Equal(t, "building", summaryFor(t, app).Status, "a rebuild in progress takes precedence over the serving phase")
	})

	t.Run("a finished build falls back to the workload phase", func(t *testing.T) {
		app := base(&kipperv1.AppBuildStatus{Phase: "Succeeded"})
		app.Status.Phase = "Running"
		assert.Equal(t, "running", summaryFor(t, app).Status)
	})

	t.Run("no build uses the workload phase", func(t *testing.T) {
		app := base(nil)
		app.Status.Phase = "Running"
		assert.Equal(t, "running", summaryFor(t, app).Status)
	})
}
