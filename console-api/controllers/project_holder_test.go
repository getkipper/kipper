package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

const projectHolderNamespace = "demo-prod"

func holderProject(shared *kipperv1.ProjectSharedStorage) *kipperv1.Project {
	return &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "demo", UID: "project-uid"},
		Spec: kipperv1.ProjectSpec{
			Environments:  []kipperv1.ProjectEnvironment{{Name: "prod"}},
			SharedStorage: shared,
		},
	}
}

// projectSharedClaim models the claim the project controller creates, which
// carries the project's labels and no owner.
func projectSharedClaim() *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: sharedUploadsClaim, Namespace: projectHolderNamespace,
			Labels: map[string]string{
				kipperLabel:         kipperValue,
				labels.ResourceType: labels.ResourceTypeSharedVolume,
				labels.Project:      "demo",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: ptr.To(platform.LonghornSingleClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("2Gi")},
			},
		},
	}
}

type projectHolderHarness struct {
	client     crclient.Client
	reconciler *ProjectReconciler
	recorder   *record.FakeRecorder
}

func newProjectHolderHarness(t *testing.T, project *kipperv1.Project, extra ...crclient.Object) *projectHolderHarness {
	t.Helper()
	scheme := testScheme()
	c := projectFakeBuilder().WithScheme(scheme).
		WithObjects(append([]crclient.Object{project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9")}, extra...)...).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	recorder := record.NewFakeRecorder(20)
	return &projectHolderHarness{
		client:     c,
		reconciler: &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c, Recorder: recorder},
		recorder:   recorder,
	}
}

func (h *projectHolderHarness) reconcile(t *testing.T, times int) {
	t.Helper()
	for range times {
		_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
		require.NoError(t, err)
	}
}

func (h *projectHolderHarness) holder(t *testing.T) (*corev1.Pod, bool) {
	t.Helper()
	var pod corev1.Pod
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: projectHolderNamespace, Name: projectHolderName}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return &pod, true
}

func (h *projectHolderHarness) condition(t *testing.T) *metav1.Condition {
	t.Helper()
	var project kipperv1.Project
	require.NoError(t, h.client.Get(context.Background(), crclient.ObjectKey{Name: "demo"}, &project))
	return apimeta.FindStatusCondition(project.Status.Conditions, conditionBackupHolderReady)
}

func TestProjectSharedStorageGetsABackupHolder(t *testing.T) {
	h := newProjectHolderHarness(t, holderProject(&kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi"}))
	h.reconcile(t, 2)

	pod, ok := h.holder(t)
	require.True(t, ok, "each environment's shared storage gets a holder")
	ref := metav1.GetControllerOf(pod)
	require.NotNil(t, ref, "the Project controls its holder, so drains and deletion treat it as managed")
	assert.Equal(t, "Project", ref.Kind)
	assert.Equal(t, "demo", ref.Name)
	assert.Equal(t, types.UID("project-uid"), ref.UID)
	assert.Equal(t, kipperValue, pod.Labels[kipperLabel])
	assert.Equal(t, labels.ResourceTypeVolumeHolder, pod.Labels[labels.ResourceType])
	assert.Equal(t, "demo", pod.Labels[labels.Project])
	assert.NotContains(t, pod.Labels, labels.VolumeName, "the Volume controller must not route a project holder to a Volume")
	require.Len(t, pod.Spec.Volumes, 1)
	assert.Equal(t, sharedUploadsClaim, pod.Spec.Volumes[0].PersistentVolumeClaim.ClaimName)
	require.Len(t, pod.Spec.Containers, 1)
	assert.True(t, pod.Spec.Containers[0].VolumeMounts[0].ReadOnly)

	cond := h.condition(t)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "Starting", cond.Reason)
}

func TestProjectHolderIsInThePodCacheAndOutOfTheVolumeMapper(t *testing.T) {
	pod := buildHolder(projectHolderTarget(holderProject(nil), projectHolderNamespace))
	rest := PodCacheNamespaces()[cache.AllNamespaces]
	require.NotNil(t, rest.LabelSelector)
	assert.True(t, rest.LabelSelector.Matches(k8slabels.Set(pod.Labels)), "the controller's pod cache must see project holders")
	assert.Empty(t, volumeForHolder(context.Background(), pod))
	assert.Equal(t, []ctrl.Request{{NamespacedName: types.NamespacedName{Name: "demo"}}}, projectForHolder(context.Background(), pod))

	volumeHolder := buildVolumeHolder(sharedVolume())
	assert.Empty(t, projectForHolder(context.Background(), volumeHolder), "a Volume's holder belongs to its Volume")
}

func TestProjectHolderIsKeptForRetainedStorageWhenSharedStorageIsOff(t *testing.T) {
	for name, shared := range map[string]*kipperv1.ProjectSharedStorage{
		"disabled": {Enabled: false},
		"unset":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			h := newProjectHolderHarness(t, holderProject(shared), projectSharedClaim())
			h.reconcile(t, 2)
			_, ok := h.holder(t)
			assert.True(t, ok, "a retained claim stays in backups after shared storage is turned off")

			pod, _ := h.holder(t)
			require.NoError(t, h.client.Delete(context.Background(), pod))
			h.reconcile(t, 1)
			_, ok = h.holder(t)
			assert.True(t, ok, "a deleted holder of retained storage is replaced")
		})
	}
}

func TestProjectWithoutSharedClaimHasNoHolder(t *testing.T) {
	h := newProjectHolderHarness(t, holderProject(nil))
	h.reconcile(t, 2)
	_, ok := h.holder(t)
	assert.False(t, ok)
	assert.Nil(t, h.condition(t))
}

func TestProjectHolderSkipsAClaimItDidNotCreate(t *testing.T) {
	claim := projectSharedClaim()
	claim.Labels[labels.Project] = "other"
	h := newProjectHolderHarness(t, holderProject(&kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi"}), claim)
	h.reconcile(t, 2)
	_, ok := h.holder(t)
	assert.False(t, ok)
	cond := h.condition(t)
	require.NotNil(t, cond)
	assert.Equal(t, "ForeignClaim", cond.Reason)
	assert.Contains(t, cond.Message, "Environment prod:")
}

func TestProjectHolderReportsRunning(t *testing.T) {
	project := holderProject(&kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi"})
	running := buildHolder(projectHolderTarget(project, projectHolderNamespace))
	running.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: kipperv1.GroupVersion.String(), Kind: "Project", Name: "demo", UID: "project-uid", Controller: ptr.To(true),
	}}
	running.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	h := newProjectHolderHarness(t, project, projectSharedClaim(), running)
	h.reconcile(t, 2)

	cond := h.condition(t)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Running", cond.Reason)
}

func TestProjectAdoptsARestoredHolder(t *testing.T) {
	project := holderProject(&kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi"})
	restored := buildHolder(projectHolderTarget(project, projectHolderNamespace))
	restored.Labels["velero.io/restore-name"] = "restore-1"
	restored.Spec.InitContainers = []corev1.Container{{Name: "restore-wait", Image: "velero/velero-restore-helper"}}
	restored.Status.Phase = corev1.PodPending
	h := newProjectHolderHarness(t, project, projectSharedClaim(), restored)
	h.reconcile(t, 2)

	pod, ok := h.holder(t)
	require.True(t, ok)
	ref := metav1.GetControllerOf(pod)
	require.NotNil(t, ref, "a restored holder with the project's labels and claim is adopted")
	assert.Equal(t, "Project", ref.Kind)
	assert.Equal(t, "Restoring", h.condition(t).Reason)
}

func TestProjectHolderRejectionRaisesAnEvent(t *testing.T) {
	project := holderProject(&kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi"})
	foreign := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: projectHolderName, Namespace: projectHolderNamespace}}
	h := newProjectHolderHarness(t, project, projectSharedClaim(), foreign)
	h.reconcile(t, 2)

	cond := h.condition(t)
	require.NotNil(t, cond)
	assert.Equal(t, "NameTaken", cond.Reason)
	var events []string
	for len(h.recorder.Events) > 0 {
		events = append(events, <-h.recorder.Events)
	}
	require.Len(t, events, 1, "the event fires when the condition changes, not on every pass")
	assert.Contains(t, events[0], "BackupHolderNameTaken")
}

func ownedProjectHolder(project *kipperv1.Project) *corev1.Pod {
	pod := buildHolder(projectHolderTarget(project, projectHolderNamespace))
	pod.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: kipperv1.GroupVersion.String(), Kind: "Project", Name: "demo", UID: "project-uid", Controller: ptr.To(true),
	}}
	return pod
}

func TestProjectHolderLetsADeletedClaimGo(t *testing.T) {
	project := holderProject(nil)
	claim := projectSharedClaim()
	claim.Finalizers = []string{"kubernetes.io/pvc-protection"}
	claim.DeletionTimestamp = &metav1.Time{Time: time.Now()}
	h := newProjectHolderHarness(t, project, claim, ownedProjectHolder(project))

	h.reconcile(t, 1)
	_, ok := h.holder(t)
	assert.False(t, ok, "the holder's mount would keep pvc-protection from ever releasing the claim")
	h.reconcile(t, 1)
	_, ok = h.holder(t)
	assert.False(t, ok, "no new holder for a claim on its way out")
	assert.Nil(t, h.condition(t))
}

func TestProjectHolderGoesWithItsClaim(t *testing.T) {
	project := holderProject(nil)
	h := newProjectHolderHarness(t, project, ownedProjectHolder(project))
	h.reconcile(t, 1)
	_, ok := h.holder(t)
	assert.False(t, ok, "a holder whose claim is gone holds nothing")
}

func TestProjectWithSharedStorageOffIgnoresATenantsOwnClaim(t *testing.T) {
	claim := projectSharedClaim()
	claim.Labels = map[string]string{"app": "mine"}
	h := newProjectHolderHarness(t, holderProject(nil), claim)
	h.reconcile(t, 2)
	_, ok := h.holder(t)
	assert.False(t, ok)
	assert.Nil(t, h.condition(t), "a project without shared storage has no shared storage to warn about")
	assert.Empty(t, h.recorder.Events)
}

func TestProjectHolderKeepsARestoreWarningUntilTheNewHolderRuns(t *testing.T) {
	project := holderProject(&kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi"})
	project.Status.Conditions = []metav1.Condition{{
		Type: conditionBackupHolderReady, Status: metav1.ConditionFalse, Reason: "RestoreStuck",
		Message: "Environment prod: the holder's restore did not finish", LastTransitionTime: metav1.Now(),
	}}
	h := newProjectHolderHarness(t, project, projectSharedClaim())
	h.reconcile(t, 1)

	_, ok := h.holder(t)
	require.True(t, ok, "the replacement holder is created")
	cond := h.condition(t)
	require.NotNil(t, cond)
	assert.Equal(t, "RestoreStuck", cond.Reason, "the warning outlives the holder that showed it")
	assert.Equal(t, "Environment prod: the holder's restore did not finish", cond.Message)
}

func TestASharedStorageClaimMapsToItsProject(t *testing.T) {
	claim := projectSharedClaim()
	assert.Equal(t, []ctrl.Request{{NamespacedName: types.NamespacedName{Name: "demo"}}}, projectForSharedClaim(context.Background(), claim))

	other := projectSharedClaim()
	other.Name = "data-db-0"
	assert.Empty(t, projectForSharedClaim(context.Background(), other))
	unlabelled := projectSharedClaim()
	unlabelled.Labels = nil
	assert.Empty(t, projectForSharedClaim(context.Background(), unlabelled))
}
