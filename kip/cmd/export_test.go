package cmd

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func TestExportSplit_WritesOneFilePerEnvironment(t *testing.T) {
	dir := t.TempDir()
	dyn := newFakeDynWithProject(t, "blog", []string{"test", "acc", "prod"})
	cluster := &config.Cluster{Name: "test"}

	err := exportSplit(context.Background(), dyn, cluster, "blog", dir)
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	sort.Strings(names)
	assert.Equal(t, []string{"acc.yaml", "prod.yaml", "test.yaml"}, names,
		"--split must write every env from the Project CR's spec.environments")
}

func TestExportSplit_NoProjectFallsBackToSingleFile(t *testing.T) {
	// When the Project CR is missing or has no environments, --split
	// degrades gracefully: a single `<project>.yaml` file gets written
	// instead of an empty directory. Better than erroring — the user
	// still gets a manifest, and the missing Project is a separate
	// problem they can fix.
	dir := t.TempDir()
	dyn := newFakeDynBare(t)
	cluster := &config.Cluster{Name: "test"}

	err := exportSplit(context.Background(), dyn, cluster, "lonely", dir)
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "lonely.yaml", entries[0].Name())
}

func TestExportSplit_ResolvesProjectCRNameWithOrgPrefix(t *testing.T) {
	// Regression for the ResolveNamespace alignment: on a cluster with
	// `org: acme`, the Project CR is stored as `acme-deck`, but the
	// operator passes `--project deck`. Split must resolve the same
	// way Project create does, otherwise the Project lookup misses and
	// we degrade to a single-file fallback instead of iterating envs.
	dir := t.TempDir()
	dyn := newFakeDynWithProject(t, "acme-deck", []string{"test", "prod"})
	cluster := &config.Cluster{Name: "acme", Org: "acme"}

	err := exportSplit(context.Background(), dyn, cluster, "deck", dir)
	require.NoError(t, err)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	sort.Strings(names)
	assert.Equal(t, []string{"prod.yaml", "test.yaml"}, names,
		"split must look up the Project CR by ResolveNamespace(project, \"\"), not by raw user input")
}

func TestExportSplit_CreatesOutputDirIfMissing(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "fresh", "subdir")
	dyn := newFakeDynWithProject(t, "p", []string{"e1", "e2"})

	err := exportSplit(context.Background(), dyn, &config.Cluster{}, "p", target)
	require.NoError(t, err)
	info, err := os.Stat(target)
	require.NoError(t, err)
	assert.True(t, info.IsDir())
}

// newFakeDynWithProject returns a fake dynamic client that has a single
// Project CR with the given envs declared. No App/Service/etc. — the
// per-env exports come back empty, which is fine for the split tests
// (we're testing iteration, not field round-trip).
func newFakeDynWithProject(t *testing.T, name string, envs []string) *fake.FakeDynamicClient {
	t.Helper()
	envObjs := make([]interface{}, len(envs))
	for i, e := range envs {
		envObjs[i] = map[string]interface{}{"name": e}
	}
	project := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Project",
			"metadata":   map[string]interface{}{"name": name},
			"spec": map[string]interface{}{
				"environments": envObjs,
			},
		},
	}
	scheme := runtime.NewScheme()
	_ = metav1.AddMetaToScheme(scheme)
	gvrToListKind := map[schema.GroupVersionResource]string{
		manifest.AppGVR:      "AppList",
		manifest.ServiceGVR:  "ServiceList",
		manifest.VolumeGVR:   "VolumeList",
		manifest.JobGVR:      "JobList",
		manifest.FunctionGVR: "FunctionList",
		manifest.ProjectGVR:  "ProjectList",
	}
	return fake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind, project)
}

func newFakeDynBare(t *testing.T) *fake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	_ = metav1.AddMetaToScheme(scheme)
	gvrToListKind := map[schema.GroupVersionResource]string{
		manifest.AppGVR:      "AppList",
		manifest.ServiceGVR:  "ServiceList",
		manifest.VolumeGVR:   "VolumeList",
		manifest.JobGVR:      "JobList",
		manifest.FunctionGVR: "FunctionList",
		manifest.ProjectGVR:  "ProjectList",
	}
	return fake.NewSimpleDynamicClientWithCustomListKinds(scheme, gvrToListKind)
}
