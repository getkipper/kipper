package gitcred

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestAllowsProject(t *testing.T) {
	e := Entry{AllowedProjects: []string{"acme", "beta"}}
	if !e.AllowsProject("acme") {
		t.Error("acme is on the allow-list")
	}
	if e.AllowsProject("gamma") {
		t.Error("gamma is not on the allow-list")
	}
	if e.AllowsProject("") {
		t.Error("an empty project must never match")
	}
	// An empty allow-list denies everything (fail closed).
	if (Entry{}).AllowsProject("acme") {
		t.Error("an empty allow-list must deny every project")
	}
}

func TestFind(t *testing.T) {
	entries := []Entry{{Name: "a"}, {Name: "b"}}
	if Find(entries, "b") == nil {
		t.Error("b should be found")
	}
	if Find(entries, "c") != nil {
		t.Error("c should not be found")
	}
	if Find(nil, "a") != nil {
		t.Error("nothing is found in an empty list")
	}
}

func TestLoadSaveRoundTrip(t *testing.T) {
	client := fake.NewClientset()
	ctx := context.Background()

	// A missing list Secret is not an error — it means no shared credentials.
	got, err := Load(ctx, client)
	if err != nil || got != nil {
		t.Errorf("Load with no Secret should be (nil, nil), got (%v, %v)", got, err)
	}

	want := []Entry{
		{Name: "shared-gh", Server: "github.com", Username: "x", Token: "t", AllowedProjects: []string{"acme"}},
	}
	if err := Save(ctx, client, want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err = Load(ctx, client)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != 1 || got[0].Name != "shared-gh" || got[0].Token != "t" || len(got[0].AllowedProjects) != 1 {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	// Save again updates in place (no duplicate Secret).
	if err := Save(ctx, client, want[:0]); err != nil {
		t.Fatalf("Save empty: %v", err)
	}
	if got, _ := Load(ctx, client); len(got) != 0 {
		t.Errorf("expected empty list after clearing, got %v", got)
	}
}

// A malformed list must fail closed (return an error), so a caller enforcing on
// the list does not mistake it for an empty list and skip enforcement.
func TestLoad_MalformedFailsClosed(t *testing.T) {
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigSecretName, Namespace: Namespace},
		Data:       map[string][]byte{"credentials": []byte("{not json")},
	})
	if _, err := Load(context.Background(), client); err == nil {
		t.Fatal("a malformed credentials list must return an error")
	}
}
