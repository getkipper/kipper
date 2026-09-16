package controllers

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// resourceTypeLabel distinguishes the children of the three workload kinds,
// which are otherwise labelled identically and may share a name in one
// namespace.
const resourceTypeLabel = "kipper.run/resource-type"

// workloadOwner is a workload whose children the provenance rule governs.
//
// The rule is the same for an App, a Function and a Job, and it must stay the
// same: three copies of it drift, and a drifted provenance rule either strands
// an object or deletes somebody else's. What differs between them is only the
// kind a controller reference names and the label their children carry.
type workloadOwner struct {
	obj          client.Object
	kind         string
	resourceType string
}

func appOwner(app *kipperv1.App) workloadOwner {
	return workloadOwner{obj: app, kind: "App", resourceType: "app"}
}

func functionOwner(fn *kipperv1.Function) workloadOwner {
	return workloadOwner{obj: fn, kind: "Function", resourceType: "function"}
}

func jobOwner(job *kipperv1.Job) workloadOwner {
	return workloadOwner{obj: job, kind: "Job", resourceType: "job"}
}

// childProvenance says whether obj is this workload's to write or to delete,
// and when it is not, why.
//
// A controller reference is the strong answer, compared on group, kind and name
// as well as UID because that is what SetControllerReference itself compares.
// An object nothing controls is this workload's only when Kipper's own labels
// say it created it for this workload.
func childProvenance(obj client.Object, owner workloadOwner) (bool, string) {
	if ref := metav1.GetControllerOf(obj); ref != nil {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return false, fmt.Sprintf("carries an unreadable controller reference %q", ref.APIVersion)
		}
		// The group is compared rather than the whole apiVersion, because that
		// is the level SetControllerReference itself compares at: a reference
		// written under another served version of the same CRD still names
		// this workload.
		if ref.UID == owner.obj.GetUID() && ref.Kind == owner.kind &&
			ref.Name == owner.obj.GetName() && gv.Group == kipperv1.GroupVersion.Group {
			return true, ""
		}
		return false, fmt.Sprintf("is controlled by %s %q, not by this %s",
			ref.Kind, ref.Name, owner.kind)
	}

	l := obj.GetLabels()
	if l[kipperLabel] != kipperValue || l["app"] != owner.obj.GetName() {
		return false, "was not created by Kipper"
	}
	// An App, a Function and a Job may share a name in one namespace and their
	// children carry the same two labels. The resource-type label is what tells
	// them apart, as it does for the CLI's workloadDeployment.
	//
	// An absent label is accepted rather than refused, because it is what an
	// object created before this label existed looks like, and refusing those
	// would leave every one of them stranded — unadoptable and so never
	// garbage-collected, which is the defect this exists to fix. Adoption
	// writes a controller reference, so the label is only ever consulted until
	// the first successful pass.
	if t := l[resourceTypeLabel]; t != "" && t != owner.resourceType {
		return false, fmt.Sprintf("belongs to a %s, not to this %s", t, owner.resourceType)
	}
	return true, ""
}

// JobOwnsChild reports whether obj is the child this Job's reconciler builds,
// rather than another workload's object sitting on the same key. A Function's
// cron trigger creates a CronJob named after the function with "-cron" on the
// end, which is a name a Job CR can also have, so the managed-by label alone
// does not separate them.
func JobOwnsChild(obj client.Object, job *kipperv1.Job) bool {
	owned, _ := childProvenance(obj, jobOwner(job))
	return owned
}

// adoptChild validates Kipper provenance before setting the controller reference.
// Reassert ownership on each reconcile to repair lost references and keep
// garbage collection working. Name alone is insufficient evidence of ownership.
func adoptChild(kind string, obj client.Object, owner workloadOwner, scheme *runtime.Scheme) error {
	if ok, why := childProvenance(obj, owner); !ok {
		return fmt.Errorf("%s %q in %s %s; rename the %s or remove that object",
			kind, obj.GetName(), obj.GetNamespace(), why, owner.resourceType)
	}
	return controllerutil.SetControllerReference(owner.obj, obj, scheme)
}

// withResourceType returns labels marked as belonging to this kind of workload.
//
// It goes on the object's own metadata rather than on a pod template, because
// that is where childProvenance reads it and no pod has any use for it. Putting
// it on the template would roll every workload on the fleet once to acquire a
// label nothing running reads.
func withResourceType(labels map[string]string, resourceType string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	out[resourceTypeLabel] = resourceType
	return out
}

// ownedByWorkload is the delete-side question, and it is the same question.
// Switching a feature off must not destroy an object that switching it on would
// have refused to touch.
func ownedByWorkload(obj client.Object, owner workloadOwner) bool {
	ok, _ := childProvenance(obj, owner)
	return ok
}
