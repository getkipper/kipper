package migration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
)

// migrateApps exports App CRs from the source cluster, strips custom domains
// (apps get temporary kipper.run URLs on the target), and creates them on the
// target. Original route configs are saved in the session for Phase 5 cutover.
func (h *Handler) migrateApps(ctx context.Context, session *Session, token *Token, namespace string, res *domainResolution) error {
	var appList kipperv1.AppList
	if err := h.CRClient.List(ctx, &appList, crclient.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing apps: %w", err)
	}

	if len(appList.Items) == 0 {
		return nil
	}

	stepName := fmt.Sprintf("Creating apps on target (%s)", namespace)
	session.AddStep(Step{
		Name:       stepName,
		Phase:      "resources",
		Status:     StepRunning,
		BytesTotal: int64(len(appList.Items)),
	})

	for i, app := range appList.Items {
		if session.IsCancelled() {
			return fmt.Errorf("migration cancelled")
		}

		specJSON, _ := json.Marshal(app.Spec)
		var specMap map[string]interface{}
		_ = json.Unmarshal(specJSON, &specMap)

		// Env references to coexisting source hosts move to their target
		// equivalents. Every app is rewritten, not only those with a route,
		// because an app can name another app's host (App.Spec.Env is mirrored
		// to app-<app>-env by the target reconciler). Empty rewrite table (Mode B)
		// leaves values untouched.
		rewriteSpecEnv(specMap, res)

		// A git app rebuilds on the target, so it must carry whatever its build
		// needed. Per-app limits travel in the spec, but a cluster default lives
		// in this cluster's deployment config and does not, so an app relying on
		// one would arrive somewhere that has never heard of it and OOM on its
		// first build.
		materialiseBuildDefaults(&appList.Items[i], specMap)

		// Decide the route disposition. Only a mover's route is saved for the
		// Phase 5 cutover; coexist and gateway hosts are never saved, so cutover
		// and the DNS screen act on movers alone. Every host is stripped — the
		// primary and any redirectFrom hosts — so the target's AppReconciler
		// assigns its own derived host during verification (deleting the whole
		// route would drop the Ingress and leave nothing to verify on; security
		// headers and rate limits stay live). Leaving redirectFrom in place
		// would make the target claim and answer for a coexist app's redirect
		// hosts while the source still serves them.
		if app.Spec.Route != nil && (app.Spec.Route.Host != "" || len(app.Spec.Route.RedirectFrom) > 0) {
			key := namespace + "/" + app.Name
			routeJSON, _ := json.Marshal(app.Spec.Route)
			if res != nil && res.byApp[key] == dispositionMove {
				var routeMap map[string]interface{}
				_ = json.Unmarshal(routeJSON, &routeMap)
				session.SaveRoute(key, routeMap)
			}
			var stripped map[string]interface{}
			_ = json.Unmarshal(routeJSON, &stripped)
			delete(stripped, "host")
			delete(stripped, "redirectFrom")
			specMap["route"] = stripped
		}

		if err := h.sendToTarget(token, fmt.Sprintf("/api/v1/migrate-target/%s/resource", session.ID), map[string]interface{}{
			"kind":      "App",
			"name":      app.Name,
			"namespace": namespace,
			"spec":      specMap,
		}); err != nil {
			session.UpdateStep(stepName, func(s *Step) {
				s.Status = StepFailed
				s.Error = fmt.Sprintf("failed to create %s: %v", app.Name, err)
			})
			return fmt.Errorf("creating app %s on target: %w", app.Name, err)
		}

		session.UpdateStep(stepName, func(s *Step) {
			s.BytesDone = int64(i + 1)
			s.Detail = fmt.Sprintf("%d/%d apps (%s)", i+1, len(appList.Items), app.Name)
		})
	}

	session.UpdateStep(stepName, func(s *Step) {
		s.Status = StepCompleted
		now := time.Now()
		s.CompletedAt = &now
	})

	return nil
}

// materialiseBuildDefaults writes this cluster's build limits into a migrated
// git app that has none of its own, so the app is self-contained on the target.
// An app with its own limits is left alone: those already travel in the spec.
func materialiseBuildDefaults(app *kipperv1.App, specMap map[string]interface{}) {
	if app.Spec.Git == nil {
		return
	}
	cpu, memory := builder.ClusterBuildDefaults()
	if cpu == "" && memory == "" {
		return
	}
	gitMap, ok := specMap["git"].(map[string]interface{})
	if !ok {
		return
	}
	existing, _ := gitMap["buildResources"].(map[string]interface{})
	resources := map[string]interface{}{}
	for k, v := range existing {
		resources[k] = v
	}
	// Presence is not the test: the builder picks the first value that parses
	// as a positive quantity, so an app carrying "0" or a typo is already
	// building on the cluster default here and would silently drop to the
	// built-in one on the target.
	usable := func(key string) bool {
		v, _ := resources[key].(string)
		return builder.UsableBuildQuantity(v)
	}
	if memory != "" && !usable("memory") {
		resources["memory"] = memory
	}
	if cpu != "" && !usable("cpu") {
		resources["cpu"] = cpu
	}
	if len(resources) > 0 {
		gitMap["buildResources"] = resources
	}
}

// migrateFunctions exports Function CRs from the source and creates them on
// the target with temporary kipper.run endpoints.
func (h *Handler) migrateFunctions(ctx context.Context, session *Session, token *Token, namespace string, res *domainResolution) error {
	var fnList kipperv1.FunctionList
	if err := h.CRClient.List(ctx, &fnList, crclient.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing functions: %w", err)
	}

	if len(fnList.Items) == 0 {
		return nil
	}

	stepName := fmt.Sprintf("Creating functions on target (%s)", namespace)
	session.AddStep(Step{
		Name:       stepName,
		Phase:      "resources",
		Status:     StepRunning,
		BytesTotal: int64(len(fnList.Items)),
	})

	for i, fn := range fnList.Items {
		if session.IsCancelled() {
			return fmt.Errorf("migration cancelled")
		}

		specJSON, _ := json.Marshal(fn.Spec)
		var specMap map[string]interface{}
		_ = json.Unmarshal(specJSON, &specMap)
		rewriteSpecEnv(specMap, res)

		if err := h.sendToTarget(token, fmt.Sprintf("/api/v1/migrate-target/%s/resource", session.ID), map[string]interface{}{
			"kind":      "Function",
			"name":      fn.Name,
			"namespace": namespace,
			"spec":      specMap,
		}); err != nil {
			session.UpdateStep(stepName, func(s *Step) {
				s.Status = StepFailed
				s.Error = fmt.Sprintf("failed to create %s: %v", fn.Name, err)
			})
			return fmt.Errorf("creating function %s on target: %w", fn.Name, err)
		}

		session.UpdateStep(stepName, func(s *Step) {
			s.BytesDone = int64(i + 1)
			s.Detail = fmt.Sprintf("%d/%d functions (%s)", i+1, len(fnList.Items), fn.Name)
		})
	}

	session.UpdateStep(stepName, func(s *Step) {
		s.Status = StepCompleted
		now := time.Now()
		s.CompletedAt = &now
	})

	return nil
}

// migrateJobs exports Job CRs from the source and creates them on the target.
func (h *Handler) migrateJobs(ctx context.Context, session *Session, token *Token, namespace string, res *domainResolution) error {
	var jobList kipperv1.JobList
	if err := h.CRClient.List(ctx, &jobList, crclient.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing jobs: %w", err)
	}

	if len(jobList.Items) == 0 {
		return nil
	}

	stepName := fmt.Sprintf("Creating jobs on target (%s)", namespace)
	session.AddStep(Step{
		Name:       stepName,
		Phase:      "resources",
		Status:     StepRunning,
		BytesTotal: int64(len(jobList.Items)),
	})

	for i, job := range jobList.Items {
		if session.IsCancelled() {
			return fmt.Errorf("migration cancelled")
		}

		specJSON, _ := json.Marshal(job.Spec)
		var specMap map[string]interface{}
		_ = json.Unmarshal(specJSON, &specMap)
		rewriteSpecEnv(specMap, res)

		if err := h.sendToTarget(token, fmt.Sprintf("/api/v1/migrate-target/%s/resource", session.ID), map[string]interface{}{
			"kind":      "Job",
			"name":      job.Name,
			"namespace": namespace,
			"spec":      specMap,
		}); err != nil {
			session.UpdateStep(stepName, func(s *Step) {
				s.Status = StepFailed
				s.Error = fmt.Sprintf("failed to create %s: %v", job.Name, err)
			})
			return fmt.Errorf("creating job %s on target: %w", job.Name, err)
		}

		session.UpdateStep(stepName, func(s *Step) {
			s.BytesDone = int64(i + 1)
			s.Detail = fmt.Sprintf("%d/%d jobs (%s)", i+1, len(jobList.Items), job.Name)
		})
	}

	session.UpdateStep(stepName, func(s *Step) {
		s.Status = StepCompleted
		now := time.Now()
		s.CompletedAt = &now
	})

	return nil
}

// defaultHealthTimeout gives a fresh target enough room to pull every app
// image over its own uplink; a plain deployment is ready long before this.
const defaultHealthTimeout = 10 * time.Minute

// healthTimeout returns how long waitForHealthy polls before failing the
// migration, honouring the KIPPER_MIGRATION_HEALTH_TIMEOUT override (a Go
// duration such as "20m").
func healthTimeout() time.Duration {
	if v := os.Getenv("KIPPER_MIGRATION_HEALTH_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return defaultHealthTimeout
}

// waitForHealthy polls the target cluster until all Deployments in the given
// namespaces are ready (at least 1 ready replica each, scale-to-zero apps
// excluded on the target side).
func (h *Handler) waitForHealthy(session *Session, token *Token, namespaces []string) error {
	stepName := "Verifying health on target"
	session.AddStep(Step{
		Name:   stepName,
		Phase:  "verification",
		Status: StepRunning,
	})

	timeout := healthTimeout()
	deadline := time.Now().Add(timeout)
	var missing []string

	for {
		if session.IsCancelled() {
			return fmt.Errorf("migration cancelled")
		}

		allReady := true
		missing = nil
		for _, ns := range namespaces {
			resp, err := h.callTarget(token, "GET", fmt.Sprintf("/api/v1/migrate-target/%s/status?namespace=%s", session.ID, ns), nil)
			if err != nil {
				allReady = false
				break
			}
			depsReady, _ := resp["deployments_ready"].(bool)
			stsReady, _ := resp["statefulsets_ready"].(bool)
			// A CR that produced no workload at all is not covered by either
			// readiness flag, because both only inspect what exists. Health has
			// to mean every resource this run sent is running, or a migration
			// that lost an app reports success and the operator finds out from
			// their users.
			if reported, ok := resp["missing_workloads"].([]interface{}); ok {
				for _, name := range reported {
					if named, ok := name.(string); ok {
						missing = append(missing, named)
					}
				}
			}
			if !depsReady || !stsReady || len(missing) > 0 {
				allReady = false
				break
			}
		}

		if allReady {
			session.UpdateStep(stepName, func(s *Step) {
				s.Status = StepCompleted
				s.Detail = "All deployments and services healthy"
				now := time.Now()
				s.CompletedAt = &now
			})
			return nil
		}

		if time.Now().After(deadline) {
			break
		}

		remaining := time.Until(deadline).Round(time.Second)
		session.UpdateStep(stepName, func(s *Step) {
			s.Detail = fmt.Sprintf("Waiting for pods to start (%s left before timeout)", remaining)
		})

		time.Sleep(2 * time.Second)
	}

	// A resource that never produced a workload is a different failure from a
	// pod that is slow to start, and a longer timeout will not fix it, so the
	// two say different things.
	if len(missing) > 0 {
		detail := fmt.Sprintf("these resources reached the target but never started a workload: %s", strings.Join(missing, ", "))
		session.UpdateStep(stepName, func(s *Step) {
			s.Status = StepFailed
			s.Error = detail
		})
		return fmt.Errorf("health check failed: %s", detail)
	}

	session.UpdateStep(stepName, func(s *Step) {
		s.Status = StepFailed
		s.Error = fmt.Sprintf("timed out waiting for target deployments to be healthy after %s; raise KIPPER_MIGRATION_HEALTH_TIMEOUT if the target needs longer", timeout)
	})
	return fmt.Errorf("health check timed out after %s", timeout)
}
