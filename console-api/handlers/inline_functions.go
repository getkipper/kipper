package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/domain"
	"github.com/getkipper/kipper/controller/pkg/secretname"
)

// InlineFunctions handles creating functions from inline code via Function CRs.
type InlineFunctions struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
	// Services drives the per-binding provisioning side of Create —
	// pod exec to add a postgres database or rabbitmq vhost, and the
	// per-binding credentials Secret. Without this the create form
	// would silently write a Function CR whose bindings reference
	// Secrets that don't exist.
	Services *Services
	Domain   string
}

// inlineCreateRequest carries the full create payload from the function
// form: code + trigger + bindings + env + secrets + dependencies. The
// handler materialises the Function CR plus the env / secrets Secrets
// in one round trip so the form can deploy with a single user click.
type inlineCreateRequest struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"` // "node" or "python"
	Code    string `json:"code"`

	// Trigger config — defaults to HTTP.
	Trigger   string `json:"trigger"`
	Schedule  string `json:"schedule"`
	Source    string `json:"source"`
	Query     string `json:"query"`
	MarkDone  string `json:"mark_done"`
	RedisList string `json:"redis_list"`
	Bucket    string `json:"bucket"`

	// Configuration the form collects in one shot.
	Env          map[string]string     `json:"env,omitempty"`
	Secrets      map[string]string     `json:"secrets,omitempty"`
	Dependencies map[string]string     `json:"dependencies,omitempty"`
	Bindings     []inlineCreateBinding `json:"bindings,omitempty"`
}

// inlineCreateBinding mirrors ServiceBinding from the v1alpha1 package
// without leaking unintended JSON tags.
type inlineCreateBinding struct {
	Service  string `json:"service"`
	Prefix   string `json:"prefix,omitempty"`
	Database string `json:"database,omitempty"`
}

type inlineCodeResponse struct {
	Name    string `json:"name"`
	Runtime string `json:"runtime"`
	Code    string `json:"code"`

	// Trigger configuration so the edit form can re-populate after a
	// page refresh. Without these fields the form loads a blank
	// schedule, source query, etc, and the user has to re-enter them
	// on every save.
	Trigger   string `json:"trigger,omitempty"`
	Schedule  string `json:"schedule,omitempty"`
	Source    string `json:"source,omitempty"`
	Query     string `json:"query,omitempty"`
	MarkDone  string `json:"mark_done,omitempty"`
	RedisList string `json:"redis_list,omitempty"`
	Bucket    string `json:"bucket,omitempty"`
}

// Create creates a Function CR from inline code. The function reconciler
// handles creating the ConfigMap, Deployment, Service, and KEDA resources.
// POST /api/v1/projects/{name}/inline-functions
func (f *InlineFunctions) Create(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	var req inlineCreateRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" || req.Code == "" || req.Runtime == "" {
		respondError(w, http.StatusBadRequest, "name, runtime, and code are required")
		return
	}

	if req.Runtime != "node" && req.Runtime != "python" {
		respondError(w, http.StatusBadRequest, "runtime must be 'node' or 'python'")
		return
	}

	trigger := buildInlineTrigger(req)
	if trigger.Type == "cron" && trigger.Schedule == "" {
		respondError(w, http.StatusBadRequest, "cron trigger requires a schedule")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Two-phase binding handling so CR-write failures don't leave
	// orphaned per-binding Secrets or rabbitmq vhosts:
	//   Phase 1 (here): read-only — resolve each binding's Database
	//     value so the CR carries the right one (e.g. rabbitmq "/"
	//     normalises to "" so the reconciler stays on the shared
	//     Secret).
	//   Phase 2 (after the CR write): provision the side-effects
	//     (per-binding credentials Secret, rabbitmqctl add_vhost,
	//     CREATE DATABASE). If provisioning fails, the CR is still
	//     in place; the user can re-bind to retry.
	bindings := make([]kipperv1.ServiceBinding, 0, len(req.Bindings))
	for _, b := range req.Bindings {
		resolvedDatabase := b.Database
		if f.Services != nil {
			resolved, rerr := f.Services.ResolveBindingDatabase(ctx, b.Service, project, b.Database)
			if rerr != nil {
				respondError(w, http.StatusBadRequest, fmt.Sprintf("failed to resolve binding for %q: %v", b.Service, rerr))
				return
			}
			resolvedDatabase = resolved
		}
		bindings = append(bindings, kipperv1.ServiceBinding{
			Name: b.Service, Prefix: b.Prefix, Database: resolvedDatabase,
		})
	}

	fn := &kipperv1.Function{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: project,
			Labels: map[string]string{
				"app":                      req.Name,
				kipperLabel:                kipperValue,
				"kipper.run/resource-type": "function",
				"kipper.run/trigger":       trigger.Type,
			},
		},
		Spec: kipperv1.FunctionSpec{
			Port:    8080,
			Runtime: req.Runtime,
			Source: &kipperv1.FunctionSource{
				Code:         req.Code,
				Dependencies: req.Dependencies,
			},
			Env:             req.Env,
			ServiceBindings: bindings,
			Triggers:        []kipperv1.FunctionTrigger{trigger},
		},
	}

	release, ok := reserveWorkloadName(ctx, w, f.CRClient, project, req.Name, "function")
	if !ok {
		return
	}

	if err := f.CRClient.Create(ctx, fn); err != nil {
		if !errors.IsAlreadyExists(err) {
			release()
		}
		if errors.IsAlreadyExists(err) {
			// Update existing Function CR with new spec.
			var existing kipperv1.Function
			if getErr := f.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: req.Name}, &existing); getErr != nil {
				respondError(w, http.StatusInternalServerError, "failed to get existing function")
				return
			}
			existing.Spec.Runtime = req.Runtime
			if existing.Spec.Source == nil {
				existing.Spec.Source = &kipperv1.FunctionSource{}
			}
			existing.Spec.Source.Code = req.Code
			existing.Spec.Source.Dependencies = req.Dependencies
			existing.Spec.Env = req.Env
			existing.Spec.ServiceBindings = bindings
			existing.Spec.Triggers = []kipperv1.FunctionTrigger{trigger}
			existing.Labels = fn.Labels
			if updateErr := f.CRClient.Update(ctx, &existing); updateErr != nil {
				respondError(w, http.StatusInternalServerError, "failed to update function")
				return
			}
		} else {
			respondError(w, http.StatusInternalServerError, "failed to create function")
			return
		}
	}

	// Phase 2: per-binding side effects now that the CR is in place.
	// Failures here don't roll back the function — the user can
	// re-bind from the edit page to retry — but we surface them in
	// the response body so the form can show a non-fatal warning.
	var bindingWarnings []string
	if f.Services != nil {
		for _, b := range bindings {
			if b.Database == "" {
				continue
			}
			if _, _, perr := f.Services.ProvisionBinding(ctx, b.Name, req.Name, project, b.Prefix, b.Database); perr != nil {
				bindingWarnings = append(bindingWarnings, fmt.Sprintf("%s: %v", b.Name, perr))
			}
		}
	}

	// Materialise the secrets Secret if any were provided. Done after the
	// Function CR is in place so a stale secret never lingers if the CR
	// create fails.
	if len(req.Secrets) > 0 && f.Client != nil {
		if err := f.upsertSecrets(ctx, project, req.Name, req.Secrets); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to set secrets")
			return
		}
	}

	url := ""
	if f.Domain != "" {
		url = "https://" + domain.SubdomainFor("fn-"+req.Name, f.Domain)
	}

	resp := map[string]any{"status": "deployed", "name": req.Name, "runtime": req.Runtime, "url": url}
	if len(bindingWarnings) > 0 {
		// Non-fatal: function is in place; surface so the form can
		// nudge the user to retry the bind from the edit page.
		resp["binding_warnings"] = bindingWarnings
	}
	respondJSON(w, http.StatusOK, resp)
}

// GetCode returns the current code for an inline function from the Function CR.
// GET /api/v1/projects/{name}/inline-functions/{fn}/code
func (f *InlineFunctions) GetCode(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var fn kipperv1.Function
	if err := f.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: fnName}, &fn); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "function not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get function")
		return
	}

	code := ""
	if fn.Spec.Source != nil {
		code = fn.Spec.Source.Code
	}

	resp := inlineCodeResponse{
		Name:    fnName,
		Runtime: fn.Spec.Runtime,
		Code:    code,
	}

	if len(fn.Spec.Triggers) > 0 {
		t := fn.Spec.Triggers[0]
		resp.Trigger = t.Type
		resp.Schedule = t.Schedule
		if t.Config != nil {
			resp.Source = t.Config["source"]
			resp.Query = t.Config["query"]
			resp.MarkDone = t.Config["markDone"]
			resp.RedisList = t.Config["list"]
			resp.Bucket = t.Config["bucket"]
		}
	}

	respondJSON(w, http.StatusOK, resp)
}

// buildInlineTrigger maps the request fields onto the FunctionTrigger
// the controller reconciles. Defaults to HTTP when no trigger is given.
func buildInlineTrigger(req inlineCreateRequest) kipperv1.FunctionTrigger {
	t := req.Trigger
	if t == "" {
		t = "http"
	}
	trig := kipperv1.FunctionTrigger{Type: t}
	if t == "cron" {
		trig.Schedule = req.Schedule
		return trig
	}
	cfg := map[string]string{}
	if req.Source != "" {
		cfg["source"] = req.Source
	}
	if req.Query != "" {
		cfg["query"] = req.Query
	}
	if req.MarkDone != "" {
		cfg["markDone"] = req.MarkDone
	}
	if req.RedisList != "" {
		cfg["list"] = req.RedisList
	}
	if req.Bucket != "" {
		cfg["bucket"] = req.Bucket
	}
	if len(cfg) > 0 {
		trig.Config = cfg
	}
	return trig
}

// upsertSecrets writes the function's secrets Secret. Mirrors the rotate
// semantics of FunctionConfig.SetSecrets: existing values are preserved
// under "<key>.__previous" before being overwritten.
func (f *InlineFunctions) upsertSecrets(ctx context.Context, namespace, fnName string, values map[string]string) error {
	existing, err := f.Client.CoreV1().Secrets(namespace).Get(ctx, secretname.Secrets(secretname.KindFunction, fnName), metav1.GetOptions{})
	if errors.IsNotFound(err) {
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      secretname.Secrets(secretname.KindFunction, fnName),
				Namespace: namespace,
				Labels: map[string]string{
					kipperLabel: kipperValue,
					"app":       fnName,
				},
			},
			Data: make(map[string][]byte, len(values)),
		}
		for k, v := range values {
			secret.Data[k] = []byte(v)
		}
		_, err := f.Client.CoreV1().Secrets(namespace).Create(ctx, secret, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	if existing.Data == nil {
		existing.Data = make(map[string][]byte)
	}
	for k, v := range values {
		if cur, ok := existing.Data[k]; ok && string(cur) != v {
			existing.Data[k+".__previous"] = cur
		}
		existing.Data[k] = []byte(v)
	}
	_, err = f.Client.CoreV1().Secrets(namespace).Update(ctx, existing, metav1.UpdateOptions{})
	return err
}

// UpdateCode updates the code, runtime, and trigger config for an
// inline function. Runtime and trigger config are mutable
// post-creation because users routinely change them after first save:
// fixing a runtime that defaulted to Node when they wanted Python,
// adjusting a cron schedule, or pointing an event trigger at a
// different source. The reconciler rolls the deployment with the new
// settings on the next pass.
// PUT /api/v1/projects/{name}/inline-functions/{fn}/code
func (f *InlineFunctions) UpdateCode(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	var req struct {
		Code    string `json:"code"`
		Runtime string `json:"runtime,omitempty"`

		// Optional trigger updates. When Trigger is non-empty the
		// function's first trigger is replaced with one built from
		// these fields, mirroring the create path.
		Trigger   string `json:"trigger,omitempty"`
		Schedule  string `json:"schedule,omitempty"`
		Source    string `json:"source,omitempty"`
		Query     string `json:"query,omitempty"`
		MarkDone  string `json:"mark_done,omitempty"`
		RedisList string `json:"redis_list,omitempty"`
		Bucket    string `json:"bucket,omitempty"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Code == "" {
		respondError(w, http.StatusBadRequest, "code is required")
		return
	}
	if req.Runtime != "" && req.Runtime != "node" && req.Runtime != "python" {
		respondError(w, http.StatusBadRequest, "runtime must be 'node' or 'python'")
		return
	}
	if req.Trigger == "cron" && req.Schedule == "" {
		respondError(w, http.StatusBadRequest, "cron trigger requires a schedule")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var fn kipperv1.Function
	if err := f.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: fnName}, &fn); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, "function not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get function")
		return
	}

	if fn.Spec.Source == nil {
		fn.Spec.Source = &kipperv1.FunctionSource{}
	}
	fn.Spec.Source.Code = req.Code
	if req.Runtime != "" {
		fn.Spec.Runtime = req.Runtime
	}
	if req.Trigger != "" {
		fn.Spec.Triggers = []kipperv1.FunctionTrigger{buildInlineTrigger(inlineCreateRequest{
			Trigger:   req.Trigger,
			Schedule:  req.Schedule,
			Source:    req.Source,
			Query:     req.Query,
			MarkDone:  req.MarkDone,
			RedisList: req.RedisList,
			Bucket:    req.Bucket,
		})}
		// Keep the trigger label in sync so /functions list filters work.
		if fn.Labels == nil {
			fn.Labels = map[string]string{}
		}
		fn.Labels["kipper.run/trigger"] = req.Trigger
	}

	if err := f.CRClient.Update(ctx, &fn); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update function")
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}
