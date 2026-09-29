package handlers

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
)

// resourceDetail describes one resource (CPU or memory) of a workload.
type resourceDetail struct {
	// Mode is how Kipper sizes it: automatic, bounded, fixed or held.
	Mode string `json:"mode"`
	// Request and Limit are the spec's values and who set each.
	Request resourceValue `json:"request"`
	Limit   resourceValue `json:"limit"`
	// Live is the allocation in the workload's current pod template.
	Live resourcePair `json:"live"`
	// Recommended is the auto-sizer's current recommendation, if any.
	Recommended *resourcePair `json:"recommended,omitempty"`
	// Pending means the pod template's allocation does not yet satisfy the spec.
	// It does not track pod rollout readiness.
	Pending bool `json:"pending"`
}

// resourceValue is a spec value and who set it: user, automatic, held or unset.
type resourceValue struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

type resourcePair struct {
	Request string `json:"request"`
	Limit   string `json:"limit"`
}

// describeResources builds the per-resource details from the owner's
// classified spec, the live container and the auto-sizer's recommendation.
func describeResources(spec resourcebounds.Spec, live *corev1.ResourceRequirements, rec *kipperv1.TunedResources) (cpu, memory *resourceDetail) {
	cpu = describeResource(spec.CPURequest, spec.CPULimit, live, corev1.ResourceCPU)
	memory = describeResource(spec.MemoryRequest, spec.MemoryLimit, live, corev1.ResourceMemory)
	cpu.Pending, memory.Pending = pending(spec, live, corev1.ResourceCPU), pending(spec, live, corev1.ResourceMemory)
	if rec != nil {
		if rec.CPURequest != "" {
			cpu.Recommended = &resourcePair{Request: rec.CPURequest, Limit: rec.CPULimit}
		}
		if rec.MemoryRequest != "" {
			memory.Recommended = &resourcePair{Request: rec.MemoryRequest, Limit: rec.MemoryLimit}
		}
	}
	return cpu, memory
}

func describeResource(request, limit resourcebounds.Quantity, live *corev1.ResourceRequirements, name corev1.ResourceName) *resourceDetail {
	d := &resourceDetail{
		Mode:    resourcebounds.ModeOf(request, limit).String(),
		Request: valueOf(request),
		Limit:   valueOf(limit),
	}
	// Read the container literally: a request with no limit has no cap.
	if live != nil {
		if q, ok := live.Requests[name]; ok {
			d.Live.Request = q.String()
		}
		if q, ok := live.Limits[name]; ok {
			d.Live.Limit = q.String()
		}
	}
	return d
}

// pending compares the pod template with the spec resolved without a
// recommendation. Automatic values and requests already within bounds satisfy
// the spec; pod readiness is checked separately.
func pending(spec resourcebounds.Spec, live *corev1.ResourceRequirements, name corev1.ResourceName) bool {
	want := &corev1.ResourceRequirements{}
	if live != nil {
		want = live.DeepCopy()
	}
	resourcebounds.Apply(want, spec, nil, live)
	w, _ := resourcebounds.PairOf(want, name)
	l, _ := resourcebounds.PairOf(live, name)
	return w.Request.Cmp(l.Request) != 0 || w.Limit.Cmp(l.Limit) != 0
}

func valueOf(q resourcebounds.Quantity) resourceValue {
	if q.Source == resourcebounds.Unset {
		return resourceValue{Source: q.Source.String()}
	}
	return resourceValue{Value: q.Value.String(), Source: q.Source.String()}
}

// tuningRecommendation reads the auto-sizer's recommendation for an App,
// Service or Function, nil when there is none, it cannot be read, or the
// record belongs to an earlier owner of the same name.
func tuningRecommendation(ctx context.Context, c crclient.Reader, kind string, owner metav1.Object) *kipperv1.TunedResources {
	var rt kipperv1.ResourceTuning
	key := crclient.ObjectKey{Namespace: owner.GetNamespace(), Name: resourcebounds.TuningName(kind, owner.GetName())}
	if err := c.Get(ctx, key, &rt); err != nil || !resourcebounds.TuningBelongsTo(&rt, owner.GetUID()) {
		return nil
	}
	return &rt.Status.Recommendation
}

// describeWorkload adds App or Function details from the spec and Deployment
// pod template. Failed reads omit the details; a missing Deployment means
// the workload has no live allocation yet.
func (res *Resources) describeWorkload(ctx context.Context, project, name string, kind ResourceKind, resp *resourcesResponse) {
	var spec resourcebounds.Spec
	var owner metav1.Object
	var err error
	crKind := "App"
	switch kind {
	case ResourceKindApp:
		var app kipperv1.App
		if err = res.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: name}, &app); err != nil {
			return
		}
		owner = &app
		spec, err = resourcebounds.AppSpec(&app)
	case ResourceKindFunction:
		crKind = "Function"
		var fn kipperv1.Function
		if err = res.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: name}, &fn); err != nil {
			return
		}
		owner = &fn
		r := fn.Spec.Resources
		spec, err = resourcebounds.OwnedSpec(r.CPURequest, r.CPULimit, r.MemoryRequest, r.MemoryLimit)
	default:
		return
	}
	if err != nil {
		return
	}
	var live *corev1.ResourceRequirements
	deploy, err := res.Client.AppsV1().Deployments(project).Get(ctx, name, metav1.GetOptions{})
	switch {
	case err == nil && len(deploy.Spec.Template.Spec.Containers) > 0:
		live = &deploy.Spec.Template.Spec.Containers[0].Resources
	case err != nil && !apierrors.IsNotFound(err):
		// A failed read cannot establish the container's allocation.
		return
	}
	resp.CPU, resp.Memory = describeResources(spec, live, tuningRecommendation(ctx, res.CRClient, crKind, owner))
}
