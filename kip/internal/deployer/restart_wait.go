package deployer

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/rollout"
)

const restartedAtAnnotation = "kipper.run/restartedAt"

// restartDone checks rollout readiness and uses stamp to identify this restart.
// pods must include all pods matching dep's selector, including terminating pods.
func restartDone(dep *appsv1.Deployment, pods []corev1.Pod, stamp string) (bool, error) {
	if dep.Spec.Template.Annotations[restartedAtAnnotation] != stamp {
		return false, nil
	}
	// Wait for status to reflect the new template before checking for a stall.
	if dep.Status.ObservedGeneration < dep.Generation {
		return false, nil
	}
	if rollout.Failed(dep) {
		return false, fmt.Errorf("the restart stalled with %s: %s", restartProgress(dep), rollout.Message(dep))
	}
	if !rollout.Settled(dep) {
		return false, nil
	}
	for i := range pods {
		if pods[i].Annotations[restartedAtAnnotation] != stamp {
			return false, nil
		}
	}
	return true, nil
}

func restartProgress(dep *appsv1.Deployment) string {
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	return fmt.Sprintf("%d/%d pods replaced, %d ready", dep.Status.UpdatedReplicas, want, dep.Status.AvailableReplicas)
}

// WaitForRestart waits for all replicas to be updated and available and all
// matching pods to carry stamp from Restart. It polls at intervals of every;
// budget limits the entire wait, including API reads.
func (d *Deployer) WaitForRestart(ctx context.Context, namespace, name, stamp string, budget, every time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var last *appsv1.Deployment
	timedOut := func() error {
		if last == nil || last.Spec.Template.Annotations[restartedAtAnnotation] != stamp {
			return fmt.Errorf("after %s, the controller has not applied the restart to %s. Run `kip app info %s` for details", budget, name, name)
		}
		return fmt.Errorf("%s is still restarting after %s: %s. Run `kip app info %s` for details", name, budget, restartProgress(last), name)
	}
	for {
		dep, err := d.Client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return timedOut()
			}
			return fmt.Errorf("reading %s while waiting for its restart: %w", name, err)
		}
		last = dep
		var pods []corev1.Pod
		if dep.Spec.Selector != nil {
			selector, err := metav1.LabelSelectorAsSelector(dep.Spec.Selector)
			if err != nil {
				return fmt.Errorf("reading the pod selector of %s: %w", name, err)
			}
			list, err := d.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
			if err != nil {
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return timedOut()
				}
				return fmt.Errorf("listing the pods of %s while waiting for its restart: %w", name, err)
			}
			pods = list.Items
		}
		done, err := restartDone(dep, pods, stamp)
		if err != nil {
			return fmt.Errorf("%w. Run `kip app info %s` for details", err, name)
		}
		if done {
			return nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return timedOut()
			}
			return ctx.Err()
		case <-time.After(every):
		}
	}
}
