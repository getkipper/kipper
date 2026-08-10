package ws

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseTailLines(t *testing.T) {
	tests := []struct {
		input string
		want  int64
	}{
		{"", 100},
		{"250", 250},
		{"500", 500},
		{"1000", 1000},
		{"1500", 1000},
		{"0", 100},
		{"-1", 100},
		{"abc", 100},
		{"1", 1},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseTailLines(tt.input)
			if got != tt.want {
				t.Errorf("parseTailLines(%q) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestAppContainerName(t *testing.T) {
	tests := []struct {
		name       string
		containers []corev1.Container
		app        string
		want       string
	}{
		{
			name:       "single container returns empty",
			containers: []corev1.Container{{Name: "my-app"}},
			app:        "my-app",
			want:       "",
		},
		{
			name: "multi container returns app name",
			containers: []corev1.Container{
				{Name: "my-app"},
				{Name: "kipper-instance-proxy"},
			},
			app:  "my-app",
			want: "my-app",
		},
		{
			name: "app container not found returns first",
			containers: []corev1.Container{
				{Name: "main"},
				{Name: "kipper-instance-proxy"},
			},
			app:  "my-app",
			want: "main",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				Spec: corev1.PodSpec{Containers: tt.containers},
			}
			got := appContainerName(pod, tt.app)
			if got != tt.want {
				t.Errorf("appContainerName() = %q, want %q", got, tt.want)
			}
		})
	}
}
