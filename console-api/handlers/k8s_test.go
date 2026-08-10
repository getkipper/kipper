package handlers

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestPortIntStr(t *testing.T) {
	tests := []struct {
		name     string
		port     int32
		expected intstr.IntOrString
	}{
		{
			name:     "standard HTTP port",
			port:     80,
			expected: intstr.FromInt32(80),
		},
		{
			name:     "application port",
			port:     8080,
			expected: intstr.FromInt32(8080),
		},
		{
			name:     "zero port",
			port:     0,
			expected: intstr.FromInt32(0),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := portIntStr(tt.port)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}
