package service

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// serviceTypeLabel is set on every pod the service controller creates and on no
// app pod, which is what tells the two apart when a project has an app and a
// service under the same name.
const serviceTypeLabel = "kipper.run/service-type"

// CrashLoop is one service Kubernetes has given up restarting.
type CrashLoop struct {
	Namespace string
	Service   string
	Message   string
}

// CrashLoopsEverywhere reports every crash-looping service on the cluster.
//
// It reads pods because nothing else carries this. A Service CR's phase
// describes the CR, and a StatefulSet reporting 0/1 ready looks identical
// whether a service is thirty seconds into starting or three days into failing.
//
// Best-effort: a cluster that refuses the pod list returns nothing rather than
// an error, because a listing that fails entirely is worse than one missing an
// annotation.
func CrashLoopsEverywhere(ctx context.Context, client kubernetes.Interface) []CrashLoop {
	return crashLoopsIn(ctx, client, metav1.NamespaceAll)
}

// CrashLoopingServices reports the crash-looping services in one namespace,
// keyed by service name.
func CrashLoopingServices(ctx context.Context, client kubernetes.Interface, namespace string) map[string]string {
	loops := crashLoopsIn(ctx, client, namespace)
	if len(loops) == 0 {
		return nil
	}
	byName := make(map[string]string, len(loops))
	for _, l := range loops {
		byName[l.Service] = l.Message
	}
	return byName
}

func crashLoopsIn(ctx context.Context, client kubernetes.Interface, namespace string) []CrashLoop {
	if client == nil {
		return nil
	}
	pods, err := client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: serviceTypeLabel,
	})
	if err != nil {
		return nil
	}

	var loops []CrashLoop
	for i := range pods.Items {
		pod := &pods.Items[i]
		name := pod.Labels["app"]
		if name == "" {
			continue
		}
		// Init containers as well as the main ones. A service's init container
		// mounts the same volume the database does, so a read-only volume stops
		// the pod before the main container starts, and Kubernetes then leaves
		// ContainerStatuses empty.
		statuses := append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...),
			pod.Status.ContainerStatuses...)
		for _, cs := range statuses {
			if cs.State.Waiting == nil || cs.State.Waiting.Reason != "CrashLoopBackOff" {
				continue
			}
			loops = append(loops, CrashLoop{
				Namespace: pod.Namespace,
				Service:   name,
				Message:   crashLoopMessage(name, cs),
			})
			break
		}
	}
	return loops
}

// crashLoopMessage names the container, how many times it has died, why it last
// died, and what to do next.
//
// It stops short of calling the restart a recovery. A crash loop is usually the
// image or the configuration, and a recreated pod comes back with both. Where
// recreation is the answer is a mount that went read-only underneath the pod,
// because the mount belongs to the pod and no number of container restarts
// clears it, and that is worth naming because it is not the obvious one.
func crashLoopMessage(service string, cs corev1.ContainerStatus) string {
	msg := fmt.Sprintf("container %q has restarted %d times and is not staying up", cs.Name, cs.RestartCount)
	if term := cs.LastTerminationState.Terminated; term != nil {
		msg += fmt.Sprintf(" (last exit code %d)", term.ExitCode)
	}
	return msg + fmt.Sprintf(". Check its logs. If the cause is storage rather than its image or configuration, 'kip service restart %s' recreates the pod, which is the one thing a container restart cannot do", service)
}
