package cmd

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestTuningModeRoundTrip pins the ConfigMap contract the resource
// controller reads every tick: missing map means auto, expert switches
// tuning off, and setting a mode creates the map on first use.
func TestTuningModeRoundTrip(t *testing.T) {
	ctx := context.Background()
	clientset := fake.NewSimpleClientset()

	mode, err := getTuningMode(ctx, clientset)
	if err != nil {
		t.Fatalf("getTuningMode on empty cluster: %v", err)
	}
	if mode != tuningModeAuto {
		t.Errorf("default mode = %q, want auto", mode)
	}

	if err := setTuningMode(ctx, clientset, tuningModeExpert); err != nil {
		t.Fatalf("setTuningMode(expert): %v", err)
	}
	if mode, _ = getTuningMode(ctx, clientset); mode != tuningModeExpert {
		t.Errorf("mode after set = %q, want expert", mode)
	}

	if err := setTuningMode(ctx, clientset, tuningModeAuto); err != nil {
		t.Fatalf("setTuningMode(auto) over existing map: %v", err)
	}
	if mode, _ = getTuningMode(ctx, clientset); mode != tuningModeAuto {
		t.Errorf("mode after reset = %q, want auto", mode)
	}
}

// TestTuningModeIgnoresGarbageValue: an unrecognised value in the map falls
// back to auto, matching the resource controller's own read.
func TestTuningModeIgnoresGarbageValue(t *testing.T) {
	clientset := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: tuningModeConfigMap, Namespace: tuningModeNamespace},
		Data:       map[string]string{"mode": "turbo"},
	})

	mode, err := getTuningMode(context.Background(), clientset)
	if err != nil {
		t.Fatalf("getTuningMode: %v", err)
	}
	if mode != tuningModeAuto {
		t.Errorf("mode = %q, want auto fallback", mode)
	}
}
