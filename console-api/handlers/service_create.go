package handlers

import (
	"context"
	stderrors "errors"
	"fmt"
	"net/http"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

type createServiceRequest struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	Namespace     string `json:"namespace"`
	Storage       string `json:"storage"`
	CPURequest    string `json:"cpu_request"`
	CPULimit      string `json:"cpu_limit"`
	MemoryRequest string `json:"memory_request"`
	MemoryLimit   string `json:"memory_limit"`
}

// supportedServiceTypes lists valid service types for validation.
var supportedServiceTypes = map[string]bool{
	"postgres":   true,
	"mysql":      true,
	"redis":      true,
	"mongodb":    true,
	"rabbitmq":   true,
	"opensearch": true,
	"minio":      true,
	"mailhog":    true,
}

// Create deploys a new stateful service.
// POST /api/v1/services
func (s *Services) Create(w http.ResponseWriter, r *http.Request) {
	var req createServiceRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || req.Type == "" {
		respondError(w, http.StatusBadRequest, "name and type are required")
		return
	}
	if req.Namespace == "" {
		req.Namespace = "default"
	}
	if !enforceCapability(w, r, req.Namespace, "kipper.write") {
		return
	}

	// Answered before the cluster is asked anything, because no state on the
	// cluster changes it and every question below costs a read.
	if !supportedServiceTypes[req.Type] {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("unsupported service type %q", req.Type))
		return
	}

	// Whether the name is already this caller's own service is asked first, and
	// the CLI asks in that order too. The credential guard's advice is about an
	// object somebody else holds, so offering it for a service that is simply
	// already there points an operator at the credentials of a running service
	// and tells them to pick another name for one they have got.
	var live kipperv1.Service
	switch err := s.CRClient.Get(r.Context(), types.NamespacedName{Name: req.Name, Namespace: req.Namespace}, &live); {
	case err == nil:
		// A service on its way out still holds the name, and a delete that has
		// stopped holds it for good. "Already exists" would send an operator
		// looking for a service they have just deleted.
		if !live.DeletionTimestamp.IsZero() {
			respondError(w, http.StatusConflict, fmt.Sprintf(
				"service %q is still being deleted and holds the name until it has finished", req.Name))
			return
		}
		respondError(w, http.StatusConflict, fmt.Sprintf("service %q already exists", req.Name))
		return
	case !errors.IsNotFound(err):
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("checking whether service %q exists: %v", req.Name, err))
		return
	}

	if err := refuseServiceNameWhoseCredentialIsTaken(r.Context(), s.CRClient, req.Namespace, req.Name); err != nil {
		// A name that collides is a conflict; a cluster that could not be asked
		// is not, and answering 409 to it would tell the caller the name is
		// taken when nothing of the sort was established.
		var taken *serviceNameTakenError
		if stderrors.As(err, &taken) {
			respondError(w, http.StatusConflict, err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	storage := req.Storage
	if storage == "" {
		storage = "1Gi"
	}

	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: req.Namespace,
			Labels: map[string]string{
				"app":                     req.Name,
				kipperLabel:               kipperValue,
				"kipper.run/service-type": req.Type,
			},
		},
		Spec: kipperv1.ServiceSpec{
			Type:    req.Type,
			Storage: storage,
			Resources: kipperv1.ServiceResources{
				CPURequest:    req.CPURequest,
				CPULimit:      req.CPULimit,
				MemoryRequest: req.MemoryRequest,
				MemoryLimit:   req.MemoryLimit,
			},
		},
	}

	if err := s.CRClient.Create(ctx, svc); err != nil {
		if errors.IsAlreadyExists(err) {
			respondError(w, http.StatusConflict, fmt.Sprintf("service %q already exists", req.Name))
			return
		}
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create service: %v", err))
		return
	}

	svcHost := fmt.Sprintf("%s.%s.svc.cluster.local", req.Name, req.Namespace)
	respondJSON(w, http.StatusOK, map[string]string{
		"status": "created",
		"name":   req.Name,
		"type":   req.Type,
		"host":   svcHost,
	})
}

// Types returns the list of supported service types.
// GET /api/v1/service-types
func (s *Services) Types(w http.ResponseWriter, _ *http.Request) {
	types := make([]map[string]string, 0)
	for name := range supportedServiceTypes {
		types = append(types, map[string]string{
			"name": name,
		})
	}
	respondJSON(w, http.StatusOK, types)
}

// refuseServiceNameWhoseCredentialIsTaken stops a service being created whose
// credentials Secret is somebody else's object.
//
// The reconciler adopts nothing on the strength of a name: a Secret this Service
// does not own is refused, permanently, and the service never starts. So the
// question at create time is whether that one object is free, and the answer is
// the same whoever is sitting on it.
//
// An app is worth naming when it is the occupier, which happens on one name: an
// app still on the credential name generated before digests keeps its git token
// in exactly the object a service called <app>-git would want.
func refuseServiceNameWhoseCredentialIsTaken(ctx context.Context, c crclient.Client, namespace, name string) error {
	if app, holds, err := appHoldingTheCredential(ctx, c, namespace, name); err != nil || holds {
		if err != nil {
			return err
		}
		return &serviceNameTakenError{Service: name, App: app}
	}
	return credentialNameFree(ctx, c, namespace, name)
}

// appHoldingTheCredential says whether an app keeps its git token in the object
// this service name would take.
//
// The app existing is not the collision: an app names its credential after a
// digest of the pair now, so only one still on the older name has anything at
// that object. Refusing on the name alone would stop a service whose name merely
// resembles an app's.
func appHoldingTheCredential(ctx context.Context, c crclient.Client, namespace, name string) (string, bool, error) {
	app, collides := secretname.AppSharingServiceCredentialName(name)
	if !collides {
		return "", false, nil
	}
	var existing kipperv1.App
	err := c.Get(ctx, types.NamespacedName{Name: app, Namespace: namespace}, &existing)
	if errors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("checking whether app %s exists: %w", app, err)
	}
	if existing.Spec.Git == nil || existing.Spec.Git.CredentialsSecret != secretname.LegacyGitCredential(app) {
		return "", false, nil
	}
	return app, true, nil
}

// credentialNameFree refuses the name while the object is still there.
//
// Nothing on a create path mints this Secret ahead of the CR, so one already in
// the namespace belongs to something else: an app that rotated onto a digest
// name and left its old token for a sweep that runs on a delay, a restore, or a
// hand-written object. Refusing now says what the reconciler would say later,
// while there is still a choice of name.
func credentialNameFree(ctx context.Context, c crclient.Client, namespace, name string) error {
	var existing corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Name: secretname.ServiceCredentials(name), Namespace: namespace}, &existing)
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking whether %s is free: %w", secretname.ServiceCredentials(name), err)
	}
	// A live controller is what makes this final: that Secret is one this service
	// can never take, no repair claims it away, and the only way out is another
	// name. Nothing else here is permanent, so nothing else is refused.
	//
	// With no owner at all, `kip service credentials --repair` hands the Secret
	// to the service that should have it, which is how a password gets back to
	// the volume it was written under.
	//
	// An owner that has gone is a weaker case, and it is allowed rather than
	// recommended. The reconciler will report SecretNotOwned and ask for the
	// reference to be pointed at this Service, which does keep the password, but
	// no kip command does that yet: the audit calls a Secret with any controller
	// reference foreign and repair claims only an unowned one. Garbage collection
	// is entitled to delete the Secret by that dangling reference in the
	// meantime. Refusing would not save it, and would take away the one window
	// where an operator can act.
	//
	// A Secret this very service already owns is not a collision at all: the
	// service exists, and saying so is the other check's job.
	ref := metav1.GetControllerOf(&existing)
	if ref == nil {
		return nil
	}
	if ours(ref) && ref.Kind == "Service" && ref.Name == name {
		return nil
	}
	live, err := ownerIsLive(ctx, c, namespace, ref)
	if err != nil || !live {
		return err
	}
	return &serviceNameTakenError{Service: name}
}

// ownerIsLive says whether the object a controller reference names is still
// there under the same identity.
//
// A reference outlives its object: garbage collection is not instant, and a
// restore brings back a dependent whose owner came back with a new UID. Reading
// the reference alone would take both for a live claim on the name.
//
// Only the two kinds Kipper creates are checked. Anything else holding this
// Secret belongs to a controller whose objects are not ours to look up, and a
// claim that cannot be disproved is treated as real.
func ownerIsLive(ctx context.Context, c crclient.Client, namespace string, ref *metav1.OwnerReference) (bool, error) {
	if !ours(ref) {
		return true, nil
	}
	var owner crclient.Object
	switch ref.Kind {
	case "Service":
		owner = &kipperv1.Service{}
	case "App":
		owner = &kipperv1.App{}
	default:
		return true, nil
	}

	err := c.Get(ctx, types.NamespacedName{Name: ref.Name, Namespace: namespace}, owner)
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking whether %s %s still holds its credentials: %w",
			ref.Kind, ref.Name, err)
	}
	return owner.GetUID() == ref.UID, nil
}

// serviceNameTakenError is the collision itself, told apart from a cluster this
// check could not ask.
type serviceNameTakenError struct {
	Service string
	// App is empty where the object is a leftover nothing points at any more.
	App string
}

func (e *serviceNameTakenError) Error() string {
	secret := secretname.ServiceCredentials(e.Service)
	if e.App == "" {
		return fmt.Sprintf("a service named %q would keep its credentials in %s, and that secret already belongs to something else in this namespace. Pick another name for the service",
			e.Service, secret)
	}
	return fmt.Sprintf("a service named %q would keep its credentials in %s, which is where the app %q keeps its git token. Pick another name for the service",
		e.Service, secret, e.App)
}

// ours says whether a controller reference names one of Kipper's own kinds. The
// kind alone does not: Service is a core kind too, and looking a core Service up
// as a kipper.run one answers not-found, which would read as a claim that has
// lapsed when it is somebody's live object.
func ours(ref *metav1.OwnerReference) bool {
	return schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind).Group == kipperv1.GroupVersion.Group
}
