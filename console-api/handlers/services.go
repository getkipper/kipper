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

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/serviceui"
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
	// BlockedReason and BlockedMessage carry the CredentialsReady condition
	// where the reconciler has refused this service's credentials. Omitted
	// entirely otherwise, which is every healthy service and every service on a
	// cluster older than the condition.
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
func credentialsBlockage(svc *kipperv1.Service) (string, string) {
	// Every entry is read rather than the first, because this CRD puts no
	// uniqueness on the condition type. A restore or an edit can leave two, and
	// stopping at a stale true one would hide a live refusal. A warning is the
	// safe thing to get wrong in the direction of showing it.
	for _, condition := range svc.Status.Conditions {
		if condition.Type != kipperv1.ConditionCredentialsReady || condition.Status != metav1.ConditionFalse {
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

		ready := "0/1"
		if status == "running" {
			ready = "1/1"
		}

		reason, message := credentialsBlockage(&svc)
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

	// Delete the CR — owner references cascade to StatefulSet, Service, and Secret
	if err := s.CRClient.Delete(ctx, svc); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("service %q not found", name))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to delete service")
		return
	}

	// Best-effort cleanup of PVCs (not owned by the CR since they're in the VCT)
	pvcs, pvcErr := s.Client.CoreV1().PersistentVolumeClaims(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", name),
	})
	if pvcErr == nil {
		for _, pvc := range pvcs.Items {
			_ = s.Client.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, pvc.Name, metav1.DeleteOptions{})
		}
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
