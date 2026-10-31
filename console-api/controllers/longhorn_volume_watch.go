package controllers

import (
	"context"
	"strings"

	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/getkipper/kipper/console-api/internal/nsowner"
)

// watchLonghornVolumes registers a Longhorn Volume watch that enqueues requests
// from toOwner and returns true. Registration requires the Volume kind and list
// and watch permissions at startup. If either check fails, it returns the builder
// unchanged and false to avoid blocking the manager on an informer that cannot
// sync. Installing Longhorn later requires a console-api restart to enable the
// watch. All controllers that register the watch share one informer.
func watchLonghornVolumes(mgr ctrl.Manager, b *builder.Builder, toOwner handler.MapFunc) (*builder.Builder, bool) {
	if _, err := mgr.GetRESTMapper().RESTMapping(longhornVolumeKind.GroupKind(), longhornVolumeKind.Version); err != nil {
		ctrl.Log.Info("Longhorn Volume discovery failed. The volume watch is disabled.", "reason", err.Error())
		return b, false
	}
	if !mayListAndWatch(mgr, longhornVolumeKind.Group, "volumes") {
		ctrl.Log.Info("Could not confirm list and watch permissions for Longhorn volumes. The volume watch is disabled.")
		return b, false
	}
	volume := &unstructured.Unstructured{}
	volume.SetGroupVersionKind(longhornVolumeKind)
	return b.Watches(volume, handler.EnqueueRequestsFromMapFunc(toOwner), builder.WithPredicates(longhornVolumeChanged)), true
}

// mayListAndWatch asks the API server whether console-api may list and watch
// resource in group. Client creation or access review failures return false.
func mayListAndWatch(mgr ctrl.Manager, group, resource string) bool {
	clientset, err := kubernetes.NewForConfig(mgr.GetConfig())
	if err != nil {
		return false
	}
	for _, verb := range []string{"list", "watch"} {
		review := &authorizationv1.SelfSubjectAccessReview{Spec: authorizationv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authorizationv1.ResourceAttributes{Group: group, Resource: resource, Verb: verb},
		}}
		got, err := clientset.AuthorizationV1().SelfSubjectAccessReviews().Create(context.Background(), review, metav1.CreateOptions{})
		if err != nil || !got.Status.Allowed {
			return false
		}
	}
	return true
}

// longhornVolumeChanged passes create, delete, and generic events, and updates
// to fields selected by watchedVolumeFields. Filtering size and timestamp
// updates avoids reconciliation on routine volume activity. The conversion
// timer refreshes the remaining inputs.
var longhornVolumeChanged = predicate.Funcs{
	UpdateFunc: func(e event.UpdateEvent) bool {
		return !equality.Semantic.DeepEqual(watchedVolumeFields(e.ObjectOld), watchedVolumeFields(e.ObjectNew))
	},
}

// watchedVolumeFields returns the fields of a Longhorn Volume that wake its
// owner, with each condition reduced to its type, status and reason.
func watchedVolumeFields(obj client.Object) []any {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return nil
	}
	field := func(path ...string) any {
		v, _, _ := unstructured.NestedFieldNoCopy(u.Object, path...)
		return v
	}
	fields := []any{
		field("spec", "numberOfReplicas"),
		field("status", "state"),
		field("status", "robustness"),
		field("status", "currentNodeID"),
		field("status", "currentImage"),
		field("status", "kubernetesStatus", "namespace"),
		field("status", "kubernetesStatus", "pvcName"),
	}
	conditions, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conditions {
		m, _ := c.(map[string]any)
		fields = append(fields, m["type"], m["status"], m["reason"])
	}
	return fields
}

// longhornVolumeClaim returns the namespace and name of the claim a Longhorn
// Volume serves. Each missing or malformed field returns an empty string.
// Objects of other types return two empty strings.
func longhornVolumeClaim(obj client.Object) (namespace, claim string) {
	u, ok := obj.(*unstructured.Unstructured)
	if !ok {
		return "", ""
	}
	namespace, _, _ = unstructured.NestedString(u.Object, "status", "kubernetesStatus", "namespace")
	claim, _, _ = unstructured.NestedString(u.Object, "status", "kubernetesStatus", "pvcName")
	return namespace, claim
}

// serviceForLonghornVolume maps a Longhorn Volume to the Service whose data
// claim (data-<service>-0) or dump claim (<service>-dump) it serves. Other claims
// matching these patterns can enqueue a nonexistent Service, which reconciliation
// safely ignores.
func serviceForLonghornVolume(_ context.Context, obj client.Object) []reconcile.Request {
	namespace, claim := longhornVolumeClaim(obj)
	name, ok := strings.CutSuffix(claim, "-dump")
	if rest, data := strings.CutPrefix(claim, "data-"); data && strings.HasSuffix(rest, "-0") {
		name, ok = strings.TrimSuffix(rest, "-0"), true
	}
	if namespace == "" || !ok || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}

// volumeForLonghornVolume maps a Longhorn Volume to the Kipper Volume whose
// shared-<volume> claim it serves. A Volume named uploads shares its claim
// name with a project's shared uploads, so that claim wakes both.
func volumeForLonghornVolume(_ context.Context, obj client.Object) []reconcile.Request {
	namespace, claim := longhornVolumeClaim(obj)
	name, ok := strings.CutPrefix(claim, "shared-")
	if namespace == "" || !ok || name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: namespace, Name: name}}}
}

// projectForLonghornVolume maps a Longhorn Volume serving a shared uploads
// claim to the Project that owns its namespace, as nsowner resolves it.
// Unresolved ownership or a failed read produces no requests.
func projectForLonghornVolume(reader client.Reader) handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		namespace, claim := longhornVolumeClaim(obj)
		if namespace == "" || claim != sharedUploadsClaim {
			return nil
		}
		project, ok, err := nsowner.Of(ctx, reader, namespace)
		if err != nil || !ok {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: project}}}
	}
}
