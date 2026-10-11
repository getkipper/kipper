package controllers

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// DatabaseProvisioner provisions a binding's database or vhost using the
// service's shared credentials.
type DatabaseProvisioner func(ctx context.Context, svc *kipperv1.Service, creds *corev1.Secret, database string) error

// bindingDatabaseAnnotation records successful provisioning as space-separated
// <service UID>/<name> entries on the derived Secret. Bindings to the same
// service within a workload share this Secret.
const bindingDatabaseAnnotation = "kipper.run/binding-database"

const (
	bindingDatabaseRetry   = time.Minute
	bindingDatabaseTimeout = 30 * time.Second
)

// ensureBindingDatabases provisions namespaces without a success marker in their
// derived Secrets. Call it after reconcileBindingSecrets has prepared the Secrets.
// It returns the number of derived bindings and any errors encountered.
func ensureBindingDatabases(ctx context.Context, c client.Client, provision DatabaseProvisioner, owner client.Object, kind secretname.Kind, bindings []kipperv1.ServiceBinding) (int, []string) {
	if provision == nil {
		return 0, nil
	}
	declared := 0
	var failures []string
	for _, b := range bindings {
		if b.Database == "" {
			continue
		}
		var svc kipperv1.Service
		if err := c.Get(ctx, types.NamespacedName{Name: b.Name, Namespace: owner.GetNamespace()}, &svc); err != nil {
			failures = append(failures, fmt.Sprintf("binding %q: reading service: %v", b.Name, err))
			continue
		}
		if !bindingIsDerived(b, svc.Spec.Type, true) {
			continue
		}
		declared++

		var derived corev1.Secret
		if err := c.Get(ctx, types.NamespacedName{Name: secretname.Binding(b.Name, kind, owner.GetName()), Namespace: owner.GetNamespace()}, &derived); err != nil {
			failures = append(failures, fmt.Sprintf("binding %q: reading its credentials: %v", b.Name, err))
			continue
		}
		marker := string(svc.UID) + "/" + b.Database
		created := strings.Fields(derived.Annotations[bindingDatabaseAnnotation])
		if slices.Contains(created, marker) {
			continue
		}
		shared, err := sharedServiceCredentials(ctx, c, &svc)
		if err != nil || shared == nil {
			failures = append(failures, fmt.Sprintf("binding %q: service %q has no usable credentials", b.Name, b.Name))
			continue
		}

		execCtx, cancel := context.WithTimeout(ctx, bindingDatabaseTimeout)
		err = provision(execCtx, &svc, shared, b.Database)
		cancel()
		if err != nil {
			failures = append(failures, fmt.Sprintf("binding %q: creating %q in service %q: %v", b.Name, b.Database, b.Name, err))
			continue
		}

		patch := client.MergeFrom(derived.DeepCopy())
		if derived.Annotations == nil {
			derived.Annotations = map[string]string{}
		}
		derived.Annotations[bindingDatabaseAnnotation] = strings.Join(append(createdFor(created, svc.UID), marker), " ")
		if err := c.Patch(ctx, &derived, patch); err != nil {
			failures = append(failures, fmt.Sprintf("binding %q: recording the created database: %v", b.Name, err))
		}
	}
	return declared, failures
}

// createdFor keeps the entries recorded for the service with this UID, dropping
// those of a service that was deleted and recreated under the same name.
func createdFor(entries []string, uid types.UID) []string {
	var kept []string
	for _, e := range entries {
		if strings.HasPrefix(e, string(uid)+"/") {
			kept = append(kept, e)
		}
	}
	return kept
}

// applyBindingDatabaseCondition reports provisioning results, including recorded
// successes. It removes the condition when there are no derived bindings or failures.
func applyBindingDatabaseCondition(conditions *[]metav1.Condition, generation int64, declared int, failures []string) {
	if declared == 0 && len(failures) == 0 {
		apimeta.RemoveStatusCondition(conditions, kipperv1.ConditionBindingDatabasesReady)
		return
	}
	cond := metav1.Condition{
		Type:               kipperv1.ConditionBindingDatabasesReady,
		Status:             metav1.ConditionTrue,
		Reason:             "DatabasesCreated",
		Message:            "All binding databases and vhosts are configured",
		ObservedGeneration: generation,
	}
	if len(failures) > 0 {
		cond.Status = metav1.ConditionFalse
		cond.Reason = "DatabaseCreateFailed"
		cond.Message = joinWithinConditionMessage(failures)
	}
	apimeta.SetStatusCondition(conditions, cond)
}

func bindingDatabaseRequeue(failures []string) time.Duration {
	if len(failures) == 0 {
		return 0
	}
	return bindingDatabaseRetry
}
