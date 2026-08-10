package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/console-api/middleware"
)

func dexConfigMapWithIssuer() *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dex-config",
			Namespace: "dex",
		},
		Data: map[string]string{
			"config.yaml": "issuer: https://dex.test/dex\nstaticPasswords: []\n",
		},
	}
}

func dexDeploymentForInvite() *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dex",
			Namespace: "dex",
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "dex"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "dex"}},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "dex", Image: "dex:latest"}}},
			},
		},
	}
}

func TestInvite_Create(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(),
		dexNamespace(),
		dexConfigMapWithIssuer(),
		dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	r := chi.NewRouter()
	r.Post("/api/v1/invites", inv.Create)

	body := `{"role":"deployer","expires":"48h","email":"newuser@example.com"}`
	req := httptest.NewRequest("POST", "/api/v1/invites", strings.NewReader(body))
	req.Host = "console-api.example.com"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp createInviteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	assert.NotEmpty(t, resp.Token)
	assert.NotEmpty(t, resp.URL)
	assert.Contains(t, resp.URL, resp.Token)
	assert.Equal(t, "deployer", resp.Role)
	assert.NotEmpty(t, resp.Expires)

	// Verify invite stored in ConfigMap
	cm, err := client.CoreV1().ConfigMaps("kipper-system").Get(context.Background(), "kipper-invites", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, cm.Data["invites"], resp.Token)
}

// An invite without an address is a bearer credential: whoever sees the link
// takes the role under whatever identity they type. The account is keyed by an
// email either way, so requiring it here asks for nothing the flow does not
// already need — it moves the choice of identity from whoever holds the token
// to whoever issues the invite, which is where that authority belongs.
func TestInvite_Create_RequiresAnAddress(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(), dexConfigMapWithIssuer(), dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	inv := &Invites{Client: client, RoleStore: store, Users: &Users{Client: client, RoleStore: store}}

	r := chi.NewRouter()
	r.Post("/api/v1/invites", inv.Create)

	for _, body := range []string{
		`{"role":"admin","expires":"48h"}`,
		`{"role":"admin","expires":"48h","email":""}`,
		`{"role":"admin","expires":"48h","email":"   "}`,
	} {
		req := httptest.NewRequest("POST", "/api/v1/invites", strings.NewReader(body))
		req.Host = "console-api.example.com"
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		assert.Equal(t, http.StatusBadRequest, rec.Code, "body %s", body)
	}

	// And nothing was stored for any of them.
	cm, err := client.CoreV1().ConfigMaps(inviteNamespace).Get(context.Background(), inviteConfigMapName, metav1.GetOptions{})
	if err == nil {
		assert.Equal(t, "{}", strings.TrimSpace(cm.Data["invites"]), "a refused invite must not be stored")
	}
}

// The stored address is trimmed, because it becomes the identity at acceptance.
func TestInvite_Create_StoresTheAddressTrimmed(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(), dexConfigMapWithIssuer(), dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	inv := &Invites{Client: client, RoleStore: store, Users: &Users{Client: client, RoleStore: store}}

	r := chi.NewRouter()
	r.Post("/api/v1/invites", inv.Create)
	req := httptest.NewRequest("POST", "/api/v1/invites",
		strings.NewReader(`{"role":"viewer","expires":"24h","email":"  someone@example.com  "}`))
	req.Host = "console-api.example.com"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp createInviteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	stored, err := inv.loadInvite(context.Background(), resp.Token)
	require.NoError(t, err)
	assert.Equal(t, "someone@example.com", stored.Email)
}

func TestInvite_Validate(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(),
		dexNamespace(),
		dexConfigMapWithIssuer(),
		dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	// Store an invite directly
	expires := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	err := inv.storeInvite(context.Background(), invite{
		Token:   "valid-token-123",
		Role:    "viewer",
		Expires: expires,
	})
	assert.NoError(t, err)

	r := chi.NewRouter()
	r.Get("/api/v1/invites/{token}", inv.Validate)

	req := httptest.NewRequest("GET", "/api/v1/invites/valid-token-123", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	assert.Equal(t, "viewer", resp["role"])
	assert.Equal(t, expires, resp["expires"])
}

// Acceptance requires the submitted address to match the invited one. Returning
// that address to an unauthenticated caller holding the token would tell whoever
// has the link exactly what to type, which is the whole check.
func TestInvite_Validate_DoesNotDiscloseTheInvitedAddress(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(), dexConfigMapWithIssuer(), dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	inv := &Invites{Client: client, RoleStore: store, Users: &Users{Client: client, RoleStore: store}}

	require.NoError(t, inv.storeInvite(context.Background(), invite{
		Token:   "secret-address-token",
		Role:    "admin",
		Email:   "invited@example.com",
		Expires: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}))

	r := chi.NewRouter()
	r.Get("/api/v1/invites/{token}", inv.Validate)
	req := httptest.NewRequest("GET", "/api/v1/invites/secret-address-token", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), "invited@example.com",
		"the address the invite is for must not be handed to whoever holds the token")
	assert.Contains(t, rec.Body.String(), "admin", "the role is still shown, so the form can describe the invite")
}

func TestInvite_Validate_Expired(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(),
		dexNamespace(),
		dexConfigMapWithIssuer(),
		dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	// Store an expired invite
	expires := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	err := inv.storeInvite(context.Background(), invite{
		Token:   "expired-token",
		Role:    "deployer",
		Expires: expires,
	})
	assert.NoError(t, err)

	r := chi.NewRouter()
	r.Get("/api/v1/invites/{token}", inv.Validate)

	req := httptest.NewRequest("GET", "/api/v1/invites/expired-token", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusGone, rec.Code)
}

func TestInvite_Validate_NotFound(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(),
		dexNamespace(),
		dexConfigMapWithIssuer(),
		dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	r := chi.NewRouter()
	r.Get("/api/v1/invites/{token}", inv.Validate)

	req := httptest.NewRequest("GET", "/api/v1/invites/nonexistent-token", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestInvite_Accept(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(),
		dexNamespace(),
		dexConfigMapWithIssuer(),
		dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	// Store a valid invite
	expires := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	err := inv.storeInvite(context.Background(), invite{
		Token:   "accept-token",
		Role:    "deployer",
		Expires: expires,
	})
	assert.NoError(t, err)

	r := chi.NewRouter()
	r.Post("/api/v1/invites/{token}/accept", inv.Accept)

	body := `{"email":"newuser@example.com","password":"Strong-pass1!"}`
	req := httptest.NewRequest("POST", "/api/v1/invites/accept-token/accept", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	assert.Equal(t, "created", resp["status"])
	assert.Equal(t, "newuser@example.com", resp["email"])
	assert.Equal(t, "deployer", resp["role"])

	// Verify user added to role store ConfigMap
	roleUsers := store.ListUsers()
	assert.Equal(t, "deployer", roleUsers["newuser@example.com"])

	// Verify invite deleted from ConfigMap
	_, err = inv.loadInvite(context.Background(), "accept-token")
	assert.Error(t, err)

	// Verify user added to Dex config
	dexCM, err := client.CoreV1().ConfigMaps("dex").Get(context.Background(), "dex-config", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, dexCM.Data["config.yaml"], "newuser@example.com")
}

// An invite addressed to someone is theirs. The token travels by email, and
// anybody who comes by it — a forwarded message, a mail archive, a shared
// inbox — could otherwise redeem it under an address of their own choosing and
// take the role and the project membership meant for the person invited.
func TestInvite_Accept_RefusesAnEmailTheInviteWasNotAddressedTo(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(), dexConfigMapWithIssuer(), dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	require.NoError(t, inv.storeInvite(context.Background(), invite{
		Token:   "addressed-token",
		Role:    "admin",
		Email:   "invited@example.com",
		Expires: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}))

	r := chi.NewRouter()
	r.Post("/api/v1/invites/{token}/accept", inv.Accept)

	body := `{"email":"attacker@example.com","password":"Strong-pass1!"}`
	req := httptest.NewRequest("POST", "/api/v1/invites/addressed-token/accept", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code, "body: %s", rec.Body.String())
	assert.NotContains(t, store.ListUsers(), "attacker@example.com", "no account for an address the invite was not sent to")
	assert.Empty(t, store.ListUsers()["attacker@example.com"], "and certainly not the admin role")

	// And the invite survives, so the person it was addressed to can still use it.
	_, err := inv.loadInvite(context.Background(), "addressed-token")
	assert.NoError(t, err, "a refused attempt must not burn the invite")
}

// Once an invite carries an address, that address is the identity — the request
// does not get to choose the spelling the account is keyed by. Matching is a
// courtesy so a typo says so; it is not the thing that decides who is created.
// EqualFold is Unicode simple folding rather than address canonicalisation, so
// letting the submitted spelling through would key an account by a string that
// merely folds to the invited one.
func TestInvite_Accept_CreatesTheAccountUnderTheInvitedAddress(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(), dexConfigMapWithIssuer(), dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	require.NoError(t, inv.storeInvite(context.Background(), invite{
		Token:   "identity-token",
		Role:    "admin",
		Email:   "Invited@Example.com",
		Expires: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}))

	r := chi.NewRouter()
	r.Post("/api/v1/invites/{token}/accept", inv.Accept)
	req := httptest.NewRequest("POST", "/api/v1/invites/identity-token/accept",
		strings.NewReader(`{"email":"  invited@example.com  ","password":"Strong-pass1!"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

	usersByEmail := store.ListUsers()
	assert.Equal(t, "admin", usersByEmail["Invited@Example.com"],
		"the role belongs to the address the invite was sent to")
	assert.NotContains(t, usersByEmail, "  invited@example.com  ",
		"and not to whatever the request typed")

	dexCM, err := client.CoreV1().ConfigMaps("dex").Get(context.Background(), "dex-config", metav1.GetOptions{})
	require.NoError(t, err)
	assert.Contains(t, dexCM.Data["config.yaml"], "Invited@Example.com")
}

// The address is the person, not the spelling of it.
func TestInvite_Accept_MatchesTheAddressCaseInsensitively(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(), dexConfigMapWithIssuer(), dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	require.NoError(t, inv.storeInvite(context.Background(), invite{
		Token:   "case-token",
		Role:    "deployer",
		Email:   "Invited@Example.com",
		Expires: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}))

	r := chi.NewRouter()
	r.Post("/api/v1/invites/{token}/accept", inv.Accept)

	body := `{"email":"invited@example.com","password":"Strong-pass1!"}`
	req := httptest.NewRequest("POST", "/api/v1/invites/case-token/accept", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
}

// The race the claim exists for: another request takes the invite between this
// one reading it and this one writing. The fake client tracks no
// resourceVersion and never returns Conflict, so the interleaving is staged —
// the write is refused once, and the re-read that follows finds the invite
// gone, which is exactly what a lost CAS looks like against a real API server.
//
// Before the claim, Accept created the account first and deleted the invite
// last, so this sequence minted an account from an invite someone else had
// already spent.
func TestInvite_Accept_RefusesAnInviteClaimedByAnotherRequest(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(), dexConfigMapWithIssuer(), dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	require.NoError(t, inv.storeInvite(context.Background(), invite{
		Token:   "race-token",
		Role:    "admin",
		Expires: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}))

	// The other request wins the write, then the invite is gone from every
	// later read.
	taken := false
	client.PrependReactor("update", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		u, ok := action.(k8stesting.UpdateAction)
		if !ok || u.GetObject().(*corev1.ConfigMap).Name != inviteConfigMapName {
			return false, nil, nil
		}
		if !taken {
			taken = true
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Resource: "configmaps"}, inviteConfigMapName, assert.AnError)
		}
		return false, nil, nil
	})
	client.PrependReactor("get", "configmaps", func(action k8stesting.Action) (bool, runtime.Object, error) {
		g, ok := action.(k8stesting.GetAction)
		if !ok || g.GetName() != inviteConfigMapName || !taken {
			return false, nil, nil
		}
		return true, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: inviteConfigMapName, Namespace: inviteNamespace},
			Data:       map[string]string{"invites": "{}"},
		}, nil
	})

	r := chi.NewRouter()
	r.Post("/api/v1/invites/{token}/accept", inv.Accept)
	req := httptest.NewRequest("POST", "/api/v1/invites/race-token/accept",
		strings.NewReader(`{"email":"loser@example.com","password":"Strong-pass1!"}`))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.NotEqual(t, http.StatusOK, rec.Code,
		"an invite another request already claimed must not be redeemed again: %s", rec.Body.String())
	assert.Empty(t, store.ListUsers()["loser@example.com"], "and no account may be minted from it")
}

// A spent invite cannot be redeemed a second time.
func TestInvite_Accept_IsSingleUse(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(), dexNamespace(), dexConfigMapWithIssuer(), dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	require.NoError(t, inv.storeInvite(context.Background(), invite{
		Token:   "once-token",
		Role:    "admin",
		Expires: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
	}))

	r := chi.NewRouter()
	r.Post("/api/v1/invites/{token}/accept", inv.Accept)

	call := func(email string) int {
		req := httptest.NewRequest("POST", "/api/v1/invites/once-token/accept",
			strings.NewReader(`{"email":"`+email+`","password":"Strong-pass1!"}`))
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec.Code
	}

	first := call("first@example.com")
	second := call("second@example.com")

	assert.Equal(t, http.StatusOK, first)
	assert.NotEqual(t, http.StatusOK, second, "the second redemption of one invite must not succeed")
	assert.Empty(t, store.ListUsers()["second@example.com"], "and must mint no second account")
}

func TestInvite_Accept_Expired(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(),
		dexNamespace(),
		dexConfigMapWithIssuer(),
		dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	// Store an expired invite
	expires := time.Now().Add(-1 * time.Hour).Format(time.RFC3339)
	err := inv.storeInvite(context.Background(), invite{
		Token:   "expired-accept-token",
		Role:    "deployer",
		Expires: expires,
	})
	assert.NoError(t, err)

	r := chi.NewRouter()
	r.Post("/api/v1/invites/{token}/accept", inv.Accept)

	body := `{"email":"newuser@example.com","password":"Strong-pass1!"}`
	req := httptest.NewRequest("POST", "/api/v1/invites/expired-accept-token/accept", strings.NewReader(body))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusGone, rec.Code)
}

func TestInvite_Accept_MissingFields(t *testing.T) {
	client := fake.NewClientset(
		kipperSystemNamespace(),
		dexNamespace(),
		dexConfigMapWithIssuer(),
		dexDeploymentForInvite(),
	)
	store := middleware.NewRoleStore(client)
	users := &Users{Client: client, RoleStore: store}
	inv := &Invites{Client: client, RoleStore: store, Users: users}

	// Store a valid invite
	expires := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	err := inv.storeInvite(context.Background(), invite{
		Token:   "missing-fields-token",
		Role:    "deployer",
		Expires: expires,
	})
	assert.NoError(t, err)

	r := chi.NewRouter()
	r.Post("/api/v1/invites/{token}/accept", inv.Accept)

	tests := []struct {
		name string
		body string
	}{
		{"missing email", `{"password":"secret"}`},
		{"missing password", `{"email":"test@example.com"}`},
		{"both missing", `{}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/api/v1/invites/missing-fields-token/accept", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusBadRequest, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

func TestParseDuration_Days(t *testing.T) {
	d, err := parseDuration("7d")
	assert.NoError(t, err)
	assert.Equal(t, 168*time.Hour, d)
}

func TestParseDuration_Hours(t *testing.T) {
	d, err := parseDuration("48h")
	assert.NoError(t, err)
	assert.Equal(t, 48*time.Hour, d)
}

func TestParseDuration_Invalid(t *testing.T) {
	_, err := parseDuration("abc")
	assert.Error(t, err)
}
