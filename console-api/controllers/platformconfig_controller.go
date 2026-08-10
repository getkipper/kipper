package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	stderrors "errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/yaml"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

const (
	// PlatformConfigName is the singleton CR name the reconciler acts on.
	// Other names are deliberately ignored so a user fat-fingering `kubectl
	// create platformconfig wrong` doesn't accidentally drive cluster state.
	PlatformConfigName = "platform"

	helmChartNamespace = "kube-system"

	componentPrometheus = "prometheus"
	componentLoki       = "loki"
)

var helmChartGVK = schema.GroupVersionKind{
	Group:   "helm.cattle.io",
	Version: "v1",
	Kind:    "HelmChart",
}

// PlatformConfigReconciler keeps system component sizing aligned with the
// active PlatformConfig. When the CR's profile changes, or a per-component
// override is set, it patches the corresponding HelmChart's valuesContent so
// helm-controller re-rolls Prometheus / Loki with the new memory limits.
// When a per-component override flips Enabled=false, it deletes the HelmChart
// so helm-controller uninstalls the release; flipping back to true re-creates
// the chart from the same template the installer uses.
type PlatformConfigReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

func (r *PlatformConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if req.Name != PlatformConfigName {
		return ctrl.Result{}, nil
	}

	var pc kipperv1.PlatformConfig
	if err := r.Get(ctx, req.NamespacedName, &pc); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	overrides := overrideMap(pc.Spec.Components)
	res := platform.EffectiveResources(pc.Spec.Profile, memoryOverrideMap(overrides))

	promEnabled := componentEnabled(overrides[componentPrometheus], pc.Spec.Profile)
	lokiEnabled := componentEnabled(overrides[componentLoki], pc.Spec.Profile)

	// Build the patch set for every supported component by walking the
	// shared path table. Multi-component charts (Prometheus + Grafana on
	// kube-prometheus-stack) collapse into a single Update because we key
	// the slice by chart name.
	pathsByChart := map[string][]memoryPath{}
	limitByComponent := map[string]string{}
	for _, name := range platform.SupportedComponents() {
		p, ok := platform.PathFor(name)
		if !ok {
			continue
		}
		overrideMem := overrides[name].MemoryLimit
		limit := platform.EffectiveLimit(name, pc.Spec.Profile, overrideMem)
		request := platform.EffectiveRequest(name, pc.Spec.Profile, overrideMem)
		pathsByChart[p.ChartName] = append(pathsByChart[p.ChartName],
			memoryPath{path: p.MemoryRequestPath, value: request},
			memoryPath{path: p.MemoryLimitPath, value: limit},
		)
		limitByComponent[name] = limit
	}

	applied := map[string]string{}
	var patchErrs []error

	// The Grafana chart reads its admin password from the grafana-admin
	// Secret (grafana.admin.existingSecret). When monitoring is enabled from
	// a state where the installer never provisioned it (a profile that was
	// monitoring-off at install, then toggled on here), the Secret must exist
	// before the chart installs or Grafana starts with a dangling reference.
	// If provisioning fails, skip creating the chart this pass: the aggregated
	// error requeues, and the next pass provisions the Secret before the chart.
	// A disable (promEnabled=false) never touches the Secret, so it must still
	// run reconcileComponentEnabled to delete the chart.
	secretReady := true
	if promEnabled {
		if err := r.ensureGrafanaAdminSecret(ctx); err != nil {
			patchErrs = append(patchErrs, fmt.Errorf("grafana admin secret: %w", err))
			secretReady = false
		}
	}
	if secretReady {
		if err := r.reconcileComponentEnabled(ctx, "kube-prometheus-stack",
			promEnabled,
			platform.KubePrometheusStackHelmChart(res),
			pathsByChart["kube-prometheus-stack"]); err != nil {
			patchErrs = append(patchErrs, fmt.Errorf("kube-prometheus-stack enable/disable: %w", err))
		}
	}
	if promEnabled {
		// The Traefik ServiceMonitor lives with the monitoring stack (its
		// CRD comes from kube-prometheus-stack), so re-enabling monitoring
		// must bring it back. Fails and requeues while the stack is still
		// installing its CRDs.
		if err := r.reconcileTraefikServiceMonitor(ctx); err != nil {
			patchErrs = append(patchErrs, fmt.Errorf("traefik servicemonitor: %w", err))
		}
	}
	if err := r.reconcileComponentEnabled(ctx, "loki",
		lokiEnabled,
		platform.LokiHelmChart(res),
		pathsByChart["loki"]); err != nil {
		patchErrs = append(patchErrs, fmt.Errorf("loki enable/disable: %w", err))
	}
	// Promtail follows Loki: shipping logs to a non-existent gateway is
	// worse than not shipping at all. Same enabled state, no separate
	// override path.
	if err := r.reconcileComponentEnabled(ctx, "promtail",
		lokiEnabled,
		platform.PromtailHelmChart(),
		pathsByChart["promtail"]); err != nil {
		patchErrs = append(patchErrs, fmt.Errorf("promtail enable/disable: %w", err))
	}

	// Patch every chart whose enable/disable rule allows it. Traefik, KEDA
	// and Velero are not toggleable today: the installer creates them and
	// the reconciler only patches; their charts always count as "present".
	for chartName, paths := range pathsByChart {
		if !chartShouldBePatched(chartName, promEnabled, lokiEnabled) {
			continue
		}
		present, err := r.patchHelmChart(ctx, chartName, paths)
		if err != nil {
			logger.Error(err, "patching chart", "chart", chartName)
			patchErrs = append(patchErrs, fmt.Errorf("%s: %w", chartName, err))
			continue
		}
		if !present {
			continue
		}
		for _, component := range platform.ComponentsByChart()[chartName] {
			if limit, ok := limitByComponent[component]; ok {
				applied[component] = limit
			}
		}
	}

	if err := r.updateStatus(ctx, &pc, applied); err != nil {
		patchErrs = append(patchErrs, err)
	}

	// Returning a non-nil error makes controller-runtime re-queue. Silent
	// swallowing here would let a transient API conflict strand a profile
	// change forever, which is exactly the failure mode this plan exists
	// to prevent.
	return ctrl.Result{}, stderrors.Join(patchErrs...)
}

// reconcileComponentEnabled ensures the HelmChart for a component matches its
// Enabled state in spec. When enabled is false the HelmChart is deleted (helm-
// controller then uninstalls the release). When enabled is true and the
// HelmChart is missing, it is created from the supplied manifest with the
// provided memory paths pre-applied so the new chart lands at the right
// limits in one round-trip rather than racing with a follow-up patch through
// the controller-runtime cache.
func (r *PlatformConfigReconciler) reconcileComponentEnabled(ctx context.Context, chartName string, enabled bool, manifest string, paths []memoryPath) error {
	logger := log.FromContext(ctx)

	chart := &unstructured.Unstructured{}
	chart.SetGroupVersionKind(helmChartGVK)
	err := r.Get(ctx, types.NamespacedName{Name: chartName, Namespace: helmChartNamespace}, chart)
	exists := err == nil
	if err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("getting HelmChart %s: %w", chartName, err)
	}

	switch {
	case enabled && !exists:
		desired, err := unstructuredFromYAML(manifest)
		if err != nil {
			return fmt.Errorf("parsing manifest for %s: %w", chartName, err)
		}
		if len(paths) > 0 {
			values, _, _ := unstructured.NestedString(desired.Object, "spec", "valuesContent")
			newValues, _, err := rewriteValuesContent(values, paths)
			if err != nil {
				return fmt.Errorf("seeding overrides on %s: %w", chartName, err)
			}
			if err := unstructured.SetNestedField(desired.Object, newValues, "spec", "valuesContent"); err != nil {
				return fmt.Errorf("writing seeded valuesContent for %s: %w", chartName, err)
			}
		}
		if err := r.Create(ctx, desired); err != nil {
			return fmt.Errorf("creating HelmChart %s: %w", chartName, err)
		}
		logger.Info("HelmChart re-created after enable", "chart", chartName)

	case !enabled && exists:
		if err := r.Delete(ctx, chart); err != nil {
			if errors.IsNotFound(err) {
				return nil
			}
			return fmt.Errorf("deleting HelmChart %s: %w", chartName, err)
		}
		logger.Info("HelmChart deleted after disable", "chart", chartName)
	}
	return nil
}

// reconcileTraefikServiceMonitor ensures the ServiceMonitor scraping
// Traefik's metrics Service exists while monitoring is enabled. When
// monitoring is disabled the object is left alone: it either disappears
// with the CRD or sits inert.
func (r *PlatformConfigReconciler) reconcileTraefikServiceMonitor(ctx context.Context) error {
	desired, err := unstructuredFromYAML(platform.TraefikServiceMonitor())
	if err != nil {
		return fmt.Errorf("parsing servicemonitor manifest: %w", err)
	}

	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err = r.Get(ctx, types.NamespacedName{Name: desired.GetName(), Namespace: desired.GetNamespace()}, existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Object["spec"] = desired.Object["spec"]
	return r.Update(ctx, existing)
}

// ensureGrafanaAdminSecret provisions the Grafana admin credentials Secret
// with a random password, creating the monitoring namespace and the Secret
// only when absent. Create-if-absent (never update) is deliberate: a re-run
// must not rotate the password and invalidate a saved login, matching the
// installer's behaviour so both provisioners agree on one stable value.
func (r *PlatformConfigReconciler) ensureGrafanaAdminSecret(ctx context.Context) error {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: platform.MonitoringNamespace}}
	if err := r.Create(ctx, ns); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("ensuring monitoring namespace: %w", err)
	}

	key := types.NamespacedName{Namespace: platform.MonitoringNamespace, Name: platform.GrafanaAdminSecretName}
	if err := r.Get(ctx, key, &corev1.Secret{}); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return fmt.Errorf("reading grafana admin secret: %w", err)
	}

	password, err := randomPassword()
	if err != nil {
		return err
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      platform.GrafanaAdminSecretName,
			Namespace: platform.MonitoringNamespace,
			Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
		},
		Data: map[string][]byte{
			platform.GrafanaAdminUserKey:     []byte(platform.GrafanaAdminUser),
			platform.GrafanaAdminPasswordKey: []byte(password),
		},
	}
	if err := r.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
		return fmt.Errorf("creating grafana admin secret: %w", err)
	}
	return nil
}

// randomPassword returns a 24-byte hex-encoded random string for the Grafana
// admin login.
func randomPassword() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating grafana admin password: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// unstructuredFromYAML parses a single-document YAML manifest into an
// Unstructured object. Used by reconcileComponentEnabled to re-create a
// HelmChart from the shared platform manifest templates.
func unstructuredFromYAML(manifest string) (*unstructured.Unstructured, error) {
	var obj map[string]interface{}
	if err := yaml.Unmarshal([]byte(manifest), &obj); err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: obj}, nil
}

// componentEnabled resolves the effective enabled state for a component.
// Explicit Enabled wins, so a user can force a component on even on the
// nano profile (which is otherwise monitoring-off by default). Without an
// explicit override, the default is on unless the profile is nano.
func componentEnabled(override kipperv1.ComponentOverride, profile string) bool {
	if override.Enabled != nil {
		return *override.Enabled
	}
	return profile != platform.ProfileNano
}

// patchHelmChart reads the named HelmChart in kube-system, rewrites the
// given memory paths inside its valuesContent, and updates the chart if
// anything changed.
//
// Returns (true, nil) when the chart existed (and was either updated or
// already matched, so the caller can record its limit in status). Returns
// (false, nil) when the chart doesn't exist — the install path may have
// skipped it (nano transitions, manual deletes), and surfacing that as an
// error would prevent reconciliation of the components that do exist.
func (r *PlatformConfigReconciler) patchHelmChart(ctx context.Context, name string, paths []memoryPath) (bool, error) {
	logger := log.FromContext(ctx)

	if len(paths) == 0 {
		return false, nil
	}

	chart := &unstructured.Unstructured{}
	chart.SetGroupVersionKind(helmChartGVK)
	if err := r.Get(ctx, types.NamespacedName{Name: name, Namespace: helmChartNamespace}, chart); err != nil {
		if errors.IsNotFound(err) {
			logger.Info("HelmChart not present; skipping", "chart", name)
			return false, nil
		}
		return false, fmt.Errorf("getting HelmChart %s: %w", name, err)
	}

	values, _, err := unstructured.NestedString(chart.Object, "spec", "valuesContent")
	if err != nil {
		return false, fmt.Errorf("reading spec.valuesContent: %w", err)
	}

	newValues, changes, err := rewriteValuesContent(values, paths)
	if err != nil {
		return false, fmt.Errorf("rewriting valuesContent for %s: %w", name, err)
	}

	if len(changes) == 0 {
		return true, nil
	}

	if err := unstructured.SetNestedField(chart.Object, newValues, "spec", "valuesContent"); err != nil {
		return false, fmt.Errorf("writing spec.valuesContent: %w", err)
	}
	if err := r.Update(ctx, chart); err != nil {
		return false, fmt.Errorf("updating HelmChart %s: %w", name, err)
	}
	logger.Info("HelmChart updated", "chart", name, "changes", changes)
	return true, nil
}

// chartShouldBePatched returns whether the chart is part of the enabled
// set. Charts whose enable state is controlled by a per-component flag
// (kube-prometheus-stack, loki, promtail) get skipped when that flag is
// off so we don't fight reconcileComponentEnabled. Charts without a
// toggle (traefik, keda, velero) always get patched if present.
func chartShouldBePatched(chartName string, promEnabled, lokiEnabled bool) bool {
	switch chartName {
	case "kube-prometheus-stack":
		return promEnabled
	case "loki", "promtail":
		return lokiEnabled
	}
	return true
}

// updateStatus merges the just-applied memory limits into the existing
// status.components entries. The reconciler owns CurrentMemoryLimit and the
// top-level Profile; everything else (LastBumpAt, LastBumpFrom/To/Reason,
// AtCeiling, RestartCount7d) belongs to other writers (the OOM watcher,
// later steps), so we must not clobber it by rebuilding the slice from
// scratch.
func (r *PlatformConfigReconciler) updateStatus(ctx context.Context, pc *kipperv1.PlatformConfig, applied map[string]string) error {
	indexByName := make(map[string]int, len(pc.Status.Components))
	for i, c := range pc.Status.Components {
		indexByName[c.Name] = i
	}

	changed := pc.Status.Profile != pc.Spec.Profile
	for name, limit := range applied {
		// AtCeiling is a transient signal: "auto-bump just hit the
		// ceiling and can't help further". A profile change or manual
		// resize that drops the limit below the ceiling means automated
		// help is back on the table; the warning should clear.
		newAtCeiling := isAtCeiling(name, limit)
		if idx, ok := indexByName[name]; ok {
			if pc.Status.Components[idx].CurrentMemoryLimit != limit {
				pc.Status.Components[idx].CurrentMemoryLimit = limit
				changed = true
			}
			if pc.Status.Components[idx].AtCeiling && !newAtCeiling {
				pc.Status.Components[idx].AtCeiling = false
				changed = true
			}
			continue
		}
		pc.Status.Components = append(pc.Status.Components, kipperv1.ComponentStatus{
			Name:               name,
			CurrentMemoryLimit: limit,
			AtCeiling:          newAtCeiling,
		})
		changed = true
	}

	if !changed {
		return nil
	}

	pc.Status.Profile = pc.Spec.Profile
	if err := r.Status().Update(ctx, pc); err != nil {
		return fmt.Errorf("updating PlatformConfig status: %w", err)
	}
	return nil
}

// memoryPath identifies a memory string deep in a HelmChart values document
// (e.g. ["prometheus", "prometheusSpec", "resources", "limits", "memory"])
// together with the value it should hold.
type memoryPath struct {
	path  []string
	value string
}

// rewriteValuesContent parses the YAML string, applies each memory path
// override, and returns the rewritten string plus a list of human-readable
// change descriptions. Returns the original string and an empty changes slice
// when nothing needed updating.
func rewriteValuesContent(content string, paths []memoryPath) (string, []string, error) {
	var values map[string]interface{}
	if err := yaml.Unmarshal([]byte(content), &values); err != nil {
		return "", nil, fmt.Errorf("parsing valuesContent: %w", err)
	}
	if values == nil {
		values = map[string]interface{}{}
	}

	var changes []string
	for _, p := range paths {
		existing, _, err := unstructured.NestedString(values, p.path...)
		if err != nil {
			return "", nil, fmt.Errorf("reading %s: %w", strings.Join(p.path, "."), err)
		}
		if existing == p.value {
			continue
		}
		if err := unstructured.SetNestedField(values, p.value, p.path...); err != nil {
			return "", nil, fmt.Errorf("writing %s: %w", strings.Join(p.path, "."), err)
		}
		changes = append(changes, fmt.Sprintf("%s: %q -> %q", strings.Join(p.path, "."), existing, p.value))
	}

	if len(changes) == 0 {
		return content, nil, nil
	}

	out, err := yaml.Marshal(values)
	if err != nil {
		return "", nil, fmt.Errorf("marshalling valuesContent: %w", err)
	}
	return string(out), changes, nil
}

// overrideMap indexes the per-component overrides by component name so
// applyOverride can do a constant-time lookup per component.
func overrideMap(components []kipperv1.ComponentOverride) map[string]kipperv1.ComponentOverride {
	out := make(map[string]kipperv1.ComponentOverride, len(components))
	for _, c := range components {
		out[c.Name] = c
	}
	return out
}

// memoryOverrideMap extracts MemoryLimit overrides from a ComponentOverride
// map for handoff to platform.EffectiveResources. The platform package
// works in plain strings; the CRD wrapper lives here.
func memoryOverrideMap(overrides map[string]kipperv1.ComponentOverride) map[string]string {
	out := make(map[string]string, len(overrides))
	for name, o := range overrides {
		if o.MemoryLimit != "" {
			out[name] = o.MemoryLimit
		}
	}
	return out
}

func (r *PlatformConfigReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// GenerationChangedPredicate suppresses reconcile triggers when only the
	// status subresource (which we write ourselves) changes. Without it, every
	// Status().Update would enqueue another reconcile and we'd loop forever
	// against the singleton PlatformConfig CR on every cluster.
	return ctrl.NewControllerManagedBy(mgr).
		For(&kipperv1.PlatformConfig{}, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		Complete(r)
}
