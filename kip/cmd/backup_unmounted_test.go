package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func claim(ns, name string, labels map[string]string) corev1.PersistentVolumeClaim {
	return corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, Labels: labels}}
}

func podMounting(ns string, phase corev1.PodPhase, claims ...string) corev1.Pod {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ns}, Status: corev1.PodStatus{Phase: phase},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}}}
	for _, c := range claims {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "v-" + c, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: c}}})
		pod.Spec.Containers[0].VolumeMounts = append(pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "v-" + c})
	}
	return pod
}

func TestVolumesNoRunningPodMountsAreNamed(t *testing.T) {
	pvcs := []corev1.PersistentVolumeClaim{
		claim("shop", "data-db-0", nil),
		claim("shop", "uploads", nil),
		claim("shop", "stopped-app-files", nil),
		claim("shop", "scratch", map[string]string{"velero.io/exclude-from-backup": "true"}),
		claim("velero", "minio", nil),
		claim("blog", "data-db-0", nil),
	}
	pods := []corev1.Pod{
		podMounting("shop", corev1.PodRunning, "data-db-0"),
		podMounting("shop", corev1.PodSucceeded, "stopped-app-files"),
		podMounting("blog", corev1.PodRunning, "data-db-0"),
		podMounting("other", corev1.PodRunning, "uploads"),
	}

	assert.Equal(t, []string{"shop/stopped-app-files", "shop/uploads"}, volumesOutsideBackups(pvcs, pods),
		"a finished pod, or a pod in another namespace with a claim of the same name, does not put a volume into the backup")
}

func TestAVolumeOnlyAnInitContainerMountsIsNamed(t *testing.T) {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "shop"}, Status: corev1.PodStatus{Phase: corev1.PodRunning},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{Name: "seed", VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "seed-data"}}}},
			InitContainers: []corev1.Container{{Name: "init", VolumeMounts: []corev1.VolumeMount{{Name: "seed"}}}},
			Containers:     []corev1.Container{{Name: "app"}},
		}}
	assert.Equal(t, []string{"shop/seed-data"}, volumesOutsideBackups([]corev1.PersistentVolumeClaim{claim("shop", "seed-data", nil)}, []corev1.Pod{pod}))
}
