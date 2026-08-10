package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/kip/internal/config"
)

func TestSuggestForAppCoversCommonFields(t *testing.T) {
	replicas := int32(3)
	d := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy-api", Namespace: "default"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "api",
							Image: "ghcr.io/acme/api:v1",
							Ports: []corev1.ContainerPort{{ContainerPort: 8080}},
							Resources: corev1.ResourceRequirements{
								Limits: corev1.ResourceList{
									corev1.ResourceMemory: resource.MustParse("256Mi"),
									corev1.ResourceCPU:    resource.MustParse("100m"),
								},
							},
							Env: []corev1.EnvVar{
								{Name: "LOG_LEVEL", Value: "info"},
								{Name: "FROM_SECRET", ValueFrom: &corev1.EnvVarSource{}},
							},
						},
					},
				},
			},
		},
	}

	got := suggestForApp(d, "myproj", "test")

	assert.Contains(t, got, "kip app deploy")
	assert.Contains(t, got, "--name legacy-api")
	assert.Contains(t, got, "--project myproj")
	assert.Contains(t, got, "--environment test")
	assert.Contains(t, got, "--image ghcr.io/acme/api:v1")
	assert.Contains(t, got, "--port 8080")
	assert.Contains(t, got, "--memory 256Mi")
	assert.Contains(t, got, "--cpu 100m")
	assert.Contains(t, got, "--env LOG_LEVEL=info")
	assert.Contains(t, got, "--replicas 3")
	assert.NotContains(t, got, "FROM_SECRET", "ValueFrom env vars cannot be expressed as --env flags")
}

func TestSuggestForServiceIncludesType(t *testing.T) {
	storage := resource.MustParse("5Gi")
	s := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "legacy-db",
			Labels: map[string]string{labels.ServiceType: "postgres"},
		},
		Spec: appsv1.StatefulSetSpec{
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{
				{
					Spec: corev1.PersistentVolumeClaimSpec{
						Resources: corev1.VolumeResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceStorage: storage},
						},
					},
				},
			},
		},
	}

	got := suggestForService(s, "myproj", "test")

	assert.Contains(t, got, "kip service add postgres")
	assert.Contains(t, got, "--name legacy-db")
	assert.Contains(t, got, "--project myproj")
	assert.Contains(t, got, "--environment test")
	assert.Contains(t, got, "--storage 5Gi")
}

func TestResolveNamespaceToProjectMatchesEnvNamespaces(t *testing.T) {
	cluster := &config.Cluster{}
	projects := []unstructured.Unstructured{
		{
			Object: map[string]interface{}{
				"metadata": map[string]interface{}{"name": "hrportal"},
				"spec": map[string]interface{}{
					"environments": []interface{}{
						map[string]interface{}{"name": "test"},
						map[string]interface{}{"name": "prod"},
					},
				},
			},
		},
	}

	project, env := resolveNamespaceToProject(cluster, projects, "hrportal-test")
	assert.Equal(t, "hrportal", project)
	assert.Equal(t, "test", env)

	project, env = resolveNamespaceToProject(cluster, projects, "elsewhere")
	assert.Equal(t, "", project, "no match should return empty strings")
	assert.Equal(t, "", env)
}
