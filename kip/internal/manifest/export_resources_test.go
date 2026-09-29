package manifest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ownedEntry(manager, fields string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{Manager: manager, FieldsType: "FieldsV1", FieldsV1: &metav1.FieldsV1{Raw: []byte(fields)}}
}

// Export user and held values, omit automatic values, and identify held values
// in the export notes.
func TestExportAppResourcesKeepsTheUsersValues(t *testing.T) {
	spec := map[string]interface{}{"resources": map[string]interface{}{
		"profile": "standard", "cpuRequest": "300m", "cpuLimit": "300m", "memoryRequest": "512Mi", "memoryLimit": "2Gi",
	}}
	owners := []metav1.ManagedFieldsEntry{
		ownedEntry("kip", `{"f:spec":{"f:resources":{"f:memoryRequest":{},"f:memoryLimit":{}}}}`),
		ownedEntry("console-api", `{"f:spec":{"f:resources":{"f:cpuRequest":{}}}}`),
		ownedEntry("before-first-apply", `{"f:spec":{"f:resources":{"f:cpuLimit":{}}}}`),
	}
	m := &Manifest{}
	got := exportAppResources(spec, owners, "web", m)

	assert.Equal(t, &ResourceSpec{Profile: "standard", CPULimit: "300m", MemoryRequest: "512Mi", MemoryLimit: "2Gi"}, got)
	assert.Equal(t, []string{`app "web": cpuLimit 300m was set by nobody Kipper can name; applying this file makes it yours`}, m.Notes)
}
