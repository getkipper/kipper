package controllers

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// publishEnvGeneration writes one generation of a workload's environment and
// returns the name a pod template can reference.
//
// The immutable Secret binds the name to fixed content, so a pod reads that
// content or fails to start. Publishing is idempotent: a valid generation this
// workload controls needs no write. A matching generation restored without an
// owner is adopted with one metadata update.
func publishEnvGeneration(ctx context.Context, c client.Client, scheme *runtime.Scheme,
	owner client.Object, kind secretname.Kind, env, labels map[string]string) (string, error) {
	name := secretname.EnvGeneration(kind, owner.GetName(), envDigest(env))

	data := make(map[string][]byte, len(env))
	for k, v := range env {
		data[k] = []byte(v)
	}

	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: owner.GetNamespace()}, &existing)
	if err == nil {
		return name, acceptEnvGeneration(ctx, c, scheme, &existing, owner, data, labels)
	}
	if !errors.IsNotFound(err) {
		return "", err
	}

	immutable := true
	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: owner.GetNamespace(),
			Labels:    labels,
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      data,
	}
	if err := controllerutil.SetControllerReference(owner, desired, scheme); err != nil {
		return "", fmt.Errorf("setting owner reference: %w", err)
	}
	if err := c.Create(ctx, desired); err != nil {
		if !errors.IsAlreadyExists(err) {
			return "", err
		}
		// Another writer got there between the read and the create. Whatever it
		// wrote has to satisfy the same invariants before a pod is pointed at it.
		if err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: owner.GetNamespace()}, &existing); err != nil {
			return "", err
		}
		return name, acceptEnvGeneration(ctx, c, scheme, &existing, owner, data, labels)
	}
	return name, nil
}

// acceptEnvGeneration validates an existing generation and adopts it if it has
// no owner references and its identity labels match, allowing reuse after a restore.
func acceptEnvGeneration(ctx context.Context, c client.Client, scheme *runtime.Scheme,
	existing *corev1.Secret, owner client.Object, data map[string][]byte, labels map[string]string) error {
	adopt := restoredWithoutOwner(existing, owner, labels)
	if err := validateEnvGeneration(existing, owner, data, adopt); err != nil {
		return err
	}
	if !adopt {
		return nil
	}
	if err := controllerutil.SetControllerReference(owner, existing, scheme); err != nil {
		return fmt.Errorf("setting owner reference: %w", err)
	}
	if err := c.Update(ctx, existing); err != nil {
		return fmt.Errorf("adopting env generation %q in %s: %w", existing.Name, existing.Namespace, err)
	}
	return nil
}

// restoredWithoutOwner identifies adoption candidates by the absence of owner
// references and matching Kipper, workload name and workload kind labels.
// This allows reuse of generations restored without owner references. Other
// labels, such as a Function's trigger, can change independently of the
// environment, so only identity labels are compared. Content is checked separately.
func restoredWithoutOwner(existing *corev1.Secret, owner client.Object, labels map[string]string) bool {
	if len(existing.OwnerReferences) > 0 {
		return false
	}
	if labels[kipperLabel] != kipperValue || labels["app"] != owner.GetName() || labels[resourceTypeLabel] == "" {
		return false
	}
	for _, k := range []string{kipperLabel, "app", resourceTypeLabel} {
		if existing.Labels[k] != labels[k] {
			return false
		}
	}
	return true
}

// validateEnvGeneration checks immutability, Secret type, ownership and exact
// content so the generation provides the environment its name promises.
// Content-derived names are guessable, so the name alone cannot establish who
// wrote the Secret. Immutability keeps content fixed, and ownership prevents
// reuse of another workload's Secret.
//
// When adopting is true, the ownership check is skipped. The caller must first
// check that owner references are absent and identity labels match, then adopt
// the generation after validation succeeds.
func validateEnvGeneration(existing *corev1.Secret, owner client.Object, data map[string][]byte, adopting bool) error {
	where := fmt.Sprintf("env generation %q in %s", existing.Name, existing.Namespace)

	if existing.Immutable == nil || !*existing.Immutable {
		return fmt.Errorf("%s is mutable. A pod requires an environment that stays fixed", where)
	}
	if existing.Type != "" && existing.Type != corev1.SecretTypeOpaque {
		return fmt.Errorf("%s is of type %q, not %q", where, existing.Type, corev1.SecretTypeOpaque)
	}
	if !adopting && !metav1.IsControlledBy(existing, owner) {
		return fmt.Errorf("%s is not controlled by this workload. Remove it or rename the workload", where)
	}
	if len(existing.Data) != len(data) {
		return fmt.Errorf("%s holds %d variables where this environment has %d", where, len(existing.Data), len(data))
	}
	for k, v := range data {
		if held, ok := existing.Data[k]; !ok || !bytes.Equal(held, v) {
			return fmt.Errorf("%s holds a different value for %q under a matching digest", where, k)
		}
	}
	return nil
}

// applyEnvPublishedCondition records whether this pass got the workload's
// environment into the object its pods read.
func applyEnvPublishedCondition(conditions *[]metav1.Condition, generation int64, err error) {
	applyEnvPublishedConditionWithConversion(conditions, generation, err, 0)
}

// applyEnvPublishedConditionWithConversion adds what the retirement pass learned:
// how many objects from before the move to published environments something in
// the namespace still reads.
//
// While that count is above zero the workload has not fully left the mutable
// model, which is worth saying out loud during the conversion and worth nothing
// afterwards — so it is a sentence in the message rather than a field that would
// outlive its usefulness.
func applyEnvPublishedConditionWithConversion(conditions *[]metav1.Condition, generation int64, err error, legacyReaders int) {
	message := "the environment this workload's pods read is current"
	if err == nil && legacyReaders > 0 {
		message = fmt.Sprintf(
			"%s; %d object(s) from before generations are still read and will be retired once nothing names them",
			message, legacyReaders)
	}
	cond := metav1.Condition{
		Type:               kipperv1.ConditionEnvPublished,
		Status:             metav1.ConditionTrue,
		Reason:             "Published",
		Message:            message,
		ObservedGeneration: generation,
	}
	if err != nil {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "PublishFailed"
		cond.Message = err.Error()
	}
	apimeta.SetStatusCondition(conditions, cond)
}

// generationUsable says whether the environment a running pod template names can
// still be left in place.
//
// Existence is not the question. The name is guessable — it is a digest of the
// content — so an object can appear at it that carries none of the guarantees
// the publication path checks before a pod is ever pointed at one. Holding a
// generation is pointing a pod at it again, so it is checked again.
//
// The digest is the whole checksum: a generation whose contents no longer hash
// to its own name is not the environment that name promised, however it came to
// be that way.
func generationUsable(ctx context.Context, reader client.Reader, owner client.Object, kind secretname.Kind, name string) (bool, error) {
	var secret corev1.Secret
	err := reader.Get(ctx, types.NamespacedName{Name: name, Namespace: owner.GetNamespace()}, &secret)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if secret.Immutable == nil || !*secret.Immutable {
		return false, nil
	}
	if secret.Type != "" && secret.Type != corev1.SecretTypeOpaque {
		return false, nil
	}
	if !metav1.IsControlledBy(&secret, owner) {
		return false, nil
	}

	held := make(map[string]string, len(secret.Data))
	for k, v := range secret.Data {
		held[k] = string(v)
	}
	// The exact name the scheme would have produced, not merely one ending in
	// the right digest. Anything between the prefix and the digest is text no
	// publication ever wrote.
	return name == secretname.EnvGeneration(kind, owner.GetName(), envDigest(held)), nil
}

// recordPublication persists what a pass published before anything downstream
// can fail.
//
// The condition and the field used to ride to the end of the reconcile on the
// final status update, so a failure in an unrelated step — a Service, an
// Ingress, an autoscaler — left the field naming the previous generation. The
// console compares that field with the pod template to decide whether a restart
// would apply anything, and a stale field answers no while a restart would in
// fact move the workload onto what was just published.
func recordPublication(ctx context.Context, c client.Client, owner client.Object,
	conditions *[]metav1.Condition, published *string, generation int64, name string, pubErr error) error {
	before := apimeta.FindStatusCondition(*conditions, kipperv1.ConditionEnvPublished)
	unchanged := *published == name && before != nil &&
		before.Status == publishedStatus(pubErr) && before.ObservedGeneration == generation

	applyEnvPublishedCondition(conditions, generation, pubErr)
	if unchanged {
		return nil
	}
	*published = name
	return c.Status().Update(ctx, owner)
}

// publishedStatus is what the condition will say, so recordPublication can tell
// whether a write is needed before it overwrites what is there. Gating on the
// name alone missed a recovery: republishing the same content after a validation
// failure leaves the name identical while the condition goes False to True.
func publishedStatus(err error) metav1.ConditionStatus {
	if err != nil {
		return metav1.ConditionFalse
	}
	return metav1.ConditionTrue
}

// templateSettlesAs compares live with an API-defaulted, dry-run candidate.
// Admission defaults make raw controller templates differ from stored templates.
// If dry-run is unsupported, compare the raw candidate and return answered=false;
// other errors propagate.
func templateSettlesAs(ctx context.Context, c client.Client, live *appsv1.Deployment, candidate corev1.PodTemplateSpec) (settles, answered bool, err error) {
	probe := live.DeepCopy()
	probe.Spec.Template = candidate
	err = c.Update(ctx, probe, client.DryRunAll)
	switch {
	case err == nil:
		return equality.Semantic.DeepEqual(live.Spec.Template, probe.Spec.Template), true, nil
	case dryRunRefused(err):
		// Answered as well as this cluster allows. The direct comparison is what
		// this replaced, so an env edit rolls the app here — worse than holding,
		// and much better than the alternative: the same probe sits in front of
		// a restart and an image change, so treating a refusal as fatal would
		// stop a workload receiving any template change at all, for good.
		return equality.Semantic.DeepEqual(live.Spec.Template, candidate), false, nil
	default:
		return false, false, fmt.Errorf("asking what the pod template would settle as: %w", err)
	}
}

// dryRunRefused recognizes the API server's unsupported-dry-run response
// by both status and message. Other admission, permission and transient errors
// propagate so callers can distinguish an invalid write from an unavailable simulation.
func dryRunRefused(err error) bool {
	return errors.IsBadRequest(err) &&
		strings.Contains(strings.ToLower(err.Error()), "does not support dry run")
}
