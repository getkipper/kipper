package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/retry"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/internal/logicalns"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

func defaultPrefix(svcType string) string {
	return kipperv1.DefaultBindingPrefix(svcType)
}

type bindRequest struct {
	Service   string `json:"service"`
	App       string `json:"app"`
	Namespace string `json:"namespace"`
	Prefix    string `json:"prefix,omitempty"`
	Database  string `json:"database,omitempty"`
	Target    string `json:"target,omitempty"`
}

type bindResponse struct {
	Service  string            `json:"service"`
	App      string            `json:"app"`
	Type     string            `json:"type"`
	Database string            `json:"database,omitempty"`
	Injected map[string]string `json:"injected"`
	Warning  string            `json:"warning,omitempty"`
}

// Bind saves a service binding for an app or function and provisions any
// requested database or vhost. Provisioning failures are returned as warnings.
func (s *Services) Bind(w http.ResponseWriter, r *http.Request) {
	var req bindRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Service == "" || req.App == "" || req.Namespace == "" {
		respondError(w, http.StatusBadRequest, "service, app, and namespace are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	// Resolve credentials in the workload's namespace to keep bindings project-scoped.
	// Authorize first to avoid exposing service names in other projects.
	appNamespace := req.Namespace
	if !enforceCapability(w, r, appNamespace, "kipper.write") {
		return
	}
	svcType, err := s.findServiceInNamespace(ctx, req.Service, appNamespace)
	if err != nil {
		respondBindLookupError(w, err, req.Service)
		return
	}

	creds, err := s.Client.CoreV1().Secrets(appNamespace).Get(ctx, secretname.ServiceCredentials(req.Service), metav1.GetOptions{})
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("credentials for %q not found", req.Service))
		return
	}

	// Verify the workload exists before provisioning its database or vhost.
	var appCR kipperv1.App
	var fnCR kipperv1.Function
	target := req.Target
	if target == "" {
		target = "app"
	}
	switch target {
	case "app":
		if err := s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: appNamespace, Name: req.App}, &appCR); err != nil {
			if apierrors.IsNotFound(err) {
				respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found in %s", req.App, appNamespace))
				return
			}
			respondError(w, http.StatusInternalServerError, "failed to get app")
			return
		}
	case "function":
		if err := s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: appNamespace, Name: req.App}, &fnCR); err != nil {
			if apierrors.IsNotFound(err) {
				respondError(w, http.StatusNotFound, fmt.Sprintf("function %q not found in %s", req.App, appNamespace))
				return
			}
			respondError(w, http.StatusInternalServerError, "failed to get function")
			return
		}
	default:
		respondError(w, http.StatusBadRequest, fmt.Sprintf("unknown target %q (expected app or function)", target))
		return
	}

	prefix := req.Prefix
	if prefix == "" {
		prefix = defaultPrefix(svcType)
	}

	database, injected, warning := s.provisionPerBinding(ctx, req.Service, appNamespace, svcType, prefix, req.Database, creds)

	// Provision once, then retry binding updates against fresh reads to preserve concurrent edits.
	setBinding := func(bindings []kipperv1.ServiceBinding) []kipperv1.ServiceBinding {
		for i := range bindings {
			if bindings[i].Name == req.Service {
				bindings[i].Prefix = prefix
				bindings[i].Database = database
				return bindings
			}
		}
		return append(bindings, kipperv1.ServiceBinding{Name: req.Service, Prefix: prefix, Database: database})
	}
	if target == "function" {
		if err := updateFunction(ctx, s.CRClient, appNamespace, req.App, func(fn *kipperv1.Function) {
			fn.Spec.ServiceBindings = setBinding(fn.Spec.ServiceBindings)
		}); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update function: %v", err))
			return
		}
	} else {
		if err := updateApp(ctx, s.CRClient, appNamespace, req.App, func(app *kipperv1.App) {
			app.Spec.ServiceBindings = setBinding(app.Spec.ServiceBindings)
		}); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update app environment: %v", err))
			return
		}
	}

	respondJSON(w, http.StatusOK, bindResponse{
		Service:  req.Service,
		App:      req.App,
		Type:     svcType,
		Database: database,
		Injected: injected,
		Warning:  warning,
	})
}

// Unbind removes a service binding from an app or function and clears matching
// environment variables when the service type or binding prefix is known.
func (s *Services) Unbind(w http.ResponseWriter, r *http.Request) {
	var req bindRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Service == "" || req.App == "" || req.Namespace == "" {
		respondError(w, http.StatusBadRequest, "service, app, and namespace are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	appNamespace := req.Namespace
	if !enforceCapability(w, r, appNamespace, "kipper.write") {
		return
	}
	// Resolve service type within the binding's namespace. A deleted service
	// still permits unbinding; clean injected env keys only when the type or
	// an explicit binding prefix identifies them.
	svcType, err := s.findServiceInNamespace(ctx, req.Service, appNamespace)
	serviceGone := errors.Is(err, errBindServiceNotFound)
	if err != nil && !serviceGone {
		respondBindLookupError(w, err, req.Service)
		return
	}

	target := req.Target
	if target == "" {
		target = "app"
	}

	// Recompute cleanup from the latest binding on each conflict retry.
	var cleanEnv bool
	unbind := func(bindings []kipperv1.ServiceBinding, env map[string]string) []kipperv1.ServiceBinding {
		prefix := defaultPrefix(svcType)
		cleanEnv = !serviceGone
		var remaining []kipperv1.ServiceBinding
		for _, b := range bindings {
			if b.Name != req.Service {
				remaining = append(remaining, b)
				continue
			}
			if b.Prefix != "" {
				prefix = b.Prefix
				cleanEnv = true
			}
		}
		if cleanEnv {
			for k := range env {
				if strings.HasPrefix(k, prefix) {
					delete(env, k)
				}
			}
		}
		return remaining
	}
	what := "app environment"
	if target == "function" {
		what = "function"
		err = updateFunction(ctx, s.CRClient, appNamespace, req.App, func(fn *kipperv1.Function) {
			fn.Spec.ServiceBindings = unbind(fn.Spec.ServiceBindings, fn.Spec.Env)
		})
	} else {
		err = updateApp(ctx, s.CRClient, appNamespace, req.App, func(app *kipperv1.App) {
			app.Spec.ServiceBindings = unbind(app.Spec.ServiceBindings, app.Spec.Env)
		})
	}
	if apierrors.IsNotFound(err) {
		respondError(w, http.StatusOK, "no bindings found")
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update "+what)
		return
	}

	// Let the reconciler retire the binding Secret after retained revisions and
	// live pods release it, so existing workloads can still restart.
	status := "unbound"
	if !cleanEnv {
		status = "unbound; service missing and binding prefix unknown. Remove any leftover environment variables manually."
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": status})
}

// InjectedEnv returns an app's service-binding variables from credential Secrets,
// with sensitive values omitted.
func (s *Services) InjectedEnv(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		respondJSON(w, http.StatusOK, []injectedVar{})
		return
	}

	var vars []injectedVar
	for _, binding := range appCR.Spec.ServiceBindings {
		// Use the reconciler's Secret selection so the preview matches the workload.
		// Lookup failures must surface: treating them as missing services could select the wrong Secret.
		svcType, lookupErr := s.findServiceInNamespace(ctx, binding.Name, project)
		typeKnown := lookupErr == nil
		if lookupErr != nil && !errors.Is(lookupErr, errBindServiceNotFound) {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("looking up service %q: %v", binding.Name, lookupErr))
			return
		}
		secretName := controllers.BindingSecretName(binding, svcType, typeKnown, secretname.KindApp, app)

		secret, err := s.Client.CoreV1().Secrets(project).Get(ctx, secretName, metav1.GetOptions{})
		if err != nil {
			continue
		}
		prefix := binding.Prefix
		if prefix == "" && typeKnown {
			prefix = defaultPrefix(svcType)
		}
		for key, val := range secret.Data {
			v := injectedVar{
				Name:    prefix + key,
				Service: binding.Name,
				Secret:  kipperv1.IsSensitiveCredentialKey(key),
			}
			if !v.Secret {
				v.Value = string(val)
			}
			vars = append(vars, v)
		}
	}

	respondJSON(w, http.StatusOK, vars)
}

type injectedVar struct {
	Name    string `json:"name"`
	Service string `json:"service"`
	Secret  bool   `json:"secret"`
	Value   string `json:"value,omitempty"`
}

// errBindServiceNotFound marks a service-resolution miss so handlers can
// answer 404 for a genuine miss and 500 for a Kubernetes API failure.
var errBindServiceNotFound = errors.New("service not found")

// respondBindLookupError maps a findServiceInNamespace error to a status
// code: a resolution miss is 404, a Kubernetes API failure is 500.
func respondBindLookupError(w http.ResponseWriter, err error, service string) {
	if errors.Is(err, errBindServiceNotFound) {
		respondError(w, http.StatusNotFound, fmt.Sprintf("service %q not found", service))
		return
	}
	respondError(w, http.StatusInternalServerError, "failed to look up service")
}

// rabbitMQVhost mirrors the postgres /db/databases entry shape so
// the bind form's existing dropdown logic renders unchanged when
// pointed at this endpoint instead.
type rabbitMQVhost struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
}

// ListRabbitMQVhosts enumerates the vhosts on a running rabbitmq
// service by running `rabbitmqctl list_vhosts -q` inside the pod.
// The default vhost "/" is flagged so the picker can pre-select it.
// GET /api/v1/services/{name}/rabbitmq/vhosts?namespace={ns}
func (s *Services) ListRabbitMQVhosts(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}
	if s.RESTConfig == nil {
		respondError(w, http.StatusServiceUnavailable, "pod exec is unavailable in this environment")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var svc kipperv1.Service
	if getErr := s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &svc); getErr != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("service %q not found", name))
		return
	}
	if svc.Spec.Type != "rabbitmq" {
		respondError(w, http.StatusBadRequest, "service is not rabbitmq")
		return
	}

	pods, err := s.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", name),
	})
	if err != nil || len(pods.Items) == 0 {
		respondError(w, http.StatusServiceUnavailable, fmt.Sprintf("no running pod found for service %q", name))
		return
	}
	podName := pods.Items[0].Name

	req := s.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: name,
			Command:   []string{"rabbitmqctl", "list_vhosts", "-q"},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(s.RESTConfig, "POST", req.URL())
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("creating executor: %v", err))
		return
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdout: &stdout, Stderr: &stderr}); err != nil {
		respondError(w, http.StatusBadGateway, fmt.Sprintf("rabbitmqctl: %s: %v", stderr.String(), err))
		return
	}

	respondJSON(w, http.StatusOK, parseRabbitMQVhosts(stdout.String()))
}

// parseRabbitMQVhosts parses unique vhost names and flags "/" as the default.
func parseRabbitMQVhosts(out string) []rabbitMQVhost {
	entries := []rabbitMQVhost{}
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		v := strings.TrimSpace(line)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		entries = append(entries, rabbitMQVhost{Name: v, Default: v == "/"})
	}
	return entries
}

// findServiceInNamespace resolves a service type from its Service CR or legacy
// StatefulSet within the given namespace, keeping lookups isolated by project.
// It returns errBindServiceNotFound on a miss and wraps Kubernetes API failures.
func (s *Services) findServiceInNamespace(ctx context.Context, name, namespace string) (svcType string, err error) {
	if name == "" || namespace == "" {
		return "", errBindServiceNotFound
	}
	var svc kipperv1.Service
	err = s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &svc)
	if err == nil {
		return svc.Spec.Type, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("looking up service %q in %q: %w", name, namespace, err)
	}
	ss, err := s.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", errBindServiceNotFound
		}
		return "", fmt.Errorf("looking up service %q in %q: %w", name, namespace, err)
	}
	svcType = ss.Labels["kipper.run/service-type"]
	if svcType == "" {
		return "", errBindServiceNotFound
	}
	return svcType, nil
}

// ResolveBindingDatabase normalizes a binding's database or vhost for its service type.
// Call it before saving the workload and provision after saving, so a failed save
// leaves no newly provisioned database or vhost.
func (s *Services) ResolveBindingDatabase(ctx context.Context, svcName, appNamespace, requested string) (string, error) {
	svcType, err := s.findServiceInNamespace(ctx, svcName, appNamespace)
	if err != nil {
		if errors.Is(err, errBindServiceNotFound) {
			return "", fmt.Errorf("service %q not found", svcName)
		}
		return "", err
	}
	if svcType == "rabbitmq" && requested == "/" {
		return "", nil
	}
	if requested == "" || !kipperv1.HasLogicalNamespace(svcType) {
		return "", nil
	}
	return requested, nil
}

// ProvisionBinding provisions a binding's database or vhost and returns its
// normalized name and a credential preview with sensitive values masked.
// Call ResolveBindingDatabase before saving the workload, then call this
// method after saving so provisioning failures can be reported as warnings.
func (s *Services) ProvisionBinding(ctx context.Context, svcName, appName, appNamespace, prefix, requested string) (string, map[string]string, error) {
	// The service must live in the workload's own namespace. This keeps a
	// project-scoped binding from reading another project's credentials.
	svcType, err := s.findServiceInNamespace(ctx, svcName, appNamespace)
	if err != nil {
		if errors.Is(err, errBindServiceNotFound) {
			return "", nil, fmt.Errorf("service %q not found", svcName)
		}
		return "", nil, err
	}
	creds, err := s.Client.CoreV1().Secrets(appNamespace).Get(ctx, secretname.ServiceCredentials(svcName), metav1.GetOptions{})
	if err != nil {
		return "", nil, fmt.Errorf("credentials for %q not found: %w", svcName, err)
	}
	if prefix == "" {
		prefix = defaultPrefix(svcType)
	}
	database, injected, warning := s.provisionPerBinding(ctx, svcName, appNamespace, svcType, prefix, requested, creds)
	if warning != "" {
		return database, injected, errors.New(warning)
	}
	return database, injected, nil
}

// provisionPerBinding provisions a database or vhost and returns its normalized
// name, a credential preview with sensitive values masked, and any warning.
// An empty name or RabbitMQ's "/" uses shared service credentials, as do
// service types without logical namespaces.
func (s *Services) provisionPerBinding(ctx context.Context, svcName, svcNamespace, svcType, prefix, requested string, creds *corev1.Secret) (database string, injected map[string]string, warning string) {
	database = requested
	if svcType == "rabbitmq" && database == "/" {
		database = ""
	}
	injected = make(map[string]string)

	if database == "" || !kipperv1.HasLogicalNamespace(svcType) {
		for key, val := range creds.Data {
			if kipperv1.IsSensitiveCredentialKey(key) {
				injected[prefix+key] = "********"
			} else {
				injected[prefix+key] = string(val)
			}
		}
		return "", injected, ""
	}

	// Return provisioning failures as warnings so the binding can be saved
	// and the workload controller can retry creation.
	if perr := s.createLogicalNamespace(ctx, svcName, svcNamespace, svcType, database, creds); perr != nil {
		warning = fmt.Sprintf("database %q was not created in %s: %v", database, svcName, perr)
	}

	// The reconciler derives the binding Secret from shared credentials so it
	// follows password rotations. Return a masked preview for the response.
	nsKey := kipperv1.LogicalNamespaceKey(svcType)
	for key, val := range creds.Data {
		switch {
		case kipperv1.IsSensitiveCredentialKey(key):
			injected[prefix+key] = "********"
		case key == nsKey:
			injected[prefix+key] = database
		default:
			injected[prefix+key] = string(val)
		}
	}
	return database, injected, warning
}

func (s *Services) createLogicalNamespace(ctx context.Context, serviceName, namespace, svcType, value string, creds *corev1.Secret) error {
	return logicalns.Create(ctx, s.Client, s.RESTConfig, serviceName, namespace, svcType, value, creds)
}

// updateApp reapplies change to a freshly read App on each conflict retry.
// The callback may run multiple times; keep external side effects outside it.
func updateApp(ctx context.Context, c crclient.Client, namespace, name string, change func(*kipperv1.App)) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var app kipperv1.App
		if err := c.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &app); err != nil {
			return err
		}
		change(&app)
		return c.Update(ctx, &app)
	})
}
