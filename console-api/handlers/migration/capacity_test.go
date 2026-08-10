package migration

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func requests(cpu, mem string) corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(cpu),
			corev1.ResourceMemory: resource.MustParse(mem),
		},
	}
}

// podRequests must follow the scheduler's effective-request formula, or the
// capacity precheck approves targets that cannot schedule the workloads.
func TestPodRequests(t *testing.T) {
	always := corev1.ContainerRestartPolicyAlways

	tests := []struct {
		name    string
		pod     corev1.Pod
		wantCPU int64
		wantMem int64
	}{
		{
			name: "regular containers sum",
			pod: corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{
				{Resources: requests("100m", "128Mi")},
				{Resources: requests("50m", "64Mi")},
			}}},
			wantCPU: 150,
			wantMem: 192 * 1024 * 1024,
		},
		{
			name: "large one-shot init floors the pod",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{Resources: requests("500m", "512Mi")}},
				Containers:     []corev1.Container{{Resources: requests("100m", "128Mi")}},
			}},
			wantCPU: 500,
			wantMem: 512 * 1024 * 1024,
		},
		{
			name: "one-shot init stage includes preceding sidecar",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					{Resources: requests("100m", "64Mi"), RestartPolicy: &always},
					{Resources: requests("200m", "128Mi")},
				},
				Containers: []corev1.Container{{Resources: requests("50m", "32Mi")}},
			}},
			// init stage: 100m sidecar + 200m one-shot = 300m, beating the
			// running state of 50m container + 100m sidecar.
			wantCPU: 300,
			wantMem: 192 * 1024 * 1024,
		},
		{
			name: "sidecar keeps its reservation at runtime",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{
					{Resources: requests("100m", "64Mi"), RestartPolicy: &always},
				},
				Containers: []corev1.Container{{Resources: requests("400m", "256Mi")}},
			}},
			wantCPU: 500,
			wantMem: 320 * 1024 * 1024,
		},
		{
			name: "pod overhead is added on top",
			pod: corev1.Pod{Spec: corev1.PodSpec{
				Containers: []corev1.Container{{Resources: requests("100m", "128Mi")}},
				Overhead: corev1.ResourceList{
					corev1.ResourceCPU:    resource.MustParse("10m"),
					corev1.ResourceMemory: resource.MustParse("16Mi"),
				},
			}},
			wantCPU: 110,
			wantMem: 144 * 1024 * 1024,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cpu, mem := podRequests(&tt.pod)
			if cpu != tt.wantCPU {
				t.Fatalf("cpu = %dm, want %dm", cpu, tt.wantCPU)
			}
			if mem != tt.wantMem {
				t.Fatalf("mem = %d, want %d", mem, tt.wantMem)
			}
		})
	}
}
