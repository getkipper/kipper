// Package quota holds the project-quota projection shared by the auto resource
// controller and the manual resource handlers. Keeping the math in one leaf
// package means an interactive resource change is judged against the namespace
// ResourceQuota exactly the way the controller judges an automatic one, so the
// API can reject an over-quota request instead of letting the rollout wedge at
// admission.
package quota

import (
	"context"
	"math"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// Fits projects the namespace usage a resource change needs and compares it
// against the quota's hard caps, returning the first dimension that does not
// fit. Two projections matter:
//
//   - steady state: current usage plus the per-replica delta, once every old
//     pod has been replaced.
//   - rollout peak: a Deployment's rolling update admits surge pods at the new
//     size while every old pod still counts against the quota, so the peak is
//     current usage plus the full new pod value per surge pod. This is the
//     projection admission actually evaluates first.
func Fits(quota *corev1.ResourceQuota, before, after corev1.ResourceRequirements, replicas, surgePods int32, podSpec *corev1.PodSpec) (dimension, projected, hard string, fits bool) {
	if replicas < 1 {
		replicas = 1
	}
	for _, dim := range []struct {
		name    string
		hardKey corev1.ResourceName
		old     corev1.ResourceList
		updated corev1.ResourceList
		resName corev1.ResourceName
		limits  bool
	}{
		{"requests.cpu", corev1.ResourceRequestsCPU, before.Requests, after.Requests, corev1.ResourceCPU, false},
		{"requests.memory", corev1.ResourceRequestsMemory, before.Requests, after.Requests, corev1.ResourceMemory, false},
		{"limits.cpu", corev1.ResourceLimitsCPU, before.Limits, after.Limits, corev1.ResourceCPU, true},
		{"limits.memory", corev1.ResourceLimitsMemory, before.Limits, after.Limits, corev1.ResourceMemory, true},
	} {
		hardQty, ok := quota.Spec.Hard[dim.hardKey]
		if !ok {
			continue
		}
		used := quota.Status.Used[dim.hardKey]

		oldVal := dim.old[dim.resName]
		newVal := dim.updated[dim.resName]
		perPodDelta := newVal.MilliValue() - oldVal.MilliValue()
		steady := used.MilliValue() + perPodDelta*int64(replicas)
		projectedMilli := steady

		if surgePods > 0 && podSpec != nil {
			// During a rolling update the new ReplicaSet can reach full size
			// while up to surgePods old pods still count against the quota, so
			// the true peak is the steady-state projection plus those lingering
			// old pods. podSpec carries the new size; back this container's own
			// change out of it to price one old pod.
			newPodEff := podSpecEffectiveMilli(podSpec, dim.resName, dim.limits)
			oldPodEff := newPodEff - perPodDelta
			if oldPodEff < 0 {
				oldPodEff = 0
			}
			peak := steady + oldPodEff*int64(surgePods)
			if peak > projectedMilli {
				projectedMilli = peak
			}
		}

		if projectedMilli > used.MilliValue() && projectedMilli > hardQty.MilliValue() {
			projectedQty := resource.NewMilliQuantity(projectedMilli, hardQty.Format)
			return dim.name, projectedQty.String(), hardQty.String(), false
		}
	}
	return "", "", "", true
}

// podSpecEffectiveMilli prices one pod the way quota admission does:
// app containers summed, init containers by their maximum.
func podSpecEffectiveMilli(spec *corev1.PodSpec, name corev1.ResourceName, limits bool) int64 {
	value := func(c corev1.Container) int64 {
		list := c.Resources.Requests
		if limits {
			list = c.Resources.Limits
		}
		if v, ok := list[name]; ok {
			return v.MilliValue()
		}
		return 0
	}
	var sum int64
	for _, c := range spec.Containers {
		sum += value(c)
	}
	var initMax int64
	for _, c := range spec.InitContainers {
		if v := value(c); v > initMax {
			initMax = v
		}
	}
	if initMax > sum {
		return initMax
	}
	return sum
}

// DeploymentSurgePods resolves how many extra pods a rolling update may admit
// on top of the desired replica count. Defaults follow Kubernetes: 25%
// maxSurge rounded up. A Recreate strategy tears everything down first, so it
// has no surge.
func DeploymentSurgePods(deploy *appsv1.Deployment, replicas int32) int32 {
	if deploy.Spec.Strategy.Type == appsv1.RecreateDeploymentStrategyType {
		return 0
	}
	maxSurge := intstr.FromString("25%")
	if ru := deploy.Spec.Strategy.RollingUpdate; ru != nil && ru.MaxSurge != nil {
		maxSurge = *ru.MaxSurge
	}
	surge, err := intstr.GetScaledValueFromIntOrPercent(&maxSurge, int(replicas), true)
	if err != nil {
		// Unparseable strategy: fall back to the Kubernetes default of one
		// surge pod rather than assume a surge-free rollout.
		return 1
	}
	if surge <= 0 {
		// An explicit maxSurge of 0 (paired with maxUnavailable) is a valid
		// surge-free strategy; pricing a phantom surge pod would wrongly block
		// increases that fit.
		return 0
	}
	// An absolute maxSurge may legitimately exceed the replica count
	// (replicas=1, maxSurge=5 admits up to six pods), so it must not be
	// clamped to replicas. Only the int32 conversion is bounded.
	if surge > math.MaxInt32 {
		surge = math.MaxInt32
	}
	return int32(surge) //nolint:gosec // bounded to MaxInt32 above
}

// Change carries a requested per-dimension resource update as quantity
// strings. An empty string leaves that dimension unchanged.
type Change struct {
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// PreflightResult reports whether a resource change fits the namespace quota
// and, when it does not, which dimension it would exhaust.
type PreflightResult struct {
	Fits      bool
	Dimension string
	Projected string
	Hard      string
}

// overlay applies the non-empty dimensions of a Change onto a copy of the
// current container resources, producing the resources the workload would run
// with after the change.
func overlay(base corev1.ResourceRequirements, ch Change) corev1.ResourceRequirements {
	out := *base.DeepCopy()
	if out.Requests == nil {
		out.Requests = corev1.ResourceList{}
	}
	if out.Limits == nil {
		out.Limits = corev1.ResourceList{}
	}
	set := func(list corev1.ResourceList, key corev1.ResourceName, raw string) {
		if raw == "" {
			return
		}
		if q, err := resource.ParseQuantity(raw); err == nil {
			list[key] = q
		}
	}
	set(out.Requests, corev1.ResourceCPU, ch.CPURequest)
	set(out.Requests, corev1.ResourceMemory, ch.MemoryRequest)
	set(out.Limits, corev1.ResourceCPU, ch.CPULimit)
	set(out.Limits, corev1.ResourceMemory, ch.MemoryLimit)
	return out
}

// projected reads the namespace ResourceQuota and runs Fits for the given
// before/after on the workload's main container. It is best-effort: no quota
// or an unreadable quota reports Fits=true, since Kubernetes admission is the
// final backstop.
func projected(ctx context.Context, c kubernetes.Interface, namespace string, before, after corev1.ResourceRequirements, replicas, surgePods int32, podSpec *corev1.PodSpec) (PreflightResult, error) {
	rq, err := c.CoreV1().ResourceQuotas(namespace).Get(ctx, kipperv1.ProjectQuotaName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return PreflightResult{Fits: true}, nil
	}
	if err != nil {
		return PreflightResult{Fits: true}, err
	}
	dim, proj, hard, fits := Fits(rq, before, after, replicas, surgePods, podSpec)
	return PreflightResult{Fits: fits, Dimension: dim, Projected: proj, Hard: hard}, nil
}

// PreflightDeployment checks whether applying ch to the main container of the
// named Deployment would exceed the namespace ResourceQuota, projecting the
// change (delta per replica plus rolling-update surge) the same way the auto
// controller does. It reports Fits=true when the Deployment does not exist
// yet: there is nothing running to project against, and admission backstops
// the first rollout.
func PreflightDeployment(ctx context.Context, c kubernetes.Interface, namespace, name string, ch Change) (PreflightResult, error) {
	deploy, err := c.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return PreflightResult{Fits: true}, nil
	}
	if err != nil {
		return PreflightResult{Fits: true}, err
	}
	if len(deploy.Spec.Template.Spec.Containers) == 0 {
		return PreflightResult{Fits: true}, nil
	}
	replicas := int32(1)
	if deploy.Spec.Replicas != nil {
		replicas = *deploy.Spec.Replicas
	}
	podSpec := deploy.Spec.Template.Spec.DeepCopy()
	before := *podSpec.Containers[0].Resources.DeepCopy()
	after := overlay(before, ch)
	podSpec.Containers[0].Resources = after
	return projected(ctx, c, namespace, before, after, replicas, DeploymentSurgePods(deploy, replicas), podSpec)
}

// PreflightStatefulSet is the StatefulSet equivalent of PreflightDeployment.
// StatefulSets replace pods in place rather than surging, so there is no
// rollout peak beyond steady state.
func PreflightStatefulSet(ctx context.Context, c kubernetes.Interface, namespace, name string, ch Change) (PreflightResult, error) {
	sts, err := c.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return PreflightResult{Fits: true}, nil
	}
	if err != nil {
		return PreflightResult{Fits: true}, err
	}
	if len(sts.Spec.Template.Spec.Containers) == 0 {
		return PreflightResult{Fits: true}, nil
	}
	replicas := int32(1)
	if sts.Spec.Replicas != nil {
		replicas = *sts.Spec.Replicas
	}
	before := *sts.Spec.Template.Spec.Containers[0].Resources.DeepCopy()
	after := overlay(before, ch)
	return projected(ctx, c, namespace, before, after, replicas, 0, nil)
}
