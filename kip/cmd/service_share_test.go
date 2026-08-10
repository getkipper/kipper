package cmd

import (
	"testing"
	"time"

	"github.com/spf13/cobra"
)

// TestServiceShareNamespaceFlags pins the flag-to-namespace wiring: the
// share command must register --project and --environment so
// resolveProjectAndEnvironment can target services outside the default
// project. The flags were missing at first, which made every share mint
// against the default namespace.
func TestServiceShareNamespaceFlags(t *testing.T) {
	for _, name := range []string{"project", "environment"} {
		if serviceShareCmd.Flag(name) == nil {
			t.Fatalf("kip service share is missing the --%s flag", name)
		}
	}

	if err := serviceShareCmd.Flags().Set("project", "supplemento"); err != nil {
		t.Fatalf("setting --project: %v", err)
	}
	if err := serviceShareCmd.Flags().Set("environment", "test"); err != nil {
		t.Fatalf("setting --environment: %v", err)
	}
	t.Cleanup(func() {
		for _, name := range []string{"project", "environment"} {
			f := serviceShareCmd.Flag(name)
			if err := f.Value.Set(f.DefValue); err != nil {
				t.Errorf("resetting --%s: %v", name, err)
			}
			f.Changed = false
		}
	})

	project, environment := resolveProjectAndEnvironment(serviceShareCmd, nil)
	if project != "supplemento" || environment != "test" {
		t.Errorf("resolved (%q, %q), want (\"supplemento\", \"test\")", project, environment)
	}
}

// TestServiceShareOperationFlags pins the mint/list/revoke/revoke-all/
// rotate-key flag surface the command dispatches on.
func TestServiceShareOperationFlags(t *testing.T) {
	for _, name := range []string{"expires", "label", "list", "revoke", "revoke-all", "rotate-key"} {
		if serviceShareCmd.Flag(name) == nil {
			t.Errorf("kip service share is missing the --%s flag", name)
		}
	}
}

func TestValidateShareOperation(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		rotateKey bool
		revokeAll bool
		list      bool
		revokeID  string
		changed   []string // flags marked as explicitly set
		wantErr   bool
	}{
		{name: "mint with service", args: []string{"mailhog"}, wantErr: false},
		{name: "mint with expires+label", args: []string{"mailhog"}, changed: []string{"expires", "label"}, wantErr: false},
		{name: "mint needs a service", args: nil, wantErr: true},
		{name: "list with service", args: []string{"mailhog"}, list: true, wantErr: false},
		{name: "revoke with service", args: []string{"mailhog"}, revokeID: "abc", wantErr: false},
		{name: "revoke-all cluster-wide", args: nil, revokeAll: true, wantErr: false},
		{name: "rotate-key cluster-wide", args: nil, rotateKey: true, wantErr: false},
		{name: "revoke-all rejects a service arg", args: []string{"mailhog"}, revokeAll: true, wantErr: true},
		{name: "rotate-key rejects a service arg", args: []string{"mailhog"}, rotateKey: true, wantErr: true},
		{name: "two operations conflict", args: nil, revokeAll: true, rotateKey: true, wantErr: true},
		{name: "list and revoke conflict", args: []string{"mailhog"}, list: true, revokeID: "abc", wantErr: true},
		{name: "expires on a non-mint op", args: []string{"mailhog"}, list: true, changed: []string{"expires"}, wantErr: true},
		{name: "label on revoke-all", args: nil, revokeAll: true, changed: []string{"label"}, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.Flags().Duration("expires", 72*time.Hour, "")
			cmd.Flags().String("label", "", "")
			for _, f := range tc.changed {
				if err := cmd.Flags().Set(f, cmd.Flag(f).DefValue); err != nil {
					t.Fatalf("marking --%s changed: %v", f, err)
				}
			}
			err := validateShareOperation(cmd, tc.args, tc.rotateKey, tc.revokeAll, tc.list, tc.revokeID)
			if (err != nil) != tc.wantErr {
				t.Errorf("validateShareOperation err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestShareAPIError(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{"error envelope", `{"error":"service type \"postgres\" has no browseable web UI to share"}`, 400, `service type "postgres" has no browseable web UI to share`},
		{"raw body", `plain text failure`, 500, "plain text failure"},
		{"empty body falls back to status", ``, 503, "share API returned status 503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shareAPIError([]byte(tc.body), tc.status); got != tc.want {
				t.Errorf("shareAPIError(%q, %d) = %q, want %q", tc.body, tc.status, got, tc.want)
			}
		})
	}
}

func TestOrDash(t *testing.T) {
	if orDash("") != "-" {
		t.Error("empty string should render as a dash")
	}
	if orDash("PO review") != "PO review" {
		t.Error("non-empty string should pass through")
	}
}
