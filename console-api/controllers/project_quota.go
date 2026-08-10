package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

const (
	projectLimitRangeName = kipperv1.ProjectLimitRangeName
	projectQuotaName      = kipperv1.ProjectQuotaName
)

// quotaValues aliases the shared parsed-quota type; tier defaults and
// parsing live in the API package so the console handlers use the same
// values and validation.
type quotaValues = kipperv1.EnvQuotaValues

// reconcileQuota manages the LimitRange and ResourceQuota of one environment
// namespace. The LimitRange is applied first so that by the time a quota
// exists, any pod missing requests or limits gets defaults at admission
// instead of being rejected.
//
// A tierless project (empty tier) gets neither object unless the environment
// carries an explicit override: the out-of-the-box state is cluster-wide
// limits only, and a tier is the opt-in for managed capacity. The removal
// branch also covers a project transitioning from a tier to tierless.
func (r *ProjectReconciler) reconcileQuota(ctx context.Context, project *kipperv1.Project, env kipperv1.ProjectEnvironment, ns string) error {
	if project.Spec.Tier == "" && env.Quota == nil {
		return r.removeQuotaObjects(ctx, ns)
	}

	if err := r.reconcileLimitRange(ctx, project, env.Name, ns); err != nil {
		return fmt.Errorf("reconciling limit range: %w", err)
	}

	quota := env.Quota
	if quota == nil {
		q := kipperv1.TierQuota(project.Spec.Tier)
		quota = &q
	}
	values, err := quota.Parsed()
	if err != nil {
		return err
	}

	var existing corev1.ResourceQuota
	err = r.Get(ctx, types.NamespacedName{Name: projectQuotaName, Namespace: ns}, &existing)
	if errors.IsNotFound(err) {
		// First quota on this namespace. If workloads already exceed the
		// tier default, capping at the default would not evict anything but
		// would block the next deploy with an opaque admission error.
		if env.Quota == nil {
			grandfathered, changed, gerr := r.grandfatherQuota(ctx, project, env.Name, ns, values)
			if gerr != nil {
				return fmt.Errorf("grandfathering quota: %w", gerr)
			}
			if changed {
				values = grandfathered
			}
		}
		return r.Create(ctx, r.buildResourceQuota(project, env.Name, ns, values))
	}
	if err != nil {
		return err
	}

	existing.Spec.Hard = quotaHard(values)
	return r.Update(ctx, &existing)
}

// removeQuotaObjects deletes the managed ResourceQuota and LimitRange of a
// tierless environment, so removing a project's tier actually lifts its caps.
func (r *ProjectReconciler) removeQuotaObjects(ctx context.Context, ns string) error {
	quota := &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: projectQuotaName, Namespace: ns}}
	if err := r.Delete(ctx, quota); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("removing resource quota: %w", err)
	}
	lr := &corev1.LimitRange{ObjectMeta: metav1.ObjectMeta{Name: projectLimitRangeName, Namespace: ns}}
	if err := r.Delete(ctx, lr); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("removing limit range: %w", err)
	}
	return nil
}

// reconcileLimitRange applies small per-container defaults so pods that omit
// requests or limits are filled in at admission rather than rejected once a
// ResourceQuota exists. Kipper-created pods all declare resources explicitly
// (enforced by a guard test), so these defaults exist for user-supplied pods
// and future code paths.
func (r *ProjectReconciler) reconcileLimitRange(ctx context.Context, project *kipperv1.Project, envName, ns string) error {
	desired := &corev1.LimitRange{
		ObjectMeta: metav1.ObjectMeta{
			Name:      projectLimitRangeName,
			Namespace: ns,
			Labels:    quotaLabels(project.Name, envName),
		},
		Spec: corev1.LimitRangeSpec{
			Limits: []corev1.LimitRangeItem{
				{
					Type: corev1.LimitTypeContainer,
					DefaultRequest: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("25m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Default: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
				},
			},
		},
	}

	var existing corev1.LimitRange
	err := r.Get(ctx, types.NamespacedName{Name: projectLimitRangeName, Namespace: ns}, &existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, desired)
	}
	if err != nil {
		return err
	}

	existing.Spec = desired.Spec
	return r.Update(ctx, &existing)
}

// grandfatherQuota measures current namespace usage and, when any dimension
// exceeds the tier default, raises the quota to the measured usage plus
// headroom and records that as an explicit EnvQuota override on the
// environment. The override makes the raised cap visible and editable
// instead of silently diverging from the tier.
func (r *ProjectReconciler) grandfatherQuota(ctx context.Context, project *kipperv1.Project, envName, ns string, tier quotaValues) (quotaValues, bool, error) {
	used, err := r.measureNamespaceUsage(ctx, ns)
	if err != nil {
		return quotaValues{}, false, err
	}

	over := used.CPURequest.Cmp(tier.CPURequest) > 0 ||
		used.CPULimit.Cmp(tier.CPULimit) > 0 ||
		used.MemoryRequest.Cmp(tier.MemoryRequest) > 0 ||
		used.MemoryLimit.Cmp(tier.MemoryLimit) > 0
	if !over {
		return tier, false, nil
	}

	raised := quotaValues{
		CPURequest:    maxQuantity(tier.CPURequest, cpuWithHeadroom(used.CPURequest)),
		CPULimit:      maxQuantity(tier.CPULimit, cpuWithHeadroom(used.CPULimit)),
		MemoryRequest: maxQuantity(tier.MemoryRequest, memoryWithHeadroom(used.MemoryRequest)),
		MemoryLimit:   maxQuantity(tier.MemoryLimit, memoryWithHeadroom(used.MemoryLimit)),
	}

	override := &kipperv1.EnvQuota{
		CPURequest:    raised.CPURequest.String(),
		CPULimit:      raised.CPULimit.String(),
		MemoryRequest: raised.MemoryRequest.String(),
		MemoryLimit:   raised.MemoryLimit.String(),
	}

	found := false
	for i := range project.Spec.Environments {
		if project.Spec.Environments[i].Name == envName {
			project.Spec.Environments[i].Quota = override
			found = true
			break
		}
	}
	if !found {
		// The environment list can be empty when the project runs on the
		// defaulted single environment.
		project.Spec.Environments = append(project.Spec.Environments,
			kipperv1.ProjectEnvironment{Name: envName, Quota: override})
	}
	if err := r.Update(ctx, project); err != nil {
		return quotaValues{}, false, fmt.Errorf("persisting quota override: %w", err)
	}
	return raised, true, nil
}

// measureNamespaceUsage sums requests and limits over non-terminal pods the
// same way the Kubernetes quota controller does: app containers are summed,
// and the pod's effective value is the maximum of that sum and the largest
// init container.
func (r *ProjectReconciler) measureNamespaceUsage(ctx context.Context, ns string) (quotaValues, error) {
	var pods corev1.PodList
	if err := r.APIReader.List(ctx, &pods, client.InNamespace(ns)); err != nil {
		return quotaValues{}, err
	}

	var used quotaValues
	for _, pod := range pods.Items {
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		used.CPURequest.Add(podEffective(&pod, corev1.ResourceCPU, false))
		used.CPULimit.Add(podEffective(&pod, corev1.ResourceCPU, true))
		used.MemoryRequest.Add(podEffective(&pod, corev1.ResourceMemory, false))
		used.MemoryLimit.Add(podEffective(&pod, corev1.ResourceMemory, true))
	}
	return used, nil
}

// podEffective returns a pod's effective request or limit for one resource:
// max(sum of app containers, max of init containers).
func podEffective(pod *corev1.Pod, name corev1.ResourceName, limits bool) resource.Quantity {
	var sum resource.Quantity
	for _, c := range pod.Spec.Containers {
		sum.Add(containerValue(c, name, limits))
	}
	var initMax resource.Quantity
	for _, c := range pod.Spec.InitContainers {
		if v := containerValue(c, name, limits); v.Cmp(initMax) > 0 {
			initMax = v
		}
	}
	return maxQuantity(sum, initMax)
}

func containerValue(c corev1.Container, name corev1.ResourceName, limits bool) resource.Quantity {
	list := c.Resources.Requests
	if limits {
		list = c.Resources.Limits
	}
	if v, ok := list[name]; ok {
		return v
	}
	return resource.Quantity{}
}

// cpuWithHeadroom adds 25% to a measured CPU value and rounds up to the next
// 500m, so a grandfathered cap leaves room for a rolling-update surge pod.
func cpuWithHeadroom(q resource.Quantity) resource.Quantity {
	milli := q.MilliValue() * 5 / 4
	milli = (milli + 499) / 500 * 500
	return *resource.NewMilliQuantity(milli, resource.DecimalSI)
}

// memoryWithHeadroom adds 25% to a measured memory value and rounds up to
// the next 512Mi.
func memoryWithHeadroom(q resource.Quantity) resource.Quantity {
	const step = 512 << 20
	bytes := q.Value() + q.Value()/4
	bytes = (bytes + step - 1) / step * step
	return *resource.NewQuantity(bytes, resource.BinarySI)
}

func maxQuantity(a, b resource.Quantity) resource.Quantity {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

func (r *ProjectReconciler) buildResourceQuota(project *kipperv1.Project, envName, ns string, v quotaValues) *corev1.ResourceQuota {
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:      projectQuotaName,
			Namespace: ns,
			Labels:    quotaLabels(project.Name, envName),
		},
		Spec: corev1.ResourceQuotaSpec{Hard: quotaHard(v)},
	}
}

func quotaHard(v quotaValues) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceRequestsCPU:    v.CPURequest,
		corev1.ResourceRequestsMemory: v.MemoryRequest,
		corev1.ResourceLimitsCPU:      v.CPULimit,
		corev1.ResourceLimitsMemory:   v.MemoryLimit,
	}
}

func quotaLabels(projectName, envName string) map[string]string {
	return map[string]string{
		kipperLabel:              kipperValue,
		kipperlabels.Project:     projectName,
		kipperlabels.Environment: envName,
	}
}
