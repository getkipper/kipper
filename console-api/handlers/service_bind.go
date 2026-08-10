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
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// defaultPrefix is a thin shim for kipperv1.DefaultBindingPrefix kept so
// existing call sites can switch over without churn. The canonical mapping
// lives in api/v1alpha1/service_bindings.go.
func defaultPrefix(svcType string) string {
	return kipperv1.DefaultBindingPrefix(svcType)
}

type bindRequest struct {
	Service   string `json:"service"`
	App       string `json:"app"`
	Namespace string `json:"namespace"`
	Prefix    string `json:"prefix,omitempty"`
	Database  string `json:"database,omitempty"`
	// Target picks which CR carries the ServiceBindings entry. "" or
	// "app" target the App CR (the original behaviour); "function"
	// targets a Function CR. The App field carries the target name in
	// either case.
	Target string `json:"target,omitempty"`
}

type bindResponse struct {
	Service  string            `json:"service"`
	App      string            `json:"app"`
	Type     string            `json:"type"`
	Database string            `json:"database,omitempty"`
	Injected map[string]string `json:"injected"`
}

// Bind injects a service's connection details into an app's environment.
// For database services (postgres, mysql, mongodb), a per-app database is
// created automatically and a per-binding credentials secret is used.
// POST /api/v1/bind
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

	// A binding is contained to the app's own project-environment namespace
	// (the reconciler resolves the credentials SecretRef there), so the
	// service must live in that same namespace. Authorize before resolving so
	// bind cannot probe service existence in a project the caller has no role
	// on.
	appNamespace := req.Namespace
	if !enforceProjectRole(w, r, appNamespace, middleware.ProjectRoleDeployer) {
		return
	}
	svcType, err := s.findServiceInNamespace(ctx, req.Service, appNamespace)
	if err != nil {
		respondBindLookupError(w, err, req.Service)
		return
	}

	// Read service credentials
	creds, err := s.Client.CoreV1().Secrets(appNamespace).Get(ctx, secretname.ServiceCredentials(req.Service), metav1.GetOptions{})
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("credentials for %q not found", req.Service))
		return
	}

	// Verify the target CR (App or Function) exists.
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

	// Determine the env var prefix
	prefix := req.Prefix
	if prefix == "" {
		prefix = defaultPrefix(svcType)
	}

	// The Database field on the wire is the binding's logical-namespace
	// value: a database name for postgres/mysql/mongodb, a vhost name
	// for rabbitmq. provisionPerBinding decides whether that needs a
	// dedicated namespace + Secret, runs the side-effects, and gives
	// us back the resolved value (which it normalises — empty or
	// rabbitmq "/" both become "" so the reconciler stays on the
	// shared credentials).
	database, injected, err := s.provisionPerBinding(ctx, req.Service, appNamespace, svcType, prefix, req.Database, creds)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Add or update service binding on the right CR.
	if target == "function" {
		bindings := fnCR.Spec.ServiceBindings
		found := false
		for i := range bindings {
			if bindings[i].Name == req.Service {
				bindings[i].Prefix = prefix
				bindings[i].Database = database
				found = true
				break
			}
		}
		if !found {
			bindings = append(bindings, kipperv1.ServiceBinding{
				Name: req.Service, Prefix: prefix, Database: database,
			})
		}
		fnCR.Spec.ServiceBindings = bindings
		if err := s.CRClient.Update(ctx, &fnCR); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update function: %v", err))
			return
		}
	} else {
		bindings := appCR.Spec.ServiceBindings
		found := false
		for i := range bindings {
			if bindings[i].Name == req.Service {
				bindings[i].Prefix = prefix
				bindings[i].Database = database
				found = true
				break
			}
		}
		if !found {
			bindings = append(bindings, kipperv1.ServiceBinding{
				Name: req.Service, Prefix: prefix, Database: database,
			})
		}
		appCR.Spec.ServiceBindings = bindings
		if err := s.CRClient.Update(ctx, &appCR); err != nil {
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
	})
}

// Unbind removes a service's connection details from an app's environment.
// POST /api/v1/unbind
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
	if !enforceProjectRole(w, r, appNamespace, middleware.ProjectRoleDeployer) {
		return
	}
	// The type lookup only feeds the default env-var prefix, but it must
	// still resolve in the binding's own namespace — a same-named service in
	// another tenant's namespace must never influence which env vars get
	// removed here.
	//
	// A service that has gone is not a reason to refuse. Unbinding is exactly
	// what a workload left pointing at a deleted service needs: it fails its
	// reconcile outright until the binding goes, so refusing here would leave
	// the only way out locked.
	//
	// What the missing service costs is the prefix its type would have given,
	// and the injected variables can only be identified by it. So the cleanup
	// runs when the binding names its own prefix and is skipped otherwise,
	// rather than run against the "_" that an empty service type derives —
	// which names nothing this binding injected and could take an unrelated
	// key with it.
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

	// The workload the binding was removed from, and the binding itself, so the
	// derived-credentials cleanup below can ask whether it derived anything and
	// whether the object under that name is this workload's.
	prefix := defaultPrefix(svcType)
	cleanEnv := !serviceGone
	switch target {
	case "function":
		var fnCR kipperv1.Function
		if err := s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: appNamespace, Name: req.App}, &fnCR); err != nil {
			respondError(w, http.StatusOK, "no bindings found")
			return
		}
		for _, b := range fnCR.Spec.ServiceBindings {
			if b.Name == req.Service {
				if b.Prefix != "" {
					prefix = b.Prefix
					cleanEnv = true
				}
				break
			}
		}
		if cleanEnv && fnCR.Spec.Env != nil {
			for k := range fnCR.Spec.Env {
				if strings.HasPrefix(k, prefix) {
					delete(fnCR.Spec.Env, k)
				}
			}
		}
		var remaining []kipperv1.ServiceBinding
		for _, b := range fnCR.Spec.ServiceBindings {
			if b.Name != req.Service {
				remaining = append(remaining, b)
			}
		}
		fnCR.Spec.ServiceBindings = remaining
		if err := s.CRClient.Update(ctx, &fnCR); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update function")
			return
		}
	default:
		var appCR kipperv1.App
		if err := s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: appNamespace, Name: req.App}, &appCR); err != nil {
			respondError(w, http.StatusOK, "no bindings found")
			return
		}
		for _, b := range appCR.Spec.ServiceBindings {
			if b.Name == req.Service {
				if b.Prefix != "" {
					prefix = b.Prefix
					cleanEnv = true
				}
				break
			}
		}
		if cleanEnv && appCR.Spec.Env != nil {
			for k := range appCR.Spec.Env {
				if strings.HasPrefix(k, prefix) {
					delete(appCR.Spec.Env, k)
				}
			}
		}
		var remaining []kipperv1.ServiceBinding
		for _, b := range appCR.Spec.ServiceBindings {
			if b.Name != req.Service {
				remaining = append(remaining, b)
			}
		}
		appCR.Spec.ServiceBindings = remaining
		if err := s.CRClient.Update(ctx, &appCR); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update app environment")
			return
		}
	}

	// The derived per-binding Secret is left to the workload's own reconcile,
	// which the binding removal above triggers. Retirement there decides when it
	// goes, and waits while a retained revision or a live pod still names it —
	// deleting one here took an env source away from something that re-reads it
	// on every container restart. Its owner reference garbage-collects it if the
	// workload goes first.

	// Say when the injected variables were left behind, so an operator who
	// unbound a vanished service knows there is something to tidy rather than
	// discovering stale entries later.
	status := "unbound"
	if !cleanEnv {
		status = "unbound; the service was already gone and the binding set no prefix, so any injected variables were left in place"
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": status})
}

// InjectedEnv returns the env vars that Kubernetes injects via EnvFrom for
// an app's service bindings. These are read-only — they come from the
// credentials secret, not from Spec.Env.
// GET /api/v1/projects/{name}/apps/{app}/env/injected
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
		// Which Secret a binding injects is the reconciler's decision, and this
		// endpoint answers for the same pod, so it asks the same way. Deciding
		// from spec.database alone made this a fourth implementation of the
		// rule: a database on a service type with no logical namespace derives
		// nothing, so this looked for a Secret nothing renders, found none, and
		// dropped the whole binding from the answer while the pod had it.
		// A Service that is genuinely absent is a different answer from one that
		// could not be read. Collapsing them would let a transient failure move
		// this endpoint onto a Secret name the pod is not using, which is the
		// disagreement it exists to avoid — the reconciler stops on that error
		// rather than guessing, so this reports it rather than answering wrong.
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

// parseRabbitMQVhosts splits the `rabbitmqctl list_vhosts -q` output
// into trimmed, deduplicated vhost names with the default flag set
// on "/". Pulled out so it can be unit-tested without a cluster.
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

// findServiceInNamespace resolves a service to its type by an EXACT lookup in
// the given namespace: the Service CR first, then the StatefulSet carrying the
// kipper.run/service-type label. There is no cluster-wide fallback, so a
// service name that collides across tenants can never resolve to another
// tenant's namespace. Returns errBindServiceNotFound on a miss and a wrapped
// error on a Kubernetes API failure.
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

// ResolveBindingDatabase computes the value a binding will carry on
// the ServiceBinding CR without running any durable side effects.
// Just the service lookup and the type-aware normalisation (e.g.
// rabbitmq "/" → ""). Cheap enough to call once per binding before
// the caller writes the CR; the matching `ProvisionBinding` runs
// the side effects afterwards.
//
// Splitting resolve and provision keeps CR-write failure paths
// (CRD validation, apiserver conflict) from leaving orphaned
// per-binding Secrets or rabbitmq vhosts behind.
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

// ProvisionBinding is the entry point other handlers use to run the
// per-binding side-effects for a single binding. It resolves the
// service + credentials + default prefix, then delegates to
// provisionPerBinding. Returns the resolved (possibly normalised)
// database value the caller should write onto the ServiceBinding CR
// — e.g. rabbitmq "/" becomes "" so the reconciler stays on the
// shared service credentials.
//
// Callers that want to gate the side effects on a successful CR
// write should use ResolveBindingDatabase first (read-only) and
// invoke ProvisionBinding only after the CR is in place.
func (s *Services) ProvisionBinding(ctx context.Context, svcName, appName, appNamespace, prefix, requested string) (string, map[string]string, error) {
	// The service must live in the function's own namespace. This keeps a
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
	return s.provisionPerBinding(ctx, svcName, appNamespace, svcType, prefix, requested, creds)
}

// provisionPerBinding decides whether the bind path needs a
// per-binding logical namespace (postgres database, rabbitmq vhost),
// runs the side-effects if so, and returns the *resolved* database
// value (which the caller writes onto the ServiceBinding CR) plus
// the env map that the bound container will see via EnvFrom.
//
// Empty input, or rabbitmq's default vhost "/", maps to the shared
// service credentials — no exec, no per-binding Secret — and the
// returned database is the empty string so the reconciler stays on
// the shared `<service>-credentials` Secret.
//
// Shared between Services.Bind and InlineFunctions.Create so a
// function created in one shot picks up the same provisioning the
// edit-mode bind path would have done.
func (s *Services) provisionPerBinding(ctx context.Context, svcName, svcNamespace, svcType, prefix, requested string, creds *corev1.Secret) (string, map[string]string, error) {
	database := requested
	if svcType == "rabbitmq" && database == "/" {
		database = ""
	}
	injected := make(map[string]string)

	if database == "" || !kipperv1.HasLogicalNamespace(svcType) {
		for key, val := range creds.Data {
			if kipperv1.IsSensitiveCredentialKey(key) {
				injected[prefix+key] = "********"
			} else {
				injected[prefix+key] = string(val)
			}
		}
		return "", injected, nil
	}

	// Provision the logical namespace inside the service. Best
	// effort: pod exec may be unavailable in tests, and the
	// namespace may already exist on re-bind. The user still gets
	// a valid per-binding Secret either way.
	if err := s.createLogicalNamespace(ctx, svcName, svcNamespace, svcType, database, creds); err != nil {
		fmt.Printf("warning: could not provision %q in %s: %v\n", database, svcName, err)
	}

	// The per-binding Secret itself is rendered by the workload's reconciler
	// from the service's shared credentials, so a later password rotation
	// reaches the binding instead of leaving it on the value copied here.

	// Show what will be injected from the per-binding secret. The
	// type-specific logical-namespace key (NAME / VHOST) gets the
	// binding's value; everything else falls through from the
	// shared credentials.
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
	return database, injected, nil
}

// createLogicalNamespace provisions the per-binding logical namespace
// (a database for postgres/mysql/mongodb, a vhost for rabbitmq) on a
// running service instance via pod exec. Commands are written
// idempotently so a re-bind that lands on the same name is a no-op.
func (s *Services) createLogicalNamespace(ctx context.Context, serviceName, namespace, svcType, value string, creds *corev1.Secret) error {
	if s.RESTConfig == nil {
		return fmt.Errorf("REST config not available for pod exec")
	}

	// Find the service pod
	pods, err := s.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", serviceName),
	})
	if err != nil || len(pods.Items) == 0 {
		return fmt.Errorf("no running pod found for service %q", serviceName)
	}
	podName := pods.Items[0].Name

	cmd, err := logicalNamespaceCmd(svcType, value, creds)
	if err != nil {
		return err
	}

	req := s.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: serviceName,
			Command:   cmd,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(s.RESTConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("creating executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return fmt.Errorf("provisioning namespace: %s: %w", stderr.String(), err)
	}

	return nil
}

// logicalNamespaceCmd returns the shell command that provisions the
// per-binding logical namespace inside the running service pod.
// Extracted so unit tests can pin the exact command shape — pod exec
// itself is not unit-testable without a cluster.
//
// User-supplied values (the namespace value and the connecting
// username) are passed to `sh -c` as positional parameters and
// referenced as "$1"/"$2" inside the script. That keeps a name
// containing shell metacharacters from breaking out of quoting and
// running arbitrary commands in the service pod.
func logicalNamespaceCmd(svcType, value string, creds *corev1.Secret) ([]string, error) {
	if !validNamespaceValue(value) {
		return nil, fmt.Errorf("invalid namespace name %q", value)
	}
	username := string(creds.Data["USERNAME"])
	switch svcType {
	case "postgres":
		// Connect to the default "app" database (psql defaults to a database
		// matching the username, which may not exist).
		defaultDB := string(creds.Data["NAME"])
		if defaultDB == "" {
			defaultDB = "app"
		}
		// $1 = namespace value, $2 = username, $3 = default DB.
		return []string{
			"sh", "-c",
			`psql -U "$2" -d "$3" -tc "SELECT 1 FROM pg_database WHERE datname='$1'" | grep -q 1 || psql -U "$2" -d "$3" -c "CREATE DATABASE \"$1\""`,
			"--", value, username, defaultDB,
		}, nil
	case "mysql":
		// Go raw strings can't contain backticks; build the script in
		// two pieces so the inner MySQL backquoted identifier survives.
		return []string{
			"sh", "-c",
			`mysql -u "$2" -p"$MYSQL_ROOT_PASSWORD" -e "CREATE DATABASE IF NOT EXISTS ` + "`$1`" + `"`,
			"--", value, username,
		}, nil
	case "mongodb":
		// MongoDB creates databases implicitly on first use; just touch a collection
		return []string{
			"sh", "-c",
			`mongosh --quiet --eval "use $1; db.createCollection('_init')"`,
			"--", value,
		}, nil
	case "rabbitmq":
		// add_vhost errors when the vhost already exists, so gate it
		// on list_vhosts; permissions are set every time so a re-bind
		// with the same vhost remains a no-op for state but
		// re-asserts the grant (cheap and harmless).
		return []string{
			"sh", "-c",
			`rabbitmqctl list_vhosts -q | grep -Fxq -- "$1" || rabbitmqctl add_vhost -- "$1" && rabbitmqctl set_permissions -p "$1" "$2" ".*" ".*" ".*"`,
			"--", value, username,
		}, nil
	}
	return nil, fmt.Errorf("logical-namespace provisioning not supported for service type %q", svcType)
}

// validNamespaceValue is a belt-and-braces check on top of the shell
// quoting above. We accept alphanumeric, underscore, dash, dot, and
// slash — enough for database names and rabbitmq vhost paths like
// "/orders" — and reject anything that could carry shell or SQL
// metacharacters. Rejecting at the boundary is much easier to reason
// about than escaping each downstream tool's quoting rules.
func validNamespaceValue(value string) bool {
	if value == "" || len(value) > 63 {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == '/':
		default:
			return false
		}
	}
	return true
}
