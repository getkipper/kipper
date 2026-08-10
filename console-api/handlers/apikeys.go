package handlers

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// APIGateway serves usage plan and API key management for one environment
// namespace. The route's {name} segment is the namespace; project
// membership is enforced by the ProjectScope middleware, exactly like the
// app and function routes.
type APIGateway struct {
	CRClient crclient.Client
}

// auditMutation logs a structured event for a key or plan change so "who
// issued or revoked this" is answerable. The actor comes from the JWT; the
// object identifier (key prefix or plan name) is non-secret. The full key and
// its secret are never logged.
func auditMutation(r *http.Request, action, namespace string, attrs ...any) {
	base := []any{
		slog.String("actor", SubjectFromRequest(r)),
		slog.String("action", action),
		slog.String("namespace", namespace),
	}
	slog.Info("apikey audit", append(base, attrs...)...)
}

var dns1123Name = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// keyAlphabet is lowercase alphanumeric so prefixes stay DNS-safe inside
// rollup object names.
const keyAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

type usagePlanResponse struct {
	Name        string             `json:"name"`
	DisplayName string             `json:"display_name,omitempty"`
	Rate        int                `json:"rate"`
	Burst       int                `json:"burst"`
	Quota       *planQuotaResponse `json:"quota,omitempty"`
	Keys        int                `json:"keys"`
}

type planQuotaResponse struct {
	Requests int64  `json:"requests"`
	Period   string `json:"period"`
}

type usagePlanRequest struct {
	Name        string             `json:"name"`
	DisplayName string             `json:"display_name,omitempty"`
	Rate        int                `json:"rate"`
	Burst       int                `json:"burst"`
	Quota       *planQuotaResponse `json:"quota,omitempty"`
}

type apiKeyResponse struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name,omitempty"`
	Plan        string   `json:"plan"`
	Prefix      string   `json:"prefix"`
	Enabled     bool     `json:"enabled"`
	Apps        []string `json:"apps"`
	Created     string   `json:"created"`
	// ExpiresAt is the RFC3339 expiry, empty when the key never expires.
	ExpiresAt string `json:"expires_at,omitempty"`
	// Key carries the full secret exactly once, in the create response.
	Key string `json:"key,omitempty"`
}

type createKeyRequest struct {
	DisplayName string   `json:"display_name,omitempty"`
	Plan        string   `json:"plan"`
	Apps        []string `json:"apps,omitempty"`
	// ExpiresAt is an optional RFC3339 instant after which the key stops
	// working. Empty means the key never expires.
	ExpiresAt string `json:"expires_at,omitempty"`
}

type updateKeyRequest struct {
	Enabled *bool    `json:"enabled,omitempty"`
	Apps    []string `json:"apps"`
}

// ListPlans returns the namespace's usage plans with their key counts.
// GET /api/v1/projects/{name}/usage-plans
func (h *APIGateway) ListPlans(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var plans kipperv1.UsagePlanList
	if err := h.CRClient.List(ctx, &plans, crclient.InNamespace(namespace)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list usage plans")
		return
	}
	var keys kipperv1.ApiKeyList
	if err := h.CRClient.List(ctx, &keys, crclient.InNamespace(namespace)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list api keys")
		return
	}
	keysPerPlan := map[string]int{}
	for _, k := range keys.Items {
		keysPerPlan[k.Spec.Plan]++
	}

	out := make([]usagePlanResponse, 0, len(plans.Items))
	for _, p := range plans.Items {
		resp := usagePlanResponse{
			Name:        p.Name,
			DisplayName: p.Spec.DisplayName,
			Rate:        p.Spec.Rate,
			Burst:       p.Spec.Burst,
			Keys:        keysPerPlan[p.Name],
		}
		if p.Spec.Quota != nil {
			resp.Quota = &planQuotaResponse{Requests: p.Spec.Quota.Requests, Period: p.Spec.Quota.Period}
		}
		out = append(out, resp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	respondJSON(w, http.StatusOK, out)
}

// UpsertPlan creates or updates a usage plan.
// PUT /api/v1/projects/{name}/usage-plans
func (h *APIGateway) UpsertPlan(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")

	var req usagePlanRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if !dns1123Name.MatchString(req.Name) || len(req.Name) > 63 {
		respondError(w, http.StatusBadRequest, "plan name must be a short lowercase alphanumeric-and-dashes name")
		return
	}
	if err := validateDisplayName(req.DisplayName); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.Rate < 1 || req.Burst < 1 {
		respondError(w, http.StatusBadRequest, "rate and burst must be at least 1")
		return
	}
	if req.Quota != nil {
		switch req.Quota.Period {
		case "day", "week", "month":
		default:
			respondError(w, http.StatusBadRequest, "quota period must be day, week or month")
			return
		}
		if req.Quota.Requests < 1 {
			respondError(w, http.StatusBadRequest, "quota requests must be at least 1")
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	spec := kipperv1.UsagePlanSpec{
		DisplayName: req.DisplayName,
		Rate:        req.Rate,
		Burst:       req.Burst,
	}
	if req.Quota != nil {
		spec.Quota = &kipperv1.PlanQuota{Requests: req.Quota.Requests, Period: req.Quota.Period}
	}

	var existing kipperv1.UsagePlan
	err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: req.Name}, &existing)
	if errors.IsNotFound(err) {
		plan := &kipperv1.UsagePlan{
			ObjectMeta: metav1.ObjectMeta{
				Name:      req.Name,
				Namespace: namespace,
				Labels:    map[string]string{kipperLabel: kipperValue},
			},
			Spec: spec,
		}
		if err := h.CRClient.Create(ctx, plan); err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create plan: %v", err))
			return
		}
		auditMutation(r, "create_plan", namespace, slog.String("plan", req.Name))
		respondJSON(w, http.StatusCreated, req)
		return
	}
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to load plan")
		return
	}

	existing.Spec = spec
	if err := h.CRClient.Update(ctx, &existing); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to update plan: %v", err))
		return
	}
	auditMutation(r, "update_plan", namespace, slog.String("plan", req.Name))
	respondJSON(w, http.StatusOK, req)
}

// DeletePlan removes an unused usage plan. Plans still referenced by keys
// are refused: deleting one would turn its keys into dead references that
// authz denies, which looks like an outage rather than a config change.
// DELETE /api/v1/projects/{name}/usage-plans/{plan}
func (h *APIGateway) DeletePlan(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	planName := chi.URLParam(r, "plan")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var keys kipperv1.ApiKeyList
	if err := h.CRClient.List(ctx, &keys, crclient.InNamespace(namespace)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list api keys")
		return
	}
	for _, k := range keys.Items {
		if k.Spec.Plan == planName {
			respondError(w, http.StatusConflict,
				fmt.Sprintf("plan %q is used by key %q; delete or reassign its keys first", planName, k.Name))
			return
		}
	}

	plan := &kipperv1.UsagePlan{ObjectMeta: metav1.ObjectMeta{Name: planName, Namespace: namespace}}
	if err := h.CRClient.Delete(ctx, plan); err != nil && !errors.IsNotFound(err) {
		respondError(w, http.StatusInternalServerError, "failed to delete plan")
		return
	}

	// Close the create/delete race from the delete side: a key referencing this
	// plan could have been created between the guard's list and this delete.
	// Such a key can never authorize now that its plan is gone, so re-list and
	// remove any that slipped in. Together with CreateKey re-reading the plan
	// after it writes the key, every interleaving leaves no orphaned key.
	var raced kipperv1.ApiKeyList
	if err := h.CRClient.List(ctx, &raced, crclient.InNamespace(namespace)); err == nil {
		for i := range raced.Items {
			if raced.Items[i].Spec.Plan == planName {
				_ = h.CRClient.Delete(ctx, &raced.Items[i])
			}
		}
	}
	auditMutation(r, "delete_plan", namespace, slog.String("plan", planName))
	w.WriteHeader(http.StatusNoContent)
}

// ListKeys returns the namespace's API keys, never their secrets.
// GET /api/v1/projects/{name}/api-keys
func (h *APIGateway) ListKeys(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var keys kipperv1.ApiKeyList
	if err := h.CRClient.List(ctx, &keys, crclient.InNamespace(namespace)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list api keys")
		return
	}

	out := make([]apiKeyResponse, 0, len(keys.Items))
	for i := range keys.Items {
		out = append(out, keyResponse(&keys.Items[i]))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	respondJSON(w, http.StatusOK, out)
}

// CreateKey issues a new API key. The full secret appears only in this
// response; the CR stores its digest and non-secret prefix.
// POST /api/v1/projects/{name}/api-keys
func (h *APIGateway) CreateKey(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")

	var req createKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Plan == "" {
		respondError(w, http.StatusBadRequest, "plan is required")
		return
	}
	if app, ok := invalidAppScope(req.Apps); !ok {
		respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid app name %q in scope", app))
		return
	}
	if err := validateDisplayName(req.DisplayName); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	var expiresAt *metav1.Time
	if req.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, req.ExpiresAt)
		if err != nil {
			respondError(w, http.StatusBadRequest, "expires_at must be an RFC3339 timestamp")
			return
		}
		if !t.After(time.Now()) {
			respondError(w, http.StatusBadRequest, "expires_at must be in the future")
			return
		}
		expiresAt = &metav1.Time{Time: t}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var plan kipperv1.UsagePlan
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: req.Plan}, &plan); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("usage plan %q does not exist", req.Plan))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to load plan")
		return
	}

	// A prefix collision is astronomically unlikely but cheap to retry;
	// error details stay out of the response so nothing about the stored
	// object leaks.
	for attempt := 0; attempt < 3; attempt++ {
		prefix, err := randomToken(8)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to generate key")
			return
		}
		secret, err := randomToken(40)
		if err != nil {
			respondError(w, http.StatusInternalServerError, "failed to generate key")
			return
		}
		fullKey := fmt.Sprintf("kip_%s_%s", prefix, secret)
		digest := sha256.Sum256([]byte(fullKey))

		key := &kipperv1.ApiKey{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "key-" + prefix,
				Namespace: namespace,
				Labels:    map[string]string{kipperLabel: kipperValue},
			},
			Spec: kipperv1.ApiKeySpec{
				DisplayName: req.DisplayName,
				Plan:        req.Plan,
				Apps:        req.Apps,
				Prefix:      prefix,
				HashSHA256:  hex.EncodeToString(digest[:]),
				ExpiresAt:   expiresAt,
			},
		}
		if err := h.CRClient.Create(ctx, key); err != nil {
			if errors.IsAlreadyExists(err) {
				continue
			}
			respondError(w, http.StatusInternalServerError, "failed to create key")
			return
		}

		// Re-read the plan after creating the key to close the race with a
		// concurrent DeletePlan: that guard lists keys before deleting, so a key
		// created just after the list would reference a plan that is now gone
		// and could never authorize. If the plan vanished, undo the key.
		var check kipperv1.UsagePlan
		if err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: req.Plan}, &check); err != nil {
			_ = h.CRClient.Delete(ctx, key)
			if errors.IsNotFound(err) {
				respondError(w, http.StatusConflict, fmt.Sprintf("usage plan %q was removed while the key was being created; retry", req.Plan))
				return
			}
			respondError(w, http.StatusInternalServerError, "failed to verify plan after key creation")
			return
		}

		auditMutation(r, "create_key", namespace, slog.String("prefix", prefix), slog.String("plan", req.Plan))
		resp := keyResponse(key)
		resp.Key = fullKey
		respondJSON(w, http.StatusCreated, resp)
		return
	}
	respondError(w, http.StatusInternalServerError, "failed to create key")
}

// invalidAppScope returns the first app name that is not a valid DNS-1123
// label within length, with ok=false. An empty scope is valid (unrestricted).
func invalidAppScope(apps []string) (offending string, ok bool) {
	for _, app := range apps {
		if !dns1123Name.MatchString(app) || len(app) > 63 {
			return app, false
		}
	}
	return "", true
}

// maxDisplayName bounds a key or plan display name. A key's name is forwarded
// verbatim as the X-Kipper-Key-Name header on every allowed request, so it
// must stay short and free of control bytes that would make the upstream proxy
// reject the request.
const maxDisplayName = 128

// validateDisplayName rejects a name that is too long or carries control
// characters. The CRD caps length as a second line of defence for direct CR
// writes; this gives the console a clean 400 and blocks the header-injection
// case the length cap alone cannot.
func validateDisplayName(name string) error {
	// Count runes to match the CRD's maxLength, which measures Unicode
	// characters rather than bytes, so the two limits agree for multibyte
	// names.
	if utf8.RuneCountInString(name) > maxDisplayName {
		return fmt.Errorf("display name must be at most %d characters", maxDisplayName)
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return fmt.Errorf("display name must not contain control characters")
	}
	return nil
}

// UpdateKey enables/disables a key or changes its app scope.
// PATCH /api/v1/projects/{name}/api-keys/{key}
func (h *APIGateway) UpdateKey(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	keyName := chi.URLParam(r, "key")

	var req updateKeyRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var key kipperv1.ApiKey
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: keyName}, &key); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("api key %q not found", keyName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to load key")
		return
	}

	if req.Enabled != nil {
		key.Spec.Enabled = req.Enabled
	}
	if req.Apps != nil {
		if app, ok := invalidAppScope(req.Apps); !ok {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("invalid app name %q in scope", app))
			return
		}
		key.Spec.Apps = req.Apps
	}
	if err := h.CRClient.Update(ctx, &key); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to update key")
		return
	}
	attrs := []any{slog.String("prefix", key.Spec.Prefix)}
	if req.Enabled != nil {
		attrs = append(attrs, slog.Bool("enabled", *req.Enabled))
	}
	auditMutation(r, "update_key", namespace, attrs...)
	respondJSON(w, http.StatusOK, keyResponse(&key))
}

// DeleteKey revokes a key. Its usage rollups stay for billing history and
// age out through retention.
// DELETE /api/v1/projects/{name}/api-keys/{key}
func (h *APIGateway) DeleteKey(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	keyName := chi.URLParam(r, "key")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	key := &kipperv1.ApiKey{ObjectMeta: metav1.ObjectMeta{Name: keyName, Namespace: namespace}}
	if err := h.CRClient.Delete(ctx, key); err != nil && !errors.IsNotFound(err) {
		respondError(w, http.StatusInternalServerError, "failed to delete key")
		return
	}
	auditMutation(r, "delete_key", namespace, slog.String("key", keyName))
	w.WriteHeader(http.StatusNoContent)
}

// keyUsageDay is one day of a key's request counters.
type keyUsageDay struct {
	Day         string `json:"day"`
	Allowed     int64  `json:"allowed"`
	DeniedRate  int64  `json:"denied_rate"`
	DeniedQuota int64  `json:"denied_quota"`
}

// keyUsageResponse carries the days plus the window they cover. From and To
// are the effective range: a request reaching past retention gets From capped,
// so a caller sees the clamp rather than silently receiving a shorter window.
type keyUsageResponse struct {
	From          string        `json:"from"`
	To            string        `json:"to"`
	RetentionDays int           `json:"retention_days"`
	Days          []keyUsageDay `json:"days"`
}

// KeyUsage returns the key's daily rollups over a UTC date window, newest
// first. Both from and to are inclusive and default to the last 30 days. The
// counters are the durable source authz flushes into; anything not yet flushed
// (up to one flush window) is not included.
// GET /api/v1/projects/{name}/api-keys/{key}/usage?from=YYYY-MM-DD&to=YYYY-MM-DD
func (h *APIGateway) KeyUsage(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	keyName := chi.URLParam(r, "key")

	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	earliest := now.AddDate(0, 0, -rollupRetentionDays).Format("2006-01-02")

	to := today
	if raw := r.URL.Query().Get("to"); raw != "" {
		d, err := time.Parse("2006-01-02", raw)
		if err != nil {
			respondError(w, http.StatusBadRequest, "to must be a YYYY-MM-DD date")
			return
		}
		if to = d.Format("2006-01-02"); to > today {
			// No rollups exist for the future; cap so the window is honest.
			to = today
		}
	}

	from := parseDateWindow(r.URL.Query().Get("from"), to)
	if from == "" {
		respondError(w, http.StatusBadRequest, "from must be a YYYY-MM-DD date")
		return
	}
	if from > to {
		respondError(w, http.StatusBadRequest, "from must not be after to")
		return
	}
	// Loud retention cap: rollups older than retention are swept, so pull from
	// forward and let the response report the effective window. If the whole
	// window predates retention (to is also older than the floor), the clamp
	// would invert it, so reject rather than return an impossible range.
	if to < earliest {
		respondError(w, http.StatusBadRequest,
			fmt.Sprintf("requested window is older than the %d-day retention; usage before %s is not kept", rollupRetentionDays, earliest))
		return
	}
	if from < earliest {
		from = earliest
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var key kipperv1.ApiKey
	if err := h.CRClient.Get(ctx, crclient.ObjectKey{Namespace: namespace, Name: keyName}, &key); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("api key %q not found", keyName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to load key")
		return
	}

	var rollups kipperv1.UsageRollupList
	if err := h.CRClient.List(ctx, &rollups, crclient.InNamespace(namespace)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list usage")
		return
	}

	days := make([]keyUsageDay, 0)
	for _, rollup := range rollups.Items {
		if rollup.Spec.KeyPrefix != key.Spec.Prefix || rollup.Spec.Day < from || rollup.Spec.Day > to {
			continue
		}
		days = append(days, keyUsageDay{
			Day:         rollup.Spec.Day,
			Allowed:     rollup.Spec.Allowed,
			DeniedRate:  rollup.Spec.DeniedRate,
			DeniedQuota: rollup.Spec.DeniedQuota,
		})
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Day > days[j].Day })
	respondJSON(w, http.StatusOK, keyUsageResponse{
		From:          from,
		To:            to,
		RetentionDays: rollupRetentionDays,
		Days:          days,
	})
}

// parseDateWindow resolves the from parameter. An empty value defaults to a
// 30-day window ending at to. A malformed value returns "" so the caller can
// reject it.
func parseDateWindow(raw, to string) string {
	if raw == "" {
		toDate, err := time.Parse("2006-01-02", to)
		if err != nil {
			return ""
		}
		return toDate.AddDate(0, 0, -29).Format("2006-01-02")
	}
	d, err := time.Parse("2006-01-02", raw)
	if err != nil {
		return ""
	}
	return d.Format("2006-01-02")
}

func keyResponse(key *kipperv1.ApiKey) apiKeyResponse {
	apps := key.Spec.Apps
	if apps == nil {
		apps = []string{}
	}
	resp := apiKeyResponse{
		Name:        key.Name,
		DisplayName: key.Spec.DisplayName,
		Plan:        key.Spec.Plan,
		Prefix:      key.Spec.Prefix,
		Enabled:     key.IsEnabled(),
		Apps:        apps,
		Created:     key.CreationTimestamp.UTC().Format(time.RFC3339),
	}
	if key.Spec.ExpiresAt != nil {
		resp.ExpiresAt = key.Spec.ExpiresAt.UTC().Format(time.RFC3339)
	}
	return resp
}

// randomToken returns n characters of crypto-random lowercase
// alphanumerics. Rejection sampling keeps the distribution uniform; a plain
// modulo would bias the first few alphabet characters.
func randomToken(n int) (string, error) {
	// 252 is the largest multiple of len(keyAlphabet) below 256.
	const limit = 256 - 256%len(keyAlphabet)
	var b strings.Builder
	b.Grow(n)
	buf := make([]byte, 64)
	for b.Len() < n {
		if _, err := rand.Read(buf); err != nil {
			return "", err
		}
		for _, c := range buf {
			if int(c) >= limit {
				continue
			}
			b.WriteByte(keyAlphabet[int(c)%len(keyAlphabet)])
			if b.Len() == n {
				break
			}
		}
	}
	return b.String(), nil
}
