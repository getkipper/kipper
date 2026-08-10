package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

const (
	platformConfigName   = "platform"
	platformConfigSchema = "platformconfigs"
)

var platformGVR = schema.GroupVersionResource{
	Group:    "kipper.run",
	Version:  "v1alpha1",
	Resource: platformConfigSchema,
}

var platformCmd = &cobra.Command{
	Use:   "platform",
	Short: "Inspect and tune Kipper's platform sizing (Prometheus, Loki, etc.)",
	Long: `Manage system component resources via the PlatformConfig CR.

The platform reconciler in console-api watches this CR and updates the
underlying HelmCharts so changes here propagate to the running pods.

Examples:
  kip platform status
  kip platform resize prometheus --memory 2Gi
  kip platform disable loki
  kip platform profile set large`,
}

var platformStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show the active platform profile and per-component state",
	RunE:  runPlatformStatus,
}

var platformResizeCmd = &cobra.Command{
	Use:   "resize <component>",
	Short: "Set a memory limit override for a platform component",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlatformResize,
}

var platformEnableCmd = &cobra.Command{
	Use:   "enable <component>",
	Short: "Re-enable a toggleable platform component (prometheus, loki)",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlatformToggle(true),
}

var platformDisableCmd = &cobra.Command{
	Use:   "disable <component>",
	Short: "Disable a toggleable platform component (prometheus, loki)",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlatformToggle(false),
}

var platformProfileCmd = &cobra.Command{
	Use:   "profile",
	Short: "Show or change the active sizing profile",
}

var platformProfileShowCmd = &cobra.Command{
	Use:   "show",
	Short: "Print the active sizing profile",
	RunE:  runPlatformProfileShow,
}

var platformProfileSetCmd = &cobra.Command{
	Use:   "set <name>",
	Short: "Change the active sizing profile (nano, small, medium, large, xlarge)",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlatformProfileSet,
}

var platformResizeMemory string

// restartTargets maps a user-facing component name to the workload that gets
// the rollout annotation. It covers the cluster components (console,
// console-api, dex, traefik) and the platform monitoring components, so one
// command restarts any of them.
var restartTargets = map[string]struct {
	Namespace string
	Kind      string
	Name      string
}{
	"console":     {"kipper-system", "Deployment", "console"},
	"console-api": {"kipper-system", "Deployment", "console-api"},
	"dex":         {"dex", "Deployment", "dex"},
	"traefik":     {"traefik", "Deployment", "traefik"},
	"prometheus":  {"monitoring", "StatefulSet", "prometheus-kube-prometheus-stack-prometheus"},
	"loki":        {"monitoring", "StatefulSet", "loki"},
	"grafana":     {"monitoring", "Deployment", "kube-prometheus-stack-grafana"},
	"promtail":    {"monitoring", "DaemonSet", "promtail"},
}

var platformRestartCmd = &cobra.Command{
	Use:   "restart <component>",
	Short: "Trigger a rolling restart of a platform or cluster component",
	Args:  cobra.ExactArgs(1),
	RunE:  runPlatformRestart,
}

func init() {
	platformResizeCmd.Flags().StringVar(&platformResizeMemory, "memory", "", "memory limit (e.g. 1Gi, 2Gi)")
	_ = platformResizeCmd.MarkFlagRequired("memory")

	platformProfileCmd.AddCommand(platformProfileShowCmd)
	platformProfileCmd.AddCommand(platformProfileSetCmd)

	platformCmd.AddCommand(platformStatusCmd)
	platformCmd.AddCommand(platformResizeCmd)
	platformCmd.AddCommand(platformEnableCmd)
	platformCmd.AddCommand(platformDisableCmd)
	platformCmd.AddCommand(platformRestartCmd)
	platformCmd.AddCommand(platformProfileCmd)

	rootCmd.AddCommand(platformCmd)
}

func runPlatformStatus(_ *cobra.Command, _ []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pc, err := getPlatformConfig(ctx, k8sClient.Dynamic())
	if err != nil {
		return err
	}
	profile, _, _ := unstructured.NestedString(pc.Object, "spec", "profile")
	fmt.Printf("\n  Platform profile: %s\n", profile)
	fmt.Printf("  %s\n\n", profileDescription(profile))

	overrides := overridesFromSpec(pc)
	statuses := statusesFromCR(pc)
	for _, name := range platform.SupportedComponents() {
		cs := statuses[name]
		ov := overrides[name]
		fmt.Printf("    %-12s %s\n", name, componentLine(name, profile, cs, ov))
	}
	fmt.Println()
	return nil
}

func runPlatformResize(_ *cobra.Command, args []string) error {
	component := args[0]
	if err := validateComponent(component); err != nil {
		return err
	}
	if _, err := resource.ParseQuantity(platformResizeMemory); err != nil {
		return fmt.Errorf("--memory %q is not a valid Kubernetes quantity: %w", platformResizeMemory, err)
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return mutatePlatformConfig(ctx, k8sClient.Dynamic(), func(pc *unstructured.Unstructured) error {
		return setComponentField(pc, component, "memoryLimit", platformResizeMemory)
	}, fmt.Sprintf("\n  ✔  %s memory override set to %s\n\n", component, platformResizeMemory))
}

func runPlatformToggle(enabled bool) func(_ *cobra.Command, args []string) error {
	verb := "enabled"
	if !enabled {
		verb = "disabled"
	}
	return func(_ *cobra.Command, args []string) error {
		component := args[0]
		if err := validateToggleComponent(component); err != nil {
			return err
		}
		_, k8sClient, err := loadCurrentCluster()
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return mutatePlatformConfig(ctx, k8sClient.Dynamic(), func(pc *unstructured.Unstructured) error {
			return setComponentField(pc, component, "enabled", enabled)
		}, fmt.Sprintf("\n  ✔  %s %s\n\n", component, verb))
	}
}

func runPlatformProfileShow(_ *cobra.Command, _ []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pc, err := getPlatformConfig(ctx, k8sClient.Dynamic())
	if err != nil {
		return err
	}
	profile, _, _ := unstructured.NestedString(pc.Object, "spec", "profile")
	fmt.Printf("\n  Profile: %s\n  %s\n\n", profile, profileDescription(profile))
	return nil
}

func runPlatformProfileSet(_ *cobra.Command, args []string) error {
	profile := args[0]
	if !isKnownProfile(profile) {
		return fmt.Errorf("unknown profile %q (valid: nano, small, medium, large, xlarge)", profile)
	}
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	return mutatePlatformConfig(ctx, k8sClient.Dynamic(), func(pc *unstructured.Unstructured) error {
		return unstructured.SetNestedField(pc.Object, profile, "spec", "profile")
	}, fmt.Sprintf("\n  ✔  Profile set to %s\n  %s\n\n", profile, profileDescription(profile)))
}

func getPlatformConfig(ctx context.Context, dyn dynamic.Interface) (*unstructured.Unstructured, error) {
	pc, err := dyn.Resource(platformGVR).Get(ctx, platformConfigName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("PlatformConfig %q not found. Run 'kip install' or upgrade an older cluster", platformConfigName)
		}
		return nil, fmt.Errorf("reading PlatformConfig: %w", err)
	}
	return pc, nil
}

// mutatePlatformConfig fetches the CR, applies mutate, and writes it back.
// Retries once on a stale-resource-version conflict so concurrent CLI and
// console edits don't bounce off each other.
func mutatePlatformConfig(ctx context.Context, dyn dynamic.Interface, mutate func(*unstructured.Unstructured) error, success string) error {
	for attempt := 0; attempt < 2; attempt++ {
		pc, err := getPlatformConfig(ctx, dyn)
		if err != nil {
			return err
		}
		if err := mutate(pc); err != nil {
			return err
		}
		if _, err := dyn.Resource(platformGVR).Update(ctx, pc, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) && attempt == 0 {
				continue
			}
			return fmt.Errorf("updating PlatformConfig: %w", err)
		}
		fmt.Print(success)
		return nil
	}
	return fmt.Errorf("updating PlatformConfig: conflict after retry")
}

// setComponentField updates a single field on a named component override,
// adding a new entry if the component is not yet in spec.components.
func setComponentField(pc *unstructured.Unstructured, name, field string, value interface{}) error {
	raw, found, err := unstructured.NestedSlice(pc.Object, "spec", "components")
	if err != nil {
		return err
	}
	if !found {
		raw = []interface{}{}
	}
	updated := false
	for i, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if n, _ := m["name"].(string); n == name {
			m[field] = value
			raw[i] = m
			updated = true
			break
		}
	}
	if !updated {
		raw = append(raw, map[string]interface{}{"name": name, field: value})
	}
	return unstructured.SetNestedSlice(pc.Object, raw, "spec", "components")
}

func overridesFromSpec(pc *unstructured.Unstructured) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	raw, _, _ := unstructured.NestedSlice(pc.Object, "spec", "components")
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if n, _ := m["name"].(string); n != "" {
			out[n] = m
		}
	}
	return out
}

func statusesFromCR(pc *unstructured.Unstructured) map[string]map[string]interface{} {
	out := map[string]map[string]interface{}{}
	raw, _, _ := unstructured.NestedSlice(pc.Object, "status", "components")
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		if n, _ := m["name"].(string); n != "" {
			out[n] = m
		}
	}
	return out
}

func componentLine(name, profile string, status, override map[string]interface{}) string {
	overrideLim, _ := override["memoryLimit"].(string)
	def := platform.EffectiveLimit(name, profile, "")
	current, _ := status["currentMemoryLimit"].(string)
	if current == "" {
		current = def
	}
	enabledStr := "on"
	if v, ok := override["enabled"].(bool); ok {
		if !v {
			enabledStr = "off"
		}
	} else if profile == platform.ProfileNano && nanoDisablesByDefault(name) {
		// nano disables the monitoring stack by default; an explicit enable
		// is the only way to flip it back on. Other charts (traefik, keda,
		// velero) keep running on nano, so don't paint them as off.
		enabledStr = "off"
	}
	out := fmt.Sprintf("%-9s limit %s", enabledStr, current)
	if overrideLim != "" {
		out += fmt.Sprintf("  (override: %s)", overrideLim)
	}
	if at, _ := status["atCeiling"].(bool); at {
		out += "  [at ceiling]"
	}
	return out
}

func profileDescription(profile string) string {
	switch profile {
	case platform.ProfileNano:
		return "sub-4 GB host. Monitoring disabled by default."
	case platform.ProfileSmall:
		return "4-8 GB host. Monitoring on with tight limits."
	case platform.ProfileMedium:
		return "8-16 GB host. Real workloads, sensible defaults."
	case platform.ProfileLarge:
		return "16-32 GB host. Comfortable headroom for app teams."
	case platform.ProfileXLarge:
		return "32 GB+ host. Mature production."
	default:
		return ""
	}
}

func runPlatformRestart(_ *cobra.Command, args []string) error {
	component := args[0]
	target, ok := restartTargets[component]
	if !ok {
		valid := ""
		for k := range restartTargets {
			if valid != "" {
				valid += ", "
			}
			valid += k
		}
		return fmt.Errorf("unknown component %q (valid: %s)", component, valid)
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cs := k8sClient.Clientset()
	stamp := time.Now().Format(time.RFC3339)

	switch target.Kind {
	case "Deployment":
		d, err := cs.AppsV1().Deployments(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting deployment %s/%s: %w", target.Namespace, target.Name, err)
		}
		if d.Spec.Template.Annotations == nil {
			d.Spec.Template.Annotations = map[string]string{}
		}
		d.Spec.Template.Annotations["kipper.run/restartedAt"] = stamp
		_, err = cs.AppsV1().Deployments(target.Namespace).Update(ctx, d, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("restarting %s: %w", component, err)
		}
	case "StatefulSet":
		s, err := cs.AppsV1().StatefulSets(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting statefulset %s/%s: %w", target.Namespace, target.Name, err)
		}
		if s.Spec.Template.Annotations == nil {
			s.Spec.Template.Annotations = map[string]string{}
		}
		s.Spec.Template.Annotations["kipper.run/restartedAt"] = stamp
		_, err = cs.AppsV1().StatefulSets(target.Namespace).Update(ctx, s, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("restarting %s: %w", component, err)
		}
	case "DaemonSet":
		d, err := cs.AppsV1().DaemonSets(target.Namespace).Get(ctx, target.Name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("getting daemonset %s/%s: %w", target.Namespace, target.Name, err)
		}
		if d.Spec.Template.Annotations == nil {
			d.Spec.Template.Annotations = map[string]string{}
		}
		d.Spec.Template.Annotations["kipper.run/restartedAt"] = stamp
		_, err = cs.AppsV1().DaemonSets(target.Namespace).Update(ctx, d, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("restarting %s: %w", component, err)
		}
	}

	fmt.Printf("\n  ✔  Restarting %s\n\n", component)
	return nil
}

func validateComponent(name string) error {
	if _, ok := platform.PathFor(name); ok {
		return nil
	}
	return fmt.Errorf("unknown component %q (valid: %s)", name, strings.Join(platform.SupportedComponents(), ", "))
}

// nanoDisablesByDefault reports whether the named component is part of
// the monitoring stack that nano installs skip. Keep in sync with the
// reconciler: prom + grafana share kube-prometheus-stack, loki + promtail
// move as one.
func nanoDisablesByDefault(name string) bool {
	switch name {
	case platform.ComponentPrometheus, platform.ComponentGrafana,
		platform.ComponentLoki, platform.ComponentPromtail:
		return true
	}
	return false
}

// validateToggleComponent is the narrower check for enable/disable. The
// reconciler only honors the Enabled flag for prometheus and loki (with
// promtail following loki), so accepting any path-table component here
// would let users persist an override that silently does nothing — the
// chart keeps running and the CLI reports it as off.
func validateToggleComponent(name string) error {
	switch name {
	case platform.ComponentPrometheus, platform.ComponentLoki:
		return nil
	}
	return fmt.Errorf("unknown component %q for enable/disable (valid: prometheus, loki)", name)
}

func isKnownProfile(p string) bool {
	switch p {
	case platform.ProfileNano, platform.ProfileSmall, platform.ProfileMedium, platform.ProfileLarge, platform.ProfileXLarge:
		return true
	}
	return false
}
