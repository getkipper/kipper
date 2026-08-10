package handlers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/console-api/middleware"
	quotapkg "github.com/getkipper/kipper/console-api/quota"
)

// GetJobResources returns the resource limits for a CronJob.
// GET /api/v1/jobs/{name}/resources
func (j *Jobs) GetResources(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	resp := resourcesResponse{}

	cj, err := j.Client.BatchV1().CronJobs("").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=kipper",
	})
	if err != nil {
		respondJSON(w, http.StatusOK, resp)
		return
	}

	for _, c := range cj.Items {
		if c.Name != name || !canAccessNamespace(r, c.Namespace) {
			continue
		}
		containers := c.Spec.JobTemplate.Spec.Template.Spec.Containers
		if len(containers) > 0 {
			resp = extractResources(containers[0])
		}
		break
	}

	respondJSON(w, http.StatusOK, resp)
}

// UpdateJobResources sets resource limits for a CronJob.
// PUT /api/v1/jobs/{name}/resources
func (j *Jobs) UpdateResources(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var req resourcesRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if err := validateResourceQuantities(req); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Find the CronJob across all namespaces
	cjList, err := j.Client.BatchV1().CronJobs("").List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=kipper",
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list cronjobs")
		return
	}

	for _, cj := range cjList.Items {
		if cj.Name != name || !canAccessNamespace(r, cj.Namespace) {
			continue
		}
		if !enforceProjectRole(w, r, cj.Namespace, middleware.ProjectRoleDeployer) {
			return
		}

		containers := cj.Spec.JobTemplate.Spec.Template.Spec.Containers
		if len(containers) == 0 {
			respondError(w, http.StatusInternalServerError, "cronjob has no containers")
			return
		}

		// No quota preflight for CronJobs: a scheduled job runs one transient
		// pod at a time with no persistent footprint in the quota's used total,
		// so there is no steady-state projection to check. Admission handles the
		// job pod when it is created.
		applyResources(&cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0], req)

		if _, err := j.Client.BatchV1().CronJobs(cj.Namespace).Update(ctx, &cj, metav1.UpdateOptions{}); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to update cronjob resources")
			return
		}

		respondJSON(w, http.StatusOK, map[string]string{"status": "updated"})
		return
	}

	respondError(w, http.StatusNotFound, "cronjob not found")
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

	resp := resourcesResponse{}

	ss, err := s.Client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		respondJSON(w, http.StatusOK, resp)
		return
	}
	if len(ss.Spec.Template.Spec.Containers) > 0 {
		resp = extractResources(ss.Spec.Template.Spec.Containers[0])
	}

	respondJSON(w, http.StatusOK, resp)
}

// UpdateServiceResources sets resource limits for a service StatefulSet.
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

	// Snapshot the pre-update limits so the telemetry log can record
	// "from". Service StatefulSets carry resources on the first
	// container by convention.
	prev := ss.Spec.Template.Spec.Containers[0].Resources
	prevMem := ""
	if v, ok := prev.Limits[corev1.ResourceMemory]; ok {
		prevMem = v.String()
	}
	prevCPU := ""
	if v, ok := prev.Limits[corev1.ResourceCPU]; ok {
		prevCPU = v.String()
	}

	// Preflight against the namespace quota (StatefulSets replace in place, no
	// surge) so an over-quota change returns 409 here instead of stalling the
	// rollout at admission. Mirror single-sided values the way applyResources
	// does before projecting.
	memReq, memLim := pairOrPassThrough(req.MemoryRequest, req.MemoryLimit)
	cpuReq, cpuLim := pairOrPassThrough(req.CPURequest, req.CPULimit)
	change := quotapkg.Change{CPURequest: cpuReq, CPULimit: cpuLim, MemoryRequest: memReq, MemoryLimit: memLim}
	if pf, err := quotapkg.PreflightStatefulSet(ctx, s.Client, namespace, name, change); err == nil && !pf.Fits {
		respondError(w, http.StatusConflict, fmt.Sprintf("resource change needs %s of %s but the namespace quota caps at %s; raise the project tier or environment quota, or reduce other workloads", pf.Projected, pf.Dimension, pf.Hard))
		return
	}

	applyResources(&ss.Spec.Template.Spec.Containers[0], req)

	if _, err := s.Client.AppsV1().StatefulSets(namespace).Update(ctx, ss, metav1.UpdateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update service resources")
		return
	}

	subject := SubjectFromRequest(r)
	if req.MemoryLimit != "" {
		s.Adjustments.Record(ctx, "service", namespace, name, "memory",
			prevMem, req.MemoryLimit, "", subject)
	}
	if req.CPULimit != "" {
		s.Adjustments.Record(ctx, "service", namespace, name, "cpu",
			prevCPU, req.CPULimit, "", subject)
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

	ready := ss.Status.ReadyReplicas == desired &&
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

// applyResources sets resource limits and requests on a container. If the
// client only sends one side of a request/limit pair, the other inherits
// from it (preserves Guaranteed QoS for callers that don't know about
// burstable resources).
func applyResources(container *corev1.Container, req resourcesRequest) {
	limits := container.Resources.Limits
	requests := container.Resources.Requests
	if limits == nil {
		limits = corev1.ResourceList{}
	}
	if requests == nil {
		requests = corev1.ResourceList{}
	}

	memReq, memLim := pairOrPassThrough(req.MemoryRequest, req.MemoryLimit)
	if memLim != "" {
		limits[corev1.ResourceMemory] = resource.MustParse(memLim)
	} else {
		delete(limits, corev1.ResourceMemory)
	}
	if memReq != "" {
		requests[corev1.ResourceMemory] = resource.MustParse(memReq)
	} else {
		delete(requests, corev1.ResourceMemory)
	}

	cpuReq, cpuLim := pairOrPassThrough(req.CPURequest, req.CPULimit)
	if cpuLim != "" {
		limits[corev1.ResourceCPU] = resource.MustParse(cpuLim)
	} else {
		delete(limits, corev1.ResourceCPU)
	}
	if cpuReq != "" {
		requests[corev1.ResourceCPU] = resource.MustParse(cpuReq)
	} else {
		delete(requests, corev1.ResourceCPU)
	}

	container.Resources = corev1.ResourceRequirements{Limits: limits, Requests: requests}
}
