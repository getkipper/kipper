package platform

import corev1 "k8s.io/api/core/v1"

// PodRequests returns peak CPU requests in millicores and memory requests in
// bytes across init and running containers, including restartable init containers
// that run alongside them. Pod-level requests override each resource's aggregate
// before overhead is added. spec must be non-nil.
func PodRequests(spec *corev1.PodSpec) (cpuMilli, memoryBytes int64) {
	var sidecarCPU, sidecarMemory, peakCPU, peakMemory int64
	for _, c := range spec.InitContainers {
		cpu, memory := c.Resources.Requests.Cpu().MilliValue(), c.Resources.Requests.Memory().Value()
		if c.RestartPolicy != nil && *c.RestartPolicy == corev1.ContainerRestartPolicyAlways {
			sidecarCPU += cpu
			sidecarMemory += memory
			cpu, memory = sidecarCPU, sidecarMemory
		} else {
			cpu += sidecarCPU
			memory += sidecarMemory
		}
		peakCPU, peakMemory = max(peakCPU, cpu), max(peakMemory, memory)
	}
	cpuMilli, memoryBytes = sidecarCPU, sidecarMemory
	for _, c := range spec.Containers {
		cpuMilli += c.Resources.Requests.Cpu().MilliValue()
		memoryBytes += c.Resources.Requests.Memory().Value()
	}
	cpuMilli, memoryBytes = max(cpuMilli, peakCPU), max(memoryBytes, peakMemory)
	if spec.Resources != nil {
		if q, ok := spec.Resources.Requests[corev1.ResourceCPU]; ok {
			cpuMilli = q.MilliValue()
		}
		if q, ok := spec.Resources.Requests[corev1.ResourceMemory]; ok {
			memoryBytes = q.Value()
		}
	}
	cpuMilli += spec.Overhead.Cpu().MilliValue()
	memoryBytes += spec.Overhead.Memory().Value()
	return cpuMilli, memoryBytes
}
