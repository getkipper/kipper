package handlers

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestAppContainerFromPod(t *testing.T) {
	tests := []struct {
		name       string
		containers []corev1.Container
		want       string
	}{
		{
			name:       "single container returns empty",
			containers: []corev1.Container{{Name: "my-app"}},
			want:       "",
		},
		{
			name: "multi container skips sidecar",
			containers: []corev1.Container{
				{Name: "my-app"},
				{Name: "kipper-instance-proxy"},
			},
			want: "my-app",
		},
		{
			name: "sidecar first still returns app",
			containers: []corev1.Container{
				{Name: "kipper-instance-proxy"},
				{Name: "my-app"},
			},
			want: "my-app",
		},
		{
			name: "multiple non-sidecar containers returns first",
			containers: []corev1.Container{
				{Name: "main"},
				{Name: "helper"},
			},
			want: "main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				Spec: corev1.PodSpec{Containers: tt.containers},
			}
			got := appContainerFromPod(pod)
			if got != tt.want {
				t.Errorf("appContainerFromPod() = %q, want %q", got, tt.want)
			}
		})
	}
}
