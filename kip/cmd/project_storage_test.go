package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

func sharedStorageProject(sharedStorage map[string]interface{}) *unstructured.Unstructured {
	spec := map[string]interface{}{"environments": []interface{}{map[string]interface{}{"name": "prod"}}}
	if sharedStorage != nil {
		spec["sharedStorage"] = sharedStorage
	}
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1", "kind": "Project",
		"metadata": map[string]interface{}{"name": "shop", "uid": "uid-shop"},
		"spec":     spec,
	}}
}

func projectStorageDyn(t *testing.T, project *unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	t.Helper()
	dyn := copiesApplyDyn()
	_, err := dyn.Resource(manifest.ProjectGVR).Create(context.Background(), project, metav1.CreateOptions{})
	require.NoError(t, err)
	return dyn
}

// The project's copies live under spec.sharedStorage, and the rest of that
// setting is kept.
func TestProjectStorageSetsTheSharedStorageCopies(t *testing.T) {
	quickCopiesPolling(t)
	dyn := projectStorageDyn(t, sharedStorageProject(map[string]interface{}{"enabled": true, "size": "5Gi"}))
	var out bytes.Buffer

	require.NoError(t, setProjectStorageCopies(context.Background(), &out, dyn, "shop", 1, false))

	got, err := dyn.Resource(manifest.ProjectGVR).Get(context.Background(), "shop", metav1.GetOptions{})
	require.NoError(t, err)
	copies, _, _ := unstructured.NestedInt64(got.Object, "spec", "sharedStorage", "copies")
	enabled, _, _ := unstructured.NestedBool(got.Object, "spec", "sharedStorage", "enabled")
	size, _, _ := unstructured.NestedString(got.Object, "spec", "sharedStorage", "size")
	assert.Equal(t, int64(1), copies)
	assert.True(t, enabled)
	assert.Equal(t, "5Gi", size)
	_, found, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "copies")
	assert.False(t, found, "a project has no top-level copies")
	assert.Contains(t, out.String(), "Copies set to 1")

	out.Reset()
	require.NoError(t, setProjectStorageCopies(context.Background(), &out, dyn, "shop", 1, false))
	assert.Contains(t, out.String(), "Copies are already 1")
}

func TestProjectStorageNeedsSharedStorage(t *testing.T) {
	for name, project := range map[string]*unstructured.Unstructured{
		"none":     sharedStorageProject(nil),
		"disabled": sharedStorageProject(map[string]interface{}{"enabled": false}),
	} {
		t.Run(name, func(t *testing.T) {
			quickCopiesPolling(t)
			dyn := projectStorageDyn(t, project)
			err := setProjectStorageCopies(context.Background(), &bytes.Buffer{}, dyn, "shop", 1, false)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "has no shared storage")
			got, _ := dyn.Resource(manifest.ProjectGVR).Get(context.Background(), "shop", metav1.GetOptions{})
			_, found, _ := unstructured.NestedFieldNoCopy(got.Object, "spec", "sharedStorage", "copies")
			assert.False(t, found)
		})
	}
}

func TestProjectStorageRefusesTwoCopiesOfSharedStorage(t *testing.T) {
	if platform.RWXCopiesQualified {
		t.Skip("shared volumes may have two copies")
	}
	dyn := projectStorageDyn(t, sharedStorageProject(map[string]interface{}{"enabled": true}))
	err := setProjectStorageCopies(context.Background(), &bytes.Buffer{}, dyn, "shop", 2, false)
	assert.ErrorContains(t, err, "shared volumes cannot have two copies")
}

func resetFlags(cmd *cobra.Command) {
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
}

// The update help says what a copy change stops and that --wait fails on a
// blocked change.
func TestTheUpdateHelpSaysWhatStopsAndWhenWaitFails(t *testing.T) {
	assert.Contains(t, strings.Join(strings.Fields(serviceUpdateCmd.Long), " "),
		"A raise on a volume created without Longhorn's revision counter stops the service once")
	for _, cmd := range []*cobra.Command{serviceUpdateCmd, volumeUpdateCmd, projectUpdateCmd} {
		assert.Contains(t, cmd.Flags().Lookup("wait").Usage, "exits non-zero when the change is blocked", cmd.CommandPath())
	}
}

// An update without --copies leaves the copies alone, so the help shows no
// default count for it and the shared storage commands require the flag.
func TestAnUpdateWithoutCopiesLeavesThemAlone(t *testing.T) {
	for _, cmd := range []*cobra.Command{serviceUpdateCmd, volumeUpdateCmd, projectUpdateCmd} {
		assert.Equal(t, "0", cmd.Flags().Lookup("copies").DefValue, cmd.CommandPath())
		assert.NotContains(t, cmd.Flags().FlagUsages(), "(default 1)", cmd.CommandPath())
	}
	for _, cmd := range []*cobra.Command{volumeUpdateCmd, projectUpdateCmd} {
		assert.Equal(t, []string{"true"}, cmd.Flags().Lookup("copies").Annotations[cobra.BashCompOneRequiredFlag], cmd.CommandPath())
	}
	cmd, args, err := rootCmd.Find([]string{"service", "update", "db", "--memory", "1Gi"})
	require.NoError(t, err)
	require.NoError(t, cmd.ParseFlags(args))
	assert.False(t, cmd.Flags().Changed("copies"), "kip service update sends copies only when --copies is given")
	resetFlags(cmd)
}

// The help of the commands for shared storage offers only the copy count they
// accept.
func TestSharedStorageHelpOffersOnlyOneCopy(t *testing.T) {
	if platform.RWXCopiesQualified {
		t.Skip("shared volumes may have two copies")
	}
	for _, cmd := range []*cobra.Command{projectUpdateCmd, volumeUpdateCmd, volumeCreateCmd} {
		usage := cmd.Flags().Lookup("copies").Usage
		assert.NotContains(t, usage, "1 or 2", cmd.CommandPath())
		assert.NotContains(t, cmd.Use, "1|2", cmd.CommandPath())
	}
}

func TestVolumeCreateHelpDescribesANewVolume(t *testing.T) {
	assert.NotContains(t, volumeCreateCmd.Flags().Lookup("copies").Usage, "live volume")
	assert.Contains(t, volumeUpdateCmd.Flags().Lookup("copies").Usage, "live volume")
}
