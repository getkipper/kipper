package controllers

import (
	"context"
	"errors"
	"strings"
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
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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

// projectUploadsClaim models the claim the project controller creates, which
// carries the project's labels and no owner.
func projectUploadsClaim() *corev1.PersistentVolumeClaim {
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
			h := newProjectHolderHarness(t, holderProject(shared), projectUploadsClaim())
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
	claim := projectUploadsClaim()
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
	h := newProjectHolderHarness(t, project, projectUploadsClaim(), running)
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
	h := newProjectHolderHarness(t, project, projectUploadsClaim(), restored)
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
	h := newProjectHolderHarness(t, project, projectUploadsClaim(), foreign)
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
	claim := projectUploadsClaim()
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
	claim := projectUploadsClaim()
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
	h := newProjectHolderHarness(t, project, projectUploadsClaim())
	h.reconcile(t, 1)

	_, ok := h.holder(t)
	require.True(t, ok, "the replacement holder is created")
	cond := h.condition(t)
	require.NotNil(t, cond)
	assert.Equal(t, "RestoreStuck", cond.Reason, "the warning outlives the holder that showed it")
	assert.Equal(t, "Environment prod: the holder's restore did not finish", cond.Message)
}

// Saving the warning before deleting its holder preserves the evidence if
// the final project status write fails after reconciling all environments.
func TestProjectHolderRestoreWarningIsSavedBeforeThePodGoes(t *testing.T) {
	project := holderProject(&kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi"})
	stuck := ownedProjectHolder(project)
	stuck.UID = "carrier"
	stuck.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-holderRestoreDeadline - time.Minute))
	stuck.Spec.InitContainers = []corev1.Container{{Name: veleroRestoreContainer}}
	stuck.Status.Phase = corev1.PodPending
	failed := false
	scheme := testScheme()
	c := projectFakeBuilder().WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: stampUID,
			SubResourceUpdate: func(ctx context.Context, c crclient.Client, sub string, obj crclient.Object, opts ...crclient.SubResourceUpdateOption) error {
				p, ok := obj.(*kipperv1.Project)
				if cond := apimeta.FindStatusCondition(p.Status.Conditions, conditionBackupHolderReady); ok && !failed && cond != nil && cond.Reason == "RestoreStuck" {
					failed = true
					return apierrors.NewConflict(schema.GroupResource{Resource: "projects"}, obj.GetName(), errors.New("stale"))
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).
		WithObjects(project, nodeWithIP("worker-1", "ExternalIP", "203.0.113.9"), projectUploadsClaim(), stuck).
		WithStatusSubresource(&kipperv1.Project{}).Build()
	h := &projectHolderHarness{client: c, reconciler: &ProjectReconciler{Client: c, Scheme: scheme, APIReader: c, Recorder: record.NewFakeRecorder(20)}}

	_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
	require.Error(t, err)
	kept, ok := h.holder(t)
	require.True(t, ok, "the pod stays while its warning is unsaved")
	assert.Equal(t, types.UID("carrier"), kept.UID)

	_, err = h.reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: "demo"}})
	require.NoError(t, err, "the pass that saves the warning also writes its own status")
	h.reconcile(t, 1)
	if p, ok := h.holder(t); ok {
		assert.NotEqual(t, types.UID("carrier"), p.UID, "the stuck holder is replaced")
	}
	cond := h.condition(t)
	require.NotNil(t, cond)
	assert.Equal(t, "RestoreStuck", cond.Reason)
}

// A restore warning from one environment is what the project reports, even
// while another environment's holder is still starting, and it stays until the
// warned environment's holder runs again.
func TestProjectHolderReportsARestoreWarningOverAnotherEnvironment(t *testing.T) {
	project := holderProject(&kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi"})
	project.Spec.Environments = []kipperv1.ProjectEnvironment{{Name: "prod"}, {Name: "staging"}}
	staging := projectUploadsClaim()
	staging.Namespace = "demo-staging"
	stuck := buildHolder(projectHolderTarget(project, "demo-staging"))
	stuck.OwnerReferences = ownedProjectHolder(project).OwnerReferences
	stuck.UID = "stuck"
	stuck.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-holderRestoreDeadline - time.Minute))
	stuck.Spec.InitContainers = []corev1.Container{{Name: veleroRestoreContainer}}
	stuck.Status.Phase = corev1.PodPending
	h := newProjectHolderHarness(t, project, projectUploadsClaim(), staging, stuck)

	h.reconcile(t, 3)
	cond := h.condition(t)
	require.NotNil(t, cond)
	assert.Equal(t, "RestoreStuck", cond.Reason, "prod's starting holder does not hide staging's warning")
	assert.Contains(t, cond.Message, "Environment staging:")
	var warnings int
	for len(h.recorder.Events) > 0 {
		if strings.Contains(<-h.recorder.Events, "BackupHolderRestoreStuck") {
			warnings++
		}
	}
	assert.Equal(t, 1, warnings, "the warning is announced once")

	var replacement corev1.Pod
	require.NoError(t, h.client.Get(context.Background(), types.NamespacedName{Namespace: "demo-staging", Name: projectHolderName}, &replacement))
	replacement.Status.Phase = corev1.PodRunning
	replacement.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, h.client.Status().Update(context.Background(), &replacement))
	h.reconcile(t, 1)
	assert.NotEqual(t, "RestoreStuck", h.condition(t).Reason, "the warning goes once staging's holder runs")
}

// A recorded restore warning is kept while its environment's replacement
// starts or warns again, without a second event, and gives way once that
// environment reports anything else, such as a new restore.
func TestARecordedRestoreWarningHoldsOnlyWhileItsEnvironmentStarts(t *testing.T) {
	stuck := metav1.Condition{Status: metav1.ConditionFalse, Reason: "RestoreStuck", Message: "the restore did not finish"}
	env := func(name, reason string) envHolder {
		return envHolder{env: name, cond: metav1.Condition{Status: metav1.ConditionFalse, Reason: reason, Message: reason}}
	}
	recordedProject := func() *kipperv1.Project {
		p := holderProject(nil)
		p.Status.Conditions = []metav1.Condition{{
			Type: conditionBackupHolderReady, Status: metav1.ConditionFalse, Reason: "RestoreStuck",
			Message: "Environment staging: the restore did not finish", LastTransitionTime: metav1.Now(),
		}}
		return p
	}
	recorder := record.NewFakeRecorder(10)
	r := &ProjectReconciler{Recorder: recorder}

	p := recordedProject()
	r.setProjectHolderCondition(p, []envHolder{{env: "prod", cond: stuck}, {env: "staging", cond: stuck}})
	cond := apimeta.FindStatusCondition(p.Status.Conditions, conditionBackupHolderReady)
	assert.Equal(t, "Environment staging: the restore did not finish", cond.Message, "the recorded warning stays")
	assert.Empty(t, recorder.Events, "a warning already announced is not announced again")

	p = recordedProject()
	r.setProjectHolderCondition(p, []envHolder{env("prod", "Starting"), env("staging", "Starting")})
	assert.Equal(t, "RestoreStuck", apimeta.FindStatusCondition(p.Status.Conditions, conditionBackupHolderReady).Reason)

	p = recordedProject()
	r.setProjectHolderCondition(p, []envHolder{env("prod", "Starting"), env("staging", "Restoring")})
	assert.NotEqual(t, "RestoreStuck", apimeta.FindStatusCondition(p.Status.Conditions, conditionBackupHolderReady).Reason,
		"a new restore replaces the warning, so a second failure is announced again")
}

func TestASharedStorageClaimMapsToItsProject(t *testing.T) {
	claim := projectUploadsClaim()
	assert.Equal(t, []ctrl.Request{{NamespacedName: types.NamespacedName{Name: "demo"}}}, projectForSharedClaim(context.Background(), claim))

	other := projectUploadsClaim()
	other.Name = "data-db-0"
	assert.Empty(t, projectForSharedClaim(context.Background(), other))
	unlabelled := projectUploadsClaim()
	unlabelled.Labels = nil
	assert.Empty(t, projectForSharedClaim(context.Background(), unlabelled))
}

// A copy change needs the volume detached, so the project's holder stops while
// the shared claim carries a copy-change stop, as a Volume's holder does. A
// holder still restoring the files is left to finish.
func TestProjectHolderStopsForACopyChange(t *testing.T) {
	project := holderProject(&kipperv1.ProjectSharedStorage{Enabled: true, Size: "2Gi"})
	stopped := func() *corev1.PersistentVolumeClaim {
		pvc := projectUploadsClaim()
		pvc.Annotations = map[string]string{platform.CopyChangeStopAnnotation: `{"at":"2026-10-24T11:59:00Z"}`}
		return pvc
	}
	running := ownedProjectHolder(project)
	running.Status.Phase = corev1.PodRunning

	for name, objs := range map[string][]crclient.Object{
		"running holder": {stopped(), running},
		"no holder":      {stopped()},
	} {
		t.Run(name, func(t *testing.T) {
			h := newProjectHolderHarness(t, project, objs...)
			holder, _, err := h.reconciler.reconcileProjectHolder(context.Background(), project, "prod", projectHolderNamespace, true)
			require.NoError(t, err)

			_, ok := h.holder(t)
			assert.False(t, ok)
			require.NotNil(t, holder)
			assert.Equal(t, holderStoppedForCopyChange, holder.cond.Reason)
		})
	}

	restoring := ownedProjectHolder(project)
	restoring.Spec.InitContainers = []corev1.Container{{Name: veleroRestoreContainer}}
	h := newProjectHolderHarness(t, project, stopped(), restoring)
	_, _, err := h.reconciler.reconcileProjectHolder(context.Background(), project, "prod", projectHolderNamespace, true)
	require.NoError(t, err)
	_, ok := h.holder(t)
	assert.True(t, ok, "a restore is never interrupted for a copy change")
}

// The project's holder belongs to Kipper, so a reduction of the shared storage
// records its stop instead of waiting for the holder as if it were an app.
func TestASharedStorageReductionDoesNotWaitForTheProjectHolder(t *testing.T) {
	c, r := retainedSharedWorld(t, map[string]interface{}{"r-s": "RW", "r-w": "RW"})
	ctx := context.Background()
	var project kipperv1.Project
	require.NoError(t, c.Get(ctx, crclient.ObjectKey{Name: "demo"}, &project))
	project.Spec.SharedStorage.Enabled = true
	require.NoError(t, c.Update(ctx, &project))
	require.NoError(t, c.Create(ctx, longhornReplica("r-w", "pvc-1", "worker-1", healthyTime, "")))
	require.NoError(t, c.Create(ctx, lhAttachmentObj("")))
	setLonghornField(t, c, "Volume", "pvc-1", int64(2), "spec", "numberOfReplicas")
	holder := buildHolder(projectHolderTarget(&project, "demo-prod"))
	holder.Spec.NodeName = "worker-1"
	holder.Status.Phase = corev1.PodRunning
	require.NoError(t, c.Create(ctx, holder))

	got := reconcileDemo(t, c, r)

	cond := apimeta.FindStatusCondition(got.Status.Conditions, kipperv1.ConditionStorageCopies)
	require.NotNil(t, cond)
	assert.NotContains(t, cond.Message, inUseMarker)
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, crclient.ObjectKey{Namespace: "demo-prod", Name: "shared-uploads"}, &pvc))
	assert.Contains(t, pvc.Annotations, platform.CopyChangeStopAnnotation)
}
