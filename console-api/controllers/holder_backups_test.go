package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sptr "k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

// longhornWithoutBackups models a local cluster grown to Longhorn, which keeps
// the backup mode it was installed with.
func longhornWithoutBackups() *kipperv1.PlatformConfig {
	return &kipperv1.PlatformConfig{
		ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName},
		Spec:       kipperv1.PlatformConfigSpec{Storage: platform.StorageLonghorn, Backup: platform.BackupNone},
	}
}

func TestClusterBackupsConfigured(t *testing.T) {
	ctx := context.Background()
	for name, tc := range map[string]struct {
		pc   *kipperv1.PlatformConfig
		want bool
	}{
		"no PlatformConfig": {nil, true},
		"in-cluster":        {&kipperv1.PlatformConfig{ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName}, Spec: kipperv1.PlatformConfigSpec{Backup: platform.BackupInCluster}}, true},
		"unrecorded":        {&kipperv1.PlatformConfig{ObjectMeta: metav1.ObjectMeta{Name: PlatformConfigName}}, true},
		"none":              {longhornWithoutBackups(), false},
	} {
		t.Run(name, func(t *testing.T) {
			b := crfake.NewClientBuilder().WithScheme(testScheme())
			if tc.pc != nil {
				b = b.WithObjects(tc.pc)
			}
			got, err := clusterBackupsConfigured(ctx, b.Build())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestVolumeHolderOnlyRunsWhenBackupsAreConfigured(t *testing.T) {
	vol := sharedVolume()
	vol.Status.Conditions = []metav1.Condition{{Type: conditionBackupHolderReady, Status: metav1.ConditionTrue, Reason: "Running"}}
	owned := buildVolumeHolder(vol)
	owned.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(vol)}
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), owned, longhornWithoutBackups())
	h.reconcile(t)

	_, ok := h.holder(t)
	assert.False(t, ok, "without backups a holder has nothing to keep mounted for")
	assert.Nil(t, h.holderCondition(t))
}

func TestVolumeHolderLeavesAForeignPodWhenBackupsAreOff(t *testing.T) {
	vol := sharedVolume()
	foreign := buildVolumeHolder(vol)
	foreign.Labels = map[string]string{"app": "something-else"}
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), foreign, longhornWithoutBackups())
	h.reconcile(t)

	_, ok := h.holder(t)
	assert.True(t, ok, "only a provable holder is deleted")
}

func TestProjectHolderOnlyRunsWhenBackupsAreConfigured(t *testing.T) {
	project := holderProject(&kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi"})
	owned := buildHolder(projectHolderTarget(project, projectHolderNamespace))
	owned.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: kipperv1.GroupVersion.String(), Kind: "Project", Name: "demo", UID: "project-uid", Controller: k8sptr.To(true),
	}}
	h := newProjectHolderHarness(t, project, projectUploadsClaim(), owned, longhornWithoutBackups())
	h.reconcile(t, 2)

	_, ok := h.holder(t)
	assert.False(t, ok)
	var got kipperv1.Project
	require.NoError(t, h.client.Get(context.Background(), crclient.ObjectKeyFromObject(project), &got))
	assert.Nil(t, apimeta.FindStatusCondition(got.Status.Conditions, conditionBackupHolderReady))
}
