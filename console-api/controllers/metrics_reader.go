package controllers

import (
	"context"
	"fmt"

	"k8s.io/client-go/kubernetes"
)

// apiServerMetricsReader reads the API server's /metrics through a clientset,
// which authenticates as the console-api ServiceAccount. That SA is granted
// get on the /metrics nonResourceURL.
type apiServerMetricsReader struct {
	client kubernetes.Interface
}

// NewAPIServerMetricsReader returns a MetricsReader backed by a clientset.
func NewAPIServerMetricsReader(client kubernetes.Interface) MetricsReader {
	return apiServerMetricsReader{client: client}
}

func (m apiServerMetricsReader) ReadMetrics(ctx context.Context) (string, error) {
	raw, err := m.client.CoreV1().RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		return "", fmt.Errorf("reading /metrics: %w", err)
	}
	return string(raw), nil
}
