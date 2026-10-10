package deployer

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/provenance"
)

func ownedFields(manager string, fields ...string) metav1.ManagedFieldsEntry {
	raw := `{"f:spec":{"f:resources":{`
	for i, f := range fields {
		if i > 0 {
			raw += ","
		}
		raw += `"f:` + f + `":{}`
	}
	raw += `}}}`
	return metav1.ManagedFieldsEntry{Manager: manager, Operation: metav1.ManagedFieldsOperationApply, FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(raw)}}
}

func seedInfoApp(t *testing.T, d *Deployer) {
	t.Helper()
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "App",
		"metadata":   map[string]interface{}{"name": "api", "namespace": "default", "uid": "app-uid"},
		"spec": map[string]interface{}{
			"image":    "ghcr.io/acme/api:v1",
			"replicas": int64(2),
			"resources": map[string]interface{}{
				"profile":       "jvm",
				"cpuRequest":    "200m",
				"cpuLimit":      "1",
				"memoryRequest": "1Gi",
				"memoryLimit":   "1Gi",
			},
			"autoscale": map[string]interface{}{"enabled": true, "minReplicas": int64(2), "maxReplicas": int64(5), "cpuTarget": int64(70)},
		},
		"status": map[string]interface{}{"phase": "Running", "replicas": int64(3), "readyReplicas": int64(3)},
	}}
	app.SetManagedFields([]metav1.ManagedFieldsEntry{
		ownedFields("kip", "cpuRequest", "cpuLimit"),
		ownedFields("console-api", "memoryRequest", "memoryLimit"),
	})
	_, err := d.Dynamic.Resource(AppGVR).Namespace("default").Create(context.Background(), app, metav1.CreateOptions{})
	require.NoError(t, err)
}

func seedLiveDeployment(t *testing.T, d *Deployer) {
	t.Helper()
	replicas := int32(3)
	_, err := d.Client.AppsV1().Deployments("default").Create(context.Background(), &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: "api",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("300m"), corev1.ResourceMemory: resource.MustParse("1Gi")},
					Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")},
				},
			}}}},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 3},
	}, metav1.CreateOptions{})
	require.NoError(t, err)
}

func seedTuning(t *testing.T, d *Deployer, ownerUID string, recommendation map[string]interface{}) {
	t.Helper()
	tuning := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "ResourceTuning",
		"metadata": map[string]interface{}{
			"name":      "app-api",
			"namespace": "default",
			"ownerReferences": []interface{}{map[string]interface{}{
				"apiVersion": "kipper.run/v1alpha1", "kind": "App", "name": "api", "uid": ownerUID, "controller": true,
			}},
		},
		"status": map[string]interface{}{"recommendation": recommendation},
	}}
	_, err := d.Dynamic.Resource(ResourceTuningGVR).Namespace("default").Create(context.Background(), tuning, metav1.CreateOptions{})
	require.NoError(t, err)
}

func TestReadAppInfo(t *testing.T) {
	ctx := context.Background()

	t.Run("reports status, scaling and who set each resource value", func(t *testing.T) {
		d, _ := testDeployer()
		seedInfoApp(t, d)
		seedLiveDeployment(t, d)
		scaled := metav1.NewTime(time.Date(2026, 10, 10, 20, 56, 0, 0, time.UTC))
		_, err := d.Client.AutoscalingV2().HorizontalPodAutoscalers("default").Create(ctx, &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "default"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{Metrics: []autoscalingv2.MetricSpec{{
				Type:     autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{Name: corev1.ResourceMemory},
			}}},
			Status: autoscalingv2.HorizontalPodAutoscalerStatus{LastScaleTime: &scaled},
		}, metav1.CreateOptions{})
		require.NoError(t, err)
		seedTuning(t, d, "app-uid", map[string]interface{}{"memoryRequest": "768Mi", "memoryLimit": "1Gi"})

		info, err := d.ReadAppInfo(ctx, "default", "api")
		require.NoError(t, err)

		assert.Equal(t, "running", info.Status.Status)
		assert.Equal(t, int32(3), info.Status.Ready)
		assert.Equal(t, "jvm", info.Profile)
		require.NotNil(t, info.Autoscale.LastScaleTime)
		assert.True(t, info.Autoscale.LastScaleTime.Equal(scaled.Time))
		assert.Equal(t, map[string]bool{"memory": true}, info.Autoscale.HPAMetrics, "the metrics come from the autoscaler's spec")

		assert.Equal(t, provenance.ModeBounded, info.CPU.Mode)
		assert.Equal(t, SpecValue{Value: "200m", Source: provenance.User}, info.CPU.Request)
		assert.Equal(t, SpecValue{Value: "1", Source: provenance.User}, info.CPU.Limit)
		assert.Equal(t, "300m", info.CPU.LiveRequest)
		assert.Equal(t, "1", info.CPU.LiveLimit)
		assert.Empty(t, info.CPU.RecommendedRequest)

		assert.Equal(t, provenance.ModeAutomatic, info.Memory.Mode)
		assert.Equal(t, provenance.Automatic, info.Memory.Request.Source)
		assert.Equal(t, "768Mi", info.Memory.RecommendedRequest)
		assert.Equal(t, "1Gi", info.Memory.RecommendedLimit)
	})

	t.Run("ignores a tuning record that belongs to an earlier app of the same name", func(t *testing.T) {
		d, _ := testDeployer()
		seedInfoApp(t, d)
		seedTuning(t, d, "old-uid", map[string]interface{}{"cpuRequest": "2"})

		info, err := d.ReadAppInfo(ctx, "default", "api")
		require.NoError(t, err)
		assert.Empty(t, info.CPU.RecommendedRequest)
	})

	t.Run("reports a Deployment that does not exist yet", func(t *testing.T) {
		d, _ := testDeployer()
		seedInfoApp(t, d)

		info, err := d.ReadAppInfo(ctx, "default", "api")
		require.NoError(t, err)
		assert.Equal(t, TemplateMissing, info.Template)
		assert.Empty(t, info.CPU.LiveRequest)
		assert.Nil(t, info.Autoscale.LastScaleTime)
	})

	t.Run("reports a Deployment it may not read as unknown, not missing", func(t *testing.T) {
		d, _ := testDeployer()
		seedInfoApp(t, d)
		d.Client.(*fake.Clientset).PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "api", nil)
		})

		info, err := d.ReadAppInfo(ctx, "default", "api")
		require.NoError(t, err)
		assert.Equal(t, TemplateUnreadable, info.Template)
	})

	t.Run("reads the pod template when the Deployment exists", func(t *testing.T) {
		d, _ := testDeployer()
		seedInfoApp(t, d)
		seedLiveDeployment(t, d)

		info, err := d.ReadAppInfo(ctx, "default", "api")
		require.NoError(t, err)
		assert.Equal(t, TemplateRead, info.Template)
	})

	t.Run("defaults the profile to standard", func(t *testing.T) {
		d, dynClient := testDeployer()
		seedApp(t, dynClient, map[string]interface{}{"image": "web:1"})

		info, err := d.ReadAppInfo(ctx, "default", "api")
		require.NoError(t, err)
		assert.Equal(t, "standard", info.Profile)
		assert.Equal(t, provenance.ModeAutomatic, info.CPU.Mode)
	})

	t.Run("returns an error for a missing app", func(t *testing.T) {
		d, _ := testDeployer()
		_, err := d.ReadAppInfo(ctx, "default", "missing")
		require.Error(t, err)
	})
}
