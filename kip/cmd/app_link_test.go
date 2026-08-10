package cmd

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// "project/app" names another project; a bare name means the caller's own,
// which is what every link meant before cross-project ones existed.
func TestParseLinkTarget(t *testing.T) {
	tests := []struct {
		in           string
		project, app string
		wantErr      bool
	}{
		{in: "docuseal", app: "docuseal"},
		{in: "docuseal/docuseal", project: "docuseal", app: "docuseal"},
		{in: "billing/api", project: "billing", app: "api"},
		{in: "", wantErr: true},
		{in: "docuseal/", wantErr: true},
		{in: "/docuseal", wantErr: true},
		{in: "a/b/c", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			project, app, err := parseLinkTarget(tt.in)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected %q to be refused", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if project != tt.project || app != tt.app {
				t.Errorf("got project=%q app=%q, want project=%q app=%q", project, app, tt.project, tt.app)
			}
		})
	}
}

// Linking twice to the same target replaces the entry rather than accumulating
// them: a duplicate would leave a stale namespace behind after a target moved,
// and the reconciler would go on opening egress to where it used to be.
func TestLinkingTwiceReplacesTheEntry(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}

	if err := setAppLink(app, "docuseal", "docuseal-test"); err != nil {
		t.Fatal(err)
	}
	if err := setAppLink(app, "billing", "billing-test"); err != nil {
		t.Fatal(err)
	}
	if err := setAppLink(app, "docuseal", "docuseal-prod"); err != nil {
		t.Fatal(err)
	}

	links, _, _ := unstructured.NestedSlice(app.Object, "spec", "links")
	if len(links) != 2 {
		t.Fatalf("got %d links, want 2: %v", len(links), links)
	}
	for _, raw := range links {
		entry := raw.(map[string]any)
		if entry["app"] == "docuseal" && entry["namespace"] != "docuseal-prod" {
			t.Errorf("the stale namespace survived: %v", entry)
		}
	}
}

// Unlinking removes the entry and reports whether there was one, so the command
// can tell an operator the difference between "withdrawn" and "there was none".
func TestRemovingALink(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	if err := setAppLink(app, "docuseal", "docuseal-test"); err != nil {
		t.Fatal(err)
	}
	if err := setAppLink(app, "billing", "billing-test"); err != nil {
		t.Fatal(err)
	}

	removed, err := removeAppLink(app, "docuseal")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Error("removing an existing link must report that it did")
	}
	links, _, _ := unstructured.NestedSlice(app.Object, "spec", "links")
	if len(links) != 1 || links[0].(map[string]any)["app"] != "billing" {
		t.Errorf("wrong link removed: %v", links)
	}

	removed, err = removeAppLink(app, "not-linked")
	if err != nil {
		t.Fatal(err)
	}
	if removed {
		t.Error("removing a link that was not there must report that it was not")
	}

	// The last one out clears the field rather than leaving an empty list.
	if _, err := removeAppLink(app, "billing"); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedSlice(app.Object, "spec", "links"); found {
		t.Error("an empty links field was left behind")
	}
}

// A public link records no dependency. The URL it sets is for a browser, which
// no egress policy applies to, so opening an internal path would be an
// allowance with nothing using it — and switching an existing link to --public
// must withdraw the one it had.
func TestAPublicLinkRecordsNoDependency(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{}}}
	if err := setAppLink(app, "docuseal", "docuseal-test"); err != nil {
		t.Fatal(err)
	}

	removed, err := removeAppLink(app, "docuseal")
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Fatal("switching to --public must withdraw the internal allowance")
	}
	if _, found, _ := unstructured.NestedSlice(app.Object, "spec", "links"); found {
		t.Error("a public link left a dependency behind")
	}
}
