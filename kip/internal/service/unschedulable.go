package service

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// UnschedulableReason returns the scheduler's message on a pod it cannot place,
// empty for a pod that is scheduled or has nothing recorded yet.
func UnschedulableReason(pod *corev1.Pod) string {
	if pod.Spec.NodeName != "" {
		return ""
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return c.Message
		}
	}
	return ""
}

// UnschedulableServices maps service names to scheduler failure messages and
// suggested remedies. It selects pods with the service-type label in namespace.
// It returns nil for a nil client, a listing error or no reported failures.
// If several pods share a service name, the last reported failure wins.
func UnschedulableServices(ctx context.Context, client kubernetes.Interface, namespace string) map[string]string {
	if client == nil {
		return nil
	}
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: serviceTypeLabel})
	if err != nil {
		return nil
	}
	var byName map[string]string
	for i := range pods.Items {
		pod := &pods.Items[i]
		name := pod.Labels["app"]
		reason := UnschedulableReason(pod)
		if name == "" || reason == "" {
			continue
		}
		if byName == nil {
			byName = map[string]string{}
		}
		byName[name] = UnschedulableMessage(name, reason)
	}
	return byName
}

// UnschedulableMessage formats the scheduler's reason with advice to reduce
// CPU or memory requests or add capacity. The advice is included for every reason.
func UnschedulableMessage(service, reason string) string {
	return fmt.Sprintf("the pod cannot be scheduled: %s For insufficient CPU or memory, reduce requests with 'kip service update %s --cpu <cpu> --memory <memory>' or add cluster capacity.", reason, service)
}
