package controllers

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/platform"
)

func annotateDataClaim(t *testing.T, c crclient.Client, key, value string) {
	t.Helper()
	pvc := storedDataClaim(t, c)
	if pvc.Annotations == nil {
		pvc.Annotations = map[string]string{}
	}
	pvc.Annotations[key] = value
	require.NoError(t, c.Update(context.Background(), pvc))
}

// refusingConverter is a converter for a one-copy volume at the given time,
// whose volume patches are refused while refuse is true. It counts the
// volume patches it receives.
func refusingConverter(c crclient.WithWatch, now *time.Time, refuse *bool, patches *int) storageConverter {
	wrapped := interceptor.NewClient(c, interceptor.Funcs{Patch: func(ctx context.Context, cl crclient.WithWatch, obj crclient.Object, patch crclient.Patch, opts ...crclient.PatchOption) error {
		if _, claim := obj.(*corev1.PersistentVolumeClaim); claim {
			return cl.Patch(ctx, obj, patch, opts...)
		}
		*patches++
		if *refuse {
			return apierrors.NewForbidden(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, "pvc-1", fmt.Errorf("webhook said no"))
		}
		return cl.Patch(ctx, obj, patch, opts...)
	}})
	return storageConverter{Reader: wrapped, Writer: wrapped, Now: func() time.Time { return *now }}
}

func copiesBackoff(t *testing.T, c crclient.Client) *platform.CopiesBackoff {
	t.Helper()
	return backoffOf(storedDataClaim(t, c))
}

// A rebuild Longhorn refused outright frees the lease for other volumes and
// is asked for again after a backoff that doubles with each refusal.
func TestARefusedRebuildReleasesTheLeaseAndBacksOff(t *testing.T) {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).
		WithObjects(withDataClaim(convWorld(oneServerCopy()...))...).Build()
	now, refuse, patches := convNow, true, 0
	sc := refusingConverter(c, &now, &refuse, &patches)

	cond, err := convertOnce(t, sc, ptr.To(int32(2)))
	require.NoError(t, err)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.Contains(t, cond.Message, "webhook said no")
	assert.Contains(t, cond.Message, "Kipper asks again at 2026-10-24T12:01:00Z")
	lease := conversionLease(t, c)
	assert.True(t, lease == nil || lease.Spec.HolderIdentity == nil, "the lease is free for other volumes")
	b := copiesBackoff(t, c)
	require.NotNil(t, b)
	assert.Equal(t, int64(2), b.Target)
	assert.Equal(t, 1, b.Failures)
	assert.Equal(t, "2026-10-24T12:01:00Z", b.Until)
	assert.Contains(t, b.Error, "webhook said no")

	now = convNow.Add(30 * time.Second)
	cond, err = convertOnce(t, sc, ptr.To(int32(2)))
	require.NoError(t, err)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesBlocked, cond.Reason)
	assert.Contains(t, cond.Message, "Kipper asks again at 2026-10-24T12:01:00Z")
	assert.Equal(t, 1, patches, "nothing is asked of Longhorn during the backoff")
	lease = conversionLease(t, c)
	assert.True(t, lease == nil || lease.Spec.HolderIdentity == nil, "no lease is taken during the backoff")

	now = convNow.Add(time.Minute)
	_, err = convertOnce(t, sc, ptr.To(int32(2)))
	require.NoError(t, err)
	assert.Equal(t, 2, patches)
	b = copiesBackoff(t, c)
	require.NotNil(t, b)
	assert.Equal(t, 2, b.Failures)
	assert.Equal(t, "2026-10-24T12:03:00Z", b.Until, "the second refusal waits two minutes")

	now, refuse = convNow.Add(3*time.Minute), false
	cond, err = convertOnce(t, sc, ptr.To(int32(2)))
	require.NoError(t, err)
	require.NotNil(t, cond)
	assert.Equal(t, kipperv1.StorageCopiesConverting, cond.Reason)
	assert.Nil(t, copiesBackoff(t, c), "a rebuild Longhorn accepted ends the backoff")
}

// The backoff belongs to the copy count it was refused for. Setting another
// count drops it.
func TestABackoffEndsWhenTheCopiesChange(t *testing.T) {
	c := crfake.NewClientBuilder().WithScheme(testScheme()).WithRESTMapper(testMapper(true)).
		WithObjects(withDataClaim(convWorld(oneServerCopy()...))...).Build()
	annotateDataClaim(t, c, platform.CopiesBackoffAnnotation, `{"target":2,"until":"2026-10-24T12:30:00Z","failures":5,"error":"refused"}`)
	now, refuse, patches := convNow, false, 0
	sc := refusingConverter(c, &now, &refuse, &patches)

	_, err := convertOnce(t, sc, ptr.To(int32(1)))
	require.NoError(t, err)
	assert.Nil(t, copiesBackoff(t, c))
}

func projectNamespace(name, project string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{kipperlabels.Project: project}}}
}

// The waiting message names the volume holding the lease only when it is in
// the same project, since the message is visible to the waiting project.
func TestAWaitingConversionNamesTheHolderInItsOwnProject(t *testing.T) {
	cases := map[string]struct {
		namespace string
		want      string
	}{
		"same namespace":  {"ns", "Waiting for the copy change of api in ns to finish."},
		"same project":    {"ns-staging", "Waiting for the copy change of api in ns-staging to finish."},
		"another project": {"other", "Waiting for the copy change of a volume in another project to finish."},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			holder := lhVolumeObj("pvc-other", 2, platform.ReplicaStorageTag)
			_ = unstructured.SetNestedField(holder.Object, tc.namespace, "status", "kubernetesStatus", "namespace")
			_ = unstructured.SetNestedField(holder.Object, "data-api-0", "status", "kubernetesStatus", "pvcName")
			shop := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "shop"}, Status: kipperv1.ProjectStatus{Namespaces: []string{"ns", "ns-staging"}}}
			other := &kipperv1.Project{ObjectMeta: metav1.ObjectMeta{Name: "blog"}, Status: kipperv1.ProjectStatus{Namespaces: []string{"other"}}}
			sc, _ := converterFor(t, convWorld(append(oneServerCopy(), heldLease("pvc-other", convNow.Add(-time.Minute)), holder,
				projectNamespace("ns", "shop"), projectNamespace("ns-staging", "shop"), projectNamespace("other", "blog"), shop, other)...)...)

			cond, err := convertOnce(t, sc, ptr.To(int32(2)))
			require.NoError(t, err)
			require.NotNil(t, cond)
			assert.Equal(t, kipperv1.StorageCopiesConversionPending, cond.Reason)
			assert.Equal(t, tc.want+" This volume's conversion starts once that one is done.", cond.Message)
		})
	}
}

// A refusal for a step without a rebuild patch proves nothing and is returned.
func TestARefusalWithoutARebuildPatchIsReturned(t *testing.T) {
	sc, _ := converterFor(t, convWorld(oneServerCopy()...)...)
	refused := apierrors.NewForbidden(schema.GroupResource{Group: "longhorn.io", Resource: "volumes"}, "pvc-1", fmt.Errorf("no"))
	cond, err := sc.refusedRebuild(context.Background(), storedDataClaim(t, sc.Writer), &lhVolume{Name: "pvc-1"}, conversionStep{}, 2, nil, refused)
	assert.Nil(t, cond)
	assert.Equal(t, refused, err)
}
