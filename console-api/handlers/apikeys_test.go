package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
)

func gatewayRouter(h *APIGateway) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/projects/{name}/usage-plans", h.ListPlans)
	r.Put("/projects/{name}/usage-plans", h.UpsertPlan)
	r.Delete("/projects/{name}/usage-plans/{plan}", h.DeletePlan)
	r.Get("/projects/{name}/api-keys", h.ListKeys)
	r.Post("/projects/{name}/api-keys", h.CreateKey)
	r.Patch("/projects/{name}/api-keys/{key}", h.UpdateKey)
	r.Delete("/projects/{name}/api-keys/{key}", h.DeleteKey)
	r.Get("/projects/{name}/api-keys/{key}/usage", h.KeyUsage)
	return r
}

func bronzePlan() *kipperv1.UsagePlan {
	return &kipperv1.UsagePlan{
		ObjectMeta: metav1.ObjectMeta{Name: "bronze", Namespace: "shop-prod"},
		Spec:       kipperv1.UsagePlanSpec{Rate: 100, Burst: 200},
	}
}

func do(t *testing.T, router *chi.Mux, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestAPIGateway_PlanLifecycle(t *testing.T) {
	crClient := testCRClient()
	router := gatewayRouter(&APIGateway{CRClient: crClient})

	rec := do(t, router, http.MethodPut, "/projects/shop-prod/usage-plans",
		`{"name":"bronze","rate":100,"burst":200,"quota":{"requests":100000,"period":"month"}}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var stored kipperv1.UsagePlan
	require.NoError(t, crClient.Get(t.Context(), crclient.ObjectKey{Namespace: "shop-prod", Name: "bronze"}, &stored))
	assert.Equal(t, 100, stored.Spec.Rate)
	require.NotNil(t, stored.Spec.Quota)
	assert.Equal(t, int64(100000), stored.Spec.Quota.Requests)

	// Update in place.
	rec = do(t, router, http.MethodPut, "/projects/shop-prod/usage-plans",
		`{"name":"bronze","rate":50,"burst":100}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, crClient.Get(t.Context(), crclient.ObjectKey{Namespace: "shop-prod", Name: "bronze"}, &stored))
	assert.Equal(t, 50, stored.Spec.Rate)
	assert.Nil(t, stored.Spec.Quota, "omitting quota clears it")

	rec = do(t, router, http.MethodDelete, "/projects/shop-prod/usage-plans/bronze", "")
	require.Equal(t, http.StatusNoContent, rec.Code)
}

func TestAPIGateway_PlanValidation(t *testing.T) {
	router := gatewayRouter(&APIGateway{CRClient: testCRClient()})

	for name, body := range map[string]string{
		"bad name":   `{"name":"Bad Name!","rate":10,"burst":10}`,
		"zero rate":  `{"name":"p","rate":0,"burst":10}`,
		"bad period": `{"name":"p","rate":10,"burst":10,"quota":{"requests":10,"period":"year"}}`,
		"zero quota": `{"name":"p","rate":10,"burst":10,"quota":{"requests":0,"period":"day"}}`,
	} {
		rec := do(t, router, http.MethodPut, "/projects/shop-prod/usage-plans", body)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", name, rec.Body.String())
	}
}

func TestAPIGateway_DeletePlanInUseRefused(t *testing.T) {
	key := &kipperv1.ApiKey{
		ObjectMeta: metav1.ObjectMeta{Name: "key-ab12cd34", Namespace: "shop-prod"},
		Spec:       kipperv1.ApiKeySpec{Plan: "bronze", Prefix: "ab12cd34", HashSHA256: "aa"},
	}
	router := gatewayRouter(&APIGateway{CRClient: testCRClient(bronzePlan(), key)})

	rec := do(t, router, http.MethodDelete, "/projects/shop-prod/usage-plans/bronze", "")
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "key-ab12cd34")
}

func TestAPIGateway_CreateKeyRevealsSecretOnce(t *testing.T) {
	crClient := testCRClient(bronzePlan())
	router := gatewayRouter(&APIGateway{CRClient: crClient})

	rec := do(t, router, http.MethodPost, "/projects/shop-prod/api-keys",
		`{"display_name":"partner","plan":"bronze","apps":["api"]}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var created apiKeyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotEmpty(t, created.Key)
	parts := strings.Split(created.Key, "_")
	require.Len(t, parts, 3, "key format is kip_<prefix>_<secret>")
	assert.Equal(t, "kip", parts[0])
	assert.Equal(t, created.Prefix, parts[1])
	assert.Len(t, parts[1], 8)
	assert.Len(t, parts[2], 40)

	// The CR stores the digest, never the key or its secret part.
	var stored kipperv1.ApiKey
	require.NoError(t, crClient.Get(t.Context(), crclient.ObjectKey{Namespace: "shop-prod", Name: created.Name}, &stored))
	digest := sha256.Sum256([]byte(created.Key))
	assert.Equal(t, hex.EncodeToString(digest[:]), stored.Spec.HashSHA256)
	assert.NotContains(t, stored.Spec.HashSHA256, parts[2])
	assert.True(t, stored.IsEnabled(), "new keys start enabled")

	// The list never carries secrets.
	rec = do(t, router, http.MethodGet, "/projects/shop-prod/api-keys", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), parts[2])
	var listed []apiKeyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	require.Len(t, listed, 1)
	assert.Empty(t, listed[0].Key)
}

func TestAPIGateway_CreateKeyWithExpiry(t *testing.T) {
	crClient := testCRClient(bronzePlan())
	router := gatewayRouter(&APIGateway{CRClient: crClient})
	exp := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	rec := do(t, router, http.MethodPost, "/projects/shop-prod/api-keys",
		`{"plan":"bronze","expires_at":"`+exp+`"}`)
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())

	var created apiKeyResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.Equal(t, exp, created.ExpiresAt, "the response echoes the expiry")

	var stored kipperv1.ApiKey
	require.NoError(t, crClient.Get(t.Context(), crclient.ObjectKey{Namespace: "shop-prod", Name: created.Name}, &stored))
	require.NotNil(t, stored.Spec.ExpiresAt, "the expiry is persisted on the CR")
}

func TestAPIGateway_CreateKeyRejectsBadExpiry(t *testing.T) {
	router := gatewayRouter(&APIGateway{CRClient: testCRClient(bronzePlan())})

	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	rec := do(t, router, http.MethodPost, "/projects/shop-prod/api-keys", `{"plan":"bronze","expires_at":"`+past+`"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a past expiry is rejected")

	rec = do(t, router, http.MethodPost, "/projects/shop-prod/api-keys", `{"plan":"bronze","expires_at":"next tuesday"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a malformed expiry is rejected")
}

func TestAPIGateway_CreateKeyRejectsUnsafeDisplayName(t *testing.T) {
	router := gatewayRouter(&APIGateway{CRClient: testCRClient(bronzePlan())})

	// A control character would become an invalid X-Kipper-Key-Name header
	// and make the upstream proxy 500 every request the key authorises.
	// json.Marshal escapes the byte, so the body is valid JSON and the
	// handler decodes a real control character to validate.
	ctrlBody, err := json.Marshal(map[string]string{"plan": "bronze", "display_name": "acme\x07partner"})
	require.NoError(t, err)
	rec := do(t, router, http.MethodPost, "/projects/shop-prod/api-keys", string(ctrlBody))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a control character in the name is rejected")

	rec = do(t, router, http.MethodPost, "/projects/shop-prod/api-keys",
		`{"plan":"bronze","display_name":"`+strings.Repeat("a", 129)+`"}`)
	assert.Equal(t, http.StatusBadRequest, rec.Code, "an over-long name is rejected")

	rec = do(t, router, http.MethodPost, "/projects/shop-prod/api-keys",
		`{"plan":"bronze","display_name":"`+strings.Repeat("a", 128)+`"}`)
	assert.Equal(t, http.StatusCreated, rec.Code, "a name at the 128 limit is accepted")

	rec = do(t, router, http.MethodPost, "/projects/shop-prod/api-keys",
		`{"plan":"bronze","display_name":"Acme Corp partner integration"}`)
	assert.Equal(t, http.StatusCreated, rec.Code, "a normal name is accepted")

	// The plan display name is validated on the same helper, so the plan
	// path rejects an unsafe name too.
	planBody, err := json.Marshal(map[string]any{"name": "silver", "rate": 10, "burst": 20, "display_name": "gold\x07plan"})
	require.NoError(t, err)
	rec = do(t, router, http.MethodPut, "/projects/shop-prod/usage-plans", string(planBody))
	assert.Equal(t, http.StatusBadRequest, rec.Code, "a control character in a plan name is rejected")
}

func TestAPIGateway_CreateKeyRequiresExistingPlan(t *testing.T) {
	router := gatewayRouter(&APIGateway{CRClient: testCRClient()})

	rec := do(t, router, http.MethodPost, "/projects/shop-prod/api-keys", `{"plan":"ghost"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "ghost")
}

func TestAPIGateway_UpdateKeyTogglesAndRescopes(t *testing.T) {
	key := &kipperv1.ApiKey{
		ObjectMeta: metav1.ObjectMeta{Name: "key-ab12cd34", Namespace: "shop-prod"},
		Spec:       kipperv1.ApiKeySpec{Plan: "bronze", Prefix: "ab12cd34", HashSHA256: "aa", Apps: []string{"api"}},
	}
	crClient := testCRClient(bronzePlan(), key)
	router := gatewayRouter(&APIGateway{CRClient: crClient})

	rec := do(t, router, http.MethodPatch, "/projects/shop-prod/api-keys/key-ab12cd34",
		`{"enabled":false,"apps":["api","webhooks"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var stored kipperv1.ApiKey
	require.NoError(t, crClient.Get(t.Context(), crclient.ObjectKey{Namespace: "shop-prod", Name: "key-ab12cd34"}, &stored))
	assert.False(t, stored.IsEnabled())
	assert.Equal(t, []string{"api", "webhooks"}, stored.Spec.Apps)
}

func TestAPIGateway_UpdateKeyRejectsInvalidAppScope(t *testing.T) {
	key := &kipperv1.ApiKey{
		ObjectMeta: metav1.ObjectMeta{Name: "key-ab12cd34", Namespace: "shop-prod"},
		Spec:       kipperv1.ApiKeySpec{Plan: "bronze", Prefix: "ab12cd34", HashSHA256: "aa", Apps: []string{"api"}},
	}
	router := gatewayRouter(&APIGateway{CRClient: testCRClient(bronzePlan(), key)})

	rec := do(t, router, http.MethodPatch, "/projects/shop-prod/api-keys/key-ab12cd34",
		`{"apps":["Not_A_Valid_Name!"]}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "invalid app name")
}

func TestAPIGateway_KeyUsage(t *testing.T) {
	now := time.Now().UTC()
	yesterday := now.AddDate(0, 0, -1).Format("2006-01-02")
	key := &kipperv1.ApiKey{
		ObjectMeta: metav1.ObjectMeta{Name: "key-ab12cd34", Namespace: "shop-prod"},
		Spec:       kipperv1.ApiKeySpec{Plan: "bronze", Prefix: "ab12cd34", HashSHA256: "aa"},
	}
	recent := &kipperv1.UsageRollup{
		ObjectMeta: metav1.ObjectMeta{Name: "rollup-ab12cd34-recent", Namespace: "shop-prod"},
		Spec:       kipperv1.UsageRollupSpec{KeyPrefix: "ab12cd34", Day: yesterday, Allowed: 42, DeniedRate: 3},
	}
	otherKey := &kipperv1.UsageRollup{
		ObjectMeta: metav1.ObjectMeta{Name: "rollup-zz99yy88-recent", Namespace: "shop-prod"},
		Spec:       kipperv1.UsageRollupSpec{KeyPrefix: "zz99yy88", Day: yesterday, Allowed: 7},
	}
	ancient := &kipperv1.UsageRollup{
		ObjectMeta: metav1.ObjectMeta{Name: "rollup-ab12cd34-20200101", Namespace: "shop-prod"},
		Spec:       kipperv1.UsageRollupSpec{KeyPrefix: "ab12cd34", Day: "2020-01-01", Allowed: 9},
	}
	router := gatewayRouter(&APIGateway{CRClient: testCRClient(key, recent, otherKey, ancient)})

	rec := do(t, router, http.MethodGet, "/projects/shop-prod/api-keys/key-ab12cd34/usage", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp keyUsageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, rollupRetentionDays, resp.RetentionDays)
	assert.Equal(t, now.Format("2006-01-02"), resp.To)
	require.Len(t, resp.Days, 1, "other keys' rollups and days outside the window are excluded")
	assert.Equal(t, yesterday, resp.Days[0].Day)
	assert.Equal(t, int64(42), resp.Days[0].Allowed)
	assert.Equal(t, int64(3), resp.Days[0].DeniedRate)
}

func TestAPIGateway_KeyUsageWindowValidation(t *testing.T) {
	key := &kipperv1.ApiKey{
		ObjectMeta: metav1.ObjectMeta{Name: "key-ab12cd34", Namespace: "shop-prod"},
		Spec:       kipperv1.ApiKeySpec{Plan: "bronze", Prefix: "ab12cd34"},
	}
	router := gatewayRouter(&APIGateway{CRClient: testCRClient(key)})
	base := "/projects/shop-prod/api-keys/key-ab12cd34/usage"

	t.Run("inverted range is rejected", func(t *testing.T) {
		rec := do(t, router, http.MethodGet, base+"?from=2026-03-31&to=2026-03-01", "")
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("malformed date is rejected", func(t *testing.T) {
		rec := do(t, router, http.MethodGet, base+"?from=march&to=2026-03-01", "")
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})
	t.Run("retention cap is reported in the effective from", func(t *testing.T) {
		rec := do(t, router, http.MethodGet, base+"?from=2000-01-01", "")
		require.Equal(t, http.StatusOK, rec.Code)
		var resp keyUsageResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		earliest := time.Now().UTC().AddDate(0, 0, -rollupRetentionDays).Format("2006-01-02")
		assert.Equal(t, earliest, resp.From, "from is capped to retention, not the requested date")
	})
	t.Run("a window entirely before retention is rejected", func(t *testing.T) {
		rec := do(t, router, http.MethodGet, base+"?from=2000-01-01&to=2000-02-01", "")
		assert.Equal(t, http.StatusBadRequest, rec.Code, "the whole window predates retention, so from would invert past to")
	})
}

func TestAPIGateway_DeleteKeyAuditsActor(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	key := &kipperv1.ApiKey{
		ObjectMeta: metav1.ObjectMeta{Name: "key-ab12cd34", Namespace: "shop-prod"},
		Spec:       kipperv1.ApiKeySpec{Plan: "bronze", Prefix: "ab12cd34"},
	}
	router := gatewayRouter(&APIGateway{CRClient: testCRClient(key)})

	req := httptest.NewRequest(http.MethodDelete, "/projects/shop-prod/api-keys/key-ab12cd34", strings.NewReader(""))
	ctx := context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: "ops@example.test"})
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req.WithContext(ctx))
	require.Equal(t, http.StatusNoContent, rec.Code)

	line := lastJSONLine(t, buf.Bytes())
	assert.Equal(t, "delete_key", line["action"])
	assert.Equal(t, "ops@example.test", line["actor"], "the audit event must name who revoked the key")
	assert.Equal(t, "shop-prod", line["namespace"])
	assert.Equal(t, "key-ab12cd34", line["key"])
}

// lastJSONLine returns the final JSON log line, so a test tolerates any
// unrelated lines emitted before the one it asserts on.
func lastJSONLine(t *testing.T, out []byte) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var line map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[len(lines)-1]), &line), "want a JSON log line, got %q", string(out))
	return line
}

func TestRandomToken(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		tok, err := randomToken(8)
		require.NoError(t, err)
		require.Len(t, tok, 8)
		for _, c := range tok {
			assert.Contains(t, keyAlphabet, string(c))
		}
		assert.False(t, seen[tok], "tokens must not repeat")
		seen[tok] = true
	}
}
