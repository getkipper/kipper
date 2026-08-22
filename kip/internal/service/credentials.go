package service

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// CredentialState is what a credentials Secret's ownership says about it.
type CredentialState string

const (
	// CredentialOwned is the healthy state: the Secret carries a controller
	// reference to the live Service CR, which is what admits its values into a
	// bound workload.
	CredentialOwned CredentialState = "owned"
	// CredentialUnowned is a Secret with no controller at all. Its service runs,
	// but every binding on it is refused, so bound workloads never receive the
	// credentials they declare.
	CredentialUnowned CredentialState = "unowned"
	// CredentialForeign is a Secret an object that still exists controls.
	// Claiming it would take it from that object, so repair leaves it alone.
	CredentialForeign CredentialState = "foreign"
	// CredentialAbandoned is a Secret whose controller reference names an object
	// that is not there: the owner was deleted, or came back from a restore
	// under a new identity. Nobody is holding it, and Kubernetes deletes a
	// dependent by exactly this kind of reference, so the Secret is on its way
	// out and the password its volume was written under goes with it.
	CredentialAbandoned CredentialState = "abandoned"
	// CredentialMissing is a service with no credentials Secret at all.
	CredentialMissing CredentialState = "missing"
)

// CredentialReport is one service's credentials and what is wrong with them.
type CredentialReport struct {
	Service string
	Secret  string
	State   CredentialState
	// Owner names the controller holding the Secret, when one does and it is
	// not this Service.
	Owner string
}

// Healthy reports whether this service's credentials can be injected.
func (r CredentialReport) Healthy() bool {
	return r.State == CredentialOwned
}

// StaleProjection is a per-binding Secret no workload owns. The workload's
// controller renders its own and never writes through one it does not own, so
// an unowned projection is a credential-bearing object nothing maintains.
type StaleProjection struct {
	Name string
}

// CredentialAudit is the state of one namespace's service credentials.
type CredentialAudit struct {
	Services    []CredentialReport
	Projections []StaleProjection
}

// NeedsRepair reports whether anything in the namespace wants fixing.
func (a CredentialAudit) NeedsRepair() bool {
	if len(a.Projections) > 0 {
		return true
	}
	for _, s := range a.Services {
		if s.State == CredentialUnowned || s.State == CredentialAbandoned {
			return true
		}
	}
	return false
}

// AuditCredentials reports the ownership of every service's credentials in a
// namespace, and any per-binding projection left without an owner.
//
// Ownership is not bookkeeping here. A binding's credentials are admitted into a
// workload only when the Secret carries a controller reference to the Service CR
// it names, so a Secret that lost that reference silently stops feeding
// everything bound to it. Nothing infers ownership from the name or the labels,
// which is why this cannot be answered by looking at a service and has to
// compare the reference against the live CR's UID.
func (m *Manager) AuditCredentials(ctx context.Context, namespace string) (CredentialAudit, error) {
	var audit CredentialAudit
	if m.Dynamic == nil {
		return audit, fmt.Errorf("service manager is not configured with a dynamic client")
	}

	crList, err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return audit, fmt.Errorf("listing service CRs: %w", err)
	}

	for _, cr := range crList.Items {
		name := cr.GetName()
		report := CredentialReport{Service: name, Secret: secretname.ServiceCredentials(name)}

		secret, err := m.Client.CoreV1().Secrets(namespace).Get(ctx, report.Secret, metav1.GetOptions{})
		switch {
		case errors.IsNotFound(err):
			report.State = CredentialMissing
		case err != nil:
			return audit, fmt.Errorf("reading credentials of %s: %w", name, err)
		default:
			owner := metav1.GetControllerOf(secret)
			switch {
			case owner == nil:
				report.State = CredentialUnowned
			case owner.UID == cr.GetUID():
				report.State = CredentialOwned
			default:
				live, liveErr := m.ownerIsLive(ctx, namespace, owner)
				if liveErr != nil {
					return audit, liveErr
				}
				report.State = CredentialAbandoned
				if live {
					report.State = CredentialForeign
				}
				report.Owner = owner.Kind + "/" + owner.Name
			}
		}
		audit.Services = append(audit.Services, report)
	}

	secrets, err := m.Client.CoreV1().Secrets(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Binding + "=" + labels.BindingTrue,
	})
	if err != nil {
		return audit, fmt.Errorf("listing binding secrets in %s: %w", namespace, err)
	}
	for i := range secrets.Items {
		if metav1.GetControllerOf(&secrets.Items[i]) == nil {
			audit.Projections = append(audit.Projections, StaleProjection{Name: secrets.Items[i].Name})
		}
	}

	return audit, nil
}

// ownerIsLive says whether the object a controller reference names is still
// there under the same identity.
//
// A reference outlives its object: garbage collection is not instant, and a
// restore brings back a dependent whose owner came back with a new UID. Reading
// the reference alone would take both for a live claim, and repair would leave
// the Secret to be deleted along with the password nothing else records.
//
// Only the two kinds Kipper creates are checked. A controller outside the group
// belongs to somebody whose objects are not ours to look up, and a claim that
// cannot be disproved is treated as real, because taking a Secret from an
// operator's own controller is the one mistake repair must never make.
func (m *Manager) ownerIsLive(ctx context.Context, namespace string, ref *metav1.OwnerReference) (bool, error) {
	if schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind).Group != manifest.ServiceGVR.Group {
		return true, nil
	}
	var gvr schema.GroupVersionResource
	switch ref.Kind {
	case "Service":
		gvr = manifest.ServiceGVR
	case "App":
		gvr = manifest.AppGVR
	default:
		return true, nil
	}
	owner, err := m.Dynamic.Resource(gvr).Namespace(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking whether %s %s still holds its credentials: %w", ref.Kind, ref.Name, err)
	}
	return owner.GetUID() == ref.UID, nil
}

// withoutController drops the controlling reference and keeps every other one.
// An object has one controller, so the dead reference has to go before the live
// one is written rather than sit beside it.
func withoutController(refs []metav1.OwnerReference) []metav1.OwnerReference {
	kept := make([]metav1.OwnerReference, 0, len(refs))
	for _, ref := range refs {
		if ref.Controller != nil && *ref.Controller {
			continue
		}
		kept = append(kept, ref)
	}
	return kept
}

// RepairCredentials gives an unowned credentials Secret back to its service and
// removes per-binding projections nothing owns.
//
// The operator asserts here what the platform will not infer. A controller that
// claimed an ownerless Secret on its name would hand whatever sits under that
// name to anything able to create a Service CR, so the reconciler refuses and
// this exists instead: a person, holding cluster credentials, saying that this
// object belongs to that service.
//
// It claims a Secret with no controller, and one whose controller reference
// names an object that is not there. Nothing is taken from an owner that exists:
// liveness is decided again here rather than trusted from the audit, because the
// two are separate reads and a Secret can be claimed in between.
//
// Projections are deleted rather than claimed. They are rendered from the
// service's shared credentials on the next reconcile, so the workload's own
// controller writes a replacement it owns, whereas a claimed one would keep
// whatever values it happens to hold.
func (m *Manager) RepairCredentials(ctx context.Context, namespace string, audit CredentialAudit) ([]string, error) {
	var done []string

	for _, report := range audit.Services {
		if report.State != CredentialUnowned && report.State != CredentialAbandoned {
			continue
		}
		secret, err := m.Client.CoreV1().Secrets(namespace).Get(ctx, report.Secret, metav1.GetOptions{})
		if err != nil {
			return done, fmt.Errorf("reading %s: %w", report.Secret, err)
		}
		// Re-check under the object we are about to write: the audit was a
		// separate read, and claiming a Secret that has acquired a controller
		// since then would take it from that controller. A reference with
		// nothing behind it is not one, and is exactly what this is here to
		// replace.
		if owner := metav1.GetControllerOf(secret); owner != nil {
			live, liveErr := m.ownerIsLive(ctx, namespace, owner)
			if liveErr != nil {
				return done, liveErr
			}
			if live {
				return done, fmt.Errorf("%s acquired an owner since the audit; run the audit again", report.Secret)
			}
		}
		// The identity is read here, last of everything and immediately before
		// the write, rather than from a list taken at the top. A service
		// deleted and recreated in between answers to a new one, and writing
		// the old would install a fresh dangling reference while reporting
		// success, which is the state this is here to clear.
		cr, err := m.Dynamic.Resource(manifest.ServiceGVR).Namespace(namespace).
			Get(ctx, report.Service, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				return done, fmt.Errorf("service %s has gone since the audit; run the audit again", report.Service)
			}
			return done, fmt.Errorf("reading service %s: %w", report.Service, err)
		}
		// A service on its way out is not one to hand a Secret to: the reference
		// would be written, reported as repaired, and collected with the service
		// moments later. The window is as long as the cleanup takes, which is as
		// long as the workload takes to stop.
		if cr.GetDeletionTimestamp() != nil {
			return done, fmt.Errorf("service %s is being deleted; run the audit again once it has gone", report.Service)
		}
		uid := cr.GetUID()

		// The dead reference is replaced rather than added to. An object has one
		// controller, and leaving the old one beside the new is invalid.
		secret.OwnerReferences = withoutController(secret.OwnerReferences)
		controller := true
		secret.OwnerReferences = append(secret.OwnerReferences, metav1.OwnerReference{
			APIVersion: manifest.ServiceGVR.GroupVersion().String(),
			Kind:       "Service",
			Name:       report.Service,
			UID:        uid,
			Controller: &controller,
			// The Secret goes when its service does, which is how one the
			// reconciler created behaves.
			BlockOwnerDeletion: &controller,
		})
		if _, err := m.Client.CoreV1().Secrets(namespace).Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			return done, fmt.Errorf("claiming %s for %s: %w", report.Secret, report.Service, err)
		}
		done = append(done, fmt.Sprintf("%s now belongs to service %s", report.Secret, report.Service))
	}

	for _, projection := range audit.Projections {
		// A projection a running workload still reads is left alone. Its pods
		// hold those values already, but the envFrom that names it is optional,
		// so a pod that restarts after the Secret went would come up without the
		// credentials it declares and fail on its first connection instead. The
		// workload's controller renders one it owns under the name it wants, and
		// the leftover can go once nothing points at it.
		referenced, err := m.projectionInUse(ctx, namespace, projection.Name)
		if err != nil {
			return done, err
		}
		if referenced {
			done = append(done, fmt.Sprintf("%s left in place: a running workload still reads it, so it goes once that workload has re-rendered", projection.Name))
			continue
		}
		if err := m.deleteUnownedProjection(ctx, namespace, projection.Name); err != nil {
			return done, err
		}
		done = append(done, fmt.Sprintf("%s removed; nothing reads it and its workload renders a replacement it owns", projection.Name))
	}

	return done, nil
}

// projectionInUse reports whether any workload's pod template still names this
// Secret, which is what makes removing it unsafe.
func (m *Manager) projectionInUse(ctx context.Context, namespace, name string) (bool, error) {
	deployments, err := m.Client.AppsV1().Deployments(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("listing deployments in %s: %w", namespace, err)
	}
	for i := range deployments.Items {
		if podTemplateReads(&deployments.Items[i].Spec.Template, name) {
			return true, nil
		}
	}
	cronJobs, err := m.Client.BatchV1().CronJobs(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return false, fmt.Errorf("listing cronjobs in %s: %w", namespace, err)
	}
	for i := range cronJobs.Items {
		if podTemplateReads(&cronJobs.Items[i].Spec.JobTemplate.Spec.Template, name) {
			return true, nil
		}
	}
	return false, nil
}

// podTemplateReads reports whether a pod template pulls environment from the
// named Secret, whether wholesale or one variable at a time.
func podTemplateReads(template *corev1.PodTemplateSpec, name string) bool {
	containers := append(append([]corev1.Container{}, template.Spec.Containers...), template.Spec.InitContainers...)
	for _, container := range containers {
		for _, source := range container.EnvFrom {
			if source.SecretRef != nil && source.SecretRef.Name == name {
				return true
			}
		}
		for _, env := range container.Env {
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == name {
				return true
			}
		}
	}
	return false
}

// deleteUnownedProjection removes one ownerless per-binding Secret, refusing to
// touch one that gained an owner since the audit read it.
func (m *Manager) deleteUnownedProjection(ctx context.Context, namespace, name string) error {
	secret, err := m.Client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading %s: %w", name, err)
	}
	if metav1.GetControllerOf(secret) != nil {
		return fmt.Errorf("%s acquired an owner since the audit; run the audit again", name)
	}
	if err := m.Client.CoreV1().Secrets(namespace).Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: uidOf(secret)},
	}); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("removing %s: %w", name, err)
	}
	return nil
}

// uidOf returns a pointer to a Secret's UID, so a delete cannot land on a
// namesake recreated between the read and the write.
func uidOf(secret *corev1.Secret) *types.UID {
	uid := secret.UID
	return &uid
}
