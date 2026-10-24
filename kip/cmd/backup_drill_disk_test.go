package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/kip/internal/service"
)

func sizedClaim(name, size string) corev1.PersistentVolumeClaim {
	c := claim("shop", name, nil)
	c.Spec.Resources.Requests = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)}
	return c
}

func labelledPod(name string, labels map[string]string, claims ...string) corev1.Pod {
	pod := podMounting("shop", corev1.PodRunning, claims...)
	pod.ObjectMeta = metav1.ObjectMeta{Namespace: "shop", Name: name, Labels: labels}
	return pod
}

func TestDrillClaimsAreTheClaimsOfThePodsTheDrillRestores(t *testing.T) {
	claims := []corev1.PersistentVolumeClaim{
		sizedClaim("data-db-0", "10Gi"), sizedClaim("db-dump", "10Gi"), sizedClaim("shared-uploads", "5Gi"),
		sizedClaim("shared-media", "1Gi"), sizedClaim("app-cache", "3Gi"),
	}
	pods := []corev1.Pod{
		labelledPod("db-0", map[string]string{"kipper.run/service-type": "postgres"}, "data-db-0", "db-dump"),
		labelledPod("shared-uploads-holder", holderPods()["project holder"], "shared-uploads"),
		labelledPod("web-1", map[string]string{"app": "web"}, "shared-uploads", "app-cache"),
		labelledPod("web-2", map[string]string{"app": "web"}, "shared-media"),
	}
	got, err := drillClaims(pods, claims)
	require.NoError(t, err)
	var names []string
	for _, c := range got {
		names = append(names, c.Name)
	}
	assert.Equal(t, []string{"data-db-0", "db-dump", "shared-uploads"}, names,
		"each restored claim counts once, and claims only app pods mount stay out as the drill leaves them out")
}

func TestDrillDiskNoteMultipliesEachVolumeByItsCopies(t *testing.T) {
	note := drillDiskNote([]drillVolume{
		{bytes: 10 << 30, copies: 2},
		{bytes: 10 << 30, copies: 1},
		{bytes: 5 << 30, copies: 1},
	})
	assert.Equal(t, "Based on the namespace's current volumes, the drill restores about 3 volumes totaling 25Gi, which take up to 35Gi of storage with their copies. "+
		"A backup taken before volumes were added, removed or resized restores different sizes.", note)
	assert.Empty(t, drillDiskNote(nil))
}

func TestADegradedDrillVolumeIsAWarning(t *testing.T) {
	assert.Equal(t, "data-db-0 has 1 of 2 copies. Longhorn could not place every copy, which is harmless for the drill's scratch copy.",
		degradedDrillWarning("data-db-0", service.ClaimCopies{Robustness: "degraded", Wanted: 2, Current: 1}))
	assert.Empty(t, degradedDrillWarning("data-db-0", service.ClaimCopies{Robustness: "healthy", Wanted: 2, Current: 2}))
	assert.Empty(t, degradedDrillWarning("data-db-0", service.ClaimCopies{}), "an unread volume says nothing")
}
