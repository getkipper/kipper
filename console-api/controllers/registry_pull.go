package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/registrycred"
)

// registryPullSecretName is the per-workload image-pull Secret name. The kind
// is part of the name so same-named workloads of different kinds in one
// namespace stage independent Secrets instead of fighting over one — their
// images can need different registries.
func registryPullSecretName(owner client.Object) string {
	kind := "workload"
	switch owner.(type) {
	case *kipperv1.App:
		kind = "app"
	case *kipperv1.Function:
		kind = "function"
	case *kipperv1.Job:
		kind = "job"
	}
	return fmt.Sprintf("%s-%s-registry-pull", owner.GetName(), kind)
}

// ensureImagePullSecret stages a scoped image-pull Secret for a workload whose
// image is a private third-party registry configured in kipper-registries, and
// returns the imagePullSecrets refs to attach to the Pod. The Secret carries
// only the single registry the image pulls from, is staged only when the
// workload's project is on the credential's allow-list, is owned by the
// workload (so it is garbage-collected with it), and is created in the
// workload's own namespace on demand — never fanned out across namespaces. A
// public image, the cluster registry (which the node trusts via the k3s
// registries mirror), or a project without a grant needs no credential, so a
// stale pull Secret is removed and nil returned.
//
// A read or stage failure returns an error so the caller aborts the reconcile
// and retries with backoff — a transient failure must never strip a working
// pull Secret from the pod template.
func ensureImagePullSecret(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, image string) ([]corev1.LocalObjectReference, error) {
	name := registryPullSecretName(owner)

	if registrycred.IsClusterRegistryImage(image) {
		if err := validateClusterImage(ctx, c, owner, image); err != nil {
			return nil, err
		}
		return nil, deleteRegistryPullSecret(ctx, c, owner.GetNamespace(), name)
	}

	entries, err := registrycred.LoadCR(ctx, c)
	if err != nil {
		return nil, fmt.Errorf("reading registry credentials for %s: %w", owner.GetName(), err)
	}
	if len(entries) == 0 {
		return nil, deleteRegistryPullSecret(ctx, c, owner.GetNamespace(), name)
	}
	project, err := workloadProject(ctx, c, owner.GetNamespace())
	if err != nil {
		return nil, err
	}
	entry := registrycred.FindForProject(entries, image, project)
	if entry == nil {
		return nil, deleteRegistryPullSecret(ctx, c, owner.GetNamespace(), name)
	}
	dockercfg, err := entry.DockerConfigJSON()
	if err != nil {
		return nil, fmt.Errorf("rendering image pull secret for %s: %w", owner.GetName(), err)
	}
	if err := upsertPullSecret(ctx, c, scheme, owner, name, dockercfg); err != nil {
		return nil, fmt.Errorf("staging image pull secret for %s: %w", owner.GetName(), err)
	}
	return []corev1.LocalObjectReference{{Name: name}}, nil
}

// validateClusterImage rejects a cluster-registry image that does not belong
// to the workload's own project. Builds push to <namespace>/<app> and the
// nodes pull the cluster registry with a node-wide identity (the k3s
// registries mirror), so without this check a hand-written workload CR could
// run — and thereby read — another project's image. A different namespace is
// allowed only when it belongs to the same project (an image promoted across
// the project's environments); anything unresolvable fails closed.
func validateClusterImage(ctx context.Context, c client.Client, owner client.Object, image string) error {
	imageNS := registrycred.ClusterImageNamespace(image)
	if imageNS == "" {
		return fmt.Errorf("cluster-registry image %q for %s has no namespace component", image, owner.GetName())
	}
	if imageNS == owner.GetNamespace() {
		return nil
	}
	ownProject, err := workloadProject(ctx, c, owner.GetNamespace())
	if err != nil {
		return err
	}
	imageProject, err := workloadProject(ctx, c, imageNS)
	if err != nil {
		return err
	}
	if ownProject == "" || imageProject != ownProject {
		return fmt.Errorf("cluster-registry image %q does not belong to the project of %s/%s", image, owner.GetNamespace(), owner.GetName())
	}
	return nil
}

// workloadProject returns the project a workload's namespace belongs to, from
// the controller-owned kipper.run/project label, or "" when the namespace is
// definitively not a managed project namespace — a tenant cannot label a
// namespace, so the result is never tenant-influenced. Only a read failure is
// an error, so a transient one is retried rather than mistaken for "no
// project".
func workloadProject(ctx context.Context, c client.Client, namespace string) (string, error) {
	var ns corev1.Namespace
	if err := c.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return "", fmt.Errorf("resolving project for namespace %s: %w", namespace, err)
	}
	if ns.Labels["app.kubernetes.io/managed-by"] != "kipper" {
		return "", nil
	}
	return ns.Labels["kipper.run/project"], nil
}

func upsertPullSecret(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, name string, dockercfg []byte) error {
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: owner.GetNamespace(),
			// kipper.run/registry marks this as a registry pull secret so the rest
			// of the system (env copy, project export) treats it as such rather
			// than as an app env Secret.
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "kipper",
				"kipper.run/registry":          "true",
			},
		},
		Type: corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{".dockerconfigjson": dockercfg},
	}
	if err := controllerutil.SetOwnerReference(owner, desired, scheme); err != nil {
		return err
	}

	var existing corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: owner.GetNamespace(), Name: name}, &existing)
	if k8serrors.IsNotFound(err) {
		return c.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	// A Secret's Type is immutable, so an existing Secret of a different type is
	// replaced rather than updated.
	if existing.Type != corev1.SecretTypeDockerConfigJson {
		if err := c.Delete(ctx, &existing); err != nil {
			return err
		}
		return c.Create(ctx, desired)
	}
	existing.Data = desired.Data
	existing.OwnerReferences = desired.OwnerReferences
	if existing.Labels == nil {
		existing.Labels = map[string]string{}
	}
	for k, v := range desired.Labels {
		existing.Labels[k] = v
	}
	return c.Update(ctx, &existing)
}

// deleteRegistryPullSecret removes a workload's pull Secret when its image no
// longer needs a configured credential. It only deletes a Secret Kipper owns, so
// a same-named Secret a user created is left untouched. A read or delete
// failure is returned so the reconcile retries until a revoked credential is
// confirmed gone rather than silently abandoning the cleanup.
func deleteRegistryPullSecret(ctx context.Context, c client.Client, namespace, name string) error {
	var existing corev1.Secret
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &existing)
	if k8serrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading stale pull secret %s/%s: %w", namespace, name, err)
	}
	if existing.Labels["app.kubernetes.io/managed-by"] != "kipper" {
		return nil
	}
	if err := c.Delete(ctx, &existing); err != nil && !k8serrors.IsNotFound(err) {
		return fmt.Errorf("deleting stale pull secret %s/%s: %w", namespace, name, err)
	}
	return nil
}

// StageFunctionPullSecret stages the image-pull Secret for the image a
// Function's container runs and returns the refs to attach to the Pod, for
// callers outside the reconcile loop (the test-run handler) that create Pods
// referencing it. Staging and reference come from one credential read, so the
// Pod spec can never make a second, different authorization decision than the
// staging did.
func StageFunctionPullSecret(ctx context.Context, c client.Client, fn *kipperv1.Function) ([]corev1.LocalObjectReference, error) {
	return ensureImagePullSecret(ctx, c, c.Scheme(), fn, effectiveFunctionImage(fn))
}

// enqueueForRegistryCredentials maps a change of the central registry-credential
// list to a reconcile of every App, so a rotation, grant, or revocation reaches
// the staged pull Secrets without waiting for an unrelated App event. A
// revocation in particular is an authorization change and must not stay staged
// indefinitely.
func (r *AppReconciler) enqueueForRegistryCredentials(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != registrycred.Namespace || obj.GetName() != registrycred.ConfigSecretName {
		return nil
	}
	var apps kipperv1.AppList
	if err := r.List(ctx, &apps); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(apps.Items))
	for i := range apps.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: apps.Items[i].Name, Namespace: apps.Items[i].Namespace,
		}})
	}
	return reqs
}

// enqueueForRegistryCredentials is the Function twin of the App mapping above.
func (r *FunctionReconciler) enqueueForRegistryCredentials(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != registrycred.Namespace || obj.GetName() != registrycred.ConfigSecretName {
		return nil
	}
	var fns kipperv1.FunctionList
	if err := r.List(ctx, &fns); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(fns.Items))
	for i := range fns.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: fns.Items[i].Name, Namespace: fns.Items[i].Namespace,
		}})
	}
	return reqs
}

// enqueueAppsForClusterIdentity requeues every App when the ClusterIdentity
// changes. A domain override or transition changes the platform serving-host
// set, so tenant routes must be re-evaluated against it promptly: a tenant App
// serving a host the platform now claims must stop serving it, which happens on
// the App's own reconcile (hostReservedForPlatform then refuses it and its
// Ingress is torn down). If this cached List fails the event is dropped and
// eviction waits for the App's next reconcile, so this is prompt enforcement,
// not an atomic cutover.
func (r *AppReconciler) enqueueAppsForClusterIdentity(ctx context.Context, _ client.Object) []reconcile.Request {
	var apps kipperv1.AppList
	if err := r.List(ctx, &apps); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(apps.Items))
	for i := range apps.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: apps.Items[i].Name, Namespace: apps.Items[i].Namespace,
		}})
	}
	return reqs
}

// enqueueFunctionsForClusterIdentity is the Function twin of the App mapping
// above.
func (r *FunctionReconciler) enqueueFunctionsForClusterIdentity(ctx context.Context, _ client.Object) []reconcile.Request {
	var fns kipperv1.FunctionList
	if err := r.List(ctx, &fns); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(fns.Items))
	for i := range fns.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: fns.Items[i].Name, Namespace: fns.Items[i].Namespace,
		}})
	}
	return reqs
}

// enqueueForRegistryCredentials is the Job twin of the App mapping above, so a
// rotation or revocation reaches a scheduled CronJob's next run.
func (r *JobReconciler) enqueueForRegistryCredentials(ctx context.Context, obj client.Object) []reconcile.Request {
	if obj.GetNamespace() != registrycred.Namespace || obj.GetName() != registrycred.ConfigSecretName {
		return nil
	}
	var jobs kipperv1.JobList
	if err := r.List(ctx, &jobs); err != nil {
		return nil
	}
	reqs := make([]reconcile.Request, 0, len(jobs.Items))
	for i := range jobs.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
			Name: jobs.Items[i].Name, Namespace: jobs.Items[i].Namespace,
		}})
	}
	return reqs
}
