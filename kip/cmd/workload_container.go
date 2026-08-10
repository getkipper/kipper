package cmd

import (
	corev1 "k8s.io/api/core/v1"
)

// kipperSidecarContainer is the instance-id proxy the app reconciler injects
// next to the app container. Logs and exec must target the workload's own
// container, never the sidecar.
const kipperSidecarContainer = "kipper-instance-proxy"

// workloadContainerName picks the workload's own container in a pod that may
// carry the kipper sidecar. The reconcilers name the main container after the
// workload, so that match wins; otherwise the first container that is not the
// sidecar. Kubernetes requires a container name on multi-container pods, so
// log and exec requests must always pass the result explicitly.
func workloadContainerName(pod *corev1.Pod, workload string) string {
	for _, c := range pod.Spec.Containers {
		if c.Name == workload {
			return c.Name
		}
	}
	for _, c := range pod.Spec.Containers {
		if c.Name != kipperSidecarContainer {
			return c.Name
		}
	}
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Name
	}
	return ""
}
