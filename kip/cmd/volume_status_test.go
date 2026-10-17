package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestVolumeListStatusShowsAnUnsupportedVolume(t *testing.T) {
	refused := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"phase": "Pending",
			"conditions": []interface{}{map[string]interface{}{
				"type": "SharedStorageUnsupported", "status": "True",
			}},
		},
	}}
	assert.Equal(t, "unsupported", volumeListStatus(refused))
	assert.Equal(t, "bound", volumeListStatus(&unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{"phase": "Bound"},
	}}))
	assert.Equal(t, "pending", volumeListStatus(&unstructured.Unstructured{Object: map[string]interface{}{}}))
}
