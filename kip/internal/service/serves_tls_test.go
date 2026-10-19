package service

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func tlsCR(generation, observed int64, status, reason string) *unstructured.Unstructured {
	cr := &unstructured.Unstructured{Object: map[string]interface{}{"status": map[string]interface{}{
		"conditions": []interface{}{map[string]interface{}{
			"type": "TLSReady", "status": status, "reason": reason, "observedGeneration": observed}}}}}
	cr.SetGeneration(generation)
	return cr
}

func TestTLSIsServedOnlyWhenTheControllerConfirmedTheCurrentSpec(t *testing.T) {
	assert.True(t, ServesTLS(tlsCR(4, 4, "True", "Serving")))
	assert.False(t, ServesTLS(tlsCR(5, 4, "True", "Serving")), "the spec changed after the last confirmation")
	assert.False(t, ServesTLS(tlsCR(4, 4, "False", "RollingOut")))
	assert.False(t, ServesTLS(&unstructured.Unstructured{Object: map[string]interface{}{}}))
}
