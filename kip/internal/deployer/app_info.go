package deployer

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/getkipper/kipper/controller/pkg/provenance"
)

// ResourceTuningGVR addresses the auto-sizer's per-workload records.
var ResourceTuningGVR = schema.GroupVersionResource{
	Group:    "kipper.run",
	Version:  "v1alpha1",
	Resource: "resourcetunings",
}

// AppInfo is one app's status, scaling and resource settings.
type AppInfo struct {
	Status    AppStatus
	Autoscale AutoscaleStatus
	Profile   string
	Template  TemplateState
	CPU       ResourceInfo
	Memory    ResourceInfo
}

// TemplateState says what is known about an app's pod template.
type TemplateState int

const (
	TemplateRead TemplateState = iota
	TemplateMissing
	TemplateUnreadable
)

// ResourceInfo combines spec values and ownership, pod-template values, and
// the latest sizing recommendation. Empty strings indicate unavailable values.
type ResourceInfo struct {
	Mode                                 provenance.Mode
	Request, Limit                       SpecValue
	LiveRequest, LiveLimit               string
	RecommendedRequest, RecommendedLimit string
}

// SpecValue is a resource value from an App's spec and who set it.
type SpecValue struct {
	Value  string
	Source provenance.Source
}

// ReadAppInfo combines stored app settings with observed scaling and resources.
// Deployment and tuning read failures leave their data unavailable. App and
// autoscaler errors follow [Deployer.ReadAutoscaleStatus].
func (d *Deployer) ReadAppInfo(ctx context.Context, namespace, name string) (AppInfo, error) {
	app, err := d.getApp(ctx, namespace, name)
	if err != nil {
		return AppInfo{}, err
	}
	autoscale, err := d.ReadAutoscaleStatus(ctx, namespace, name)
	if err != nil {
		return AppInfo{}, err
	}
	info := AppInfo{Status: appStatusFromCR(app), Autoscale: autoscale, Profile: "standard"}
	if p, _, _ := unstructured.NestedString(app.Object, "spec", "resources", "profile"); p != "" {
		info.Profile = p
	}

	var live corev1.ResourceRequirements
	dep, err := d.Client.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		info.Template = TemplateMissing
	case err != nil:
		info.Template = TemplateUnreadable
	case len(dep.Spec.Template.Spec.Containers) > 0:
		live = dep.Spec.Template.Spec.Containers[0].Resources
	}
	recommended := d.recommendation(ctx, namespace, name, app.GetUID())

	info.CPU = resourceInfo(app, "cpu", live, corev1.ResourceCPU, recommended)
	info.Memory = resourceInfo(app, "memory", live, corev1.ResourceMemory, recommended)
	return info, nil
}

// resourceInfo uses prefix ("cpu" or "memory") to look up spec and recommendation fields.
func resourceInfo(app *unstructured.Unstructured, prefix string, live corev1.ResourceRequirements, name corev1.ResourceName, recommended map[string]interface{}) ResourceInfo {
	specValue := func(field string) SpecValue {
		v, _, _ := unstructured.NestedString(app.Object, "spec", "resources", field)
		return SpecValue{Value: v, Source: provenance.ClassifyApp(v, app.GetManagedFields(), field)}
	}
	quantity := func(list corev1.ResourceList) string {
		if q, ok := list[name]; ok {
			return q.String()
		}
		return ""
	}
	r := ResourceInfo{
		Request:     specValue(prefix + "Request"),
		Limit:       specValue(prefix + "Limit"),
		LiveRequest: quantity(live.Requests),
		LiveLimit:   quantity(live.Limits),
	}
	r.RecommendedRequest, _ = recommended[prefix+"Request"].(string)
	r.RecommendedLimit, _ = recommended[prefix+"Limit"].(string)
	r.Mode = provenance.ModeOf(r.Request.Source, r.Limit.Source, below(r.Request.Value, r.Limit.Value))
	return r
}

// below reports whether both quantities are valid and a is lower than b.
func below(a, b string) bool {
	qa, errA := resource.ParseQuantity(a)
	qb, errB := resource.ParseQuantity(b)
	return errA == nil && errB == nil && qa.Cmp(qb) < 0
}

// recommendation reads optional sizing data owned by this app's UID, so a
// recreated app cannot inherit its predecessor's recommendation. Read errors
// and missing or malformed recommendation maps return nil.
func (d *Deployer) recommendation(ctx context.Context, namespace, name string, appUID types.UID) map[string]interface{} {
	tuning, err := d.Dynamic.Resource(ResourceTuningGVR).Namespace(namespace).Get(ctx, "app-"+name, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	owner := metav1.GetControllerOf(tuning)
	if owner == nil || owner.UID != appUID {
		return nil
	}
	rec, _, _ := unstructured.NestedMap(tuning.Object, "status", "recommendation")
	return rec
}
