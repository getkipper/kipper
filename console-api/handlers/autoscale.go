package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/resourcebounds"
	"github.com/getkipper/kipper/controller/pkg/capacity"
	"github.com/getkipper/kipper/controller/pkg/rollout"
)

// Autoscale provides handlers for HPA management.
type Autoscale struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
}

// capacityAPIVersion tells the console that GET returns the capacity state
// and PUT accepts enabled and replicas.
const capacityAPIVersion = 1

// HPAScaleReason marks the resource log entries that record an autoscaler
// changing an app's replica count.
const HPAScaleReason = "HPA autoscaling"

const maxScaleActivity = 20

type autoscaleResponse struct {
	Enabled         bool   `json:"enabled"`
	MinReplicas     int32  `json:"min_replicas"`
	MaxReplicas     int32  `json:"max_replicas"`
	CPUTarget       int32  `json:"cpu_target"`
	MemoryTarget    int32  `json:"memory_target"`
	CurrentReplicas int32  `json:"current_replicas"`
	CurrentCPU      string `json:"current_cpu"`
	CurrentMemory   string `json:"current_memory"`

	// Nullable fields distinguish unavailable observations from zero values.
	Replicas           *int32                `json:"replicas"`
	DeploymentReplicas *int32                `json:"deployment_replicas"`
	RunningReplicas    *int32                `json:"running_replicas"`
	ReadyReplicas      *int32                `json:"ready_replicas"`
	Conditions         []autoscalerCondition `json:"conditions"`
	LastScaleTime      *metav1.Time          `json:"last_scale_time"`
	Stopped            *bool                 `json:"stopped"`
	QuotaBlocked       *bool                 `json:"quota_blocked"`
	AutoscalingReady   *autoscalerCondition  `json:"autoscaling_ready"`
	Tracked            *trackedMetrics       `json:"tracked"`
	Activity           []scaleActivity       `json:"activity"`
	ActivityPartial    bool                  `json:"activity_partial"`
	CapacityAPI        int                   `json:"capacity_api"`
}

// trackedMetrics names the metrics automatic sizing treats as the autoscaler's
// and leaves alone.
type trackedMetrics struct {
	CPU    bool `json:"cpu"`
	Memory bool `json:"memory"`
}

type autoscalerCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

// scaleActivity is one recent autoscaler event or scale log entry.
type scaleActivity struct {
	Time    string `json:"time"`
	Source  string `json:"source"`
	Type    string `json:"type,omitempty"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type autoscaleRequest struct {
	Enabled      *bool  `json:"enabled"`
	Replicas     *int32 `json:"replicas"`
	MinReplicas  *int32 `json:"min_replicas"`
	MaxReplicas  *int32 `json:"max_replicas"`
	CPUTarget    *int32 `json:"cpu_target"`
	MemoryTarget *int32 `json:"memory_target"`
}

// Get returns the autoscaling config for an app with its capacity state:
// the stored, desired, running and ready replica counts, the autoscaler's conditions
// and recent scale activity.
// GET /api/v1/projects/{name}/apps/{app}/autoscale
func (a *Autoscale) Get(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var appCR kipperv1.App
	if err := a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR); err != nil {
		if errors.IsNotFound(err) {
			respondJSON(w, http.StatusOK, autoscaleResponse{Enabled: false, CapacityAPI: capacityAPIVersion})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	}

	resp := autoscaleResponse{
		Replicas:        ptr.To(replicasOrDefault(appCR.Spec.Replicas)),
		Stopped:         ptr.To(appCR.Spec.Stopped != nil),
		ActivityPartial: true,
		CapacityAPI:     capacityAPIVersion,
		Tracked:         &trackedMetrics{},
	}
	if c := apimeta.FindStatusCondition(appCR.Status.Conditions, kipperv1.ConditionAutoscalingReady); c != nil {
		resp.AutoscalingReady = &autoscalerCondition{Type: c.Type, Status: string(c.Status), Reason: c.Reason, Message: c.Message}
	}
	if dep, err := a.Client.AppsV1().Deployments(project).Get(ctx, app, metav1.GetOptions{}); err == nil {
		resp.DeploymentReplicas = ptr.To(replicasOrDefault(dep.Spec.Replicas))
		resp.RunningReplicas = ptr.To(dep.Status.Replicas)
		resp.ReadyReplicas = ptr.To(dep.Status.ReadyReplicas)
		resp.QuotaBlocked = ptr.To(rollout.QuotaBlocked(dep))
	}
	resp.Activity = a.scaleActivity(ctx, project, app)

	as := appCR.Spec.Autoscale
	if as == nil {
		respondJSON(w, http.StatusOK, resp)
		return
	}
	resp.Enabled = as.Enabled
	resp.MinReplicas = ptr.Deref(as.MinReplicas, 0)
	resp.MaxReplicas = ptr.Deref(as.MaxReplicas, 0)
	resp.CPUTarget = ptr.Deref(as.CPUTarget, 0)
	resp.MemoryTarget = ptr.Deref(as.MemoryTarget, 0)
	if !as.Enabled {
		respondJSON(w, http.StatusOK, resp)
		return
	}

	// Read current metrics from the HPA status (created by the reconciler)
	hpa, err := a.Client.AutoscalingV2().HorizontalPodAutoscalers(project).Get(ctx, app, metav1.GetOptions{})
	resp.Tracked = trackedFor(&appCR, hpa, err)
	if err == nil {
		resp.CurrentReplicas = hpa.Status.CurrentReplicas
		resp.LastScaleTime = hpa.Status.LastScaleTime
		resp.Conditions = make([]autoscalerCondition, 0, len(hpa.Status.Conditions))
		for _, c := range hpa.Status.Conditions {
			resp.Conditions = append(resp.Conditions, autoscalerCondition{Type: string(c.Type), Status: string(c.Status), Reason: c.Reason, Message: c.Message})
		}
		for _, status := range hpa.Status.CurrentMetrics {
			if status.Resource == nil || status.Resource.Current.AverageUtilization == nil {
				continue
			}
			switch status.Resource.Name {
			case "cpu":
				resp.CurrentCPU = fmt.Sprintf("%d%%", *status.Resource.Current.AverageUtilization)
			case "memory":
				resp.CurrentMemory = fmt.Sprintf("%d%%", *status.Resource.Current.AverageUtilization)
			}
		}
	}

	respondJSON(w, http.StatusOK, resp)
}

// trackedFor applies the resource controller's rule: the autoscaler that an
// unusable policy left running may read either metric when it cannot be read.
func trackedFor(app *kipperv1.App, hpa *autoscalingv2.HorizontalPodAutoscaler, readErr error) *trackedMetrics {
	if readErr != nil {
		hpa = nil
		if !errors.IsNotFound(readErr) && !app.Spec.Autoscale.Policy().Usable() {
			return &trackedMetrics{CPU: true, Memory: true}
		}
	}
	t := resourcebounds.TrackedMetrics(app, hpa)
	return &trackedMetrics{CPU: t.CPU, Memory: t.Memory}
}

// scaleActivity merges the autoscaler's recent events with the resource
// controller's scale log, newest first. Both sources expire entries, so the
// list is partial. It returns nil when neither source could be read.
func (a *Autoscale) scaleActivity(ctx context.Context, project, app string) []scaleActivity {
	var activity []scaleActivity
	read := false

	events, err := a.Client.CoreV1().Events(project).List(ctx, metav1.ListOptions{
		FieldSelector: fields.Set{"involvedObject.kind": "HorizontalPodAutoscaler", "involvedObject.name": app}.String(),
	})
	if err == nil {
		read = true
		for _, e := range events.Items {
			if e.InvolvedObject.Kind != "HorizontalPodAutoscaler" || e.InvolvedObject.Name != app {
				continue
			}
			activity = append(activity, scaleActivity{
				Time: eventTime(&e).UTC().Format(time.RFC3339), Source: "hpa_event", Type: e.Type, Reason: e.Reason, Message: e.Message,
			})
		}
	}

	if entries, err := a.resourceLog(ctx); err == nil {
		read = true
		for _, e := range entries {
			if e.Namespace != project || e.App != app || e.Reason != HPAScaleReason {
				continue
			}
			activity = append(activity, scaleActivity{Time: e.Time, Source: "scale_log", Reason: e.Reason, Message: e.Action})
		}
	}

	if !read {
		return nil
	}
	// Both sources write RFC 3339 in UTC, so the strings sort by time.
	sort.SliceStable(activity, func(i, j int) bool { return activity[i].Time > activity[j].Time })
	if len(activity) > maxScaleActivity {
		activity = activity[:maxScaleActivity]
	}
	if activity == nil {
		activity = []scaleActivity{}
	}
	return activity
}

// resourceLog reads the resource controller's log. A log that does not exist
// yet is empty.
func (a *Autoscale) resourceLog(ctx context.Context) ([]ResourceLogEntry, error) {
	cm, err := a.Client.CoreV1().ConfigMaps(modeConfigMapNamespace).Get(ctx, resourceLogConfigMap, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var entries []ResourceLogEntry
	if data, ok := cm.Data["entries"]; ok {
		if err := json.Unmarshal([]byte(data), &entries); err != nil {
			return nil, err
		}
	}
	return entries, nil
}

// eventTime returns when an event last happened, falling back through the
// fields older and newer event writers fill.
func eventTime(e *corev1.Event) time.Time {
	switch {
	case e.Series != nil && !e.Series.LastObservedTime.IsZero():
		return e.Series.LastObservedTime.Time
	case !e.LastTimestamp.IsZero():
		return e.LastTimestamp.Time
	case !e.EventTime.IsZero():
		return e.EventTime.Time
	case !e.FirstTimestamp.IsZero():
		return e.FirstTimestamp.Time
	}
	return e.CreationTimestamp.Time
}

// Set stores an app's autoscaling policy and replica bounds in one App update.
// An absent enabled field means true. With enabled false the request may
// carry the desired replica count, and a request without bounds removes them.
// PUT /api/v1/projects/{name}/apps/{app}/autoscale
func (a *Autoscale) Set(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	var req autoscaleRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	enabled := req.Enabled == nil || *req.Enabled
	switch {
	case enabled && req.Replicas != nil:
		respondError(w, http.StatusBadRequest, "replicas can only be set with enabled false, because autoscaling sets the count while it is on")
		return
	case !enabled && req.MinReplicas != nil && req.MaxReplicas == nil:
		respondError(w, http.StatusBadRequest, "set max_replicas with min_replicas, or leave both out to remove the bounds")
		return
	case req.Replicas != nil && *req.Replicas < 0:
		respondError(w, http.StatusBadRequest, "replicas must be a non-negative integer")
		return
	}
	policy := &capacity.Policy{
		Enabled:      enabled,
		MinReplicas:  req.MinReplicas,
		MaxReplicas:  req.MaxReplicas,
		CPUTarget:    req.CPUTarget,
		MemoryTarget: req.MemoryTarget,
	}
	if err := capacity.Validate(policy, req.Replicas); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	minReplicas, maxReplicas, _ := policy.Bounds()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var (
		appCR   kipperv1.App
		resp    map[string]any
		write   bool
		readErr error
	)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		appCR = kipperv1.App{}
		readErr = a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR)
		if readErr != nil {
			return readErr
		}
		if enabled {
			resp, write = setOn(&appCR, policy, minReplicas, maxReplicas, req), true
		} else {
			resp, write = a.setOff(ctx, &appCR, policy, req)
		}
		if !write {
			return nil
		}
		return a.CRClient.Update(ctx, &appCR)
	})
	switch {
	case readErr != nil:
		respondError(w, http.StatusNotFound, fmt.Sprintf("app %q not found", app))
		return
	case err != nil:
		respondUpdateError(w, err, "failed to configure autoscaling")
		return
	case !write:
		respondJSON(w, http.StatusOK, resp)
		return
	case enabled:
		respondJSON(w, http.StatusOK, withStoppedNote(resp, &appCR, "autoscaling"))
	default:
		respondJSON(w, http.StatusOK, withStoppedNote(resp, &appCR, "autoscaling change"))
	}
}

// setOn enables the policy and clamps the stored count for the caller to save.
func setOn(appCR *kipperv1.App, policy *capacity.Policy, minReplicas, maxReplicas int32, req autoscaleRequest) map[string]any {
	appCR.Spec.Autoscale = &kipperv1.AppAutoscale{
		Enabled:      true,
		MinReplicas:  &minReplicas,
		MaxReplicas:  &maxReplicas,
		CPUTarget:    positiveOrNil(req.CPUTarget),
		MemoryTarget: positiveOrNil(req.MemoryTarget),
	}
	resp := map[string]any{"status": "enabled"}
	from := replicasOrDefault(appCR.Spec.Replicas)
	if to, moved := policy.IntoBounds(from); moved {
		appCR.Spec.Replicas = &to
		resp["replicas_moved"] = replicasMove{From: from, To: to}
	}
	return resp
}

// setOff prepares a disabled policy and count for the caller to save. Without
// an explicit count, switching off uses the Deployment's desired count for
// non-stopped apps, falling back to the stored count, then clamps to bounds.
// It returns the response and whether the spec needs writing.
func (a *Autoscale) setOff(ctx context.Context, appCR *kipperv1.App, policy *capacity.Policy, req autoscaleRequest) (map[string]any, bool) {
	var block *kipperv1.AppAutoscale
	if lo, hi, ok := policy.Bounds(); ok {
		block = &kipperv1.AppAutoscale{
			MinReplicas:  &lo,
			MaxReplicas:  &hi,
			CPUTarget:    positiveOrNil(req.CPUTarget),
			MemoryTarget: positiveOrNil(req.MemoryTarget),
		}
	}

	stored := replicasOrDefault(appCR.Spec.Replicas)
	replicas := stored
	resp := map[string]any{"status": "disabled"}
	if req.Replicas != nil {
		replicas = *req.Replicas
	} else {
		transition := *policy
		transition.Enabled = appCR.Spec.Autoscale != nil && appCR.Spec.Autoscale.Enabled
		res := capacity.PlanSwitchOff(&transition, stored, appCR.Spec.Stopped != nil, a.deploymentReplicas(ctx, appCR.Namespace, appCR.Name))
		from := stored
		if res.WritesReplicas() {
			from, replicas = res.Live, res.Replicas
		} else {
			replicas, _ = policy.IntoBounds(stored)
		}
		switch {
		case res.CountUnknown && replicas != stored:
			resp["warning"] = fmt.Sprintf("the running count could not be read, so the stored count of %d was moved into the new bounds as %d", stored, replicas)
		case res.CountUnknown:
			resp["warning"] = countUnknownWarning(stored)
		}
		if replicas != from {
			resp["replicas_moved"] = replicasMove{From: from, To: replicas}
		}
	}
	resp["replicas"] = replicas

	before := appCR.Spec.DeepCopy()
	appCR.Spec.Autoscale = block
	if replicas != stored {
		appCR.Spec.Replicas = &replicas
	}
	return resp, !equality.Semantic.DeepEqual(before, &appCR.Spec)
}

// Delete disables autoscaling while retaining the bounds and planned replica count.
// DELETE /api/v1/projects/{name}/apps/{app}/autoscale
func (a *Autoscale) Delete(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var (
		appCR   kipperv1.App
		res     capacity.SwitchOff
		readErr error
	)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		appCR = kipperv1.App{}
		readErr = a.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: app}, &appCR)
		if readErr != nil {
			return readErr
		}
		res = capacity.PlanSwitchOff(storedPolicy(&appCR), replicasOrDefault(appCR.Spec.Replicas), appCR.Spec.Stopped != nil, a.deploymentReplicas(ctx, project, app))
		if res.AlreadyOff {
			return nil
		}
		appCR.Spec.Autoscale.Enabled = false
		if res.WritesReplicas() {
			appCR.Spec.Replicas = &res.Replicas
		}
		return a.CRClient.Update(ctx, &appCR)
	})
	switch {
	case errors.IsNotFound(readErr):
		respondJSON(w, http.StatusOK, map[string]string{"status": "disabled"})
		return
	case readErr != nil:
		respondError(w, http.StatusInternalServerError, "failed to get app")
		return
	case err != nil:
		respondUpdateError(w, err, "failed to disable autoscaling")
		return
	case res.AlreadyOff:
		respondJSON(w, http.StatusOK, map[string]any{"status": "disabled"})
		return
	}

	resp := map[string]any{"status": "disabled", "replicas": res.Replicas}
	switch {
	case res.CountUnknown:
		resp["warning"] = countUnknownWarning(res.Replicas)
	case res.InvalidBounds:
		resp["warning"] = fmt.Sprintf("%s keeps running %d replicas, but its stored bounds are invalid. Remove them, or save valid ones with autoscaling left off.", app, res.Replicas)
	case res.Clamped():
		resp["replicas_moved"] = replicasMove{From: res.Live, To: res.Replicas}
	}
	respondJSON(w, http.StatusOK, withStoppedNote(resp, &appCR, "autoscaling change"))
}

// deploymentReplicas reads the Deployment's desired count, defaulting to 1.
func (a *Autoscale) deploymentReplicas(ctx context.Context, project, app string) func() (int32, error) {
	return func() (int32, error) {
		dep, err := a.Client.AppsV1().Deployments(project).Get(ctx, app, metav1.GetOptions{})
		if err != nil {
			return 0, err
		}
		return replicasOrDefault(dep.Spec.Replicas), nil
	}
}

func countUnknownWarning(stored int32) string {
	return fmt.Sprintf("the running count could not be read, so the stored count of %d applies", stored)
}

// respondUpdateError preserves admission details and distinguishes conflicts
// from other write failures.
func respondUpdateError(w http.ResponseWriter, err error, failure string) {
	if errors.IsInvalid(err) {
		respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if errors.IsConflict(err) {
		respondError(w, http.StatusConflict, "the app kept changing while this was saved; try again")
		return
	}
	respondError(w, http.StatusInternalServerError, failure)
}

// replicasMove reports a desired count adjusted to stay within the bounds.
type replicasMove struct {
	From int32 `json:"from"`
	To   int32 `json:"to"`
}

func storedPolicy(app *kipperv1.App) *capacity.Policy {
	return app.Spec.Autoscale.Policy()
}

// replicasOrDefault reads a replica count where an absent value is the default 1.
func replicasOrDefault(n *int32) int32 {
	if n == nil {
		return 1
	}
	return *n
}

// withStoppedNote explains when a saved change will take effect on a stopped app.
func withStoppedNote(resp map[string]any, app *kipperv1.App, what string) map[string]any {
	if app.Spec.Stopped != nil {
		resp["note"] = fmt.Sprintf("%s is stopped; the %s applies when the app is started", app.Name, what)
	}
	return resp
}

func positiveOrNil(v *int32) *int32 {
	if v == nil || *v <= 0 {
		return nil
	}
	return v
}
