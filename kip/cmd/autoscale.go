package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/getkipper/kipper/kip/internal/deployer"
)

var appAutoscaleCmd = &cobra.Command{
	Use:   "autoscale [app-name]",
	Short: "Configure automatic scaling for an app",
	Long: `Configure horizontal pod autoscaling based on CPU and memory usage.

The minimum and maximum are bounds for the app's replica count, whether the
autoscaler or you set it. While autoscaling is on, the autoscaler sets the
count. Switching it off keeps the count it last set, and the bounds keep
applying to 'kip app scale' until you remove them with --remove.

Flags you leave out keep their stored values. On a first enable the minimum
is 1, the maximum 5, and CPU 70% when no target is given. A target of 0
removes that target. --off and --remove ignore --min, --max, --cpu and --memory
and say so; to change a setting and switch off, run the change first.

Examples:
  kip app autoscale api --min 1 --max 5 --cpu 70
  kip app autoscale api --min 2 --max 10 --cpu 80 --memory 80
  kip app autoscale api --max 8
  kip app autoscale api --status
  kip app autoscale api --off
  kip app autoscale api --remove`,
	Args: cobra.ExactArgs(1),
	RunE: runAutoscale,
}

func init() {
	appAutoscaleCmd.Flags().Int32("min", 0, "minimum number of replicas (1 on a first enable)")
	appAutoscaleCmd.Flags().Int32("max", 0, "maximum number of replicas (5 on a first enable)")
	appAutoscaleCmd.Flags().Int32("cpu", 0, "target CPU utilization percentage (e.g. 70); 0 removes the target")
	appAutoscaleCmd.Flags().Int32("memory", 0, "target memory utilization percentage (e.g. 80); 0 removes the target")
	appAutoscaleCmd.Flags().Bool("status", false, "show current autoscaling status and any autoscaling problem")
	appAutoscaleCmd.Flags().Bool("off", false, "switch autoscaling off, keeping the current replica count and the bounds")
	appAutoscaleCmd.Flags().Bool("remove", false, "remove the bounds from an app whose autoscaling is off")
	appAutoscaleCmd.Flags().String("project", "", "project name")
	appAutoscaleCmd.Flags().String("environment", "", "target environment")

	appCmd.AddCommand(appAutoscaleCmd)
}

func runAutoscale(cmd *cobra.Command, args []string) error {
	appName := args[0]
	showStatus, _ := cmd.Flags().GetBool("status")
	off, _ := cmd.Flags().GetBool("off")
	remove, _ := cmd.Flags().GetBool("remove")

	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}
	ctx := context.Background()
	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}

	mode := autoscaleMode(showStatus, off, remove)
	if notice := ignoredEditFlagsNotice(appName, mode, changedEditFlags(cmd.Flags())); notice != "" {
		fmt.Println(notice)
	}
	switch mode {
	case modeStatus:
		st, err := d.ReadAutoscaleStatus(ctx, ns, appName)
		if err != nil {
			return err
		}
		fmt.Printf("\n%s\n\n", strings.Join(autoscaleStatusLines(appName, st), "\n"))
		return nil

	case modeOff, modeOffAndRemove:
		res, err := d.DisableAutoscale(ctx, ns, appName)
		if err != nil {
			return switchOffError(appName, err)
		}
		switch {
		case res.AlreadyOff:
			fmt.Printf("  Autoscaling is not on for %s\n", appName)
		case res.Stopped:
			fmt.Printf("  ✔  Autoscaling switched off for %s\n", appName)
			fmt.Printf("  %s is stopped; it runs %d replicas when started\n", appName, res.Replicas)
		case res.CountUnknown:
			fmt.Printf("  ✔  Autoscaling switched off for %s\n", appName)
			fmt.Printf("  ⚠  The running count could not be read, so the stored count of %d applies\n", res.Replicas)
		case res.InvalidBounds:
			fmt.Printf("  ✔  Autoscaling switched off for %s; it keeps running %d replicas\n", appName, res.Replicas)
			fmt.Println(invalidBoundsWarning(appName))
		case res.Live != res.Replicas:
			fmt.Printf("  ✔  Autoscaling switched off for %s\n", appName)
			fmt.Printf("  It was running %d replicas; the count is now %d to stay within the bounds\n", res.Live, res.Replicas)
		default:
			fmt.Printf("  ✔  Autoscaling switched off for %s; it keeps running %d replicas\n", appName, res.Replicas)
		}
		if mode == modeOff {
			return nil
		}
		fallthrough

	case modeRemove:
		if err := d.RemoveAutoscale(ctx, ns, appName); err != nil {
			return err
		}
		fmt.Printf("  ✔  Bounds removed from %s\n", appName)
		return nil
	}

	var change deployer.AutoscaleChange
	for _, f := range []struct {
		name string
		dst  **int32
	}{
		{"min", &change.MinReplicas}, {"max", &change.MaxReplicas},
		{"cpu", &change.CPUTarget}, {"memory", &change.MemoryTarget},
	} {
		if cmd.Flags().Changed(f.name) {
			v, _ := cmd.Flags().GetInt32(f.name)
			*f.dst = &v
		}
	}

	res, err := d.EnableAutoscale(ctx, ns, appName, change)
	if err != nil {
		return fmt.Errorf("configuring autoscaling: %w", err)
	}

	lo, hi, _ := res.Policy.Bounds()
	fmt.Printf("\n  ✔  Autoscaling enabled for %s\n", appName)
	if res.Stopped {
		fmt.Printf("  %s is stopped; autoscaling takes over when it is started\n", appName)
	}
	fmt.Printf("  Replicas: %d–%d\n", lo, hi)
	if res.ReplicasMoved != nil {
		fmt.Printf("  Desired count moved from %d to %d to stay within the bounds\n", res.ReplicasMoved[0], res.ReplicasMoved[1])
	}
	if t := res.Policy.CPUTarget; t != nil && *t > 0 {
		fmt.Printf("  CPU target: %d%%\n", *t)
	}
	if t := res.Policy.MemoryTarget; t != nil && *t > 0 {
		fmt.Printf("  Memory target: %d%%\n", *t)
	}
	fmt.Println()
	return nil
}

// autoscaleStatusLines shows the Deployment’s desired count while autoscaling
// is enabled and the stored App count otherwise. Missing observations appear as unknown.
func autoscaleStatusLines(name string, st deployer.AutoscaleStatus) []string {
	p := st.Policy
	on := p != nil && p.Enabled
	var lines []string

	if on {
		var targets []string
		if p.CPUTarget != nil && *p.CPUTarget > 0 {
			targets = append(targets, fmt.Sprintf("CPU %d%%", *p.CPUTarget))
		}
		if p.MemoryTarget != nil && *p.MemoryTarget > 0 {
			targets = append(targets, fmt.Sprintf("memory %d%%", *p.MemoryTarget))
		}
		lines = append(lines, fmt.Sprintf("  Autoscaling: on (%s)", strings.Join(targets, ", ")))
	} else {
		lines = append(lines, "  Autoscaling: off")
	}

	desired := fmt.Sprintf("  Desired: %d", st.Replicas)
	if on {
		desired = "  Desired: unknown (set by autoscaling)"
		if st.LiveDesired != nil {
			desired = fmt.Sprintf("  Desired: %d (set by autoscaling)", *st.LiveDesired)
		}
	}
	if lo, hi, ok := p.Bounds(); ok {
		desired += fmt.Sprintf("   Min: %d   Max: %d", lo, hi)
		if !on {
			desired += " (the bounds apply to the desired count)"
		}
	}
	lines = append(lines, desired)

	if st.Ready != nil {
		lines = append(lines, fmt.Sprintf("  Ready: %d", *st.Ready))
	} else {
		lines = append(lines, "  Ready: unknown")
	}

	if on {
		for _, metric := range []struct {
			name   string
			target *int32
		}{{"cpu", p.CPUTarget}, {"memory", p.MemoryTarget}} {
			if metric.target == nil || *metric.target == 0 {
				continue
			}
			current := "unknown"
			if v, ok := st.CurrentMetric[metric.name]; ok {
				current = fmt.Sprintf("%d%%", v)
			}
			lines = append(lines, fmt.Sprintf("  %s: target %d%%, current %s", metric.name, *metric.target, current))
		}
	}

	if c := st.Condition; c != nil && c.Status != "True" {
		lines = append(lines, fmt.Sprintf("  ⚠  Autoscaling is not ready (%s): %s", c.Reason, c.Message))
	}
	if st.Stopped {
		lines = append(lines, fmt.Sprintf("  %s is stopped; these settings apply when it is started", name))
	}
	if st.HPAExists && !on {
		lines = append(lines, "  ⚠  an autoscaler exists that the app's spec does not describe; Kipper removes it on the next reconcile when it is Kipper's own")
	}
	return lines
}

// switchOffError reports a failed switch-off. When the cluster refused it
// because the stored block breaks the rules, it names the command that makes
// the block valid, so the switch-off can be run again.
func switchOffError(appName string, err error) error {
	var invalid *deployer.InvalidPolicyRefusal
	if errors.As(err, &invalid) {
		advice := strings.TrimSuffix(invalidPolicyAdvice(appState{Name: appName, Policy: &invalid.Policy}), ".")
		return fmt.Errorf("switching autoscaling off: %w. %s, then switch off", err, advice)
	}
	return fmt.Errorf("switching autoscaling off: %w", err)
}

// invalidBoundsWarning follows a switch-off that kept invalid bounds.
func invalidBoundsWarning(command string) string {
	return "  ⚠  The stored bounds are invalid. " + disabledBoundsAdvice(command)
}

// changedEditFlags lists the policy flags given on the command line with
// their values, in the order the help shows them.
func changedEditFlags(flags *pflag.FlagSet) []string {
	var edits []string
	for _, name := range []string{"min", "max", "cpu", "memory"} {
		if f := flags.Lookup(name); f != nil && f.Changed {
			edits = append(edits, fmt.Sprintf("--%s %s", name, f.Value))
		}
	}
	return edits
}

// ignoredEditFlagsNotice names the policy flags that --off and --remove leave
// unapplied, and for --off the two commands that apply them and switch off.
func ignoredEditFlagsNotice(name string, mode autoscaleModeKind, edits []string) string {
	if len(edits) == 0 {
		return ""
	}
	given := strings.Join(edits, " ")
	switch mode {
	case modeOff:
		return fmt.Sprintf("  ⚠  Ignored %s, because --off only switches autoscaling off. To change them as well, run 'kip app autoscale %s %s' first, then 'kip app autoscale %s --off'.",
			given, name, given, name)
	case modeRemove, modeOffAndRemove:
		return fmt.Sprintf("  ⚠  Ignored %s, because --remove deletes the bounds and targets.", given)
	}
	return ""
}

type autoscaleModeKind int

const (
	modeEdit autoscaleModeKind = iota
	modeStatus
	modeOff
	modeRemove
	modeOffAndRemove
)

// autoscaleMode gives read-only status precedence over edits. Combining --off
// and --remove disables autoscaling before removing the policy.
func autoscaleMode(status, off, remove bool) autoscaleModeKind {
	switch {
	case status:
		return modeStatus
	case off && remove:
		return modeOffAndRemove
	case off:
		return modeOff
	case remove:
		return modeRemove
	}
	return modeEdit
}

func autoscaledReplicasNote(name string, replicas int) string {
	return fmt.Sprintf("  Autoscaling is on for %s, so the stored count of %d has no effect while autoscaling is on; 'kip app autoscale %s --off' keeps the count the autoscaler last set", name, replicas, name)
}
