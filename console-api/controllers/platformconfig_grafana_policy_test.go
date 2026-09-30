package controllers

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func mediumPlatformConfig() *kipperv1.PlatformConfig {
	return &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Profile: platform.ProfileMedium},
	}
}

func reconcilePlatform(t *testing.T, c client.Client) error {
	t.Helper()
	r := &PlatformConfigReconciler{Client: c, Scheme: testScheme()}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: PlatformConfigName}})
	return err
}

func grafanaPolicyIn(t *testing.T, c client.Client) *networkingv1.NetworkPolicy {
	t.Helper()
	var np networkingv1.NetworkPolicy
	err := c.Get(context.Background(), types.NamespacedName{Namespace: platform.MonitoringNamespace, Name: platform.GrafanaNetworkPolicyName}, &np)
	if apierrors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return &np
}

func prometheusChartExists(t *testing.T, c client.Client) bool {
	t.Helper()
	chart := &unstructured.Unstructured{}
	chart.SetGroupVersionKind(helmChartGVK)
	err := c.Get(context.Background(), types.NamespacedName{Namespace: helmChartNamespace, Name: "kube-prometheus-stack"}, chart)
	if apierrors.IsNotFound(err) {
		return false
	}
	require.NoError(t, err)
	return true
}

func TestPlatformConfigReconciler_AppliesGrafanaPolicyWithTheChart(t *testing.T) {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(mediumPlatformConfig()).WithStatusSubresource(&kipperv1.PlatformConfig{}).Build()

	_ = reconcilePlatform(t, c)

	np := grafanaPolicyIn(t, c)
	require.NotNil(t, np, "enabling monitoring must put the Grafana policy in place")
	assert.Equal(t, platform.TraefikNamespace, np.Spec.Ingress[0].From[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	assert.True(t, prometheusChartExists(t, c))
}

func TestPlatformConfigReconciler_GrafanaPolicyFollowsTheIngressController(t *testing.T) {
	ic := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kipper-system", Name: "ingress-controller"},
		Data:       map[string]string{"namespace": "traefik", "labelKey": "app", "labelValue": "edge"},
	}
	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(mediumPlatformConfig(), ic).WithStatusSubresource(&kipperv1.PlatformConfig{}).Build()

	_ = reconcilePlatform(t, c)

	np := grafanaPolicyIn(t, c)
	require.NotNil(t, np)
	from := np.Spec.Ingress[0].From[0]
	assert.Equal(t, "traefik", from.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"])
	assert.Equal(t, "edge", from.PodSelector.MatchLabels["app"])
}

func TestPlatformConfigReconciler_NoChartWithoutGrafanaPolicy(t *testing.T) {
	// If the policy cannot be applied, Grafana must not start in auth-proxy
	// mode with its port open to every pod.
	c := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(mediumPlatformConfig()).WithStatusSubresource(&kipperv1.PlatformConfig{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Patch: func(ctx context.Context, cl client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return errors.New("policy write refused")
				}
				return cl.Patch(ctx, obj, patch, opts...)
			},
			Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*networkingv1.NetworkPolicy); ok {
					return errors.New("policy write refused")
				}
				return cl.Create(ctx, obj, opts...)
			},
		}).Build()

	err := reconcilePlatform(t, c)

	assert.Error(t, err, "a failed policy write must requeue")
	assert.False(t, prometheusChartExists(t, c), "the chart must not be created without the policy")
}
