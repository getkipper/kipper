package controllers

import (
	"context"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// ensureMoverPod creates the export mover pod for a transfer if it does not
// exist yet and returns it. The pod is deterministic per CR, owned by it,
// and carries no service-account token: its only credentials are the
// per-transfer bearer token and, for object storage, the service's
// credentials Secret.
func (r *DataTransferReconciler) ensureMoverPod(ctx context.Context, dt *kipperv1.DataTransfer) (*corev1.Pod, error) {
	var existing corev1.Pod
	err := r.podReader().Get(ctx, types.NamespacedName{Name: moverPodName(dt), Namespace: dt.Namespace}, &existing)
	if err == nil {
		return &existing, nil
	}
	if !errors.IsNotFound(err) {
		return nil, err
	}

	pod, err := r.buildMoverPod(dt)
	if err != nil {
		return nil, err
	}
	if err := controllerutil.SetControllerReference(dt, pod, r.Scheme); err != nil {
		return nil, err
	}
	if err := r.Create(ctx, pod); err != nil {
		if errors.IsAlreadyExists(err) {
			if getErr := r.podReader().Get(ctx, types.NamespacedName{Name: pod.Name, Namespace: pod.Namespace}, &existing); getErr == nil {
				return &existing, nil
			}
		}
		return nil, err
	}
	return pod, nil
}

// podReader prefers the uncached API reader; tests without one fall back to
// the fake client.
func (r *DataTransferReconciler) podReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *DataTransferReconciler) buildMoverPod(dt *kipperv1.DataTransfer) (*corev1.Pod, error) {
	chunkSize := dt.Spec.ChunkSizeBytes
	if chunkSize == 0 {
		chunkSize = 128 * 1024 * 1024
	}
	concurrency := dt.Spec.Concurrency
	if concurrency == 0 {
		concurrency = 4
	}

	args := []string{
		"export",
		"--target-url", dt.Spec.TargetBaseURL,
		"--transfer-id", dt.Name,
		"--token-env", "DATAMOVER_TOKEN",
		"--chunk-size", strconv.FormatInt(chunkSize, 10),
		"--concurrency", strconv.Itoa(int(concurrency)),
	}

	var (
		volumes      []corev1.Volume
		volumeMounts []corev1.VolumeMount
		env          []corev1.EnvVar
	)

	env = append(env, corev1.EnvVar{
		Name: "DATAMOVER_TOKEN",
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: tokenSecretName(dt)},
				Key:                  "token",
			},
		},
	})

	switch dt.Spec.Kind {
	case "volume":
		if dt.Spec.Source.Volume == "" {
			return nil, fmt.Errorf("volume transfer %s has no source volume", dt.Name)
		}
		args = append(args, "--mode", "volume", "--path", "/data")
		volumes = append(volumes, corev1.Volume{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "shared-" + dt.Spec.Source.Volume,
					ReadOnly:  true,
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/data", ReadOnly: true})

	case "servicePVC":
		// The service's PVC moves as raw bytes while the statefulset is
		// scaled to zero, keeping the engine's on-disk layout identical on
		// the target without speaking its object protocol.
		if dt.Spec.Source.Service == "" {
			return nil, fmt.Errorf("service transfer %s has no source service", dt.Name)
		}
		args = append(args, "--mode", "volume", "--path", "/data")
		volumes = append(volumes, corev1.Volume{
			Name: "data",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "data-" + dt.Spec.Source.Service + "-0",
					ReadOnly:  true,
				},
			},
		})
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/data", ReadOnly: true})

	default:
		return nil, fmt.Errorf("unsupported transfer kind %q", dt.Spec.Kind)
	}

	volumes = append(volumes, corev1.Volume{
		Name:         "tmp",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"})

	no := false
	yes := true
	root := int64(0)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      moverPodName(dt),
			Namespace: dt.Namespace,
			Labels: map[string]string{
				kipperLabel:                    kipperValue,
				"kipper.run/resource-type":     "datatransfer-mover",
				"kipper.run/migration-session": dt.Spec.SessionID,
			},
		},
		Spec: corev1.PodSpec{
			RestartPolicy:                corev1.RestartPolicyNever,
			AutomountServiceAccountToken: &no,
			Volumes:                      volumes,
			Containers: []corev1.Container{{
				Name:         "mover",
				Image:        r.DatamoverImage,
				Args:         args,
				Env:          env,
				VolumeMounts: volumeMounts,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("512Mi"),
					},
				},
				SecurityContext: &corev1.SecurityContext{
					// Volume files carry arbitrary app-defined owners; the
					// read side needs root, everything else is dropped.
					RunAsUser:                &root,
					AllowPrivilegeEscalation: &no,
					ReadOnlyRootFilesystem:   &yes,
					Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
				},
			}},
		},
	}, nil
}
