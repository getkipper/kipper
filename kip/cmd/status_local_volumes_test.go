package cmd

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func localClaim(namespace, name, class string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.PersistentVolumeClaimSpec{StorageClassName: &class},
	}
}

func retainedPV(name, service, state, usedBytes string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{ObjectMeta: metav1.ObjectMeta{
		Name:   name,
		Labels: map[string]string{platform.RetainedLocalCopyLabel: "true"},
		Annotations: map[string]string{
			platform.RetainedLocalCopyServiceAnnotation:   service,
			platform.RetainedLocalCopyStateAnnotation:     state,
			platform.RetainedLocalCopyUsedBytesAnnotation: usedBytes,
		},
	}}
}

func TestStatusListsLocalVolumesLeftOnALonghornCluster(t *testing.T) {
	marked := copiesStatusObject("Service", "shop", "db", 0, "", "", "")
	marked.SetAnnotations(map[string]string{platform.StorageMoveAnnotation: "kipper-storage-move-abc"})
	dyn := copiesStatusClient(
		marked,
		copiesStatusObject("Service", "shop", "cache", 0, "", "", ""),
		copiesStatusObject("Service", "blog", "pg", 0, "", "", ""),
		copiesStatusObject("Service", "blog", "moved", 0, "", "", ""),
	)
	client := fake.NewClientset(
		localClaim("shop", "data-db-0", platform.LocalPathClass),
		localClaim("shop", "data-cache-0", platform.LocalPathClass),
		localClaim("blog", "data-pg-0", platform.LocalPathClass),
		localClaim("blog", "data-moved-0", "longhorn-single"),
		localClaim("blog", "unrelated", platform.LocalPathClass),
		retainedPV("pvc-1", "shop/db", platform.RetainedLocalCopyMoving, "1073741824"),
		retainedPV("pvc-2", "blog/moved", platform.RetainedLocalCopyMoved, "2147483648"),
		retainedPV("pvc-3", "blog/old", platform.RetainedLocalCopyMoved, "1073741824"),
	)

	var out bytes.Buffer
	reportLocalVolumes(context.Background(), &out, client, dyn)
	got := out.String()

	assert.Contains(t, got, "Moves to Longhorn not finished:\n    ⚠  shop/db\n")
	assert.Contains(t, got, "'kip service storage longhorn <name>'")
	assert.Contains(t, got, "--rollback")
	assert.Contains(t, got, "Service volumes still on local storage:\n    ·  blog/pg\n    ·  shop/cache\n")
	assert.NotContains(t, got, "·  shop/db\n", "a service in the middle of a move is listed once, as unfinished")
	assert.NotContains(t, got, "unrelated")
	assert.NotContains(t, got, "blog/moved\n    ·")
	assert.Contains(t, got, "Local copies kept after a move: 2 (3.0 GiB)")
	assert.Contains(t, got, "'kip cluster storage prune'")
}

func TestStatusShowsNothingWhenNoLocalVolumesAreLeft(t *testing.T) {
	dyn := copiesStatusClient(copiesStatusObject("Service", "shop", "db", 0, "", "", ""))
	client := fake.NewClientset(localClaim("shop", "data-db-0", "longhorn-single"))

	var out bytes.Buffer
	reportLocalVolumes(context.Background(), &out, client, dyn)

	assert.Empty(t, out.String())
}
