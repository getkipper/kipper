package cmd

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/controller/pkg/capacity"
	"github.com/getkipper/kipper/kip/internal/deployer"
)

func i32p(v int32) *int32 { return &v }

func TestAutoscaleStatusLines(t *testing.T) {
	t.Run("policy on shows desired as set by autoscaling, the bounds and the metrics", func(t *testing.T) {
		out := strings.Join(autoscaleStatusLines("web", deployer.AutoscaleStatus{
			Policy:        &capacity.Policy{Enabled: true, MinReplicas: i32p(2), MaxReplicas: i32p(5), CPUTarget: i32p(70)},
			Replicas:      2,
			LiveDesired:   i32p(4),
			Ready:         i32p(4),
			HPAExists:     true,
			CurrentMetric: map[string]int32{"cpu": 12},
		}), "\n")
		assert.Contains(t, out, "Autoscaling: on")
		assert.Contains(t, out, "Desired: 4 (set by autoscaling)")
		assert.Contains(t, out, "Min: 2")
		assert.Contains(t, out, "Max: 5")
		assert.Contains(t, out, "cpu: target 70%, current 12%")
	})

	t.Run("policy off keeps the bounds visible", func(t *testing.T) {
		out := strings.Join(autoscaleStatusLines("web", deployer.AutoscaleStatus{
			Policy:      &capacity.Policy{MinReplicas: i32p(2), MaxReplicas: i32p(5), CPUTarget: i32p(70)},
			Replicas:    3,
			LiveDesired: i32p(3),
			Ready:       i32p(3),
		}), "\n")
		assert.Contains(t, out, "Autoscaling: off")
		assert.Contains(t, out, "Desired: 3")
		assert.NotContains(t, out, "set by autoscaling")
		assert.Contains(t, out, "Min: 2")
		assert.Contains(t, out, "bounds apply to the desired count")
	})

	t.Run("no block and no live data", func(t *testing.T) {
		out := strings.Join(autoscaleStatusLines("web", deployer.AutoscaleStatus{Replicas: 1}), "\n")
		assert.Contains(t, out, "Autoscaling: off")
		assert.Contains(t, out, "Desired: 1")
		assert.Contains(t, out, "Ready: unknown")
		assert.NotContains(t, out, "Min:")
	})

	t.Run("a condition that is not True is shown with its reason and message", func(t *testing.T) {
		out := strings.Join(autoscaleStatusLines("web", deployer.AutoscaleStatus{
			Policy:    &capacity.Policy{MinReplicas: i32p(1), MaxReplicas: i32p(5)},
			Replicas:  8,
			Condition: &deployer.AutoscalingCondition{Status: "False", Reason: "ReplicasOutsideBounds", Message: "replicas 8 is outside 1 to 5"},
		}), "\n")
		assert.Contains(t, out, "⚠  Autoscaling is not ready (ReplicasOutsideBounds): replicas 8 is outside 1 to 5")
	})

	t.Run("a True condition adds nothing", func(t *testing.T) {
		out := strings.Join(autoscaleStatusLines("web", deployer.AutoscaleStatus{
			Policy:    &capacity.Policy{Enabled: true, MaxReplicas: i32p(5), CPUTarget: i32p(70)},
			Replicas:  2,
			Condition: &deployer.AutoscalingCondition{Status: "True", Reason: "AutoscalerReady"},
		}), "\n")
		assert.NotContains(t, out, "not ready")
	})

	t.Run("a stopped app and an autoscaler the spec does not describe are named", func(t *testing.T) {
		out := strings.Join(autoscaleStatusLines("web", deployer.AutoscaleStatus{Replicas: 2, Stopped: true, HPAExists: true}), "\n")
		assert.Contains(t, out, "web is stopped")
		assert.Contains(t, out, "an autoscaler exists that the app's spec does not describe")
	})
}

func TestAutoscaleModePrecedence(t *testing.T) {
	assert.Equal(t, modeStatus, autoscaleMode(true, true, false), "--status --off stays read-only, as released")
	assert.Equal(t, modeOff, autoscaleMode(false, true, false), "--off with other flags only switches off, as released")
	assert.Equal(t, modeOffAndRemove, autoscaleMode(false, true, true))
	assert.Equal(t, modeRemove, autoscaleMode(false, false, true))
	assert.Equal(t, modeEdit, autoscaleMode(false, false, false))
}

func TestAutoscaledReplicasNote(t *testing.T) {
	note := autoscaledReplicasNote("api", 3)
	assert.Contains(t, note, "has no effect while autoscaling is on")
	assert.Contains(t, note, "'kip app autoscale api --off' keeps the count the autoscaler last set")
}

func TestIgnoredEditFlagsNotice(t *testing.T) {
	flags := pflag.NewFlagSet("autoscale", pflag.ContinueOnError)
	for _, name := range []string{"min", "max", "cpu", "memory"} {
		flags.Int32(name, 0, "")
	}
	require.NoError(t, flags.Parse([]string{"--max", "6", "--min", "2"}))
	edits := changedEditFlags(flags)
	assert.Equal(t, []string{"--min 2", "--max 6"}, edits)

	off := ignoredEditFlagsNotice("web", modeOff, edits)
	assert.Contains(t, off, "Ignored --min 2 --max 6, because --off only switches autoscaling off")
	assert.Contains(t, off, "run 'kip app autoscale web --min 2 --max 6' first, then 'kip app autoscale web --off'")

	for _, mode := range []autoscaleModeKind{modeRemove, modeOffAndRemove} {
		assert.Contains(t, ignoredEditFlagsNotice("web", mode, edits), "Ignored --min 2 --max 6, because --remove deletes the bounds and targets")
	}

	assert.Empty(t, ignoredEditFlagsNotice("web", modeOff, nil))
	assert.Empty(t, changedEditFlags(pflag.NewFlagSet("empty", pflag.ContinueOnError)))
}

// Switching off has already happened when this prints, and --min or --max
// would switch autoscaling back on, so the advice names --remove.
func TestInvalidBoundsWarningPointsAtRemove(t *testing.T) {
	warning := invalidBoundsWarning("web")
	assert.Contains(t, warning, "The stored bounds are invalid")
	assert.Contains(t, warning, "'kip app autoscale web --remove'")
	assert.NotContains(t, warning, "--min")
	assert.NotContains(t, warning, "--max")
}

func TestSwitchOffErrorAdvisesHowToFixAnInvalidBlock(t *testing.T) {
	refusal := errors.New(`App.kipper.run "api" is invalid: spec.autoscale: Invalid value: "object": set maxReplicas with minReplicas, or leave both out to remove the bounds`)

	t.Run("a minimum without a maximum is fixed by setting the maximum", func(t *testing.T) {
		err := switchOffError("api", &deployer.InvalidPolicyRefusal{
			Policy: capacity.Policy{Enabled: true, MinReplicas: i32p(3), CPUTarget: i32p(70)},
			Err:    refusal,
		})

		require.ErrorIs(t, err, refusal)
		assert.Contains(t, err.Error(), "switching autoscaling off: ")
		assert.Contains(t, err.Error(), "Fix it with 'kip app autoscale api --max 5', then switch off")
	})

	t.Run("any other failure is reported as it is", func(t *testing.T) {
		err := switchOffError("api", refusal)

		require.ErrorIs(t, err, refusal)
		assert.Equal(t, "switching autoscaling off: "+refusal.Error(), err.Error())
	})
}
