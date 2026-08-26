package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/nsowner"
	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/controller/pkg/capability"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

type stubMemberSource map[string]map[string]string

func (s stubMemberSource) ProjectMembers(_ context.Context, project string) (map[string]string, bool, error) {
	m, ok := s[project]
	return m, ok, nil
}

func projectNS(name, project string) *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: map[string]string{kipperlabels.Project: project},
		},
	}
}

// withResolver wires a test resolver and restores the previous one so parallel
// packages and later tests are unaffected.
func withResolver(t *testing.T) {
	t.Helper()
	client := fake.NewClientset(projectNS("blog", "blog"), projectNS("shop", "shop"))
	roles := middleware.NewRoleStore(fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"},
		Data:       map[string]string{"users": `{"root@test.com":"admin","dev@test.com":"member","outsider@test.com":"member"}`},
	}))
	members := stubMemberSource{"blog": {"dev@test.com": "deployer"}}
	prev := projectResolver
	SetProjectResolver(middleware.NewProjectAccessResolver(client, roles, members, handlerOwners(t,
		projectNS("blog", "blog"), projectNS("shop", "shop"),
	)))
	t.Cleanup(func() { projectResolver = prev })
}

func reqAs(email, namespace string) *http.Request {
	req := httptest.NewRequest("GET", "/x?namespace="+namespace, nil)
	ctx := context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: email})
	return req.WithContext(ctx)
}

func TestEnforceCapability(t *testing.T) {
	withResolver(t)

	tests := []struct {
		name     string
		email    string
		ns       string
		required capability.Name
		allow    bool
	}{
		{"deployer holds a write capability", "dev@test.com", "blog", "kipper.write", true},
		{"deployer holds a read capability", "dev@test.com", "blog", "workloads.read", true},
		{"deployer does not hold an owner's", "dev@test.com", "blog", "members.manage", false},
		{"non-member denied", "dev@test.com", "shop", "workloads.read", false},
		{"outsider denied", "outsider@test.com", "blog", "workloads.read", false},
		{"admin allowed anywhere", "root@test.com", "shop", "kipper.write", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			got := enforceCapability(rec, reqAs(tt.email, tt.ns), tt.ns, tt.required)
			if got != tt.allow {
				t.Fatalf("enforceCapability = %v, want %v (status %d)", got, tt.allow, rec.Code)
			}
		})
	}
}

func TestCanAccessNamespace(t *testing.T) {
	withResolver(t)

	if !canAccessNamespace(reqAs("dev@test.com", "blog"), "blog") {
		t.Error("member should access their namespace")
	}
	if canAccessNamespace(reqAs("dev@test.com", "shop"), "shop") {
		t.Error("non-member should not access a foreign namespace")
	}
	if !canAccessNamespace(reqAs("root@test.com", "shop"), "shop") {
		t.Error("admin should access any namespace")
	}
}

func TestFilterPodsByAccess(t *testing.T) {
	withResolver(t)

	pods := []corev1.Pod{
		{ObjectMeta: metav1.ObjectMeta{Name: "a", Namespace: "blog"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "b", Namespace: "shop"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "blog"}},
	}
	kept := filterPodsByAccess(reqAs("dev@test.com", ""), pods)
	if len(kept) != 2 {
		t.Fatalf("expected 2 pods in accessible namespaces, got %d", len(kept))
	}
	for _, p := range kept {
		if p.Namespace != "blog" {
			t.Errorf("unexpected pod from namespace %q", p.Namespace)
		}
	}
}

func TestFilterSeriesByAccess(t *testing.T) {
	withResolver(t)

	series := []PromSeries{
		{Labels: map[string]string{"namespace": "blog", "label_app": "web"}},
		{Labels: map[string]string{"namespace": "shop", "label_app": "web"}},
		{Labels: map[string]string{"label_app": "orphan"}},
	}

	member := filterSeriesByAccess(reqAs("dev@test.com", ""), series)
	if len(member) != 1 || member[0].Labels["namespace"] != "blog" {
		t.Fatalf("member should see only their own namespace series, got %v", member)
	}

	admin := filterSeriesByAccess(reqAs("root@test.com", ""), series)
	if len(admin) != 2 {
		t.Fatalf("admin should see both project series (orphan dropped), got %d", len(admin))
	}
}

// handlerOwners builds the namespace-owner reader these tests need, with a
// Project claiming each namespace it is given.
//
// Ownership is the claim now, not the label, so a fixture that sets only the
// label describes a cluster nobody could have reached honestly. Deriving the
// projects from the namespaces keeps the two agreeing by construction.
func handlerOwners(t *testing.T, namespaces ...*corev1.Namespace) nsowner.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := kipperv1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the kipper scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("registering the core scheme: %v", err)
	}

	claims := map[string][]kipperv1.NamespaceClaim{}
	objs := make([]crclient.Object, 0, len(namespaces))
	for _, ns := range namespaces {
		ns := ns.DeepCopy()
		if ns.UID == "" {
			ns.UID = types.UID(ns.Name + "-uid")
		}
		objs = append(objs, ns)
		if project := ns.Labels[kipperlabels.Project]; project != "" {
			claims[project] = append(claims[project], kipperv1.NamespaceClaim{Name: ns.Name, UID: ns.UID})
		}
	}
	for project, held := range claims {
		objs = append(objs, &kipperv1.Project{
			ObjectMeta: metav1.ObjectMeta{Name: project},
			Status:     kipperv1.ProjectStatus{NamespaceClaims: held},
		})
	}
	return crfake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}
