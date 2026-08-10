package handlers

import (
	"context"
	"net/http"
	"sort"
	"strconv"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
)

// Adjustments owns the ResourceAdjustment cluster log: writing one
// record per Apply (when telemetry is on) and exposing the log to the
// console.
type Adjustments struct {
	CRClient crclient.Client
}

// adjustmentResponse mirrors the CR for the JSON wire format. Keeps the
// internal Go names (camelCase) out of the public API.
type adjustmentResponse struct {
	Name      string `json:"name"`
	Component string `json:"component"`
	Scope     string `json:"scope"`
	Namespace string `json:"namespace,omitempty"`
	Kind      string `json:"kind"`
	From      string `json:"from,omitempty"`
	To        string `json:"to"`
	Reason    string `json:"reason,omitempty"`
	At        string `json:"at"`
	AppliedBy string `json:"applied_by,omitempty"`
}

type adjustmentsListResponse struct {
	Items     []adjustmentResponse `json:"items"`
	Telemetry bool                 `json:"telemetry_enabled"`
}

// Record writes a ResourceAdjustment if PlatformConfig.spec.telemetry
// has it switched on. Failure is best-effort and silent — telemetry
// must never block a user-facing apply.
//
// scope is one of: "platform", "app", "service", "function".
// kind is "memory" or "cpu".
func (a *Adjustments) Record(ctx context.Context, scope, namespace, component, kind, from, to, reason, appliedBy string) {
	if a == nil || a.CRClient == nil {
		return
	}
	if to == "" || to == from {
		return
	}
	enabled, _ := a.telemetryEnabled(ctx)
	if !enabled {
		return
	}
	obj := &kipperv1.ResourceAdjustment{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: "radj-",
		},
		Spec: kipperv1.ResourceAdjustmentSpec{
			Component: component,
			Scope:     scope,
			Namespace: namespace,
			Kind:      kind,
			From:      from,
			To:        to,
			Reason:    reason,
			At:        metav1.Now(),
			AppliedBy: appliedBy,
		},
	}
	_ = a.CRClient.Create(ctx, obj)
}

func (a *Adjustments) telemetryEnabled(ctx context.Context) (bool, error) {
	var pc kipperv1.PlatformConfig
	err := a.CRClient.Get(ctx, types.NamespacedName{Name: platformConfigName}, &pc)
	if err != nil {
		return false, err
	}
	return pc.Spec.Telemetry != nil && pc.Spec.Telemetry.RecordResourceAdjustments, nil
}

// List serves GET /api/v1/resources/adjustments — newest first, capped
// by an optional ?limit= query param (default 100, max 500). Returns
// the telemetry_enabled flag alongside so the console can render an
// honest "telemetry is off" empty state when the cluster opted out.
func (a *Adjustments) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			if n > 500 {
				n = 500
			}
			limit = n
		}
	}

	enabled, _ := a.telemetryEnabled(ctx)

	var list kipperv1.ResourceAdjustmentList
	if err := a.CRClient.List(ctx, &list); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list adjustments")
		return
	}

	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		return items[i].Spec.At.After(items[j].Spec.At.Time)
	})
	if len(items) > limit {
		items = items[:limit]
	}

	out := adjustmentsListResponse{
		Items:     make([]adjustmentResponse, 0, len(items)),
		Telemetry: enabled,
	}
	for _, it := range items {
		// Only surface adjustments in projects the caller belongs to; cluster
		// -scoped entries (no namespace) resolve to admins only.
		if !canAccessNamespace(r, it.Spec.Namespace) {
			continue
		}
		out.Items = append(out.Items, adjustmentResponse{
			Name:      it.Name,
			Component: it.Spec.Component,
			Scope:     it.Spec.Scope,
			Namespace: it.Spec.Namespace,
			Kind:      it.Spec.Kind,
			From:      it.Spec.From,
			To:        it.Spec.To,
			Reason:    it.Spec.Reason,
			At:        it.Spec.At.UTC().Format(time.RFC3339),
			AppliedBy: it.Spec.AppliedBy,
		})
	}
	respondJSON(w, http.StatusOK, out)
}

// SubjectFromRequest derives the appliedBy identifier from the request.
// Returns "system" when there's no JWT (admin Job, dev-mode probes,
// etc.) so the log never carries empty values.
func SubjectFromRequest(r *http.Request) string {
	if claims := middleware.UserFromContext(r.Context()); claims != nil {
		if claims.Email != "" {
			return claims.Email
		}
		if claims.Subject != "" {
			return claims.Subject
		}
	}
	return "system"
}
