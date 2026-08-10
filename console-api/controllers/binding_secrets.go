package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// derivedBindingLabel marks a Secret this controller rendered from a service's
// shared credentials, as opposed to the shared credentials themselves. It is
// what tells the two apart structurally, so nothing has to guess from the name.
//
// The string lives in controller/pkg/labels because the handlers and the
// migration path select on it too, and three spellings of one label is three
// chances for a selector to quietly match nothing.
const derivedBindingLabel = labels.Binding

// bindingSecretName is the Secret a binding injects into its workload.
//
// A binding that pins a logical namespace gets a Secret of its own, derived
// from the service's shared credentials with that one key overridden. Every
// other binding reads the shared credentials directly, so there is nothing to
// derive and nothing to keep in step.
//
// The derived name carries the workload kind: an App and a Function may share a
// name in one namespace, and without it both would bind to one object and the
// second reconciler to run would overwrite the first's database name.
//
// The service type decides this as much as the binding does, and leaving it out
// was a defect: a `database` on a service type that has no logical namespace —
// redis, mailhog — named a derived Secret that reconcileBindingSecrets never
// renders, because it takes the shared-credentials branch for exactly those
// types. The pod then referenced an object nothing creates and the binding was
// refused. Both now ask the same question.
//
// A type that could not be read falls back to what the binding says, which is
// how this behaved before the type was consulted at all. Inside a reconcile it
// cannot happen — a binding naming a service that will not read fails earlier.
func bindingSecretName(b kipperv1.ServiceBinding, svcType string, typeKnown bool, kind secretname.Kind, workloadName string) string {
	return BindingSecretName(b, svcType, typeKnown, kind, workloadName)
}

// BindingSecretName is bindingSecretName for the handlers, which answer for the
// same pod and so must not decide this differently.
func BindingSecretName(b kipperv1.ServiceBinding, svcType string, typeKnown bool, kind secretname.Kind, workloadName string) string {
	if bindingIsDerived(b, svcType, typeKnown) {
		return secretname.Binding(b.Name, kind, workloadName)
	}
	return secretname.ServiceCredentials(b.Name)
}

// bindingIsDerived reports whether a binding gets a Secret of its own rather
// than reading the service's shared credentials.
//
// One answer, because three places act on it and they must agree: the name the
// pod references, whether reconcileBindingSecrets renders anything, and whose
// ownership the injection gate demands. When they disagreed, a `database` on a
// service type with no logical namespace named a Secret nothing renders and was
// then refused for not being owned by the workload — so the binding vanished
// from the pod.
func bindingIsDerived(b kipperv1.ServiceBinding, svcType string, typeKnown bool) bool {
	return b.Database != "" && (!typeKnown || kipperv1.HasLogicalNamespace(svcType))
}

// reconcileBindingSecrets renders the per-binding credentials Secret for every
// binding that pins a logical namespace, projecting the service's shared
// credentials with that binding's database or vhost substituted in.
//
// The bind handler used to write this object once, when the binding was
// created, and nothing revisited it. A service password rotated afterwards left
// every bound workload holding the old one until someone re-bound by hand.
// Deriving it on each pass makes the shared credentials the single source and
// this Secret a projection, which is what lets `${DB_PASSWORD}` resolve to a
// credential that still works.
//
// A declared binding that cannot be rendered fails the reconcile. Skipping it
// would have the injection gate refuse it moments later, and the pod template
// would then be rewritten without credentials the workload is already running
// with — the envFrom is optional, so the pod would start and fail on its first
// connection instead. Stopping here keeps the last working template while the
// cause is reported and the reconcile retries.
func reconcileBindingSecrets(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, kind secretname.Kind, bindings []kipperv1.ServiceBinding) (renderedBindings, map[string]bool, string, error) {
	desired := map[string]bool{}
	rendered := renderedBindings{}
	digest := sha256.New()
	sources := 0

	for i, b := range bindings {
		name := secretname.Binding(b.Name, kind, owner.GetName())

		if b.Database != "" {
			desired[name] = true
		}

		var svc kipperv1.Service
		if err := c.Get(ctx, types.NamespacedName{Name: b.Name, Namespace: owner.GetNamespace()}, &svc); err != nil {
			if errors.IsNotFound(err) {
				return nil, nil, "", fmt.Errorf("binding %q names service %q, which does not exist: unbind it or restore the service", b.Name, b.Name)
			}
			return nil, nil, "", fmt.Errorf("reading service %q: %w", b.Name, err)
		}
		shared, err := sharedServiceCredentials(ctx, c, &svc)
		if err != nil {
			return nil, nil, "", err
		}
		if shared == nil {
			return nil, nil, "", fmt.Errorf("binding %q has no usable credentials for service %q: its %s Secret is missing or is not owned by that service",
				b.Name, b.Name, secretname.ServiceCredentials(b.Name))
		}

		// A binding that pins no logical namespace reads the shared credentials
		// straight through envFrom. There is nothing to derive, but its pods
		// hold that password just the same, so it still has to reach the digest
		// — otherwise a rotation rolls the workloads that pinned a database and
		// silently leaves every other bound workload authenticating with a
		// password the service has stopped accepting.
		if !bindingIsDerived(b, svc.Spec.Type, true) {
			delete(desired, name)
			hashBindingShape(digest, i, b, svc.Spec.Type)
			hashInto(digest, shared.Name, shared.Data)
			rendered.keep(shared.Name, shared.Data)
			sources++
			continue
		}

		data := make(map[string][]byte, len(shared.Data))
		for k, v := range shared.Data {
			data[k] = v
		}
		data[kipperv1.LogicalNamespaceKey(svc.Spec.Type)] = []byte(b.Database)

		if err := writeDerivedBindingSecret(ctx, c, scheme, owner, name, data, b.Name); err != nil {
			return nil, nil, "", err
		}
		hashBindingShape(digest, i, b, svc.Spec.Type)
		hashInto(digest, name, data)
		rendered.keep(name, data)
		sources++
	}

	// Projections this workload no longer wants are not deleted here. Retirement
	// owns their lifetime, alongside the generations, so there is one grace and
	// one protocol rather than a second rule that would drift from it.
	// A workload with no credential sources gets no hash rather than the digest
	// of nothing, which every unbound workload would otherwise carry as a pod
	// annotation that means nothing and rolls it once to acquire.
	if sources == 0 {
		return rendered, desired, "", nil
	}
	return rendered, desired, hex.EncodeToString(digest.Sum(nil)), nil
}

// renderedBindings is what one pass wrote or verified for each binding Secret,
// keyed by Secret name.
//
// It exists because the render must not read back what the same pass just
// wrote. The manager's client serves reads from an informer cache, so a Get
// issued moments after a write returns the object as it was before it — and the
// pod-template hash is computed from the new data. A rotation would therefore
// roll pods carrying the new password through envFrom and the old one composed
// into ${DB_PASSWORD}, and the corrective re-render on the next pass changes no
// hash, so nothing rolls again and the pod stays split.
type renderedBindings map[string]map[string][]byte

// keep stores a copy, so a later mutation of the caller's map cannot reach back
// into the snapshot the render reads.
func (r renderedBindings) keep(name string, data map[string][]byte) {
	held := make(map[string][]byte, len(data))
	for k, v := range data {
		held[k] = v
	}
	r[name] = held
}

// envVars renders one held Secret as ordered variables, or reports false when
// this pass did not produce that Secret.
func (r renderedBindings) envVars(name string) ([]corev1.EnvVar, bool) {
	data, ok := r[name]
	if !ok {
		return nil, false
	}
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]corev1.EnvVar, 0, len(keys))
	for _, k := range keys {
		out = append(out, corev1.EnvVar{Name: k, Value: string(data[k])})
	}
	return out, true
}

// ClearBindingsToService removes every binding naming this service from every
// App and Function in the namespace, deleting the credentials each one derived.
//
// It runs from the Service's own finalizer, so it covers every way a service
// can be deleted — kip, the console, kubectl, a GitOps prune — rather than only
// the paths that remember to ask. That matters because a workload left
// declaring a binding to a service that no longer exists fails
// reconcileBindingSecrets, and since that is fail-closed the whole reconcile
// aborts: no image change, no scale and no env update lands, while the error's
// own advice to unbind is the one thing that has stopped working.
//
// Clearing a binding that is already gone is a no-op, so the console handler
// doing this first as its retryable pre-step costs nothing.
func ClearBindingsToService(ctx context.Context, c client.Client, service, namespace string) error {
	// The service's type used to be read here, to decide which bindings had
	// derived a Secret worth deleting. Nothing is deleted on this path any more:
	// removing the binding is all it does, and the workload's own reconcile
	// retires the projection once nothing names it.
	var apps kipperv1.AppList
	if err := c.List(ctx, &apps, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing apps: %w", err)
	}
	for i := range apps.Items {
		app := &apps.Items[i]
		remaining, changed := dropBindingsToService(service, app.Spec.ServiceBindings)
		if changed {
			app.Spec.ServiceBindings = remaining
			if err := c.Update(ctx, app); err != nil {
				return fmt.Errorf("updating app %q: %w", app.Name, err)
			}
		}
	}

	var fns kipperv1.FunctionList
	if err := c.List(ctx, &fns, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing functions: %w", err)
	}
	for i := range fns.Items {
		fn := &fns.Items[i]
		remaining, changed := dropBindingsToService(service, fn.Spec.ServiceBindings)
		if changed {
			fn.Spec.ServiceBindings = remaining
			if err := c.Update(ctx, fn); err != nil {
				return fmt.Errorf("updating function %q: %w", fn.Name, err)
			}
		}
	}
	return nil
}

// derivedSecretBelongsTo reports whether this Secret is the projection this
// workload rendered.
//
// The controller-owner UID is the ordinary test and the one that matters for a
// name collision: an object somebody else created never carries it.
//
// A restore breaks that test without changing what the object is. The workload
// comes back with a new UID while its Secret keeps the old reference, or loses
// the reference altogether. Refusing there strands a credential nobody can
// clear: the render will not overwrite an object it does not own, unbinding
// will not delete it, the pruner will not either, and rebinding needs that
// exact name. So a Secret carrying the label this render stamps — which nothing
// else writes — and a surviving reference naming this same workload is treated
// as ours. A foreign object at a colliding name has neither.
func derivedSecretBelongsTo(secret *corev1.Secret, owner client.Object, kind secretname.Kind) bool {
	ref := metav1.GetControllerOf(secret)
	if ref != nil && ref.UID == owner.GetUID() {
		return true
	}
	if secret.Labels[derivedBindingLabel] != "true" {
		return false
	}
	if ref == nil {
		return true
	}
	return strings.EqualFold(ref.Kind, string(kind)) && ref.Name == owner.GetName()
}

// dropBindingsToService removes every binding naming the service from one
// workload's list. A workload may declare the same service more than once, so
// this drops all of them rather than the first.
//
// It used to delete the credentials each binding derived, which is why it once
// needed the service type and a client. Retirement owns a projection's lifetime
// now, and waits while a retained revision or a live pod still names it, so all
// that is left here is the list.
func dropBindingsToService(service string, bindings []kipperv1.ServiceBinding) ([]kipperv1.ServiceBinding, bool) {
	var remaining []kipperv1.ServiceBinding
	changed := false

	for _, b := range bindings {
		if b.Name != service {
			remaining = append(remaining, b)
			continue
		}
		changed = true
	}
	return remaining, changed
}

// hashBindingShape folds everything about a binding that changes what the pod
// reads, short of the credential values themselves.
//
// The digest used to cover only the credentials, because the rest of the shape
// was carried by the pod template: the prefix appeared in the container's
// EnvFrom entry, and the declared order was the order of those entries. Once
// the environment is published as one flattened generation neither is on the
// template, so a binding renamed from DB_ to POSTGRES_, or two bindings
// swapping places and with them which one wins a name they share, would leave
// the workload running the old environment with nothing to say it had changed.
//
// The declared index is part of it, which is why the bindings are no longer
// sorted before the walk: sorting is what discarded the order. Two reconciles
// over the same state still produce the same hash, because a CR's slice order
// is as stable as the values in it.
func hashBindingShape(digest hash.Hash, index int, b kipperv1.ServiceBinding, serviceType string) {
	prefix := b.Prefix
	if prefix == "" {
		prefix = kipperv1.DefaultBindingPrefix(serviceType)
	}
	// hash.Hash never returns an error from Write, as hashInto documents.
	_, _ = fmt.Fprintf(digest, "\x00%d\x00%s\x00%s\x00%s", index, b.Name, prefix, b.Database)
}

// stringMapEqual compares two label sets.
func stringMapEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// hashInto folds one derived Secret into the source digest, keys in order.
func hashInto(digest hash.Hash, name string, data map[string][]byte) {
	keys := make([]string, 0, len(data))
	for k := range data {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	// hash.Hash never returns an error from Write, which is why the results are
	// discarded rather than plumbed through a function that cannot fail.
	digest.Write([]byte(name)) //nolint:errcheck // documented above
	for _, k := range keys {
		digest.Write([]byte{0}) //nolint:errcheck // documented above
		digest.Write([]byte(k)) //nolint:errcheck // documented above
		digest.Write([]byte{0}) //nolint:errcheck // documented above
		digest.Write(data[k])   //nolint:errcheck // documented above
	}
}

// sharedServiceCredentials returns the Secret a Service owns, or nil when it is
// absent or not the Service's own.
//
// Ownership is the whole check. The name is derived from a field on the
// workload CR, and a CR can be written directly, so a binding could otherwise
// name any `<x>-credentials` Secret in the namespace — another app's git token,
// for one — and have this function copy it into a Secret the workload reads.
// Requiring the Service's controller reference by UID means the object has to
// have been created for that exact Service CR, which adoption never fakes.
func sharedServiceCredentials(ctx context.Context, c client.Client, svc *kipperv1.Service) (*corev1.Secret, error) {
	var shared corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Name: secretname.ServiceCredentials(svc.Name), Namespace: svc.Namespace}, &shared)
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading credentials for service %q: %w", svc.Name, err)
	}
	if owner := metav1.GetControllerOf(&shared); owner == nil || owner.UID != svc.UID {
		return nil, nil
	}
	return &shared, nil
}

// writeDerivedBindingSecret creates or updates a Secret this workload owns,
// refusing one it does not. Refusing rather than adopting keeps the derived
// name from being a way to overwrite an object that happens to sit under it.
func writeDerivedBindingSecret(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, name string, data map[string][]byte, service string) error {
	labels := map[string]string{
		"app":                     owner.GetName(),
		kipperLabel:               kipperValue,
		derivedBindingLabel:       "true",
		"kipper.run/service":      service,
		"kipper.run/derived-from": secretname.ServiceCredentials(service),
	}

	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: owner.GetNamespace()}, &existing)
	if errors.IsNotFound(err) {
		desired := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   owner.GetNamespace(),
				Labels:      labels,
				Annotations: map[string]string{kipperv1.DataUpdatedAtAnnotation: time.Now().Format(time.RFC3339Nano)},
			},
			Data: data,
		}
		if err := controllerutil.SetControllerReference(owner, desired, scheme); err != nil {
			return fmt.Errorf("setting owner reference: %w", err)
		}
		if err := c.Create(ctx, desired); err != nil {
			// A reconcile and a handler-driven test run can derive the same
			// projection at the same moment. Losing that race means the object
			// this pass wanted now exists, which is the outcome it was after;
			// failing here would turn a valid test run into a 500 depending on
			// which writer arrived first. The next pass rewrites the contents if
			// they differ.
			if errors.IsAlreadyExists(err) {
				return nil
			}
			return err
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading binding secret %q: %w", name, err)
	}

	if ref := metav1.GetControllerOf(&existing); ref == nil || ref.UID != owner.GetUID() {
		// Someone else's object under our derived name — most likely the shared
		// credentials of a service actually called <svc>-<kind>-<workload>.
		// Overwriting it would make this render a way to clobber whatever it is,
		// and carrying on would roll the workload without the binding, so this
		// stops the reconcile where an operator can see it.
		return fmt.Errorf("secret %q already exists and is not owned by %s/%s; rename the service or the workload",
			name, owner.GetNamespace(), owner.GetName())
	}

	// Labels are re-asserted whether or not the data moved. They are not
	// decoration: migration decides what to leave behind by the binding label,
	// so a projection that lost it would travel to the target, arrive owned by
	// nobody, and wedge the workload that needs the name. Restoring it only when
	// the password happens to have changed would leave that window open
	// indefinitely.
	labelsChanged := !stringMapEqual(existing.Labels, labels)
	if bytesMapEqual(existing.Data, data) {
		if !labelsChanged {
			// A no-op render must not restamp, or the console shows a restart
			// banner every reconcile for a credential nobody changed.
			return nil
		}
		existing.Labels = labels
		return c.Update(ctx, &existing)
	}
	if existing.Annotations == nil {
		existing.Annotations = map[string]string{}
	}
	existing.Annotations[kipperv1.DataUpdatedAtAnnotation] = time.Now().Format(time.RFC3339Nano)
	existing.Labels = labels
	existing.Data = data
	return c.Update(ctx, &existing)
}

// enqueueWorkloadsBoundToCredentials maps a change of a service's shared
// credentials to a reconcile of every workload that derives a Secret from them.
//
// Without it a rotated password only reaches a binding whenever something else
// happens to touch that workload, which for a stable app is never. The whole
// point of deriving the Secret is that the rotation propagates, so the change
// has to wake the workloads that depend on it.
//
// Every binding on the service is woken, whether or not it derives a Secret. A
// binding that pins no logical namespace reads the shared credentials straight
// through envFrom, so its running pods hold the old password just the same, and
// "it picks the new one up on its next start" is true and useless when the next
// start may be never.
func workloadsBoundToCredentials[T any](secret client.Object, items []T, binding func(*T) (string, string, []kipperv1.ServiceBinding)) []reconcile.Request {
	service, ok := serviceOfCredentialsSecret(secret)
	if !ok {
		return nil
	}
	var reqs []reconcile.Request
	for i := range items {
		name, namespace, bindings := binding(&items[i])
		if namespace != secret.GetNamespace() {
			continue
		}
		for _, b := range bindings {
			// Any binding on this service, pinned or not: both shapes put the
			// rotated password into their pods.
			if b.Name == service {
				reqs = append(reqs, reconcile.Request{
					NamespacedName: types.NamespacedName{Name: name, Namespace: namespace},
				})
				break
			}
		}
	}
	return reqs
}

// serviceOfCredentialsSecret recovers the service name from a shared
// credentials Secret, reporting false for anything else.
//
// Derived Secrets also end in -credentials and must not match: they are outputs
// of this render, and treating one as an input would have every render enqueue
// itself. They are told apart by the label this controller writes on them, not
// by looking for "-app-" or "-function-" in the name — a service legitimately
// called my-app-db would have been misread as derived, and its rotations would
// then never have reached anything bound to it.
func serviceOfCredentialsSecret(secret client.Object) (string, bool) {
	if secret.GetLabels()[derivedBindingLabel] == "true" {
		return "", false
	}
	const suffix = "-credentials"
	name := secret.GetName()
	if !strings.HasSuffix(name, suffix) {
		return "", false
	}
	service := strings.TrimSuffix(name, suffix)
	if service == "" {
		return "", false
	}
	return service, true
}

// enqueueAppsForServiceCredentials wakes every App deriving a Secret from the
// changed credentials.
func (r *AppReconciler) enqueueAppsForServiceCredentials(ctx context.Context, obj client.Object) []reconcile.Request {
	var apps kipperv1.AppList
	if err := r.List(ctx, &apps, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	return workloadsBoundToCredentials(obj, apps.Items, func(a *kipperv1.App) (string, string, []kipperv1.ServiceBinding) {
		return a.Name, a.Namespace, a.Spec.ServiceBindings
	})
}

// enqueueFunctionsForServiceCredentials is the Function twin.
func (r *FunctionReconciler) enqueueFunctionsForServiceCredentials(ctx context.Context, obj client.Object) []reconcile.Request {
	var fns kipperv1.FunctionList
	if err := r.List(ctx, &fns, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}
	return workloadsBoundToCredentials(obj, fns.Items, func(f *kipperv1.Function) (string, string, []kipperv1.ServiceBinding) {
		return f.Name, f.Namespace, f.Spec.ServiceBindings
	})
}

// declaredProjectionNames is every projection name this workload's bindings
// could have derived, whether or not each one actually does.
//
// It is the fallback keep-set for a pass that failed before the render could say
// which projections it wants. A superset is the safe direction: it keeps a
// projection that is no longer wanted for another pass, where the alternative
// sweeps one that is.
func declaredProjectionNames(bindings []kipperv1.ServiceBinding, kind secretname.Kind, workload string) map[string]bool {
	names := map[string]bool{}
	for _, b := range bindings {
		names[secretname.Binding(b.Name, kind, workload)] = true
	}
	return names
}
