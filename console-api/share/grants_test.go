package share

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func testGrant(t *testing.T, now time.Time) Grant {
	t.Helper()
	g, err := NewGrant("uid-1234", "mailhog", "supplemento-test", "mailhog-supplemento-test.storefront.com", "PO review", "admin@storefront.com", 72*time.Hour, now)
	if err != nil {
		t.Fatalf("NewGrant: %v", err)
	}
	return g
}

func storeAt(client *fake.Clientset, now *time.Time) *GrantStore {
	s := NewGrantStore(client)
	s.now = func() time.Time { return *now }
	return s
}

func TestGrantLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	client := fake.NewSimpleClientset()
	s := storeAt(client, &now)

	g := testGrant(t, now)
	if err := s.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got := s.Get(ctx, g.JTI)
	if got == nil {
		t.Fatal("expected the grant to resolve")
	}
	if got.ServiceUID != "uid-1234" || got.Label != "PO review" {
		t.Errorf("grant fields lost: %+v", got)
	}

	list, err := s.List(ctx, g.Namespace, g.Service)
	if err != nil || len(list) != 1 {
		t.Fatalf("List = %v, %v; want 1 grant", list, err)
	}

	if err := s.Revoke(ctx, g.Namespace, g.Service, g.JTI); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if s.Get(ctx, g.JTI) != nil {
		t.Error("revoked grant still resolves")
	}
}

// TestGrantRevocationHonoursCacheTTL pins the revocation SLA: a cached
// positive entry keeps answering until the TTL passes, then the deletion
// is visible. Revoke through the same store invalidates immediately; this
// covers a delete arriving from another replica.
func TestGrantRevocationHonoursCacheTTL(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	client := fake.NewSimpleClientset()
	s := storeAt(client, &now)

	g := testGrant(t, now)
	if err := s.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Get(ctx, g.JTI) == nil {
		t.Fatal("expected the grant to resolve")
	}

	// Another replica deletes the Secret directly.
	if err := client.CoreV1().Secrets(SigningSecretNamespace).Delete(ctx, grantSecretName(g.JTI), metav1.DeleteOptions{}); err != nil {
		t.Fatalf("deleting grant secret: %v", err)
	}

	if s.Get(ctx, g.JTI) == nil {
		t.Fatal("inside the TTL the cached grant should still answer")
	}
	now = now.Add(grantCacheTTL + time.Second)
	if s.Get(ctx, g.JTI) != nil {
		t.Error("after the TTL the revocation must be visible")
	}
}

func TestGrantExpiryBeatsCache(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	client := fake.NewSimpleClientset()
	s := storeAt(client, &now)

	g := testGrant(t, now)
	if err := s.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if s.Get(ctx, g.JTI) == nil {
		t.Fatal("expected the grant to resolve")
	}

	// A freshly cached entry must not outlive the grant's own expiry.
	now = g.ExpiresAt.Add(time.Second)
	if s.Get(ctx, g.JTI) != nil {
		t.Error("expired grant still resolves")
	}
}

func TestGrantCapAndPrune(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	client := fake.NewSimpleClientset()
	s := storeAt(client, &now)

	for i := 0; i < MaxGrantsPerService; i++ {
		if err := s.Create(ctx, testGrant(t, now)); err != nil {
			t.Fatalf("Create %d: %v", i, err)
		}
	}
	if err := s.Create(ctx, testGrant(t, now)); !errors.Is(err, ErrGrantLimit) {
		t.Fatalf("expected ErrGrantLimit, got %v", err)
	}

	// Once the existing grants expire, minting prunes them and succeeds.
	now = now.Add(73 * time.Hour)
	if err := s.Create(ctx, testGrant(t, now)); err != nil {
		t.Fatalf("Create after expiry: %v", err)
	}
	list, err := s.List(ctx, "supplemento-test", "mailhog")
	if err != nil || len(list) != 1 {
		t.Fatalf("List after prune = %d grants, %v; want 1", len(list), err)
	}
}

func TestRevokeRefusesForeignService(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	client := fake.NewSimpleClientset()
	s := storeAt(client, &now)

	g := testGrant(t, now)
	if err := s.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Revoke(ctx, "other-ns", "other-svc", g.JTI); err == nil {
		t.Fatal("expected a cross-service revoke to fail")
	}
	if s.Get(ctx, g.JTI) == nil {
		t.Error("the grant must survive the refused revoke")
	}
}

func TestRevokeAllForService(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	client := fake.NewSimpleClientset()
	s := storeAt(client, &now)

	mine := testGrant(t, now)
	if err := s.Create(ctx, mine); err != nil {
		t.Fatalf("Create: %v", err)
	}
	other, err := NewGrant("uid-9", "pgweb", "blog-prod", "pgweb-blog-prod.storefront.com", "", "admin@storefront.com", time.Hour, now)
	if err != nil {
		t.Fatalf("NewGrant: %v", err)
	}
	if err := s.Create(ctx, other); err != nil {
		t.Fatalf("Create other: %v", err)
	}

	if err := s.RevokeAllForService(ctx, mine.Namespace, mine.Service); err != nil {
		t.Fatalf("RevokeAllForService: %v", err)
	}
	if s.Get(ctx, mine.JTI) != nil {
		t.Error("service grant survived revoke-all")
	}
	if s.Get(ctx, other.JTI) == nil {
		t.Error("another service's grant must survive")
	}

	if err := s.RevokeAll(ctx); err != nil {
		t.Fatalf("RevokeAll: %v", err)
	}
	if s.Get(ctx, other.JTI) != nil {
		t.Error("global revoke-all left a grant behind")
	}
}

// TestRevokeAllForServiceFailsClosedOnNonConvergence pins the fail-closed
// contract the service finalizer relies on: if grants keep reappearing and
// the sweep never sees an empty pass, the store reports an error rather than
// a clean revoke, so the caller retains the finalizer.
func TestRevokeAllForServiceFailsClosedOnNonConvergence(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	client := fake.NewSimpleClientset()
	s := storeAt(client, &now)

	// A list reactor that always reports one matching grant, whatever the
	// deletes do — the pathological "mint storm" the bound guards against.
	// The typed fake re-filters the reactor's list by the label selector,
	// so the secret carries the full service-scoped label set.
	stubborn := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      grantSecretName("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
			Namespace: SigningSecretNamespace,
			Labels: map[string]string{
				GrantLabel:          "true",
				GrantServiceLabel:   "mailhog",
				GrantNamespaceLabel: "supplemento-test",
			},
		},
	}
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &corev1.SecretList{Items: []corev1.Secret{*stubborn}}, nil
	})

	err := s.RevokeAllForService(ctx, "supplemento-test", "mailhog")
	if err == nil {
		t.Fatal("a non-converging revoke reported success; the finalizer would release with grants intact")
	}
	if !strings.Contains(err.Error(), "did not converge") {
		t.Errorf("error = %v, want a non-convergence error", err)
	}
}

// TestGetFailsClosedOnGarbage: malformed grants, version drift, and jti
// shapes v1 never mints all resolve to nil, indistinguishably.
func TestGetFailsClosedOnGarbage(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)

	badGrants := map[string]string{
		"11111111111111111111111111111111": `not json`,
		"22222222222222222222222222222222": `{"v":2,"jti":"22222222222222222222222222222222","serviceUID":"u","service":"s","namespace":"n","host":"h","expiresAt":"2100-01-01T00:00:00Z"}`,
		"33333333333333333333333333333333": `{"v":1,"jti":"MISMATCH","serviceUID":"u","service":"s","namespace":"n","host":"h","expiresAt":"2100-01-01T00:00:00Z"}`,
		"44444444444444444444444444444444": `{"v":1,"jti":"44444444444444444444444444444444","serviceUID":"","service":"s","namespace":"n","host":"h","expiresAt":"2100-01-01T00:00:00Z"}`,
	}
	var objs []runtime.Object
	for jti, raw := range badGrants {
		objs = append(objs, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{
				Name:      grantSecretName(jti),
				Namespace: SigningSecretNamespace,
				Labels:    map[string]string{GrantLabel: "true", GrantServiceLabel: "s", GrantNamespaceLabel: "n"},
			},
			Data: map[string][]byte{grantField: []byte(raw)},
		})
	}
	client := fake.NewSimpleClientset(objs...)
	s := storeAt(client, &now)

	for jti := range badGrants {
		if s.Get(ctx, jti) != nil {
			t.Errorf("garbage grant %s resolved", jti)
		}
	}
	// A jti shape v1 never mints fails before any API call.
	if s.Get(ctx, "not-a-jti") != nil {
		t.Error("malformed jti resolved")
	}
	if s.Get(ctx, strings.Repeat("A", 32)) != nil {
		t.Error("uppercase jti resolved")
	}
}

func TestGrantMatchesComparesEveryField(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	kr := &Keyring{CurrentKID: "kid1", CurrentKey: []byte("0123456789abcdef0123456789abcdef")}
	g := testGrant(t, now)

	tok, err := MintGrant(kr, g, now)
	if err != nil {
		t.Fatalf("MintGrant: %v", err)
	}
	claims, err := ValidateGrantToken(kr, tok, g.Host, now)
	if err != nil {
		t.Fatalf("ValidateGrantToken: %v", err)
	}

	if !g.Matches(claims, g.Host) {
		t.Fatal("a freshly minted token must match its grant")
	}

	mutations := map[string]Grant{}
	m := g
	m.ServiceUID = "uid-other"
	mutations["serviceUID"] = m
	m = g
	m.Host = "other-host.storefront.com"
	mutations["host"] = m
	m = g
	m.Service = "other"
	mutations["service"] = m
	m = g
	m.Namespace = "other-ns"
	mutations["namespace"] = m
	m = g
	m.JTI = "ffffffffffffffffffffffffffffffff"
	mutations["jti"] = m
	m = g
	m.ExpiresAt = g.ExpiresAt.Add(time.Hour)
	mutations["expiresAt"] = m

	for field, mutated := range mutations {
		if mutated.Matches(claims, g.Host) {
			t.Errorf("grant with drifted %s still matches the token", field)
		}
	}
}

// TestGetReturnsACopy: mutating a returned grant must not poison the cache
// another request reads from.
func TestGetReturnsACopy(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1_700_000_000, 0)
	client := fake.NewSimpleClientset()
	s := storeAt(client, &now)

	g := testGrant(t, now)
	if err := s.Create(ctx, g); err != nil {
		t.Fatalf("Create: %v", err)
	}

	first := s.Get(ctx, g.JTI)
	if first == nil {
		t.Fatal("expected the grant to resolve")
	}
	first.ServiceUID = "poisoned"
	first.Host = "evil.example.com"

	second := s.Get(ctx, g.JTI)
	if second == nil {
		t.Fatal("expected the grant to resolve again")
	}
	if second.ServiceUID != "uid-1234" || second.Host != g.Host {
		t.Error("mutating a returned grant leaked into the cache")
	}
}
