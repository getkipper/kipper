package cmd

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/ssh"
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
var restartTargets = map[string]restartTarget{
	"console":     {"kipper-system", "Deployment", "console"},
	"console-api": {"kipper-system", "Deployment", "console-api"},
	"dex":         {"dex", "Deployment", "dex"},
	"traefik":     {"traefik", "Deployment", "traefik"},
	"prometheus":  {"monitoring", "StatefulSet", "prometheus-kube-prometheus-stack-prometheus"},
	"loki":        {"monitoring", "StatefulSet", "loki"},
	"grafana":     {"monitoring", "Deployment", "kube-prometheus-stack-grafana"},
	"promtail":    {"monitoring", "DaemonSet", "promtail"},
	// The release names are fixed by the installer, so these workload names are
	// stable across clusters; all three verified on two live clusters.
	"kube-state-metrics": {"monitoring", "Deployment", "kube-prometheus-stack-kube-state-metrics"},
	"keda":               {"keda", "Deployment", "keda-operator"},
	"velero":             {"velero", "Deployment", "velero"},
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
	enabled := enabledOverrides(overrides)
	for _, name := range platform.SupportedComponents() {
		cs := statuses[name]
		ov := overrides[name]
		fmt.Printf("    %-18s %s\n", name, componentLine(name, profile, cs, ov, enabled))
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

// clusterProfile returns the cluster's platform profile. It returns "" if
// PlatformConfig is absent, as on older clusters, or spec.profile is missing or
// is not a string. Other read errors are returned.
func clusterProfile(ctx context.Context, dyn dynamic.Interface) (string, error) {
	pc, err := dyn.Resource(platformGVR).Get(ctx, platformConfigName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading PlatformConfig: %w", err)
	}
	profile, _, _ := unstructured.NestedString(pc.Object, "spec", "profile")
	return profile, nil
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

// enabledOverrides pulls the explicit enable/disable flags out of the override
// set, in the shape platform.EffectiveEnabled reads: a component with no entry
// is absent rather than false.
func enabledOverrides(overrides map[string]map[string]interface{}) map[string]*bool {
	out := map[string]*bool{}
	for name, ov := range overrides {
		if v, ok := ov["enabled"].(bool); ok {
			flag := v
			out[name] = &flag
		}
	}
	return out
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

// componentLine renders one component's row. enabled comes from the whole
// override set rather than this component's own entry, because a component
// that shares a chart runs or does not run with its sibling: grafana and
// kube-state-metrics live in kube-prometheus-stack, so disabling prometheus
// takes them with it, and a row reading "on" for a pod that is not deployed is
// worse than no row at all.
func componentLine(name, profile string, status, override map[string]interface{}, enabled map[string]*bool) string {
	overrideLim, _ := override["memoryLimit"].(string)
	def := platform.EffectiveLimit(name, profile, "")
	current, _ := status["currentMemoryLimit"].(string)
	if current == "" {
		current = def
	}
	enabledStr := "on"
	if !platform.EffectiveEnabled(name, enabled, profile) {
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
		return "For hosts below 6 GB. Monitoring is off by default to leave memory for apps."
	case platform.ProfileSmall:
		return "For 6-8 GB hosts. Monitoring uses tight memory limits to leave room for apps."
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

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dial := func() (restartHost, error) {
		cfg, err := hostSSHConfig(cluster, "")
		if err != nil {
			return nil, err
		}
		client, err := ssh.Dial(cfg)
		if err != nil {
			return nil, err
		}
		return client, nil
	}
	overSSH, err := restartComponent(ctx, k8sClient.Clientset(), dial, target, time.Now().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("restarting %s: %w", component, err)
	}
	if overSSH {
		fmt.Printf("\n  ✔  Restarting %s over SSH because the Kubernetes API request failed or your login was rejected.\n\n", component)
		return nil
	}
	fmt.Printf("\n  ✔  Restarting %s\n\n", component)
	return nil
}

type restartTarget struct {
	Namespace string
	Kind      string
	Name      string
}

type restartHost interface {
	Run(command string) (string, error)
	Close() error
}

// restartComponent annotates target's pod template to trigger a rolling restart.
// On an Unauthorized response or a wrapped *url.Error, it retries the same patch
// with kubectl on the server over SSH. Login can fail while Traefik serves its
// fallback certificate. Other errors, including Forbidden and NotFound, are
// returned without trying SSH. overSSH is true only when the SSH patch succeeds.
func restartComponent(ctx context.Context, cs kubernetes.Interface, dial func() (restartHost, error), target restartTarget, stamp string) (overSSH bool, err error) {
	patch := []byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{"kipper.run/restartedAt":%q}}}}}`, stamp))
	apiErr := patchPodTemplate(ctx, cs, target, patch)
	if apiErr == nil {
		return false, nil
	}
	// A *url.Error indicates a request failure before a response or a failed
	// redirect, neither of which carries an API authorization decision.
	// Preserve other errors because a body truncated after the status line
	// may contain a refusal that SSH must not bypass.
	var noResponse *url.Error
	if !apierrors.IsUnauthorized(apiErr) && !errors.As(apiErr, &noResponse) {
		return false, apiErr
	}
	host, err := dial()
	if err != nil {
		return false, fmt.Errorf("the Kubernetes API failed (%w), and SSH to the server failed: %v", apiErr, err)
	}
	defer func() { _ = host.Close() }()
	command := fmt.Sprintf("kubectl -n %s patch %s %s --type merge -p '%s' --request-timeout=20s",
		target.Namespace, strings.ToLower(target.Kind), target.Name, patch)
	if _, err := host.Run(command); err != nil {
		return false, fmt.Errorf("the Kubernetes API failed (%w), and kubectl on the server failed: %v", apiErr, err)
	}
	return true, nil
}

func patchPodTemplate(ctx context.Context, cs kubernetes.Interface, target restartTarget, patch []byte) error {
	var err error
	switch target.Kind {
	case "Deployment":
		_, err = cs.AppsV1().Deployments(target.Namespace).Patch(ctx, target.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	case "StatefulSet":
		_, err = cs.AppsV1().StatefulSets(target.Namespace).Patch(ctx, target.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	case "DaemonSet":
		_, err = cs.AppsV1().DaemonSets(target.Namespace).Patch(ctx, target.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	default:
		err = fmt.Errorf("unsupported workload kind %q", target.Kind)
	}
	return err
}

func validateComponent(name string) error {
	if _, ok := platform.PathFor(name); ok {
		return nil
	}
	return fmt.Errorf("unknown component %q (valid: %s)", name, strings.Join(platform.SupportedComponents(), ", "))
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
