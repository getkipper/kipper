package controllers

import (
	"context"
	stderrors "errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func TestRewriteValuesContent(t *testing.T) {
	t.Run("updates nested memory fields and leaves the rest alone", func(t *testing.T) {
		original := `prometheus:
  prometheusSpec:
    retention: 3d
    resources:
      requests:
        cpu: 100m
        memory: 256Mi
      limits:
        memory: 512Mi
    serviceMonitorSelectorNilUsesHelmValues: false
grafana:
  enabled: true
  adminPassword: kipper
`
		paths := []memoryPath{
			{path: []string{"prometheus", "prometheusSpec", "resources", "requests", "memory"}, value: "1Gi"},
			{path: []string{"prometheus", "prometheusSpec", "resources", "limits", "memory"}, value: "2Gi"},
		}

		got, changes, err := rewriteValuesContent(original, paths)
		require.NoError(t, err)
		require.Len(t, changes, 2, "both paths should report as changed")

		var parsed map[string]interface{}
		require.NoError(t, yaml.Unmarshal([]byte(got), &parsed))

		req, _, _ := unstructured.NestedString(parsed, "prometheus", "prometheusSpec", "resources", "requests", "memory")
		lim, _, _ := unstructured.NestedString(parsed, "prometheus", "prometheusSpec", "resources", "limits", "memory")
		assert.Equal(t, "1Gi", req)
		assert.Equal(t, "2Gi", lim)

		// Sibling fields must survive the rewrite intact.
		retention, _, _ := unstructured.NestedString(parsed, "prometheus", "prometheusSpec", "retention")
		grafanaEnabled, _, _ := unstructured.NestedBool(parsed, "grafana", "enabled")
		grafanaPw, _, _ := unstructured.NestedString(parsed, "grafana", "adminPassword")
		assert.Equal(t, "3d", retention)
		assert.True(t, grafanaEnabled)
		assert.Equal(t, "kipper", grafanaPw)
	})

	t.Run("returns the original unchanged when values already match", func(t *testing.T) {
		original := `singleBinary:
  resources:
    requests:
      memory: 128Mi
    limits:
      memory: 384Mi
`
		paths := []memoryPath{
			{path: []string{"singleBinary", "resources", "requests", "memory"}, value: "128Mi"},
			{path: []string{"singleBinary", "resources", "limits", "memory"}, value: "384Mi"},
		}

		got, changes, err := rewriteValuesContent(original, paths)
		require.NoError(t, err)
		assert.Empty(t, changes, "no changes expected when target equals current")
		assert.Equal(t, original, got, "exact byte equality so the caller can short-circuit the Update")
	})

	t.Run("creates intermediate maps when the path is missing", func(t *testing.T) {
		original := `prometheus:
  prometheusSpec:
    retention: 3d
`
		paths := []memoryPath{
			{path: []string{"prometheus", "prometheusSpec", "resources", "limits", "memory"}, value: "1Gi"},
		}

		got, changes, err := rewriteValuesContent(original, paths)
		require.NoError(t, err)
		require.Len(t, changes, 1)
		assert.Contains(t, got, "memory: 1Gi")
	})
}

func TestEffectiveResourcesClamping(t *testing.T) {
	// The clamping logic lives in controller/pkg/platform so both the
	// reconciler and the kip upgrade path get it for free. These tests
	// pin the contract from the reconciler's perspective; matching tests
	// in the kip installer module guard the upgrade-side rendering.
	t.Run("override raises limit, request stays at profile default", func(t *testing.T) {
		res := platform.EffectiveResources(platform.ProfileMedium, map[string]string{
			platform.ComponentPrometheus: "2Gi",
		})
		assert.Equal(t, "512Mi", res.PrometheusMemoryRequest, "raising the limit doesn't move the request")
		assert.Equal(t, "2Gi", res.PrometheusMemoryLimit)
	})

	t.Run("override below profile request clamps the request down", func(t *testing.T) {
		// medium profile request is 512Mi. A user setting limit to 128Mi
		// would otherwise produce request=512Mi/limit=128Mi which K8s
		// rejects.
		res := platform.EffectiveResources(platform.ProfileMedium, map[string]string{
			platform.ComponentPrometheus: "128Mi",
		})
		assert.Equal(t, "128Mi", res.PrometheusMemoryRequest,
			"request must be clamped to match the new lower limit")
		assert.Equal(t, "128Mi", res.PrometheusMemoryLimit)
	})

	t.Run("override exactly equal to profile request keeps request as-is", func(t *testing.T) {
		res := platform.EffectiveResources(platform.ProfileMedium, map[string]string{
			platform.ComponentPrometheus: "512Mi",
		})
		assert.Equal(t, "512Mi", res.PrometheusMemoryRequest)
		assert.Equal(t, "512Mi", res.PrometheusMemoryLimit)
	})

	t.Run("malformed override quantity falls through without crashing", func(t *testing.T) {
		res := platform.EffectiveResources(platform.ProfileMedium, map[string]string{
			platform.ComponentPrometheus: "twelve potatoes",
		})
		assert.Equal(t, "512Mi", res.PrometheusMemoryRequest, "request stays at profile default when limit doesn't parse")
		assert.Equal(t, "twelve potatoes", res.PrometheusMemoryLimit, "let the chart layer surface the bad input")
	})

	t.Run("loki override clamping works the same way", func(t *testing.T) {
		res := platform.EffectiveResources(platform.ProfileMedium, map[string]string{
			platform.ComponentLoki: "64Mi",
		})
		assert.Equal(t, "64Mi", res.LokiMemoryRequest)
		assert.Equal(t, "64Mi", res.LokiMemoryLimit)
	})
}

func TestPlatformConfigReconciler_PatchesPrometheusForXLarge(t *testing.T) {
	scheme := testScheme()

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileXLarge},
	}

	promChart := newHelmChart("kube-prometheus-stack", `prometheus:
  prometheusSpec:
    retention: 3d
    resources:
      requests:
        cpu: 100m
        memory: 256Mi
      limits:
        memory: 512Mi
`)
	lokiChart := newHelmChart("loki", `singleBinary:
  resources:
    requests:
      memory: 128Mi
    limits:
      memory: 384Mi
`)

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, promChart, lokiChart).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)

	gotProm := &unstructured.Unstructured{}
	gotProm.SetGroupVersionKind(helmChartGVK)
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kube-prometheus-stack", Namespace: helmChartNamespace}, gotProm))

	values, _, _ := unstructured.NestedString(gotProm.Object, "spec", "valuesContent")
	assert.Contains(t, values, "memory: 1Gi", "xlarge should request 1Gi for Prometheus")
	assert.Contains(t, values, "memory: 2Gi", "xlarge should limit Prometheus at 2Gi")
	assert.NotContains(t, values, "memory: 512Mi", "old 512Mi limit should be gone")

	gotLoki := &unstructured.Unstructured{}
	gotLoki.SetGroupVersionKind(helmChartGVK)
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "loki", Namespace: helmChartNamespace}, gotLoki))
	lokiValues, _, _ := unstructured.NestedString(gotLoki.Object, "spec", "valuesContent")
	assert.Contains(t, lokiValues, "memory: 512Mi", "xlarge should request 512Mi for Loki")
	assert.Contains(t, lokiValues, "memory: 1Gi", "xlarge should limit Loki at 1Gi")
}

func TestPlatformConfigReconciler_EnsuresTraefikServiceMonitor(t *testing.T) {
	scheme := testScheme()

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileMedium},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)

	sm := &unstructured.Unstructured{}
	sm.SetGroupVersionKind(schema.GroupVersionKind{Group: "monitoring.coreos.com", Version: "v1", Kind: "ServiceMonitor"})
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "traefik", Namespace: "monitoring"}, sm),
		"monitoring enabled must (re)create the traefik ServiceMonitor")

	endpoints, _, err := unstructured.NestedSlice(sm.Object, "spec", "endpoints")
	require.NoError(t, err)
	require.Len(t, endpoints, 1)
	port, _, _ := unstructured.NestedString(endpoints[0].(map[string]interface{}), "port")
	assert.Equal(t, "metrics", port, "must scrape the dedicated metrics port, not the main service")
}

func TestPlatformConfigReconciler_HonorsPerComponentOverride(t *testing.T) {
	scheme := testScheme()

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileMedium,
			Components: []kipperv1.ComponentOverride{
				{Name: componentPrometheus, MemoryLimit: "3Gi"},
			},
		},
	}

	promChart := newHelmChart("kube-prometheus-stack", `prometheus:
  prometheusSpec:
    resources:
      requests:
        memory: 256Mi
      limits:
        memory: 512Mi
`)

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, promChart).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)

	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(helmChartGVK)
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kube-prometheus-stack", Namespace: helmChartNamespace}, got))

	values, _, _ := unstructured.NestedString(got.Object, "spec", "valuesContent")
	assert.Contains(t, values, "memory: 3Gi", "override should win over the medium profile's 1Gi default")
	// Reaffirm the default Prometheus request stays in place when only the
	// limit is overridden.
	assert.Contains(t, values, "memory: 512Mi", "request should follow the medium default (512Mi) because override only sets limit")
}

func TestPlatformConfigReconciler_PatchesNewComponentsFromPathTable(t *testing.T) {
	scheme := testScheme()

	// Overrides on three of the new components: grafana (shares the
	// kube-prometheus-stack chart with Prometheus), traefik (its own
	// chart), and keda (a nested operator-specific path).
	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileMedium,
			Components: []kipperv1.ComponentOverride{
				{Name: platform.ComponentGrafana, MemoryLimit: "384Mi"},
				{Name: platform.ComponentTraefik, MemoryLimit: "384Mi"},
				{Name: platform.ComponentKeda, MemoryLimit: "192Mi"},
			},
		},
	}

	promChart := newHelmChart("kube-prometheus-stack", `prometheus:
  prometheusSpec:
    resources:
      requests:
        memory: 256Mi
      limits:
        memory: 512Mi
grafana:
  enabled: true
  resources:
    requests:
      memory: 64Mi
    limits:
      memory: 128Mi
`)
	traefikChart := newHelmChart("traefik", `ingressRoute:
  dashboard:
    enabled: false
resources:
  requests:
    memory: 64Mi
  limits:
    memory: 256Mi
`)
	kedaChart := newHelmChart("keda", `resources:
  operator:
    requests:
      memory: 64Mi
    limits:
      memory: 256Mi
`)

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, promChart, traefikChart, kedaChart).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)

	// Grafana's override lands inside the shared chart, alongside the
	// Prometheus values which stay at the medium-profile default.
	gotProm := &unstructured.Unstructured{}
	gotProm.SetGroupVersionKind(helmChartGVK)
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kube-prometheus-stack", Namespace: helmChartNamespace}, gotProm))
	values, _, _ := unstructured.NestedString(gotProm.Object, "spec", "valuesContent")
	grafanaLim, _, _ := unstructured.NestedString(yamlToMap(t, values), "grafana", "resources", "limits", "memory")
	promLim, _, _ := unstructured.NestedString(yamlToMap(t, values), "prometheus", "prometheusSpec", "resources", "limits", "memory")
	assert.Equal(t, "384Mi", grafanaLim)
	assert.Equal(t, "1Gi", promLim, "Prometheus untouched: medium profile default still applies")

	// Traefik chart updates its own resources block.
	gotTraefik := &unstructured.Unstructured{}
	gotTraefik.SetGroupVersionKind(helmChartGVK)
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "traefik", Namespace: helmChartNamespace}, gotTraefik))
	traefikValues, _, _ := unstructured.NestedString(gotTraefik.Object, "spec", "valuesContent")
	traefikLim, _, _ := unstructured.NestedString(yamlToMap(t, traefikValues), "resources", "limits", "memory")
	assert.Equal(t, "384Mi", traefikLim)

	// KEDA's override is keyed under resources.operator (the nested path).
	gotKeda := &unstructured.Unstructured{}
	gotKeda.SetGroupVersionKind(helmChartGVK)
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "keda", Namespace: helmChartNamespace}, gotKeda))
	kedaValues, _, _ := unstructured.NestedString(gotKeda.Object, "spec", "valuesContent")
	kedaLim, _, _ := unstructured.NestedString(yamlToMap(t, kedaValues), "resources", "operator", "limits", "memory")
	assert.Equal(t, "192Mi", kedaLim)

	// Status reflects the per-component limits.
	var after kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &after))
	byName := map[string]string{}
	for _, c := range after.Status.Components {
		byName[c.Name] = c.CurrentMemoryLimit
	}
	assert.Equal(t, "384Mi", byName[platform.ComponentGrafana])
	assert.Equal(t, "384Mi", byName[platform.ComponentTraefik])
	assert.Equal(t, "192Mi", byName[platform.ComponentKeda])
}

func yamlToMap(t *testing.T, s string) map[string]interface{} {
	t.Helper()
	var m map[string]interface{}
	require.NoError(t, yaml.Unmarshal([]byte(s), &m))
	return m
}

func TestPlatformConfigReconciler_SkipsNanoReconcile(t *testing.T) {
	scheme := testScheme()

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileNano},
	}

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err, "nano profile must reconcile cleanly without HelmCharts present")

	var gotPC kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &gotPC))
	assert.Equal(t, platform.ProfileNano, gotPC.Status.Profile)
}

func TestPlatformConfigReconciler_ClearsAtCeilingWhenLimitDropsBelow(t *testing.T) {
	// A user manually drops the prometheus limit from 4Gi (ceiling) down
	// to 2Gi. The AtCeiling flag from the previous auto-bump must clear,
	// otherwise the console keeps showing the misleading warning.
	scheme := testScheme()

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileXLarge,
			Components: []kipperv1.ComponentOverride{
				{Name: componentPrometheus, MemoryLimit: "2Gi"},
			},
		},
		Status: kipperv1.PlatformConfigStatus{
			Profile: platform.ProfileXLarge,
			Components: []kipperv1.ComponentStatus{
				{
					Name:               componentPrometheus,
					CurrentMemoryLimit: "4Gi", // previously bumped to ceiling
					AtCeiling:          true,
				},
			},
		},
	}

	promChart := newHelmChart("kube-prometheus-stack", `prometheus:
  prometheusSpec:
    resources:
      requests:
        memory: 1Gi
      limits:
        memory: 4Gi
`)

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, promChart).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)

	var got kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))

	var prom *kipperv1.ComponentStatus
	for i := range got.Status.Components {
		if got.Status.Components[i].Name == componentPrometheus {
			prom = &got.Status.Components[i]
		}
	}
	require.NotNil(t, prom)
	assert.Equal(t, "2Gi", prom.CurrentMemoryLimit, "current limit follows the new override")
	assert.False(t, prom.AtCeiling, "AtCeiling must clear when the limit drops below the ceiling")
}

func TestPlatformConfigReconciler_PreservesBumpHistoryOnRefresh(t *testing.T) {
	// When the OOM watcher writes a bump record, the spec change triggers
	// the PlatformConfigReconciler. The reconciler must merge into the
	// existing status component entry, not rebuild it from scratch — losing
	// LastBumpAt would also lose the cooldown that prevents runaway bumps.
	scheme := testScheme()

	bumpedAt := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileSmall,
			Components: []kipperv1.ComponentOverride{
				{Name: componentPrometheus, MemoryLimit: "1Gi"},
			},
		},
		Status: kipperv1.PlatformConfigStatus{
			Profile: platform.ProfileSmall,
			Components: []kipperv1.ComponentStatus{
				{
					Name:               componentPrometheus,
					CurrentMemoryLimit: "1Gi",
					LastBumpAt:         &bumpedAt,
					LastBumpFrom:       "512Mi",
					LastBumpTo:         "1Gi",
					LastBumpReason:     "OOMKilled detected on pod prometheus-...",
					AtCeiling:          false,
				},
			},
		},
	}

	promChart := newHelmChart("kube-prometheus-stack", `prometheus:
  prometheusSpec:
    resources:
      requests:
        memory: 256Mi
      limits:
        memory: 512Mi
`)

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, promChart).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)

	var got kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &got))

	var prom *kipperv1.ComponentStatus
	for i := range got.Status.Components {
		if got.Status.Components[i].Name == componentPrometheus {
			prom = &got.Status.Components[i]
		}
	}
	require.NotNil(t, prom, "prometheus status entry must remain after reconcile")
	assert.Equal(t, "1Gi", prom.CurrentMemoryLimit, "applied limit should be reflected")
	require.NotNil(t, prom.LastBumpAt, "LastBumpAt must survive reconcile or the cooldown breaks")
	assert.Equal(t, "512Mi", prom.LastBumpFrom)
	assert.Equal(t, "1Gi", prom.LastBumpTo)
	assert.Contains(t, prom.LastBumpReason, "OOMKilled")
}

func TestPlatformConfigReconciler_NoStatusWriteAtSteadyState(t *testing.T) {
	scheme := testScheme()

	// PlatformConfig already in steady state: spec matches a prior reconcile
	// whose status reflects what every chart already carries. The path
	// table covers seven components across six charts, so this fixture
	// has to seed all of them — otherwise the reconciler would patch in
	// the missing defaults and that counts as a status change.
	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName, ResourceVersion: "5"},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileMedium},
		Status: kipperv1.PlatformConfigStatus{
			Profile: platform.ProfileMedium,
			Components: []kipperv1.ComponentStatus{
				{Name: componentPrometheus, CurrentMemoryLimit: "1Gi"},
				{Name: platform.ComponentGrafana, CurrentMemoryLimit: "128Mi"},
				{Name: componentLoki, CurrentMemoryLimit: "512Mi"},
				{Name: platform.ComponentPromtail, CurrentMemoryLimit: "128Mi"},
				{Name: platform.ComponentTraefik, CurrentMemoryLimit: "256Mi"},
				{Name: platform.ComponentKeda, CurrentMemoryLimit: "256Mi"},
				{Name: platform.ComponentVelero, CurrentMemoryLimit: "512Mi"},
			},
		},
	}

	promChart := newHelmChart("kube-prometheus-stack", `prometheus:
  prometheusSpec:
    resources:
      requests:
        memory: 512Mi
      limits:
        memory: 1Gi
grafana:
  resources:
    requests:
      memory: 64Mi
    limits:
      memory: 128Mi
`)
	lokiChart := newHelmChart("loki", `singleBinary:
  resources:
    requests:
      memory: 256Mi
    limits:
      memory: 512Mi
`)
	promtailChart := newHelmChart("promtail", `resources:
  requests:
    memory: 32Mi
  limits:
    memory: 128Mi
`)
	traefikChart := newHelmChart("traefik", `resources:
  requests:
    memory: 64Mi
  limits:
    memory: 256Mi
`)
	kedaChart := newHelmChart("keda", `resources:
  operator:
    requests:
      memory: 64Mi
    limits:
      memory: 256Mi
`)
	veleroChart := newHelmChart("velero", `resources:
  requests:
    memory: 256Mi
  limits:
    memory: 512Mi
`)

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, promChart, lokiChart, promtailChart, traefikChart, kedaChart, veleroChart).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)

	var after kipperv1.PlatformConfig
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: PlatformConfigName}, &after))

	// A status write would bump ResourceVersion. Steady-state reconcile must
	// avoid that or the watch loop will self-enqueue forever.
	assert.Equal(t, "5", after.ResourceVersion,
		"reconcile at steady state must not write status (would loop the watcher)")
}

func TestPlatformConfigReconciler_DeletesHelmChartWhenDisabled(t *testing.T) {
	scheme := testScheme()
	no := false

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileMedium,
			Components: []kipperv1.ComponentOverride{
				{Name: componentLoki, Enabled: &no},
			},
		},
	}

	promChart := newHelmChart("kube-prometheus-stack", `prometheus:
  prometheusSpec:
    resources:
      requests:
        memory: 256Mi
      limits:
        memory: 512Mi
`)
	lokiChart := newHelmChart("loki", `singleBinary:
  resources:
    requests:
      memory: 128Mi
    limits:
      memory: 384Mi
`)

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, promChart, lokiChart).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)

	check := &unstructured.Unstructured{}
	check.SetGroupVersionKind(helmChartGVK)
	err = fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "loki", Namespace: helmChartNamespace}, check)
	assert.Error(t, err, "loki HelmChart should have been deleted")

	// Prometheus must remain untouched.
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kube-prometheus-stack", Namespace: helmChartNamespace}, check))
}

func TestPlatformConfigReconciler_RecreatesHelmChartWhenRe_enabled(t *testing.T) {
	scheme := testScheme()
	yes := true

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileMedium,
			Components: []kipperv1.ComponentOverride{
				{Name: componentLoki, Enabled: &yes},
			},
		},
	}

	// No loki HelmChart present — simulate a previously disabled cluster.
	promChart := newHelmChart("kube-prometheus-stack", `prometheus:
  prometheusSpec:
    resources:
      requests:
        memory: 256Mi
      limits:
        memory: 512Mi
`)

	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc, promChart).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)

	loki := &unstructured.Unstructured{}
	loki.SetGroupVersionKind(helmChartGVK)
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "loki", Namespace: helmChartNamespace}, loki),
		"loki HelmChart must be recreated when explicitly re-enabled")

	values, _, _ := unstructured.NestedString(loki.Object, "spec", "valuesContent")
	assert.Contains(t, values, "memory: 256Mi", "loki request should match medium profile")
	assert.Contains(t, values, "memory: 512Mi", "loki limit should match medium profile")
}

func TestPlatformConfigReconciler_IgnoresUnknownCRName(t *testing.T) {
	scheme := testScheme()

	fakeClient := crfake.NewClientBuilder().WithScheme(scheme).Build()
	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "not-the-one"}})
	require.NoError(t, err)
	assert.Empty(t, res, "non-singleton names must be silently ignored")
}

// newHelmChart builds a fake helm.cattle.io HelmChart that the reconciler can
// find and update. Skipping the spec.chart / spec.repo fields keeps the test
// focused on the valuesContent rewrite logic; the real chart fields don't
// participate in the reconciler's decisions.
func newHelmChart(name, valuesContent string) *unstructured.Unstructured {
	chart := &unstructured.Unstructured{}
	chart.SetGroupVersionKind(helmChartGVK)
	chart.SetName(name)
	chart.SetNamespace(helmChartNamespace)
	_ = unstructured.SetNestedField(chart.Object, valuesContent, "spec", "valuesContent")
	return chart
}

// Sanity check that the helmChartGVK matches what we render in real installer
// manifests. If somebody renames the group, both this constant and the
// installer's HelmChart manifests must move together.
func TestHelmChartGVKMatchesInstallerManifest(t *testing.T) {
	assert.Equal(t, "helm.cattle.io", helmChartGVK.Group)
	assert.Equal(t, "v1", helmChartGVK.Version)
	assert.Equal(t, "HelmChart", helmChartGVK.Kind)
	assert.True(t, strings.HasPrefix(helmChartGVK.GroupVersion().String(), "helm.cattle.io/"))
}

func TestPlatformConfigReconciler_ProvisionsGrafanaAdminSecret(t *testing.T) {
	scheme := testScheme()

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileMedium},
	}
	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)

	secret := &corev1.Secret{}
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: platform.MonitoringNamespace, Name: platform.GrafanaAdminSecretName}, secret),
		"enabling monitoring must provision the grafana admin secret")

	assert.Equal(t, platform.GrafanaAdminUser, string(secret.Data[platform.GrafanaAdminUserKey]))
	password := string(secret.Data[platform.GrafanaAdminPasswordKey])
	assert.NotEmpty(t, password, "the admin password must be set")
	assert.NotEqual(t, "kipper", password, "the admin password must not be the old static value")

	// A second reconcile must not rotate the password (create-if-absent), or
	// a saved login would break on every reconcile.
	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)
	again := &corev1.Secret{}
	require.NoError(t, fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: platform.MonitoringNamespace, Name: platform.GrafanaAdminSecretName}, again))
	assert.Equal(t, password, string(again.Data[platform.GrafanaAdminPasswordKey]),
		"the admin password must stay stable across reconciles")
}

func TestPlatformConfigReconciler_SkipsChartWhenGrafanaSecretFails(t *testing.T) {
	scheme := testScheme()

	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileMedium},
	}
	// Fail the Secret create so the admin credential is never provisioned.
	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Secret); ok {
					return errors.NewInternalError(stderrors.New("boom"))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.Error(t, err, "a failed admin-secret provision must surface (and requeue)")

	// The HelmChart must NOT have been created, or Grafana would boot with a
	// dangling existingSecret reference.
	chart := &unstructured.Unstructured{}
	chart.SetGroupVersionKind(helmChartGVK)
	getErr := fakeClient.Get(context.Background(),
		types.NamespacedName{Name: "kube-prometheus-stack", Namespace: helmChartNamespace}, chart)
	assert.True(t, errors.IsNotFound(getErr),
		"the monitoring chart must not be created when the admin secret could not be provisioned")
}

func TestPlatformConfigReconciler_SkipsGrafanaSecretWhenMonitoringDisabled(t *testing.T) {
	scheme := testScheme()

	disabled := false
	pc := &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec: kipperv1.PlatformConfigSpec{
			Profile: platform.ProfileMedium,
			Components: []kipperv1.ComponentOverride{
				{Name: componentPrometheus, Enabled: &disabled},
			},
		},
	}
	fakeClient := crfake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pc).
		WithStatusSubresource(&kipperv1.PlatformConfig{}).
		Build()

	r := &PlatformConfigReconciler{Client: fakeClient, Scheme: scheme}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	require.NoError(t, err)

	err = fakeClient.Get(context.Background(),
		types.NamespacedName{Namespace: platform.MonitoringNamespace, Name: platform.GrafanaAdminSecretName}, &corev1.Secret{})
	assert.True(t, errors.IsNotFound(err), "no grafana secret should be provisioned while monitoring is disabled")
}
