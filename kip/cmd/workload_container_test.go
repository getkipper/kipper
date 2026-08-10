package cmd

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestWorkloadContainerName pins the sidecar fix: log and exec targets must
// resolve to the workload's own container on pods carrying the
// kipper-instance-proxy sidecar, where an unnamed container request fails
// and a positional pick can land on the proxy.
func TestWorkloadContainerName(t *testing.T) {
	pod := func(names ...string) *corev1.Pod {
		p := &corev1.Pod{}
		for _, n := range names {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: n})
		}
		return p
	}

	tests := []struct {
		name     string
		pod      *corev1.Pod
		workload string
		want     string
	}{
		{"app with sidecar", pod("webapp", kipperSidecarContainer), "webapp", "webapp"},
		{"sidecar listed first", pod(kipperSidecarContainer, "webapp"), "webapp", "webapp"},
		{"single container app", pod("webapp"), "webapp", "webapp"},
		{"service container named after engine", pod("postgres"), "mydb", "postgres"},
		{"engine container plus sidecar", pod(kipperSidecarContainer, "postgres"), "mydb", "postgres"},
		{"only the sidecar", pod(kipperSidecarContainer), "webapp", kipperSidecarContainer},
		{"no containers", pod(), "webapp", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := workloadContainerName(tt.pod, tt.workload); got != tt.want {
				t.Errorf("workloadContainerName() = %q, want %q", got, tt.want)
			}
		})
	}
}
