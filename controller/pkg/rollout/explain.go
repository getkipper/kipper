package rollout

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Reason names why a rollout has not finished.
type Reason string

const (
	Complete         Reason = ""
	Unschedulable    Reason = "Unschedulable"
	QuotaExceeded    Reason = "QuotaExceeded"
	PodsRefused      Reason = "PodsRefused"
	NotBecomingReady Reason = "NotBecomingReady"
	DeadlineExceeded Reason = "DeadlineExceeded"
	InProgress       Reason = "InProgress"
)

const keepServing = "Any healthy current pods continue serving."

// Explain returns the first matching rollout issue: scheduling, admission,
// startup timeout, progress deadline, or ongoing progress. newPods must belong
// to the newest ReplicaSet; budget is the startup timeout.
func Explain(dep *appsv1.Deployment, newPods []corev1.Pod, budget time.Duration, now time.Time) (Reason, string) {
	if Settled(dep) {
		return Complete, ""
	}
	for i := range newPods {
		if c := podCondition(&newPods[i], corev1.PodScheduled); c != nil &&
			c.Status == corev1.ConditionFalse && c.Reason == corev1.PodReasonUnschedulable {
			return Unschedulable, fmt.Sprintf("A new pod cannot be placed: %s %s Lower the CPU or memory request, or add capacity.", c.Message, keepServing)
		}
	}
	for _, c := range dep.Status.Conditions {
		if c.Type == appsv1.DeploymentReplicaFailure && c.Status == corev1.ConditionTrue {
			if strings.Contains(c.Message, "exceeded quota") {
				return QuotaExceeded, fmt.Sprintf("The project quota prevents new pods from being created: %s %s", c.Message, keepServing)
			}
			return PodsRefused, fmt.Sprintf("The cluster rejected new pods: %s %s", c.Message, keepServing)
		}
	}
	// Check scheduling and admission before completion: a scale-up can
	// retain NewReplicaSetAvailable while its new pods are blocked.
	if Finished(dep) {
		return Complete, ""
	}
	for i := range newPods {
		p := &newPods[i]
		ready := podCondition(p, corev1.PodReady)
		if p.Status.Phase == corev1.PodRunning && p.Status.StartTime != nil &&
			(ready == nil || ready.Status != corev1.ConditionTrue) &&
			now.Sub(p.Status.StartTime.Time) > budget {
			return NotBecomingReady, fmt.Sprintf("New pods have not become ready within %s. %s", budget, keepServing)
		}
	}
	if Failed(dep) {
		return DeadlineExceeded, fmt.Sprintf("The rollout stopped making progress: %s %s", Message(dep), keepServing)
	}
	want := int32(1)
	if dep.Spec.Replicas != nil {
		want = *dep.Spec.Replicas
	}
	if dep.Status.UpdatedReplicas > want {
		return InProgress, fmt.Sprintf("Scaling to %d pod(s).", want)
	}
	return InProgress, fmt.Sprintf("Replacing pods: %d of %d updated.", dep.Status.UpdatedReplicas, want)
}

// NewestReplicaSet returns the Deployment's highest-revision ReplicaSet,
// or nil if no owned ReplicaSet has a valid revision.
func NewestReplicaSet(dep *appsv1.Deployment, sets []appsv1.ReplicaSet) *appsv1.ReplicaSet {
	var newest *appsv1.ReplicaSet
	best := -1
	for i := range sets {
		if !ownedBy(&sets[i], dep) {
			continue
		}
		rev, err := strconv.Atoi(sets[i].Annotations["deployment.kubernetes.io/revision"])
		if err != nil {
			continue
		}
		if rev > best {
			best, newest = rev, &sets[i]
		}
	}
	return newest
}

func ownedBy(rs *appsv1.ReplicaSet, dep *appsv1.Deployment) bool {
	for _, ref := range rs.OwnerReferences {
		if ref.UID == dep.UID {
			return true
		}
	}
	return false
}

func podCondition(p *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// PodsOf returns the pods rs controls, or nil when rs is nil.
func PodsOf(rs *appsv1.ReplicaSet, pods []corev1.Pod) []corev1.Pod {
	if rs == nil {
		return nil
	}
	var out []corev1.Pod
	for i := range pods {
		if ref := metav1.GetControllerOf(&pods[i]); ref != nil && ref.UID == rs.UID {
			out = append(out, pods[i])
		}
	}
	return out
}
