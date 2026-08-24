package handlers

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/serviceui"
	"github.com/getkipper/kipper/controller/pkg/datavolume"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// Services provides handlers for stateful service management.
type Services struct {
	Client      kubernetes.Interface
	CRClient    crclient.Client
	RESTConfig  *rest.Config
	Adjustments *Adjustments
	// Domain is the cluster's base domain (CLUSTER_DOMAIN env var,
	// e.g. "example.com"). Used to build per-service UI URLs for
	// services whose type ships a browseable UI. Empty disables
	// UI URL resolution — ServiceInfo just returns an empty ui_url.
	Domain string
}

type serviceResponse struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	Ready     string `json:"ready"`
	Storage   string `json:"storage"`
	// BlockedReason and BlockedMessage carry whichever refusal the service is
	// standing on: credentials the reconciler would not use, a name that belongs
	// to something else, or a deletion that cannot finish. Omitted entirely
	// otherwise, which is every healthy service and every service on a cluster
	// older than the conditions.
	BlockedReason  string `json:"blockedReason,omitempty"`
	BlockedMessage string `json:"blockedMessage,omitempty"`
}

type serviceInfoResponse struct {
	Name                string   `json:"name"`
	Type                string   `json:"type"`
	Host                string   `json:"host"`
	Port                string   `json:"port"`
	Username            string   `json:"username"`
	Database            string   `json:"database"`
	DefaultPrefix       string   `json:"default_prefix"`
	InjectedEnvTemplate []string `json:"injected_env_template"`
	// UIURL points at the browseable web UI for services whose
	// type ships one (MailHog inbox today; RabbitMQ Management,
	// OpenSearch Dashboards, etc. will follow). Empty when the
	// service has no UI or the cluster has no Domain configured.
	UIURL string `json:"ui_url,omitempty"`
}

// credentialsBlockage reads the reason and the remedy off a service the
// reconciler has refused, and answers empty for one it has not.
//
// Only a condition that is false is a blockage: the reconciler removes it
// entirely once the cause clears, so a true one is somebody else's convention
// and says nothing an operator has to act on.
func blockage(svc *kipperv1.Service, wanted string) (string, string) {
	// Every entry is read rather than the first, because this CRD puts no
	// uniqueness on the condition type. A restore or an edit can leave two, and
	// stopping at a stale true one would hide a live refusal. A warning is the
	// safe thing to get wrong in the direction of showing it.
	for _, condition := range svc.Status.Conditions {
		if condition.Type != wanted || condition.Status != metav1.ConditionFalse {
			continue
		}
		return condition.Reason, condition.Message
	}
	return "", ""
}

// List returns all Kipper-managed stateful services.
// GET /api/v1/services?namespace=blog-test
func (s *Services) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var serviceList kipperv1.ServiceList
	namespace := r.URL.Query().Get("namespace")
	var listOpts []crclient.ListOption
	if namespace != "" {
		listOpts = append(listOpts, crclient.InNamespace(namespace))
	}
	if err := s.CRClient.List(ctx, &serviceList, listOpts...); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list services")
		return
	}

	services := make([]serviceResponse, 0, len(serviceList.Items))
	for _, svc := range serviceList.Items {
		// Only surface services in projects the caller belongs to.
		if !canAccessNamespace(r, svc.Namespace) {
			continue
		}

		status := strings.ToLower(svc.Status.Phase)
		if status == "" {
			status = "pending"
		}
		// A service being deleted keeps the phase it had until it goes, and
		// destroying its data takes as long as the workload takes to stop. Left
		// as "running" it reads as a delete that did nothing.
		if !svc.DeletionTimestamp.IsZero() {
			status = "deleting"
		}

		reason, message := blockage(&svc, kipperv1.ConditionCredentialsReady)
		if reason == "" {
			reason, message = blockage(&svc, kipperv1.ConditionNameFree)
		}
		// A delete that has stopped on something no retry clears would otherwise
		// sit at "deleting" for good with the reason only in the controller's
		// log.
		if !svc.DeletionTimestamp.IsZero() {
			if stuck, why := blockage(&svc, kipperv1.ConditionCleanupComplete); stuck != "" {
				reason, message = stuck, why
			}
		}

		ready := "0/1"
		if status == "running" {
			ready = "1/1"
		}

		services = append(services, serviceResponse{
			Name:           svc.Name,
			Namespace:      svc.Namespace,
			Type:           svc.Spec.Type,
			Status:         status,
			Ready:          ready,
			Storage:        svc.Spec.Storage,
			BlockedReason:  reason,
			BlockedMessage: message,
		})
	}

	respondJSON(w, http.StatusOK, services)
}

// Delete removes a stateful service and its associated resources.
// DELETE /api/v1/services/{name}?namespace={ns}&confirm=true
func (s *Services) Delete(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	if r.URL.Query().Get("confirm") != "true" {
		respondError(w, http.StatusBadRequest, "confirm=true query parameter required to delete a stateful service")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	svc, findErr := s.findServiceCR(ctx, name, namespace)
	if findErr != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("service %q not found", name))
		return
	}

	// A service already on its way out cannot be marked: the API server rejects
	// a finalizer added to an object that is being deleted, so the request would
	// fail on the patch and report nothing useful. What happens to its volume
	// was settled by whoever deleted it.
	if !svc.DeletionTimestamp.IsZero() {
		respondError(w, http.StatusConflict, fmt.Sprintf(
			"service %q is already being deleted; kip service list says what is holding it up", name))
		return
	}

	ns := svc.Namespace

	// Unbind before deleting. Cleaning up afterwards cannot be retried — the
	// endpoint answers 404 once the CR is gone — and a workload left declaring a
	// binding to a service that no longer exists now fails its reconcile rather
	// than merely losing env, so a silent partial cleanup strands it with no
	// signal. Doing this first makes the whole operation retryable: if it fails,
	// nothing has been deleted yet. The reverse order is benign if the delete
	// below fails — the workloads are unbound and the service is still there.
	if err := s.clearBindingsTo(ctx, name, ns); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to remove bindings to %q: %v", name, err))
		return
	}

	// Ask for the data before deleting, because the volume outlives the CR:
	// nothing owns a claim a StatefulSet built from its template. The finalizer
	// destroys it, which is the only place that can wait for the workload to
	// stop. This handler cannot: it is capped well below the time a database
	// takes to go, and once the CR has left it answers 404, so a cleanup it did
	// itself could never be retried.
	//
	// Both calls are pinned to the service that was read: the mark carries its
	// resourceVersion and the delete its UID, so neither can land on a service
	// somebody created under the same name in between. The data goes with
	// whichever one the operator confirmed or with none at all.
	marked := svc.DeepCopy()
	if marked.Annotations == nil {
		marked.Annotations = map[string]string{}
	}
	marked.Annotations[datavolume.DeleteAnnotation] = "true"
	// The finalizer goes on with the mark. A service the controller has never
	// reconciled carries neither, and the API server would take it away before
	// anything had read the mark, leaving the volume with nothing left to
	// remove it and the console reporting the data destroyed.
	//
	// It is the data finalizer rather than the cleanup one, because a controller
	// from the build before this one knows the cleanup finalizer and would take
	// it off without ever looking at the mark.
	controllerutil.AddFinalizer(marked, controllers.DataFinalizer)
	lock := crclient.MergeFromWithOptions(svc, crclient.MergeFromWithOptimisticLock{})
	if err := s.CRClient.Patch(ctx, marked, lock); err != nil {
		// The same interference the delete below reports, one call earlier, and
		// it gets the same answer: somebody wrote to this service between the
		// read and the mark, so the service is still there. What ran before this
		// is the unbinding, which is safe to leave done and safe to repeat.
		if errors.IsConflict(err) {
			respondError(w, http.StatusConflict, fmt.Sprintf(
				"service %q changed while it was being deleted, so this request did not delete it; check where it stands and try again", name))
			return
		}
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to mark %q for data deletion: %v", name, err))
		return
	}

	// Delete the CR — owner references cascade to StatefulSet, Service, and Secret.
	//
	// Pinned to the version the mark landed at as well as to the service, so a
	// writer who takes the mark off in between cannot leave this answering that
	// the data went while the finalizer keeps the volume.
	decided := marked.ResourceVersion
	if err := s.CRClient.Delete(ctx, svc, crclient.Preconditions{UID: &svc.UID, ResourceVersion: &decided}); err != nil {
		switch {
		case errors.IsNotFound(err):
			respondError(w, http.StatusNotFound, fmt.Sprintf("service %q not found", name))
		case errors.IsConflict(err):
			// Only that this delete was not the one taken. Another may have been
			// accepted a moment earlier and be running now.
			respondError(w, http.StatusConflict, fmt.Sprintf(
				"service %q changed while it was being deleted, so this request did not delete it; check where it stands and try again", name))
		default:
			respondError(w, http.StatusInternalServerError, "failed to delete service")
		}
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// clearBindingsTo removes every binding naming this service from both workload
// kinds, before the CR is deleted, so the console path stays retryable with
// nothing destroyed if it fails.
//
// The Service's finalizer does the same thing on its way out, which is what
// covers kubectl and GitOps as well. This runs first rather than instead: by
// the time the finalizer runs the CR is already deleting, and a failure there
// is only visible in the controller log.
func (s *Services) clearBindingsTo(ctx context.Context, service, namespace string) error {
	return controllers.ClearBindingsToService(ctx, s.CRClient, service, namespace)
}

// Info returns connection details for a service (without password).
// GET /api/v1/services/{name}?namespace={ns}
func (s *Services) Info(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	svc, err := s.findServiceCR(ctx, name, namespace)
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("service %q not found", name))
		return
	}
	ns := svc.Namespace

	secretName := svc.Status.CredentialsSecret
	if secretName == "" {
		secretName = secretname.ServiceCredentials(name)
	}

	secret, err := s.Client.CoreV1().Secrets(ns).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get credentials")
		return
	}

	prefix := kipperv1.DefaultBindingPrefix(svc.Spec.Type)
	uiURL := ""
	if serviceui.Browseable(svc.Spec.Type) {
		if host := serviceui.Hostname(name, ns, s.Domain); host != "" {
			uiURL = "https://" + host
		}
	}
	host := string(secret.Data["HOST"])
	port := string(secret.Data["PORT"])
	username := string(secret.Data["USERNAME"])
	if svc.Spec.Type == "minio" {
		// S3 credentials: surface the endpoint's host/port and the
		// access key under the same generic connection fields.
		if u, err := url.Parse(string(secret.Data["ENDPOINT"])); err == nil {
			host = u.Hostname()
			port = u.Port()
		}
		username = string(secret.Data["ACCESS_KEY"])
	}
	respondJSON(w, http.StatusOK, serviceInfoResponse{
		Name:                name,
		Type:                svc.Spec.Type,
		Host:                host,
		Port:                port,
		Username:            username,
		Database:            string(secret.Data["NAME"]),
		DefaultPrefix:       prefix,
		InjectedEnvTemplate: kipperv1.InjectedEnvNames(svc.Spec.Type, prefix),
		UIURL:               uiURL,
	})
}

// findServiceCR looks up a Service CR scoped to a namespace. Two
// services may share a name across projects, so callers must provide
// the namespace explicitly — the frontend always knows it from the
// list view.
func (s *Services) findServiceCR(ctx context.Context, name, namespace string) (*kipperv1.Service, error) {
	if namespace == "" {
		return nil, fmt.Errorf("namespace is required to look up service %q", name)
	}
	var svc kipperv1.Service
	if err := s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &svc); err != nil {
		return nil, err
	}
	return &svc, nil
}
