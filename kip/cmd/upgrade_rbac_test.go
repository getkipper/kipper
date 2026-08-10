package cmd

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/kip/internal/installer"
)

func TestConsoleRBACManifestShape(t *testing.T) {
	docs := strings.Split(installer.ConsoleRBACManifest, "\n---\n")
	require.Len(t, docs, 3)

	kinds := make([]string, 0, 3)
	for _, doc := range docs {
		var obj map[string]interface{}
		require.NoError(t, yaml.Unmarshal([]byte(doc), &obj))
		kind, _ := obj["kind"].(string)
		kinds = append(kinds, kind)
		_, known := consoleRBACGVRs[kind]
		assert.True(t, known, "kind %q must have a GVR mapping in applyConsoleRBAC", kind)
	}
	assert.Equal(t, []string{"ServiceAccount", "ClusterRole", "ClusterRoleBinding"}, kinds)

	// The API-gateway resources are the permission gap that motivated
	// re-applying RBAC on upgrade; losing them regresses every console
	// API-key view on existing clusters.
	for _, resource := range []string{"usageplans", "apikeys", "usagerollups"} {
		assert.Contains(t, installer.ConsoleRBACManifest, resource)
	}
	// The manifest is applied verbatim; a stray format placeholder means a
	// template got merged back in.
	assert.NotContains(t, installer.ConsoleRBACManifest, "%s")
}

func TestApplyConsoleRBACCreatesAndUpdates(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())

	require.NoError(t, applyConsoleRBAC(context.Background(), dyn))

	role, err := dyn.Resource(consoleRBACGVRs["ClusterRole"]).Get(context.Background(), "console-api", metav1.GetOptions{})
	require.NoError(t, err)
	rules, found, err := unstructured.NestedSlice(role.Object, "rules")
	require.NoError(t, err)
	require.True(t, found)
	assert.NotEmpty(t, rules)

	sa, err := dyn.Resource(consoleRBACGVRs["ServiceAccount"]).Namespace("kipper-system").Get(context.Background(), "console-api", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "console-api", sa.GetName())

	// Second run must update in place, not fail on AlreadyExists.
	require.NoError(t, applyConsoleRBAC(context.Background(), dyn))
}
