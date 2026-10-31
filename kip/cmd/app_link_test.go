package cmd

import (
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/config"
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

// TestAPublicLinkUsesTheDerivedHostWhenTheRouteHasNone verifies that --public
// uses the same derived host as the reconciler and console link handler.
// Environment lookup failures must return an error to prevent storing a guessed URL.
func TestAPublicLinkUsesTheDerivedHostWhenTheRouteHasNone(t *testing.T) {
	target := func(route map[string]any) *unstructured.Unstructured {
		spec := map[string]any{"port": int64(8080)}
		if route != nil {
			spec["route"] = route
		}
		return &unstructured.Unstructured{Object: map[string]any{
			"metadata": map[string]any{"name": "api"},
			"spec":     spec,
		}}
	}
	lookupFails := errors.New("namespaces is forbidden")

	cases := []struct {
		name, env, domain string
		envErr            error
		route             map[string]any
		want              string
		wantErr           bool
	}{
		{name: "explicit host", route: map[string]any{"host": "api.example.com", "path": "/v1"}, domain: "lab.kipper.run", want: "https://api.example.com/v1"},
		{name: "explicit host ignores the environment and domain", env: "prod", route: map[string]any{"host": "api.example.com"}, want: "https://api.example.com"},
		{name: "explicit host needs no environment lookup", envErr: lookupFails, route: map[string]any{"host": "api.example.com"}, want: "https://api.example.com"},
		{name: "derived host", route: map[string]any{"path": "/"}, domain: "lab.kipper.run", want: "https://api--lab.kipper.run"},
		{name: "derived host with a path", route: map[string]any{"path": "/v1"}, domain: "lab.kipper.run", want: "https://api--lab.kipper.run/v1"},
		{name: "derived host in the default environment", env: "default", route: map[string]any{}, domain: "lab.kipper.run", want: "https://api--lab.kipper.run"},
		{name: "derived host in an environment", env: "prod", route: map[string]any{}, domain: "example.com", want: "https://api-prod.example.com"},
		{name: "derived host when the environment lookup fails", envErr: lookupFails, route: map[string]any{}, domain: "lab.kipper.run", wantErr: true},
		{name: "no route", domain: "lab.kipper.run", wantErr: true},
		{name: "no host and no cluster domain", route: map[string]any{}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			environment := func() (string, error) { return tc.env, tc.envErr }
			got, err := publicLinkURL(target(tc.route), environment, tc.domain)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("got %q, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTheLinkTargetEnvironmentComesFromTheFlagsAndContext verifies that the
// target's environment can be recovered from the namespace selection inputs.
// Project membership grants no access to Namespace objects, so only targets
// found by scanning require an environment label lookup.
func TestTheLinkTargetEnvironmentComesFromTheFlagsAndContext(t *testing.T) {
	cases := []struct {
		name                       string
		targetProject              string
		flagProject, flagEnv       string
		currentProject, currentEnv string
		want                       string
		known                      bool
	}{
		{name: "another project with an environment flag", targetProject: "docs", flagEnv: "prod", currentProject: "shop", currentEnv: "acc", want: "prod", known: true},
		{name: "another project without the flag is its default environment", targetProject: "docs", currentProject: "shop", currentEnv: "acc", want: "", known: true},
		{name: "own project from the context", currentProject: "shop", currentEnv: "prod", want: "prod", known: true},
		{name: "own project from the flags", flagProject: "shop", flagEnv: "acc", currentProject: "other", currentEnv: "prod", want: "acc", known: true},
		{name: "no project anywhere", known: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().String("project", "", "")
			cmd.Flags().String("environment", "", "")
			if tc.flagProject != "" {
				_ = cmd.Flags().Set("project", tc.flagProject)
			}
			if tc.flagEnv != "" {
				_ = cmd.Flags().Set("environment", tc.flagEnv)
			}
			cluster := &config.Cluster{CurrentProject: tc.currentProject, CurrentEnvironment: tc.currentEnv}

			got, known := linkTargetEnvironment(cmd, cluster, tc.targetProject)
			if got != tc.want || known != tc.known {
				t.Errorf("got (%q, %v), want (%q, %v)", got, known, tc.want, tc.known)
			}
		})
	}
}
