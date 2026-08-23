package handlers

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func quotaProject(tier string, envs ...kipperv1.ProjectEnvironment) *kipperv1.Project {
	return &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{Name: "shop"},
		Spec:       kipperv1.ProjectSpec{Tier: tier, Environments: envs},
		// The two namespaces these fixtures use, claimed, because usage is only
		// read out of a namespace established as this project's and a project
		// that claims nothing establishes nothing once the label stops
		// answering. The UIDs are newKipperNamespace's. Claiming a namespace a
		// given fixture does not create costs nothing, because a claim covers
		// an object and there is no object.
		Status: kipperv1.ProjectStatus{NamespaceClaims: []kipperv1.NamespaceClaim{
			{Name: "shop-test", UID: "uid-shop-test"},
			{Name: "shop-prod", UID: "uid-shop-prod"},
		}},
	}
}

func liveQuota(ns string, hard, used map[corev1.ResourceName]string) *corev1.ResourceQuota {
	toList := func(m map[corev1.ResourceName]string) corev1.ResourceList {
		out := corev1.ResourceList{}
		for k, v := range m {
			out[k] = resource.MustParse(v)
		}
		return out
	}
	return &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{Name: projectQuotaObjectName, Namespace: ns},
		Spec:       corev1.ResourceQuotaSpec{Hard: toList(hard)},
		Status:     corev1.ResourceQuotaStatus{Hard: toList(hard), Used: toList(used)},
	}
}

func quotaRouter(h *Quota) *chi.Mux {
	r := chi.NewRouter()
	r.Get("/projects/{name}/quota", h.Get)
	r.Put("/projects/{name}/quota", h.Set)
	return r
}

func TestQuota_Get(t *testing.T) {
	h := &Quota{
		// The namespaces the project holds. Usage is only read out of a
		// namespace established as this project's, so a fixture without them
		// reports caps and no usage.
		Client: fake.NewClientset(
			newKipperNamespace("shop-test", "shop", "test", "0"),
			newKipperNamespace("shop-prod", "shop", "prod", "1"),
			liveQuota("shop-test",
				map[corev1.ResourceName]string{
					corev1.ResourceRequestsCPU: "2", corev1.ResourceLimitsCPU: "6",
					corev1.ResourceRequestsMemory: "6Gi", corev1.ResourceLimitsMemory: "12Gi",
				},
				map[corev1.ResourceName]string{
					corev1.ResourceRequestsCPU: "500m", corev1.ResourceLimitsCPU: "7",
					corev1.ResourceRequestsMemory: "1Gi", corev1.ResourceLimitsMemory: "2Gi",
				},
			)),
		CRClient: testCRClient(quotaProject("small",
			kipperv1.ProjectEnvironment{Name: "test"},
			kipperv1.ProjectEnvironment{Name: "prod", Quota: &kipperv1.EnvQuota{
				CPURequest: "6", CPULimit: "12", MemoryRequest: "12Gi", MemoryLimit: "24Gi",
			}},
		)),
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/shop/quota", nil)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp QuotaResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	assert.Equal(t, "small", resp.Tier)
	assert.Len(t, resp.Tiers, 3)
	require.Len(t, resp.Environments, 2)

	test := resp.Environments[0]
	assert.Equal(t, "tier", test.Source)
	assert.Equal(t, "2", test.Hard.CPURequest)
	require.NotNil(t, test.Used)
	assert.Equal(t, "500m", test.Used.CPURequest)
	require.NotNil(t, test.OverQuota, "usage was read, so the comparison ran")
	assert.True(t, *test.OverQuota, "limits.cpu used 7 > hard 6 must flag over-quota")

	prod := resp.Environments[1]
	assert.Equal(t, "override", prod.Source)
	assert.Equal(t, "6", prod.Hard.CPURequest, "override values shown when no live quota object exists yet")
	assert.Nil(t, prod.Used)
	// prod has no live ResourceQuota in this fixture, so nothing compared its
	// usage against its caps and the answer is unknown rather than false.
	assert.Nil(t, prod.OverQuota)
}

func TestQuota_GetDefaultedEnvironment(t *testing.T) {
	h := &Quota{Client: fake.NewClientset(), CRClient: testCRClient(quotaProject(""))}

	req := httptest.NewRequest(http.MethodGet, "/projects/shop/quota", nil)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// A tierless project reports no tier, no caps ("none" source), and the
	// tierless environment limit.
	var resp QuotaResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "", resp.Tier)
	assert.Equal(t, 6, resp.EnvLimit)
	require.Len(t, resp.Environments, 1)
	assert.Equal(t, "test", resp.Environments[0].Environment)
	assert.Equal(t, "shop-test", resp.Environments[0].Namespace)
	assert.Equal(t, "none", resp.Environments[0].Source)
	assert.Equal(t, "", resp.Environments[0].Hard.CPURequest)
}

func TestQuota_ClearTierWithPointerSemantics(t *testing.T) {
	crClient := testCRClient(quotaProject("medium", kipperv1.ProjectEnvironment{Name: "test"}))
	h := &Quota{Client: fake.NewClientset(), CRClient: crClient}

	// An update without the tier field leaves the tier alone.
	req := httptest.NewRequest(http.MethodPut, "/projects/shop/quota",
		strings.NewReader(`{"environments":[{"name":"test","quota":null}]}`))
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var stored kipperv1.Project
	require.NoError(t, crClient.Get(req.Context(), crclient.ObjectKey{Name: "shop"}, &stored))
	assert.Equal(t, "medium", stored.Spec.Tier, "omitting tier must not change it")

	// An explicit empty tier clears it: the project becomes tierless.
	req = httptest.NewRequest(http.MethodPut, "/projects/shop/quota", strings.NewReader(`{"tier":""}`))
	rec = httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.NoError(t, crClient.Get(req.Context(), crclient.ObjectKey{Name: "shop"}, &stored))
	assert.Equal(t, "", stored.Spec.Tier)

	var resp QuotaResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "", resp.Tier)
	assert.Equal(t, "none", resp.Environments[0].Source)
}

func TestQuota_SetTierAndOverride(t *testing.T) {
	crClient := testCRClient(quotaProject("small", kipperv1.ProjectEnvironment{Name: "test"}))
	h := &Quota{Client: fake.NewClientset(), CRClient: crClient}

	body := `{"tier":"medium","environments":[{"name":"test","quota":{"cpu_request":"6","cpu_limit":"12","memory_request":"12Gi","memory_limit":"24Gi"}}]}`
	req := httptest.NewRequest(http.MethodPut, "/projects/shop/quota", strings.NewReader(body))
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var stored kipperv1.Project
	require.NoError(t, crClient.Get(req.Context(), crclient.ObjectKey{Name: "shop"}, &stored))
	assert.Equal(t, "medium", stored.Spec.Tier)
	require.NotNil(t, stored.Spec.Environments[0].Quota)
	assert.Equal(t, "6", stored.Spec.Environments[0].Quota.CPURequest)
}

func TestQuota_SetClearsOverride(t *testing.T) {
	crClient := testCRClient(quotaProject("small",
		kipperv1.ProjectEnvironment{Name: "test", Quota: &kipperv1.EnvQuota{
			CPURequest: "6", CPULimit: "12", MemoryRequest: "12Gi", MemoryLimit: "24Gi",
		}},
	))
	h := &Quota{Client: fake.NewClientset(), CRClient: crClient}

	body := `{"environments":[{"name":"test","quota":null}]}`
	req := httptest.NewRequest(http.MethodPut, "/projects/shop/quota", strings.NewReader(body))
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var stored kipperv1.Project
	require.NoError(t, crClient.Get(req.Context(), crclient.ObjectKey{Name: "shop"}, &stored))
	assert.Nil(t, stored.Spec.Environments[0].Quota)
}

func TestQuota_SetBelowUsageWarnsThenForces(t *testing.T) {
	// The namespace currently uses 3 CPU of requests; dropping to the small
	// tier (2 CPU) must be refused with the offending dimension until the
	// caller confirms with force.
	crClient := testCRClient(quotaProject("large", kipperv1.ProjectEnvironment{Name: "test"}))
	h := &Quota{
		// The namespaces the project holds. Usage is only read out of a
		// namespace established as this project's, so a fixture without them
		// reports caps and no usage.
		Client: fake.NewClientset(
			newKipperNamespace("shop-test", "shop", "test", "0"),
			newKipperNamespace("shop-prod", "shop", "prod", "1"),
			liveQuota("shop-test",
				map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "8"},
				map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "3"},
			)),
		CRClient: crClient,
	}

	body := `{"tier":"small"}`
	req := httptest.NewRequest(http.MethodPut, "/projects/shop/quota", strings.NewReader(body))
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	var conflict struct {
		Warnings []QuotaWarning `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &conflict))
	require.Len(t, conflict.Warnings, 1)
	assert.Equal(t, "requests.cpu", conflict.Warnings[0].Dimension)
	assert.Equal(t, "3", conflict.Warnings[0].Used)
	assert.Equal(t, "2", conflict.Warnings[0].NewCap)

	var stored kipperv1.Project
	require.NoError(t, crClient.Get(req.Context(), crclient.ObjectKey{Name: "shop"}, &stored))
	assert.Equal(t, "large", stored.Spec.Tier, "a refused change must not be applied")

	forced := httptest.NewRequest(http.MethodPut, "/projects/shop/quota", strings.NewReader(`{"tier":"small","force":true}`))
	rec = httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, forced)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	require.NoError(t, crClient.Get(req.Context(), crclient.ObjectKey{Name: "shop"}, &stored))
	assert.Equal(t, "small", stored.Spec.Tier)
}

func TestQuota_SetRejectsInvalidInput(t *testing.T) {
	h := &Quota{Client: fake.NewClientset(), CRClient: testCRClient(quotaProject("small", kipperv1.ProjectEnvironment{Name: "test"}))}

	for name, body := range map[string]string{
		"bad tier":       `{"tier":"galactic"}`,
		"bad quantity":   `{"environments":[{"name":"test","quota":{"cpu_request":"lots","cpu_limit":"4","memory_request":"4Gi","memory_limit":"8Gi"}}]}`,
		"unknown env":    `{"environments":[{"name":"prod","quota":null}]}`,
		"malformed json": `{`,
	} {
		req := httptest.NewRequest(http.MethodPut, "/projects/shop/quota", strings.NewReader(body))
		rec := httptest.NewRecorder()
		quotaRouter(h).ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", name, rec.Body.String())
	}
}

// Ownership has to stop the request going out, not just stop its answer being
// used. A Go if-statement runs its initializer before it evaluates the
// condition, so reading first and discarding on `owned` still issued a
// privileged GET against a namespace belonging to somebody else.
//
// Asserting on the response cannot see that: the values were already being
// dropped. This asserts on what the client was asked to do.
func TestQuota_DoesNotReadAForeignNamespacesQuota(t *testing.T) {
	client := fake.NewClientset(
		newKipperNamespace("shop-test", "shop", "test", "0"),
		// shop declares prod; somebody else holds the namespace.
		newKipperNamespace("shop-prod", "somebody-else", "prod", "1"),
		liveQuota("shop-prod",
			map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "99"},
			map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "42"},
		),
	)
	h := &Quota{Client: client, CRClient: testCRClient(quotaProject("small",
		kipperv1.ProjectEnvironment{Name: "test"},
		kipperv1.ProjectEnvironment{Name: "prod"},
	))}

	req := httptest.NewRequest(http.MethodGet, "/projects/shop/quota", nil)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	for _, action := range client.Actions() {
		get, ok := action.(k8stesting.GetAction)
		if !ok || action.GetResource().Resource != "resourcequotas" {
			continue
		}
		assert.NotEqual(t, "shop-prod", get.GetNamespace(),
			"the quota of a namespace shop does not own must not be read at all")
	}

	// And nothing of it reaches the response.
	assert.NotContains(t, rec.Body.String(), `"42"`, "foreign usage must not be reported")
}

// The same for the warning path, which reads usage to decide whether a new cap
// would be breached. Somebody else's usage is neither the caller's to see nor
// governed by the cap they are setting.
func TestQuota_SetIgnoresAForeignNamespacesUsage(t *testing.T) {
	client := fake.NewClientset(
		newKipperNamespace("shop-prod", "somebody-else", "prod", "0"),
		liveQuota("shop-prod",
			map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "99"},
			map[corev1.ResourceName]string{corev1.ResourceRequestsCPU: "98"},
		),
	)
	crClient := testCRClient(quotaProject("large", kipperv1.ProjectEnvironment{Name: "prod"}))
	h := &Quota{Client: client, CRClient: crClient}

	// A tier whose caps are far below the foreign namespace's usage. If that
	// usage were read, this would be refused as over-quota.
	body := strings.NewReader(`{"tier":"small"}`)
	req := httptest.NewRequest(http.MethodPut, "/projects/shop/quota", body)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)

	assert.NotEqual(t, http.StatusConflict, rec.Code,
		"a warning must not be raised from usage in a namespace the project does not own: %s", rec.Body.String())

	for _, action := range client.Actions() {
		get, ok := action.(k8stesting.GetAction)
		if !ok || action.GetResource().Resource != "resourcequotas" {
			continue
		}
		assert.NotEqual(t, "shop-prod", get.GetNamespace(),
			"the foreign namespace's quota must not be read")
	}
}

// The warning path reads namespaces to establish ownership, so the error it can
// carry is a Kubernetes one. None of that is the caller's to read.
func TestQuota_SetRedactsAnOwnershipOutage(t *testing.T) {
	client := fake.NewClientset(newKipperNamespace("shop-prod", "shop", "prod", "0"))
	client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd leader election in progress")
	})
	h := &Quota{
		Client:   client,
		CRClient: testCRClient(quotaProject("large", kipperv1.ProjectEnvironment{Name: "prod"})),
	}

	body := strings.NewReader(`{"tier":"small"}`)
	req := httptest.NewRequest(http.MethodPut, "/projects/shop/quota", body)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "etcd",
		"the underlying failure must not be echoed to the caller")
}

// The other Kubernetes error on the same request. Fixing the ownership check's
// error and leaving this one would have moved the disclosure rather than closed
// it.
func TestQuota_SetRedactsAFailedUpdate(t *testing.T) {
	crClient := crfake.NewClientBuilder().WithScheme(testScheme()).
		WithObjects(quotaProject("large", kipperv1.ProjectEnvironment{Name: "prod"})).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(context.Context, crclient.WithWatch, crclient.Object, ...crclient.UpdateOption) error {
				return apierrors.NewServiceUnavailable("etcd leader election in progress")
			},
		}).Build()
	h := &Quota{
		Client:   fake.NewClientset(newKipperNamespace("shop-prod", "shop", "prod", "0")),
		CRClient: crClient,
	}

	body := strings.NewReader(`{"tier":"large","force":true}`)
	req := httptest.NewRequest(http.MethodPut, "/projects/shop/quota", body)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "etcd",
		"a failed update must not carry the Kubernetes error to the caller")
}

// An ownership check that could not run is not a quota answer. Suppressing the
// error alongside absent and foreign namespaces returned 200 with declared caps
// and no usage, which is exactly what a healthy environment looks like before
// its quota publishes status.
func TestQuota_GetRefusesWhenOwnershipCannotBeRead(t *testing.T) {
	client := fake.NewClientset(newKipperNamespace("shop-prod", "shop", "prod", "0"))
	client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd leader election in progress")
	})
	h := &Quota{
		Client:   client,
		CRClient: testCRClient(quotaProject("small", kipperv1.ProjectEnvironment{Name: "prod"})),
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/shop/quota", nil)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"an unreadable ownership check must not read as a healthy environment: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "etcd", "and must not carry the cause to the caller")
}

// A namespace that is absent or somebody else's is still skipped, and the
// environment still reported with its declared caps. Only a check that could
// not run stops the request.
func TestQuota_GetStillReportsAnEnvironmentWhoseNamespaceIsForeign(t *testing.T) {
	h := &Quota{
		Client:   fake.NewClientset(newKipperNamespace("shop-prod", "somebody-else", "prod", "0")),
		CRClient: testCRClient(quotaProject("small", kipperv1.ProjectEnvironment{Name: "prod"})),
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/shop/quota", nil)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp QuotaResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Environments, 1)
	assert.Nil(t, resp.Environments[0].Used, "no usage is read out of a namespace the project does not own")
	assert.NotEmpty(t, resp.Environments[0].Hard.CPURequest, "the project's own declared caps are still reported")
}

// A write that committed must not be reported as failed. Set used to build its
// response by calling Get, so once Get began failing closed on an unreadable
// ownership check, a successful update answered 500 and invited a retry against
// state that had already changed.
func TestQuota_SetSucceedsWhenThePostUpdateReadCannotRun(t *testing.T) {
	client := fake.NewClientset(newKipperNamespace("shop-prod", "shop", "prod", "0"))
	// Every namespace read fails, so the ownership check cannot run at all.
	client.PrependReactor("get", "namespaces", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd leader election in progress")
	})
	crClient := testCRClient(quotaProject("small", kipperv1.ProjectEnvironment{Name: "prod"}))
	h := &Quota{Client: client, CRClient: crClient}

	// force skips the pre-update usage check, so nothing reads a namespace
	// until after the update has committed.
	body := strings.NewReader(`{"tier":"large","force":true}`)
	req := httptest.NewRequest(http.MethodPut, "/projects/shop/quota", body)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code,
		"the update committed, so the request succeeded: %s", rec.Body.String())

	// And it really did commit, which is what makes reporting failure wrong.
	var stored kipperv1.Project
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Name: "shop"}, &stored))
	assert.Equal(t, "large", stored.Spec.Tier, "the tier must be the one that was written")

	// The degraded body is still a quota view carrying the new caps, so a
	// caller reading the response gets the change it asked for.
	var resp QuotaResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "large", resp.Tier)
	require.Len(t, resp.Environments, 1)
	assert.Nil(t, resp.Environments[0].Used, "usage is what could not be read")

	// Against the encoded response, not the decoded struct. Unmarshalling an
	// omitted member and an explicit null both give a nil pointer, so a Go-level
	// assertion cannot tell the two apart — and the contract the console is
	// typed against is the key being present and null.
	var raw struct {
		Environments []map[string]json.RawMessage `json:"environments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Len(t, raw.Environments, 1)
	over, present := raw.Environments[0]["over_quota"]
	require.True(t, present, "over_quota must be sent rather than omitted")
	assert.JSONEq(t, "null", string(over),
		"nothing compared usage against the caps, so this must be null rather than false")
}

// The third state, which the other two tests leave uncovered: usage was read,
// compared, and found within its caps. A pointer to false and an unknown are
// different answers and only one of them is a claim.
func TestQuota_GetReportsAnExplicitFalseWhenUsageIsWithinCaps(t *testing.T) {
	h := &Quota{
		Client: fake.NewClientset(
			newKipperNamespace("shop-prod", "shop", "prod", "0"),
			liveQuota("shop-prod",
				map[corev1.ResourceName]string{corev1.ResourceLimitsCPU: "6"},
				map[corev1.ResourceName]string{corev1.ResourceLimitsCPU: "1"},
			),
		),
		CRClient: testCRClient(quotaProject("small", kipperv1.ProjectEnvironment{Name: "prod"})),
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/shop/quota", nil)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp QuotaResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Environments, 1)
	require.NotNil(t, resp.Environments[0].OverQuota,
		"usage was compared, so the answer is known")
	assert.False(t, *resp.Environments[0].OverQuota, "1 of 6 is within its cap")

	var raw struct {
		Environments []map[string]json.RawMessage `json:"environments"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.JSONEq(t, "false", string(raw.Environments[0]["over_quota"]),
		"a comparison that ran and found nothing over must say so, not go quiet")
}

// The healthy path keeps the contract it always had: a successful update
// answers with the same live-enriched view a GET returns. Falling back to the
// no-usage view for every update, rather than only when the read fails, made a
// PUT report over_quota as false over a namespace that was over its limit.
func TestQuota_SetReturnsLiveUsageOnTheHealthyPath(t *testing.T) {
	h := &Quota{
		Client: fake.NewClientset(
			newKipperNamespace("shop-prod", "shop", "prod", "0"),
			liveQuota("shop-prod",
				map[corev1.ResourceName]string{corev1.ResourceLimitsCPU: "6"},
				map[corev1.ResourceName]string{corev1.ResourceLimitsCPU: "7"},
			),
		),
		CRClient: testCRClient(quotaProject("small", kipperv1.ProjectEnvironment{Name: "prod"})),
	}

	body := strings.NewReader(`{"tier":"small","force":true}`)
	req := httptest.NewRequest(http.MethodPut, "/projects/shop/quota", body)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp QuotaResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Environments, 1)
	require.NotNil(t, resp.Environments[0].Used, "a successful update reports live usage")
	require.NotNil(t, resp.Environments[0].OverQuota)
	assert.True(t, *resp.Environments[0].OverQuota,
		"limits.cpu used 7 over hard 6 must be reported by the update too, not only by a later GET")
}

// A quota object that cannot be read is not a quota object that is not there.
// Both used to leave usage unset, so a 403 or a 503 answered 200 with declared
// caps and no usage — the same shape a quota that has not published status yet
// produces, and the same shape a healthy tierless project produces.
func TestQuota_GetRefusesWhenTheQuotaObjectCannotBeRead(t *testing.T) {
	client := fake.NewClientset(newKipperNamespace("shop-prod", "shop", "prod", "0"))
	client.PrependReactor("get", "resourcequotas", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			corev1.Resource("resourcequotas"), projectQuotaObjectName,
			goerrors.New("the console service account may not read this"))
	})
	h := &Quota{
		Client:   client,
		CRClient: testCRClient(quotaProject("small", kipperv1.ProjectEnvironment{Name: "prod"})),
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/shop/quota", nil)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusInternalServerError, rec.Code,
		"a quota that could not be read must not read as one that is not there: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "service account",
		"and the cause must not reach the caller")
}

// The other half: an absent quota object stays an ordinary state. A tierless
// project has none, and a new environment has none until its reconcile runs.
func TestQuota_GetReportsAnEnvironmentWhoseQuotaDoesNotExistYet(t *testing.T) {
	h := &Quota{
		Client:   fake.NewClientset(newKipperNamespace("shop-prod", "shop", "prod", "0")),
		CRClient: testCRClient(quotaProject("small", kipperv1.ProjectEnvironment{Name: "prod"})),
	}

	req := httptest.NewRequest(http.MethodGet, "/projects/shop/quota", nil)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp QuotaResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Environments, 1)
	assert.Nil(t, resp.Environments[0].Used, "there is no quota object to read usage from")
	assert.Nil(t, resp.Environments[0].OverQuota, "so nothing compared it against the caps")
	assert.NotEmpty(t, resp.Environments[0].Hard.CPURequest, "the declared caps still stand")
}

// And on the write path it degrades rather than failing, for the same reason a
// failed ownership read does: the update has already committed.
func TestQuota_SetSucceedsWhenTheQuotaObjectCannotBeRead(t *testing.T) {
	client := fake.NewClientset(newKipperNamespace("shop-prod", "shop", "prod", "0"))
	client.PrependReactor("get", "resourcequotas", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd leader election in progress")
	})
	crClient := testCRClient(quotaProject("small", kipperv1.ProjectEnvironment{Name: "prod"}))
	h := &Quota{Client: client, CRClient: crClient}

	body := strings.NewReader(`{"tier":"large","force":true}`)
	req := httptest.NewRequest(http.MethodPut, "/projects/shop/quota", body)
	rec := httptest.NewRecorder()
	quotaRouter(h).ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code,
		"the update committed, so the request succeeded: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "etcd")

	var stored kipperv1.Project
	require.NoError(t, crClient.Get(context.Background(), crclient.ObjectKey{Name: "shop"}, &stored))
	assert.Equal(t, "large", stored.Spec.Tier)
}
