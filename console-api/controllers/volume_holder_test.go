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
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

const holderTestNamespace = "project-test"

type holderHarness struct {
	client     crclient.Client
	reconciler *VolumeReconciler
	recorder   *record.FakeRecorder
}

func sharedVolume() *kipperv1.Volume {
	return &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "uploads", Namespace: holderTestNamespace, UID: "vol-uid",
			Finalizers: []string{volumeFinalizer},
		},
		Spec: kipperv1.VolumeSpec{Size: "5Gi"},
	}
}

func newHolderHarness(t *testing.T, funcs interceptor.Funcs, objs ...crclient.Object) *holderHarness {
	t.Helper()
	scheme := testScheme()
	c := crfake.NewClientBuilder().WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&kipperv1.Volume{}).
		WithInterceptorFuncs(funcs).
		Build()
	recorder := record.NewFakeRecorder(20)
	return &holderHarness{
		client:     c,
		reconciler: &VolumeReconciler{Client: c, Scheme: scheme, Recorder: recorder},
		recorder:   recorder,
	}
}

func (h *holderHarness) reconcile(t *testing.T) ctrl.Result {
	t.Helper()
	res, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "uploads", Namespace: holderTestNamespace},
	})
	require.NoError(t, err)
	return res
}

func (h *holderHarness) holder(t *testing.T) (*corev1.Pod, bool) {
	t.Helper()
	var pod corev1.Pod
	err := h.client.Get(context.Background(), types.NamespacedName{Name: "volume-holder-uploads", Namespace: holderTestNamespace}, &pod)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(t, err)
	return &pod, true
}

func (h *holderHarness) claim(t *testing.T) *corev1.PersistentVolumeClaim {
	t.Helper()
	var pvc corev1.PersistentVolumeClaim
	require.NoError(t, h.client.Get(context.Background(), types.NamespacedName{Name: "shared-uploads", Namespace: holderTestNamespace}, &pvc))
	return &pvc
}

func (h *holderHarness) holderCondition(t *testing.T) *metav1.Condition {
	t.Helper()
	var vol kipperv1.Volume
	require.NoError(t, h.client.Get(context.Background(), types.NamespacedName{Name: "uploads", Namespace: holderTestNamespace}, &vol))
	return apimeta.FindStatusCondition(vol.Status.Conditions, conditionBackupHolderReady)
}

func (h *holderHarness) events() []string {
	var out []string
	for {
		select {
		case e := <-h.recorder.Events:
			out = append(out, e)
		default:
			return out
		}
	}
}

// sharedClaim models a controller-created claim, or a restored, ownerless claim
// when owner is nil.
func sharedClaim(owner *kipperv1.Volume) *corev1.PersistentVolumeClaim {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name: "shared-uploads", Namespace: holderTestNamespace,
			Labels: map[string]string{
				kipperLabel:         kipperValue,
				labels.ResourceType: labels.ResourceTypeSharedVolume,
				labels.VolumeName:   "uploads",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: ptr.To(platform.LonghornSharedClass),
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")},
			},
		},
	}
	if owner != nil {
		pvc.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(owner)}
	}
	return pvc
}

func volumeControllerRef(vol *kipperv1.Volume) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: kipperv1.GroupVersion.String(), Kind: "Volume",
		Name: vol.Name, UID: vol.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}
}

// restoredHolder models a pending restored holder with its original labels,
// no owner reference and Velero's restore init container before the sleeper.
func restoredHolder() *corev1.Pod {
	pod := buildVolumeHolder(sharedVolume())
	pod.Labels["velero.io/restore-name"] = "restore-1"
	pod.Spec.InitContainers = []corev1.Container{{Name: "restore-wait", Image: "velero/velero-restore-helper"}}
	pod.Status.Phase = corev1.PodPending
	return pod
}

func TestVolumeHolderIsCreatedForALonghornSharedVolume(t *testing.T) {
	h := newHolderHarness(t, interceptor.Funcs{}, sharedVolume())
	h.reconcile(t)

	pod, ok := h.holder(t)
	require.True(t, ok, "a supported shared volume gets a holder pod")

	ref := metav1.GetControllerOf(pod)
	require.NotNil(t, ref, "the holder is owned by its Volume")
	assert.Equal(t, "Volume", ref.Kind)
	assert.Equal(t, types.UID("vol-uid"), ref.UID)

	assert.Equal(t, kipperValue, pod.Labels[kipperLabel])
	assert.Equal(t, labels.ResourceTypeVolumeHolder, pod.Labels[labels.ResourceType])
	assert.Equal(t, "uploads", pod.Labels[labels.VolumeName])
	assert.NotContains(t, pod.Labels, "app", "a holder must not look like an app's pod")
	assert.NotContains(t, pod.Labels, "kipper.run/service-type", "a holder must not look like a service's pod")

	spec := pod.Spec
	assert.Equal(t, ptr.To(false), spec.AutomountServiceAccountToken)
	require.NotNil(t, spec.SecurityContext)
	assert.Equal(t, ptr.To(true), spec.SecurityContext.RunAsNonRoot)
	require.NotNil(t, spec.SecurityContext.RunAsUser)
	require.NotNil(t, spec.SecurityContext.RunAsGroup)
	assert.NotZero(t, *spec.SecurityContext.RunAsUser)
	assert.NotZero(t, *spec.SecurityContext.RunAsGroup)
	assert.Nil(t, spec.SecurityContext.FSGroup, "an fsGroup would rewrite ownership of the volume's files")
	require.NotNil(t, spec.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, spec.SecurityContext.SeccompProfile.Type)
	assert.False(t, spec.HostNetwork || spec.HostPID || spec.HostIPC)

	require.Len(t, spec.Containers, 1)
	c := spec.Containers[0]
	assert.Contains(t, c.Image, "@sha256:", "the holder image is pinned by digest")
	assert.Empty(t, c.Ports)
	require.NotNil(t, c.SecurityContext)
	assert.Equal(t, ptr.To(false), c.SecurityContext.AllowPrivilegeEscalation)
	assert.Equal(t, ptr.To(true), c.SecurityContext.ReadOnlyRootFilesystem)
	require.NotNil(t, c.SecurityContext.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, c.SecurityContext.Capabilities.Drop)
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		assert.Contains(t, c.Resources.Requests, name)
		assert.Contains(t, c.Resources.Limits, name)
	}

	require.Len(t, c.VolumeMounts, 1)
	assert.True(t, c.VolumeMounts[0].ReadOnly, "the sleeper mounts the claim read-only")
	require.Len(t, spec.Volumes, 1)
	src := spec.Volumes[0].PersistentVolumeClaim
	require.NotNil(t, src)
	assert.Equal(t, "shared-uploads", src.ClaimName)
	assert.False(t, src.ReadOnly, "the claim itself stays writable so a restore can write through this pod")
	assert.Equal(t, spec.Volumes[0].Name, c.VolumeMounts[0].Name)

	cond := h.holderCondition(t)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "Starting", cond.Reason)
}

func TestVolumeHolderIsNotCreatedOnLocalStorage(t *testing.T) {
	h := newHolderHarness(t, interceptor.Funcs{}, sharedVolume(), localPlatformConfig())
	h.reconcile(t)

	_, ok := h.holder(t)
	assert.False(t, ok, "local storage has no shared volumes, so it has no holders")
}

func TestVolumeHolderReportsRunning(t *testing.T) {
	vol := sharedVolume()
	running := buildVolumeHolder(vol)
	running.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(vol)}
	running.Status.Phase = corev1.PodRunning
	running.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), running)
	h.reconcile(t)

	cond := h.holderCondition(t)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
	assert.Equal(t, "Running", cond.Reason)
}

func TestVolumeHolderRefusesAnUnexpectedClaim(t *testing.T) {
	cases := map[string]func(*corev1.PersistentVolumeClaim){
		"single writer": func(p *corev1.PersistentVolumeClaim) {
			p.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		},
		"block mode": func(p *corev1.PersistentVolumeClaim) {
			p.Spec.VolumeMode = ptr.To(corev1.PersistentVolumeBlock)
		},
		"other class": func(p *corev1.PersistentVolumeClaim) {
			p.Spec.StorageClassName = ptr.To("longhorn-single")
		},
		"no class": func(p *corev1.PersistentVolumeClaim) {
			p.Spec.StorageClassName = nil
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			vol := sharedVolume()
			pvc := sharedClaim(vol)
			mutate(pvc)
			h := newHolderHarness(t, interceptor.Funcs{}, vol, pvc)
			h.reconcile(t)

			_, ok := h.holder(t)
			assert.False(t, ok)
			cond := h.holderCondition(t)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, "UnsupportedClaim", cond.Reason)
			assert.NotEmpty(t, h.events())
		})
	}
}

func TestVolumeHolderRefusesAClaimSomethingElseControls(t *testing.T) {
	vol := sharedVolume()
	pvc := sharedClaim(nil)
	pvc.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "apps/v1", Kind: "StatefulSet", Name: "db", UID: "other", Controller: ptr.To(true),
	}}
	h := newHolderHarness(t, interceptor.Funcs{}, vol, pvc)
	h.reconcile(t)

	_, ok := h.holder(t)
	assert.False(t, ok)
	assert.Equal(t, "StatefulSet", metav1.GetControllerOf(h.claim(t)).Kind, "a foreign claim is left alone")
	cond := h.holderCondition(t)
	require.NotNil(t, cond)
	assert.Equal(t, "ForeignClaim", cond.Reason)
}

// A restore brings the claim back without its owner reference. Kipper's own
// labels for this volume prove where it came from.
func TestVolumeAdoptsARestoredClaim(t *testing.T) {
	vol := sharedVolume()
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(nil))
	h.reconcile(t)

	ref := metav1.GetControllerOf(h.claim(t))
	require.NotNil(t, ref, "the restored claim is adopted")
	assert.Equal(t, types.UID("vol-uid"), ref.UID)
	_, ok := h.holder(t)
	assert.True(t, ok)
}

func TestVolumeDoesNotAdoptAnUnlabelledClaim(t *testing.T) {
	vol := sharedVolume()
	pvc := sharedClaim(nil)
	delete(pvc.Labels, labels.VolumeName)
	h := newHolderHarness(t, interceptor.Funcs{}, vol, pvc)
	h.reconcile(t)

	assert.Nil(t, metav1.GetControllerOf(h.claim(t)), "a claim matched by name alone is not adopted")
	_, ok := h.holder(t)
	assert.False(t, ok)
	cond := h.holderCondition(t)
	require.NotNil(t, cond)
	assert.Equal(t, "ForeignClaim", cond.Reason)
}

// Velero restores pods before custom resources, so a holder may still be
// restoring when its Volume returns. Adoption preserves that restore.
func TestVolumeAdoptsARestoredHolderAndLeavesItRunning(t *testing.T) {
	vol := sharedVolume()
	restored := restoredHolder()
	restored.UID = "restored-pod"
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(nil), restored)
	h.reconcile(t)

	pod, ok := h.holder(t)
	require.True(t, ok)
	assert.Equal(t, types.UID("restored-pod"), pod.UID, "the restored holder is kept")
	assert.Len(t, pod.Spec.InitContainers, 1, "Velero's restore container is left in place")
	ref := metav1.GetControllerOf(pod)
	require.NotNil(t, ref, "the restored holder is adopted")
	assert.Equal(t, types.UID("vol-uid"), ref.UID)

	cond := h.holderCondition(t)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "Restoring", cond.Reason)
}

func TestVolumeHolderLeavesAPodItCannotProveIsItsOwn(t *testing.T) {
	cases := map[string]func(*corev1.Pod){
		"no labels": func(p *corev1.Pod) { p.Labels = nil },
		"another volume": func(p *corev1.Pod) {
			p.Labels[labels.VolumeName] = "other"
		},
		"another resource type": func(p *corev1.Pod) {
			p.Labels[labels.ResourceType] = "app"
		},
		"not managed by kipper": func(p *corev1.Pod) {
			delete(p.Labels, kipperLabel)
		},
		"another claim": func(p *corev1.Pod) {
			p.Spec.Volumes[0].PersistentVolumeClaim.ClaimName = "shared-other"
		},
		"another controller": func(p *corev1.Pod) {
			p.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "web", UID: "rs", Controller: ptr.To(true),
			}}
		},
		"a replaced volume": func(p *corev1.Pod) {
			stale := volumeControllerRef(sharedVolume())
			stale.UID = "old-vol-uid"
			p.OwnerReferences = []metav1.OwnerReference{stale}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			vol := sharedVolume()
			squatter := restoredHolder()
			squatter.UID = "squatter"
			mutate(squatter)
			h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), squatter)
			res := h.reconcile(t)

			pod, ok := h.holder(t)
			require.True(t, ok, "the pod is never deleted")
			assert.Equal(t, types.UID("squatter"), pod.UID)
			assert.Equal(t, squatter.OwnerReferences, pod.OwnerReferences, "the pod is never adopted")
			cond := h.holderCondition(t)
			require.NotNil(t, cond)
			assert.Equal(t, "NameTaken", cond.Reason)
			assert.Positive(t, res.RequeueAfter, "the name is checked again later")
		})
	}
}

// Outside monitoring, the pod cache holds only labelled holders. A foreign pod
// with the holder's name causes a create conflict, requiring an API read before
// the same ownership check can run.
func TestVolumeHolderJudgesAPodOutsideTheCache(t *testing.T) {
	vol := sharedVolume()
	squatter := restoredHolder()
	squatter.UID = "squatter"
	squatter.Labels = map[string]string{"app": "web"}
	scheme := testScheme()
	direct := crfake.NewClientBuilder().WithScheme(scheme).WithObjects(squatter).Build()
	h := newHolderHarness(t, interceptor.Funcs{
		Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod {
				return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, key.Name)
			}
			return c.Get(ctx, key, obj, opts...)
		},
	}, vol, sharedClaim(vol), squatter)
	h.reconciler.APIReader = direct
	res := h.reconcile(t)

	var pod corev1.Pod
	require.NoError(t, direct.Get(context.Background(), types.NamespacedName{Name: "volume-holder-uploads", Namespace: holderTestNamespace}, &pod))
	assert.Equal(t, types.UID("squatter"), pod.UID)
	cond := h.holderCondition(t)
	require.NotNil(t, cond)
	assert.Equal(t, "NameTaken", cond.Reason)
	assert.Positive(t, res.RequeueAfter)
}

// A terminal holder, such as an evicted pod, cannot support backups or restores
// and needs replacement.
func TestVolumeHolderReplacesAnEndedHolder(t *testing.T) {
	vol := sharedVolume()
	ended := buildVolumeHolder(vol)
	ended.UID = "ended"
	ended.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(vol)}
	ended.Status.Phase = corev1.PodFailed
	ended.Status.Reason = "Evicted"
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), ended)
	h.reconcile(t)

	if pod, ok := h.holder(t); ok {
		assert.NotEqual(t, types.UID("ended"), pod.UID, "the ended holder is gone")
	}
}

// Quota or admission rejection prevents backups when no app mounts the volume.
// Reporting the rejection and retrying lets the operator resolve it.
func TestVolumeHolderRejectionIsReportedAndRetried(t *testing.T) {
	vol := sharedVolume()
	h := newHolderHarness(t, interceptor.Funcs{
		Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
			if _, isPod := obj.(*corev1.Pod); isPod {
				return apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, obj.GetName(),
					errors.New("exceeded quota: compute, requested: limits.memory=16Mi"))
			}
			return c.Create(ctx, obj, opts...)
		},
	}, vol, sharedClaim(vol))
	res := h.reconcile(t)

	_, ok := h.holder(t)
	assert.False(t, ok)
	assert.Positive(t, res.RequeueAfter, "a rejected holder is tried again")
	cond := h.holderCondition(t)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "CreateRejected", cond.Reason)
	assert.Contains(t, cond.Message, "exceeded quota")
	events := h.events()
	require.NotEmpty(t, events)
	assert.True(t, strings.HasPrefix(events[0], corev1.EventTypeWarning), events[0])

	// The same rejection on the retry is not announced twice.
	h.reconcile(t)
	assert.Empty(t, h.events())
}

func TestVolumeForHolderMapsOnlyHolderPods(t *testing.T) {
	holder := buildVolumeHolder(sharedVolume())
	assert.Equal(t, []ctrl.Request{{NamespacedName: types.NamespacedName{Namespace: holderTestNamespace, Name: "uploads"}}},
		volumeForHolder(context.Background(), holder))

	app := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "web-1", Namespace: holderTestNamespace,
		Labels: map[string]string{kipperLabel: kipperValue, "app": "web"}}}
	assert.Empty(t, volumeForHolder(context.Background(), app))
}

// The OOM watcher needs every monitoring pod cached. Elsewhere, only holders
// need to be cached.
func TestPodCacheNamespacesKeepMonitoringAndAddHolders(t *testing.T) {
	ns := PodCacheNamespaces()

	mon, ok := ns[monitoringNamespace]
	require.True(t, ok)
	assert.Nil(t, mon.LabelSelector, "the OOM watcher still sees every monitoring pod")

	rest, ok := ns[cache.AllNamespaces]
	require.True(t, ok)
	require.NotNil(t, rest.LabelSelector)
	assert.True(t, rest.LabelSelector.Matches(k8slabels.Set(buildVolumeHolder(sharedVolume()).Labels)))
	assert.False(t, rest.LabelSelector.Matches(k8slabels.Set{kipperLabel: kipperValue, "app": "web"}))
}

// A holder that cannot start puts backups at risk when no app mounts the volume,
// so the Volume reports the cause.
func TestVolumeHolderNamesWhyItCannotStart(t *testing.T) {
	cases := map[string]struct {
		mutate func(*corev1.Pod)
		reason string
	}{
		"unschedulable": {func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.Conditions = []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable,
				Message: "0/1 nodes are available: insufficient memory",
			}}
		}, "Unschedulable"},
		"image pull": {func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodPending
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "holder",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "pull refused"}}}}
		}, "ContainerFailing"},
		"crash loop after a restore": {func(p *corev1.Pod) {
			p.Spec.InitContainers = []corev1.Container{{Name: "restore-wait"}}
			p.Status.Phase = corev1.PodRunning
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "restore-wait",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
			p.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "holder",
				State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}}}
		}, "ContainerFailing"},
		"not started after the grace": {func(p *corev1.Pod) {
			p.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-2 * holderStartGrace))
			p.Status.Phase = corev1.PodPending
		}, "NotStarting"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			vol := sharedVolume()
			pod := buildVolumeHolder(vol)
			pod.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(vol)}
			tc.mutate(pod)
			h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), pod)
			h.reconcile(t)

			cond := h.holderCondition(t)
			require.NotNil(t, cond)
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, tc.reason, cond.Reason)
			assert.NotEmpty(t, h.events(), "a holder that cannot start is announced")
		})
	}
}

// Rechecking after the start grace catches stalled holders even without a pod event.
func TestVolumeHolderStartingIsRecheckedAfterTheGrace(t *testing.T) {
	vol := sharedVolume()
	pod := buildVolumeHolder(vol)
	pod.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(vol)}
	pod.CreationTimestamp = metav1.Now()
	pod.Status.Phase = corev1.PodPending
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), pod)
	res := h.reconcile(t)

	assert.Equal(t, "Starting", h.holderCondition(t).Reason)
	assert.Positive(t, res.RequeueAfter)
	assert.LessOrEqual(t, res.RequeueAfter, holderStartGrace)
}

// A restore can take hours, so restoring holders have a longer deadline.
func TestVolumeHolderRestoringOutlastsTheStartGrace(t *testing.T) {
	vol := sharedVolume()
	pod := restoredHolder()
	pod.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(vol)}
	pod.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-10 * holderStartGrace))
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "restore-wait",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), pod)
	res := h.reconcile(t)

	assert.Equal(t, "Restoring", h.holderCondition(t).Reason)
	assert.Positive(t, res.RequeueAfter, "the restore deadline is checked without a pod event")
	assert.LessOrEqual(t, res.RequeueAfter, holderRestoreDeadline-10*holderStartGrace+time.Minute)
}

// A failed volume restore leaves Velero's restore container waiting indefinitely
// for its completion file. Replacing it after the deadline lets backups resume
// while the Volume retains the restore warning.
func TestVolumeHolderStuckInARestoreIsReplaced(t *testing.T) {
	vol := sharedVolume()
	pod := restoredHolder()
	pod.UID = "stuck"
	pod.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(vol)}
	pod.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-holderRestoreDeadline - time.Minute))
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "restore-wait",
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), pod)
	h.reconcile(t)

	if p, ok := h.holder(t); ok {
		assert.NotEqual(t, types.UID("stuck"), p.UID, "the stuck holder is replaced")
	}
	cond := h.holderCondition(t)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "RestoreStuck", cond.Reason)
	events := h.events()
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "BackupHolderRestoreStuck")

	h.reconcile(t)
	assert.Equal(t, "RestoreStuck", h.holderCondition(t).Reason)
	fresh, ok := h.holder(t)
	require.True(t, ok)
	fresh.Status.Phase = corev1.PodRunning
	fresh.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, h.client.Status().Update(context.Background(), fresh))
	h.reconcile(t)
	assert.Equal(t, "Running", h.holderCondition(t).Reason)
}

// Replacing a terminal holder lets backups resume. A warning event on the
// Volume preserves evidence of an interrupted restore.
func TestVolumeHolderAnnouncesAnInterruptedRestore(t *testing.T) {
	vol := sharedVolume()
	pod := restoredHolder()
	pod.UID = "interrupted"
	pod.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(vol)}
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Reason = "Evicted"
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), pod)
	h.reconcile(t)

	if p, ok := h.holder(t); ok {
		assert.NotEqual(t, types.UID("interrupted"), p.UID)
	}
	events := h.events()
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "BackupHolderRestoreInterrupted")
}

// A successfully completed restore init container remains in the spec, so
// its status determines whether the holder is still restoring.
func TestVolumeHolderAfterAFinishedRestoreIsStarting(t *testing.T) {
	vol := sharedVolume()
	pod := restoredHolder()
	pod.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(vol)}
	pod.CreationTimestamp = metav1.Now()
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "restore-wait",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), pod)
	h.reconcile(t)

	assert.Equal(t, "Starting", h.holderCondition(t).Reason)
}

func terminatingHolder(vol *kipperv1.Volume, since time.Duration) *corev1.Pod {
	pod := buildVolumeHolder(vol)
	pod.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(vol)}
	pod.Finalizers = []string{"test.example.com/hold"}
	at := metav1.NewTime(time.Now().Add(-since))
	pod.DeletionTimestamp = &at
	pod.Status.Phase = corev1.PodRunning
	return pod
}

// A terminating holder reserves its name while its node is down. The Volume
// reports the delay after the grace period so the operator can resolve it.
func TestVolumeHolderStuckTerminatingIsReported(t *testing.T) {
	vol := sharedVolume()
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), terminatingHolder(vol, 2*holderStartGrace))
	h.reconcile(t)

	cond := h.holderCondition(t)
	require.NotNil(t, cond)
	assert.Equal(t, "StuckTerminating", cond.Reason)
	assert.Contains(t, cond.Message, "kip node remove")
	assert.NotEmpty(t, h.events())
	_, ok := h.holder(t)
	assert.True(t, ok, "the terminating pod is left to the kubelet")
}

func TestVolumeHolderRecentlyTerminatingIsRecheckedAfterTheGrace(t *testing.T) {
	vol := sharedVolume()
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), terminatingHolder(vol, time.Minute))
	res := h.reconcile(t)

	assert.Equal(t, "Starting", h.holderCondition(t).Reason)
	assert.Positive(t, res.RequeueAfter)
	assert.LessOrEqual(t, res.RequeueAfter, holderStartGrace)
	assert.Empty(t, h.events())
}

// Deleting a holder during a restore interrupts it. While deletion is pending
// within the grace period, the Volume retains the interruption warning.
func TestVolumeHolderRemovedDuringARestoreIsReported(t *testing.T) {
	vol := sharedVolume()
	pod := terminatingHolder(vol, time.Minute)
	pod.Spec.InitContainers = []corev1.Container{{Name: "restore-wait"}}
	pod.Status.Phase = corev1.PodFailed
	h := newHolderHarness(t, interceptor.Funcs{}, vol, sharedClaim(vol), pod)
	h.reconcile(t)

	assert.Equal(t, "RestoreInterrupted", h.holderCondition(t).Reason)
	events := h.events()
	require.Len(t, events, 1)
	assert.Contains(t, events[0], "BackupHolderRestoreInterrupted")
}

// Saving the restore warning before deleting the pod preserves the evidence
// needed to retry if the status update fails.
func TestVolumeHolderRestoreWarningIsSavedBeforeThePodGoes(t *testing.T) {
	cases := map[string]func(*corev1.Pod){
		"stuck": func(p *corev1.Pod) {
			p.CreationTimestamp = metav1.NewTime(metav1.Now().Add(-holderRestoreDeadline - time.Minute))
		},
		"interrupted": func(p *corev1.Pod) {
			p.Status.Phase = corev1.PodFailed
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			vol := sharedVolume()
			pod := restoredHolder()
			pod.UID = "carrier"
			pod.OwnerReferences = []metav1.OwnerReference{volumeControllerRef(vol)}
			mutate(pod)
			failed := false
			h := newHolderHarness(t, interceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c crclient.Client, sub string, obj crclient.Object, opts ...crclient.SubResourceUpdateOption) error {
					if !failed {
						failed = true
						return apierrors.NewConflict(schema.GroupResource{Resource: "volumes"}, obj.GetName(), errors.New("stale"))
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
			}, vol, sharedClaim(vol), pod)

			_, err := h.reconciler.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: types.NamespacedName{Name: "uploads", Namespace: holderTestNamespace},
			})
			require.Error(t, err)
			kept, ok := h.holder(t)
			require.True(t, ok, "the pod stays while its warning is unsaved")
			assert.Equal(t, types.UID("carrier"), kept.UID)

			h.reconcile(t)
			h.reconcile(t)
			cond := h.holderCondition(t)
			require.NotNil(t, cond)
			assert.Contains(t, []string{"RestoreStuck", "RestoreInterrupted"}, cond.Reason)
		})
	}
}
