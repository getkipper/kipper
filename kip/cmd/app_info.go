package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/getkipper/kipper/controller/pkg/capacity"
	"github.com/getkipper/kipper/controller/pkg/provenance"
	"github.com/getkipper/kipper/kip/internal/deployer"
)

var appInfoCmd = &cobra.Command{
	Use:   "info [app-name]",
	Short: "Show an app's status, scaling, and resource settings",
	Args:  cobra.ExactArgs(1),
	RunE:  runAppInfo,
}

func init() {
	appInfoCmd.Flags().String("project", "", "project name")
	appInfoCmd.Flags().String("environment", "", "target environment")
	appCmd.AddCommand(appInfoCmd)
}

func runAppInfo(cmd *cobra.Command, args []string) error {
	appName := args[0]
	ns, k8sClient, err := resolveAppNamespace(cmd, appName)
	if err != nil {
		return err
	}
	d := &deployer.Deployer{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	info, err := d.ReadAppInfo(context.Background(), ns, appName)
	if err != nil {
		return err
	}
	writeAppInfo(os.Stdout, appName, info, time.Now())
	return nil
}

func writeAppInfo(out io.Writer, name string, info deployer.AppInfo, now time.Time) {
	say(out, "\n  App: %s\n\n", name)
	say(out, "  Status:  %s, %d/%d ready\n", info.Status.Status, info.Status.Ready, info.Status.Replicas)
	say(out, "  Image:   %s\n\n", info.Status.Image)

	for _, line := range autoscaleStatusLines(name, info.Autoscale) {
		say(out, "%s\n", line)
	}
	if t := info.Autoscale.LastScaleTime; t != nil {
		say(out, "  Last scaled: %s (%s ago)\n", t.UTC().Format("2006-01-02 15:04 MST"), humanAge(now, *t))
	}

	say(out, "\n  Resources (profile %s, from the pod template)\n", info.Profile)
	tracks := func(resource string, target func(*capacity.Policy) *int32) tracking {
		return tracked(info.Autoscale, resource, target)
	}
	writeResource(out, "CPU:", info.CPU, info.Template, cpuNote(info.CPU, tracks("cpu", func(p *capacity.Policy) *int32 { return p.CPUTarget })))
	writeResource(out, "Memory:", info.Memory, info.Template, memoryNote(info.Memory, tracks("memory", func(p *capacity.Policy) *int32 { return p.MemoryTarget })))
	if info.Status.RolloutWaiting != "" {
		say(out, "  Rollout incomplete: some pods may still use earlier values.\n")
	}
	say(out, "\n")
}

func writeResource(out io.Writer, label string, r deployer.ResourceInfo, template deployer.TemplateState, note string) {
	switch template {
	case deployer.TemplateMissing:
		say(out, "  %-9snot deployed yet\n", label)
	case deployer.TemplateUnreadable:
		say(out, "  %-9sunknown (the Deployment could not be read)\n", label)
	default:
		say(out, "  %-9srequest %s, limit %s\n", label, orNotSet(r.LiveRequest), orNotSet(r.LiveLimit))
	}
	say(out, "           %s\n", note)
}

func orNotSet(v string) string {
	if v == "" {
		return "not set"
	}
	return v
}

type tracking int

const (
	untracked tracking = iota
	trackedByAutoscaler
	trackingUnknown
)

// tracked follows the controller's sizing rules. An invalid enabled policy
// retains the existing autoscaler, so its spec determines the tracked resources.
func tracked(st deployer.AutoscaleStatus, resource string, target func(*capacity.Policy) *int32) tracking {
	p := st.Policy
	if p == nil || !p.Enabled {
		return untracked
	}
	if p.Usable() {
		if t := target(p); t != nil && *t > 0 {
			return trackedByAutoscaler
		}
		return untracked
	}
	if st.HPAUnreadable {
		return trackingUnknown
	}
	if st.HPAExists && st.HPAMetrics[resource] {
		return trackedByAutoscaler
	}
	return untracked
}

func cpuNote(r deployer.ResourceInfo, t tracking) string {
	return trackedNote(r, t, "CPU", "the autoscaler tracks CPU, so Kipper leaves the CPU request alone")
}

func memoryNote(r deployer.ResourceInfo, t tracking) string {
	return trackedNote(r, t, "memory", "the autoscaler tracks memory, so Kipper only raises it after an out-of-memory kill")
}

func trackedNote(r deployer.ResourceInfo, t tracking, name, trackedText string) string {
	if r.Mode != provenance.ModeAutomatic && r.Mode != provenance.ModeBounded {
		return sizingNote(r)
	}
	switch t {
	case trackedByAutoscaler:
		return trackedText
	case trackingUnknown:
		return name + " sizing unknown: permission denied when reading the autoscaler"
	}
	return sizingNote(r)
}

func sizingNote(r deployer.ResourceInfo) string {
	switch r.Mode {
	case provenance.ModeBounded:
		return fmt.Sprintf("Kipper adjusts the request between %s and %s", r.Request.Value, r.Limit.Value)
	case provenance.ModeFixed:
		return "fixed at the size you set"
	case provenance.ModeHeld:
		return "kept until you set them; original ownership is unknown"
	}
	note := "sized by Kipper"
	differsRequest := r.RecommendedRequest != "" && !sameSize(r.RecommendedRequest, r.LiveRequest)
	differsLimit := r.RecommendedLimit != "" && !sameSize(r.RecommendedLimit, r.LiveLimit)
	if differsRequest || differsLimit {
		note += fmt.Sprintf("; latest recommendation: request %s, limit %s", orLive(r.RecommendedRequest, r.LiveRequest), orLive(r.RecommendedLimit, r.LiveLimit))
	}
	return note
}

func orLive(recommended, live string) string {
	if recommended == "" {
		return live
	}
	return recommended
}

// sameSize compares two resource values as quantities, so 2048Mi equals 2Gi.
func sameSize(a, b string) bool {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return qa.Cmp(qb) == 0
}
