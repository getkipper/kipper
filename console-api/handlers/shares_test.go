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
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/share"
)

const sharesTestHost = "mailhog-supplemento-test.example.com"

func sharesTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding kipper scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding networking scheme: %v", err)
	}
	return scheme
}

// sharesFixture wires a Shares handler over fake clients with a
// reconciled mailhog service.
func sharesFixture(t *testing.T) *Shares {
	t.Helper()
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "mailhog", Namespace: "supplemento-test", UID: "uid-mailhog-1"},
		Spec:       kipperv1.ServiceSpec{Type: "mailhog"},
	}
	ing := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "mailhog-ui", Namespace: "supplemento-test"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{Host: sharesTestHost}},
		},
	}
	clientset := k8sfake.NewSimpleClientset()
	return &Shares{
		Client:   clientset,
		CRClient: crfake.NewClientBuilder().WithScheme(sharesTestScheme(t)).WithObjects(svc, ing).Build(),
		Grants:   share.NewGrantStore(clientset),
		Domain:   "example.com",
	}
}

func sharesRequest(t *testing.T, method, path, body string) *http.Request {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	return r
}

func routeShares(s *Shares) chi.Router {
	r := chi.NewRouter()
	r.Post("/services/{name}/shares", s.Create)
	r.Get("/services/{name}/shares", s.List)
	r.Delete("/services/{name}/shares/{id}", s.Revoke)
	r.Delete("/shares", s.RevokeAll)
	r.Post("/shares/rotate-key", s.RotateKey)
	return r
}

func TestSharesCreateListRevoke(t *testing.T) {
	s := sharesFixture(t)
	router := routeShares(s)

	// Mint.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, sharesRequest(t, http.MethodPost,
		"/services/mailhog/shares?namespace=supplemento-test",
		`{"expires_in":"72h","label":"PO review"}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d, body %s", w.Code, w.Body.String())
	}
	var created shareLinkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding create response: %v", err)
	}
	if !strings.HasPrefix(created.URL, "https://"+sharesTestHost+"/?kipper_share=") {
		t.Errorf("URL = %q, want the UI host with the token param", created.URL)
	}
	if created.Label != "PO review" || created.ID == "" {
		t.Errorf("created = %+v, want label and id", created)
	}
	if got := time.Until(created.ExpiresAt); got < 71*time.Hour || got > 73*time.Hour {
		t.Errorf("expiry %s not ~72h out", created.ExpiresAt)
	}

	// The minted token round-trips through the real validators.
	keyring, err := share.LoadKeyring(context.Background(), s.Client)
	if err != nil {
		t.Fatalf("loading keyring after mint: %v", err)
	}
	token := created.URL[strings.Index(created.URL, "=")+1:]
	claims, err := share.ValidateGrantToken(keyring, token, sharesTestHost, time.Now())
	if err != nil {
		t.Fatalf("minted token failed validation: %v", err)
	}
	if claims.ServiceUID != "uid-mailhog-1" {
		t.Errorf("uid claim = %q", claims.ServiceUID)
	}
	grant := s.Grants.Get(context.Background(), claims.ID)
	if grant == nil || !grant.Matches(claims, sharesTestHost) {
		t.Fatal("minted token's grant missing or mismatched")
	}

	// List identifies the link without exposing the token.
	w = httptest.NewRecorder()
	router.ServeHTTP(w, sharesRequest(t, http.MethodGet, "/services/mailhog/shares?namespace=supplemento-test", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("list status = %d", w.Code)
	}
	var listed []shareLinkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decoding list: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != created.ID || listed[0].URL != "" {
		t.Fatalf("list = %+v; want the one link, without a URL", listed)
	}

	// Revoke kills the grant.
	w = httptest.NewRecorder()
	router.ServeHTTP(w, sharesRequest(t, http.MethodDelete, "/services/mailhog/shares/"+created.ID+"?namespace=supplemento-test", ""))
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke status = %d", w.Code)
	}
	if s.Grants.Get(context.Background(), created.ID) != nil {
		t.Error("grant survived revoke")
	}
}

func TestSharesCreateRefusesNonBrowseable(t *testing.T) {
	s := sharesFixture(t)
	db := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "mydb", Namespace: "supplemento-test", UID: "uid-db"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres"},
	}
	s.CRClient = crfake.NewClientBuilder().WithScheme(sharesTestScheme(t)).WithObjects(db).Build()

	w := httptest.NewRecorder()
	routeShares(s).ServeHTTP(w, sharesRequest(t, http.MethodPost, "/services/mydb/shares?namespace=supplemento-test", `{}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a UI-less service", w.Code)
	}
}

func TestSharesCreateRequiresReconciledIngress(t *testing.T) {
	s := sharesFixture(t)
	svc := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "mailhog", Namespace: "supplemento-test", UID: "uid-mailhog-1"},
		Spec:       kipperv1.ServiceSpec{Type: "mailhog"},
	}
	// No Ingress: the UI does not route yet.
	s.CRClient = crfake.NewClientBuilder().WithScheme(sharesTestScheme(t)).WithObjects(svc).Build()

	w := httptest.NewRecorder()
	routeShares(s).ServeHTTP(w, sharesRequest(t, http.MethodPost, "/services/mailhog/shares?namespace=supplemento-test", `{}`))
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409 while the UI is unreconciled", w.Code)
	}
}

func TestSharesCreateCapsLifetime(t *testing.T) {
	s := sharesFixture(t)
	w := httptest.NewRecorder()
	routeShares(s).ServeHTTP(w, sharesRequest(t, http.MethodPost,
		"/services/mailhog/shares?namespace=supplemento-test", `{"expires_in":"2000h"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for an over-cap lifetime", w.Code)
	}
}

func TestSharesRevokeReportsStorageFailure(t *testing.T) {
	s := sharesFixture(t)
	router := routeShares(s)

	// Mint a link so there is something to revoke.
	w := httptest.NewRecorder()
	router.ServeHTTP(w, sharesRequest(t, http.MethodPost, "/services/mailhog/shares?namespace=supplemento-test", `{}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d", w.Code)
	}
	var created shareLinkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding: %v", err)
	}

	// Make the store's read fail with a transient error, not a not-found.
	fc := s.Client.(*k8sfake.Clientset)
	fc.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("etcd is down")
	})

	w = httptest.NewRecorder()
	router.ServeHTTP(w, sharesRequest(t, http.MethodDelete, "/services/mailhog/shares/"+created.ID+"?namespace=supplemento-test", ""))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("revoke status = %d, want 503 for a storage failure (must not report a live link as gone)", w.Code)
	}
}

func TestSharesRevokeAllAndRotate(t *testing.T) {
	s := sharesFixture(t)
	router := routeShares(s)

	w := httptest.NewRecorder()
	router.ServeHTTP(w, sharesRequest(t, http.MethodPost, "/services/mailhog/shares?namespace=supplemento-test", `{}`))
	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d", w.Code)
	}
	var created shareLinkResponse
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	token := created.URL[strings.Index(created.URL, "=")+1:]

	// Revoke-all clears the grant.
	w = httptest.NewRecorder()
	router.ServeHTTP(w, sharesRequest(t, http.MethodDelete, "/shares", ""))
	if w.Code != http.StatusNoContent {
		t.Fatalf("revoke-all status = %d", w.Code)
	}
	if s.Grants.Get(context.Background(), created.ID) != nil {
		t.Error("grant survived revoke-all")
	}

	// Two rotations retire the original key: the old token no longer
	// even verifies, independent of its missing grant.
	for i := 0; i < 2; i++ {
		w = httptest.NewRecorder()
		router.ServeHTTP(w, sharesRequest(t, http.MethodPost, "/shares/rotate-key", ""))
		if w.Code != http.StatusOK {
			t.Fatalf("rotate status = %d", w.Code)
		}
	}
	keyring, err := share.LoadKeyring(context.Background(), s.Client)
	if err != nil {
		t.Fatalf("loading keyring: %v", err)
	}
	if _, err := share.ValidateGrantToken(keyring, token, sharesTestHost, time.Now()); err == nil {
		t.Error("a token signed by a twice-rotated-away key still validates")
	}
}
