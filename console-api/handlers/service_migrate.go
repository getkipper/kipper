package handlers

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/handlers/migrationjob"
)

// MigrateDataRequest is the body of POST .../migrate-data.
type migrateDataRequest struct {
	// SourceNamespace is the namespace the source service lives in,
	// typically `<project>-<source-env>`. The source service must share
	// the target service's name (env-copy preserves names, so this is
	// the natural case).
	SourceNamespace string `json:"source_namespace"`
	// Confirm must equal the target service name. A server-side guard
	// against accidental triggers — even if the frontend forgets the
	// type-the-name modal, the API stays safe.
	Confirm string `json:"confirm"`
}

// supportedMigrationTypes lists the service types that have a builder
// registered. Keep alphabetical for predictable error messages.
var supportedMigrationTypes = map[string]bool{
	"postgres": true,
}

// MigrateData kicks off a one-shot data migration from a same-typed
// service in another namespace into this service. The operation is
// destructive — the target database is dropped+recreated before the
// restore runs. Callers must pass `confirm` equal to the target service
// name.
//
// POST /api/v1/services/{name}/migrate-data?namespace=<target-ns>
func (s *Services) MigrateData(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	var req migrateDataRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.SourceNamespace == "" {
		respondError(w, http.StatusBadRequest, "source_namespace is required")
		return
	}
	if req.SourceNamespace == namespace {
		respondError(w, http.StatusBadRequest, "source_namespace must differ from the target")
		return
	}
	// The target namespace is gated by the route wrapper, but the source is
	// supplied in the body. Copying another project's data requires deploy
	// access to the source project too.
	if !enforceCapability(w, r, req.SourceNamespace, "kipper.write") {
		return
	}
	if req.Confirm != name {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("confirm must equal the target service name %q to authorise this destructive operation", name))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	target, err := s.findServiceCR(ctx, name, namespace)
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("target service %q not found in %s", name, namespace))
		return
	}

	source, err := s.findServiceCR(ctx, name, req.SourceNamespace)
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("source service %q not found in %s", name, req.SourceNamespace))
		return
	}

	if source.Spec.Type != target.Spec.Type {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("type mismatch: source is %q, target is %q", source.Spec.Type, target.Spec.Type))
		return
	}
	if !supportedMigrationTypes[target.Spec.Type] {
		respondError(w, http.StatusNotImplemented, fmt.Sprintf("data migration for service type %q is not implemented yet", target.Spec.Type))
		return
	}

	spec := buildMigrationSpec(target, req.SourceNamespace)
	jobName, err := migrationjob.Submit(ctx, s.Client, spec)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to start migration: %v", err))
		return
	}

	respondJSON(w, http.StatusAccepted, map[string]string{
		"job_name": jobName,
		"phase":    string(migrationjob.PhasePending),
	})
}

// buildMigrationSpec dispatches to the per-type builder. Adding a new
// service type means a new branch here plus an entry in
// supportedMigrationTypes.
func buildMigrationSpec(target *kipperv1.Service, sourceNamespace string) migrationjob.Spec {
	image := serviceMigrationImage(target)

	if target.Spec.Type == "postgres" {
		return migrationjob.BuildPostgres(migrationjob.PostgresSpec{
			TargetNamespace: target.Namespace,
			TargetService:   target.Name,
			SourceNamespace: sourceNamespace,
			Image:           image,
		})
	}
	// Unreachable — supportedMigrationTypes guards before we get here.
	return migrationjob.Spec{}
}

// serviceMigrationImage picks the container image used by the migration
// Job's client tools. Defaults to whatever the target StatefulSet runs,
// since matching versions guarantees protocol/dump-format compatibility.
func serviceMigrationImage(target *kipperv1.Service) string {
	if target.Spec.Type == "postgres" {
		v := target.Spec.Version
		if v == "" {
			v = "16-alpine"
		}
		return "postgres:" + v
	}
	return ""
}

// MigrateDataStatus returns the most recent migration Job for this
// service plus its phase and tailed logs.
//
// GET /api/v1/services/{name}/migrate-data/status?namespace=<target-ns>
func (s *Services) MigrateDataStatus(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	jobs, err := s.Client.BatchV1().Jobs(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("kipper.run/migration=true,kipper.run/service=%s", name),
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list migration jobs")
		return
	}
	if len(jobs.Items) == 0 {
		respondJSON(w, http.StatusOK, migrationjob.Status{Phase: ""})
		return
	}

	// Sort newest first — the user wants the latest run.
	sort.Slice(jobs.Items, func(i, j int) bool {
		return jobs.Items[i].CreationTimestamp.After(jobs.Items[j].CreationTimestamp.Time)
	})
	latest := jobs.Items[0]

	status, err := migrationjob.GetStatus(ctx, s.Client, namespace, latest.Name)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to read job status: %v", err))
		return
	}
	respondJSON(w, http.StatusOK, status)
}
