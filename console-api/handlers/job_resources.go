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
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
	quotapkg "github.com/getkipper/kipper/console-api/quota"
)

// GetResources returns what a job runs with.
// GET /api/v1/jobs/{name}/resources
func (j *Jobs) GetResources(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	job, ok := j.resolveJob(ctx, w, r, chi.URLParam(r, "name"), "kipper.read")
	if !ok {
		return
	}
	j.Resources.getResources(w, r, job.Namespace, job.Name, ResourceKindJob)
}

// UpdateResources sets what a job runs with.
// PUT /api/v1/jobs/{name}/resources
//
// The write lands on the Job CR rather than the CronJob it produces, because
// the reconciler rebuilds that template from the CR and a direct edit survives
// only until the next pass.
func (j *Jobs) UpdateResources(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	job, ok := j.resolveJob(ctx, w, r, chi.URLParam(r, "name"), "kipper.write")
	if !ok {
		return
	}
	j.Resources.updateResources(w, r, job.Namespace, job.Name, ResourceKindJob)
}

// GetServiceResources returns the resource limits for a service StatefulSet.
// GET /api/v1/services/{name}/resources?namespace={ns}
func (s *Services) GetResources(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	resp := resourcesResponse{PartialEdits: true}

	ss, err := s.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		respondJSON(w, http.StatusOK, resp)
		return
	}
	if len(ss.Spec.Template.Spec.Containers) > 0 {
		resp = extractResources(ss.Spec.Template.Spec.Containers[0])
		resp.PartialEdits = true
		var svc kipperv1.Service
		if s.CRClient != nil && s.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: name}, &svc) == nil {
			r := svc.Spec.Resources
			if spec, err := resourcebounds.OwnedSpec(r.CPURequest, r.CPULimit, r.MemoryRequest, r.MemoryLimit); err == nil {
				resp.CPU, resp.Memory = describeResources(spec, &ss.Spec.Template.Spec.Containers[0].Resources,
					tuningRecommendation(ctx, s.CRClient, "Service", &svc))
			}
		}
	}

	respondJSON(w, http.StatusOK, resp)
}

// UpdateResources saves a service's bounds on its Service CR. The reconciler
// applies them to the StatefulSet; direct edits there would be overwritten.
// PUT /api/v1/services/{name}/resources?namespace={ns}
func (s *Services) UpdateResources(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	var req resourcesRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateResourceQuantities(req); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateRequestWithinLimit(req); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	ss, err := s.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		respondError(w, http.StatusNotFound, "service not found")
		return
	}
	if len(ss.Spec.Template.Spec.Containers) == 0 {
		respondError(w, http.StatusInternalServerError, "statefulset has no containers")
		return
	}

	// Snapshot the running limits so the telemetry log can record "from".
	// Service StatefulSets carry resources on the first container by convention.
	prev := ss.Spec.Template.Spec.Containers[0].Resources
	prevMem, prevCPU := "", ""
	if v, ok := prev.Limits[corev1.ResourceMemory]; ok {
		prevMem = v.String()
	}
	if v, ok := prev.Limits[corev1.ResourceCPU]; ok {
		prevCPU = v.String()
	}

	edits := resourcebounds.Edits{
		CPU:    pairEdit(req.CPURequest, req.CPULimit),
		Memory: pairEdit(req.MemoryRequest, req.MemoryLimit),
	}
	_, cpuLim := setValues(edits.CPU)
	_, memLim := setValues(edits.Memory)

	// Preflight against the namespace quota (StatefulSets replace in place, no
	// surge) so an over-quota change returns 409 here instead of stalling the
	// rollout at admission. Bounds are priced at the size the container keeps.
	change := projectedChange(edits, prev)
	if pf, err := quotapkg.PreflightStatefulSet(ctx, s.Client, namespace, name, change); err == nil && !pf.Fits {
		respondError(w, http.StatusConflict, fmt.Sprintf("resource change needs %s of %s but the namespace quota caps at %s; raise the project tier or environment quota, or reduce other workloads", pf.Projected, pf.Dimension, pf.Hard))
		return
	}

	if err := resourcebounds.WriteQuantities(ctx, s.CRClient, &kipperv1.Service{}, namespace, name, resourcebounds.ConsoleManager, edits); err != nil {
		switch {
		case errors.IsNotFound(err):
			respondError(w, http.StatusNotFound, "service not found")
		case errors.IsConflict(err):
			respondError(w, http.StatusConflict, "this service changed while the limits were being saved; read it again and reapply them")
		default:
			respondError(w, http.StatusInternalServerError, "failed to update service resources")
		}
		return
	}

	subject := SubjectFromRequest(r)
	if memLim != "" {
		s.Adjustments.Record(ctx, "service", namespace, name, "memory", prevMem, memLim, "", subject)
	}
	if cpuLim != "" {
		s.Adjustments.Record(ctx, "service", namespace, name, "cpu", prevCPU, cpuLim, "", subject)
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// GetServiceRolloutStatus returns whether a service StatefulSet has finished rolling out.
// GET /api/v1/services/{name}/rollout?namespace={ns}
func (s *Services) RolloutStatus(w http.ResponseWriter, r *http.Request) {
	name, namespace, ok := requireService(w, r)
	if !ok {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	ss, err := s.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		respondError(w, http.StatusNotFound, "service not found")
		return
	}

	desired := int32(1)
	if ss.Spec.Replicas != nil {
		desired = *ss.Spec.Replicas
	}

	ready := ss.Status.ObservedGeneration >= ss.Generation &&
		ss.Status.ReadyReplicas == desired &&
		ss.Status.UpdatedReplicas == desired &&
		ss.Status.CurrentRevision == ss.Status.UpdateRevision

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"ready":   ready,
		"desired": desired,
		"updated": ss.Status.UpdatedReplicas,
		"current": ss.Status.ReadyReplicas,
	})
}

// extractResources reads resource limits/requests from a container spec.
func extractResources(container corev1.Container) resourcesResponse {
	resp := resourcesResponse{}
	if container.Resources.Limits != nil {
		if m, ok := container.Resources.Limits[corev1.ResourceMemory]; ok {
			resp.MemoryLimit = m.String()
		}
		if c, ok := container.Resources.Limits[corev1.ResourceCPU]; ok {
			resp.CPULimit = c.String()
		}
	}
	if container.Resources.Requests != nil {
		if m, ok := container.Resources.Requests[corev1.ResourceMemory]; ok {
			resp.MemoryRequest = m.String()
		}
		if c, ok := container.Resources.Requests[corev1.ResourceCPU]; ok {
			resp.CPURequest = c.String()
		}
	}
	return resp
}
