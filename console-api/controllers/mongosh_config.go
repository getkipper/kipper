package controllers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// mongoshConf disables telemetry and snippet index downloads for mongosh.
const mongoshConf = `mongosh:
  forceDisableTelemetry: true
  snippetIndexSourceURLs: ""
`

const mongoshConfigVolume = "mongosh-config"

func mongoshConfigMapName(service string) string { return service + "-mongosh" }

func mongoshConfigMount() corev1.VolumeMount {
	return corev1.VolumeMount{Name: mongoshConfigVolume, MountPath: "/etc/mongosh.conf", SubPath: "mongosh.conf", ReadOnly: true}
}

func mongoshConfigPodVolume(service string) corev1.Volume {
	return corev1.Volume{Name: mongoshConfigVolume, VolumeSource: corev1.VolumeSource{
		ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: mongoshConfigMapName(service)}},
	}}
}

// reconcileMongoshConfig creates or updates the service's mongosh config.
// An existing ConfigMap must be controlled by this service to allow updates.
func (r *ServiceReconciler) reconcileMongoshConfig(ctx context.Context, svc *kipperv1.Service) error {
	data := map[string]string{"mongosh.conf": mongoshConf}
	var existing corev1.ConfigMap
	err := r.Get(ctx, types.NamespacedName{Namespace: svc.Namespace, Name: mongoshConfigMapName(svc.Name)}, &existing)
	if errors.IsNotFound(err) {
		cm := &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: mongoshConfigMapName(svc.Name), Namespace: svc.Namespace,
				Labels: map[string]string{kipperLabel: kipperValue, "app": svc.Name},
			},
			Data: data,
		}
		if err := controllerutil.SetControllerReference(svc, cm, r.Scheme); err != nil {
			return err
		}
		return r.Create(ctx, cm)
	}
	if err != nil {
		return err
	}
	if owner := metav1.GetControllerOf(&existing); owner == nil || owner.UID != svc.UID {
		return &nameTakenError{Kind: "ConfigMap", Name: existing.Name, Namespace: svc.Namespace}
	}
	if stringMapEqual(existing.Data, data) {
		return nil
	}
	existing.Data = data
	return r.Update(ctx, &existing)
}

// withVolume replaces the first volume with v's name, or appends v if absent.
func withVolume(volumes []corev1.Volume, v corev1.Volume) []corev1.Volume {
	for i := range volumes {
		if volumes[i].Name == v.Name {
			volumes[i] = v
			return volumes
		}
	}
	return append(volumes, v)
}
