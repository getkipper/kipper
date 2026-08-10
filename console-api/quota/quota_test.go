package quota

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func rq(hardCPU, usedCPU string) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: kipperv1.ProjectQuotaName, Namespace: "shop"},
		Spec:       corev1.ResourceQuotaSpec{Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(hardCPU)}},
		Status:     corev1.ResourceQuotaStatus{Used: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(usedCPU)}},
	}
}

func deploy(replicas int32, cpuReq string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop"},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}, // no surge, keeps the math simple
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name: "api",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpuReq)},
						},
					}},
				},
			},
		},
	}
}

func TestPreflightDeployment(t *testing.T) {
	tests := []struct {
		name      string
		objs      []runtime.Object
		change    Change
		wantFits  bool
		wantDimIf string
	}{
		{
			name:     "increase within quota fits",
			objs:     []runtime.Object{deploy(1, "100m"), rq("4", "1")},
			change:   Change{CPURequest: "500m"},
			wantFits: true,
		},
		{
			name:      "increase over quota is blocked",
			objs:      []runtime.Object{deploy(1, "100m"), rq("2", "1")},
			change:    Change{CPURequest: "3"},
			wantFits:  false,
			wantDimIf: "requests.cpu",
		},
		{
			name:     "decrease always fits",
			objs:     []runtime.Object{deploy(1, "1"), rq("2", "1900m")},
			change:   Change{CPURequest: "100m"},
			wantFits: true,
		},
		{
			name:     "per-replica delta multiplies over replicas",
			objs:     []runtime.Object{deploy(3, "100m"), rq("2", "300m")},
			change:   Change{CPURequest: "800m"}, // delta 700m x 3 = 2100m + 300m used = 2400m > 2000m
			wantFits: false,
		},
		{
			name:     "no quota on namespace fits",
			objs:     []runtime.Object{deploy(1, "100m")},
			change:   Change{CPURequest: "100"},
			wantFits: true,
		},
		{
			name:     "missing deployment fits",
			objs:     []runtime.Object{rq("2", "1")},
			change:   Change{CPURequest: "100"},
			wantFits: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientset(tt.objs...)
			pf, err := PreflightDeployment(context.Background(), c, "shop", "api", tt.change)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if pf.Fits != tt.wantFits {
				t.Fatalf("Fits=%v want %v (result %+v)", pf.Fits, tt.wantFits, pf)
			}
			if !tt.wantFits && tt.wantDimIf != "" && pf.Dimension != tt.wantDimIf {
				t.Errorf("dimension=%q want %q", pf.Dimension, tt.wantDimIf)
			}
		})
	}
}

func TestPreflightStatefulSet(t *testing.T) {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop"},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr(int32(2)),
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name:      "db",
				Resources: corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("200m")}},
			}}}},
		},
	}
	c := fake.NewClientset(sts, rq("2", "400m"))

	// Raise each replica to 1 CPU: delta 800m x 2 = 1600m + 400m used = 2000m,
	// not over the 2000m hard cap, so it fits (boundary).
	pf, err := PreflightStatefulSet(context.Background(), c, "shop", "db", Change{CPURequest: "1"})
	if err != nil {
		t.Fatal(err)
	}
	if !pf.Fits {
		t.Errorf("expected boundary change to fit, got %+v", pf)
	}

	// One more milli over the cap blocks.
	pf, err = PreflightStatefulSet(context.Background(), c, "shop", "db", Change{CPURequest: "1001m"})
	if err != nil {
		t.Fatal(err)
	}
	if pf.Fits {
		t.Errorf("expected over-cap change to be blocked, got %+v", pf)
	}
}

func TestDeploymentSurgePods(t *testing.T) {
	plain := &appsv1.Deployment{}
	if got := DeploymentSurgePods(plain, 8); got != 2 {
		t.Errorf("default 25%% of 8 = 2, got %d", got)
	}
	recreate := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}}}
	if got := DeploymentSurgePods(recreate, 4); got != 0 {
		t.Errorf("recreate has no surge, got %d", got)
	}
}

func ptr[T any](v T) *T { return &v }
