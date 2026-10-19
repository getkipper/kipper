package cmd

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/getkipper/kipper/kip/internal/installer"
)

const lockError = "ssh ops@203.0.113.10: apt-get install ufw on lab.example.org failed " +
	"for admin@lab.example.org with password Not-A-Real-Pass1: E: Could not get lock /var/lib/dpkg/lock-frontend"

func failedStepOn(host installer.HostOS, profile string) error {
	return fmt.Errorf("install: %w", &installer.StepError{
		Step: "Configuring firewall", Host: host, Profile: profile, Err: errors.New(lockError),
	})
}

func failedStep() error {
	return failedStepOn(installer.HostOS{ID: "ubuntu", VersionID: "24.04", Family: installer.FamilyDebian}, "small")
}

func linkFields(t *testing.T, link string) url.Values {
	t.Helper()
	u, err := url.Parse(link)
	if err != nil {
		t.Fatalf("not a URL: %q: %v", link, err)
	}
	return u.Query()
}

func TestInstallIssueLink_DescribesTheFailedStep(t *testing.T) {
	link := installIssueLink(failedStep(), issueContext{
		KipVersion: "v0.31.0",
		Client:     "darwin/arm64",
		Flags:      []string{"--host", "--profile"},
	})
	q := linkFields(t, link)

	if got, want := q.Get("title"), `kip install failed at "Configuring firewall"`; got != want {
		t.Errorf("title = %q, want %q", got, want)
	}
	if got := q.Get("kip-version"); got != "v0.31.0" {
		t.Errorf("kip-version = %q", got)
	}
	if got, want := q.Get("environment"), "kip on darwin/arm64, server ubuntu 24.04, profile small"; got != want {
		t.Errorf("environment = %q, want %q", got, want)
	}
	if repro := q.Get("repro"); !strings.Contains(repro, "--host --profile") {
		t.Errorf("repro %q does not list the flags that were set", repro)
	}
}

func TestInstallIssueLink_CarriesNoErrorText(t *testing.T) {
	link := installIssueLink(failedStep(), issueContext{})

	decoded, err := url.QueryUnescape(link)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"203.0.113.10", "lab.example.org", "Not-A-Real-Pass1", "dpkg"} {
		if strings.Contains(decoded, fragment) {
			t.Errorf("link carries %q from the error:\n%s", fragment, decoded)
		}
	}
	if linkFields(t, link).Has("logs") {
		t.Error("the logs field is for the user to paste into")
	}
}

func TestInstallIssueLink_NamesOnlyRecognizedDistributions(t *testing.T) {
	for _, tc := range []struct {
		host    installer.HostOS
		profile string
		want    string
	}{
		{installer.HostOS{ID: "rocky", VersionID: "9.4"}, "medium", "server rocky 9.4, profile medium"},
		{installer.HostOS{ID: "alice-build", VersionID: "lab.example.org", Family: installer.FamilyDebian}, "small", "server debian-like other, profile small"},
		{installer.HostOS{ID: "acme", VersionID: "192.168.1.5"}, "x y", "server other other, profile custom"},
		{installer.HostOS{ID: "ubuntu", VersionID: "24.04"}, "acmeproduction", "server ubuntu 24.04, profile custom"},
	} {
		env := linkFields(t, installIssueLink(failedStepOn(tc.host, tc.profile), issueContext{})).Get("environment")
		if !strings.HasSuffix(env, tc.want) {
			t.Errorf("%+v: environment = %q, want suffix %q", tc.host, env, tc.want)
		}
	}
}

func TestInstallIssueLink_OnlyForAFailedStep(t *testing.T) {
	for name, err := range map[string]error{
		"no error":          nil,
		"preflight refusal": errors.New("the name lab.kipper.run is already registered"),
	} {
		if link := installIssueLink(err, issueContext{}); link != "" {
			t.Errorf("%s: got a link %q", name, link)
		}
		if report := installErrorReport(err, issueContext{}); report != "" {
			t.Errorf("%s: got an error report %q", name, report)
		}
	}
}

func TestInstallErrorReport_KeepsTheCauseAndScrubsTheRest(t *testing.T) {
	report := installErrorReport(failedStep(), issueContext{
		Secrets: []string{"Not-A-Real-Pass1"},
		Names:   []string{"203.0.113.10", "lab.example.org", "ops"},
	})

	if !strings.Contains(report, "Could not get lock /var/lib/dpkg/lock-frontend") {
		t.Errorf("report lost the cause: %q", report)
	}
	for _, private := range []string{"203.0.113.10", "lab.example.org", "Not-A-Real-Pass1", "ops@"} {
		if strings.Contains(report, private) {
			t.Errorf("report carries %q: %q", private, report)
		}
	}
}

func TestInstallErrorReport_KeepsOnlyTheLastLines(t *testing.T) {
	var lines []string
	for i := 1; i <= errorReportLines+5; i++ {
		lines = append(lines, fmt.Sprintf("line %d", i))
	}
	err := &installer.StepError{Step: "Installing k3s", Err: errors.New(strings.Join(lines, "\n"))}

	got := strings.Split(installErrorReport(err, issueContext{}), "\n")
	if len(got) != errorReportLines {
		t.Fatalf("got %d lines, want %d", len(got), errorReportLines)
	}
	if got[len(got)-1] != fmt.Sprintf("line %d", errorReportLines+5) {
		t.Errorf("last line = %q", got[len(got)-1])
	}
}

func TestInstallKnownValues_SplitsSecretsFromNames(t *testing.T) {
	opts := installer.Options{
		Host: "203.0.113.10", Domain: "lab.example.org", ConsoleDomain: "console.example.org",
		ConsoleAPIDomain: "api.example.org", DexDomain: "login.example.org", AdminEmail: "ops@example.org",
		SSHUser: "deploy", Org: "acme", OrgDisplayName: "Acme Corp",
		BackupStorage: &installer.BackupStorageConfig{
			Bucket: "acme-backups", Endpoint: "https://s3.example.net", AccessKeyID: "AKIA-not-real", SecretAccessKey: "not-a-secret",
		},
		BackupRepositoryKey: []byte("repo-key-not-real"),
	}
	result := &installer.Result{Domain: "lab.example.org", AdminPassword: "pw-not-real", GatewayToken: "gw-not-real"}

	secrets, names := installKnownValues(opts, result)
	assertHolds(t, "secrets", secrets, "AKIA-not-real", "not-a-secret", "repo-key-not-real", "pw-not-real", "gw-not-real")
	assertHolds(t, "names", names, "203.0.113.10", "console.example.org", "api.example.org", "login.example.org",
		"ops@example.org", "deploy", "acme", "Acme Corp", "acme-backups", "https://s3.example.net")
}

func TestInstallKnownValues_LeavesOutTheRootUser(t *testing.T) {
	_, names := installKnownValues(installer.Options{SSHUser: "root"}, nil)
	for _, v := range names {
		if v == "root" {
			t.Fatal("root would be replaced in every path such as /root")
		}
	}
}

func assertHolds(t *testing.T, label string, values []string, want ...string) {
	t.Helper()
	have := map[string]bool{}
	for _, v := range values {
		have[v] = true
	}
	for _, w := range want {
		if !have[w] {
			t.Errorf("%s miss %q", label, w)
		}
	}
}
