package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/getkipper/kipper/kip/internal/auth"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/doctor"
	"github.com/getkipper/kipper/kip/internal/installer"
)

func rebootedCluster() []doctorGroup {
	return []doctorGroup{
		{"Host", []doctor.Result{
			{ID: "ssh", Status: doctor.Pass, Message: "reachable over SSH"},
			{ID: "k3s", Status: doctor.Pass, Message: "k3s is running (server booted 9 minutes ago)"},
		}},
		{"Gateway hop", []doctor.Result{
			{ID: "hop-secret", Status: doctor.Pass, Message: "hop certificate present"},
			{ID: "served-cert", Status: doctor.Fail, Message: "port 443 serves Traefik's built-in fallback certificate, not the hop certificate.",
				Detail: "dial 203.0.113.10:443 via lab.example.org for ops@example.org"},
			{ID: "heartbeat", Status: doctor.Skip, Message: "not checked (no heartbeat lines in the last 48 hours; console-api may have restarted)"},
		}},
	}
}

func TestRenderDoctor_GroupsChecksAndCountsProblems(t *testing.T) {
	var out bytes.Buffer
	failures := renderDoctor(&out, "lab", rebootedCluster())

	if failures != 1 {
		t.Errorf("failures = %d, want 1", failures)
	}
	text := out.String()
	for _, want := range []string{"kip doctor: lab", "  Host\n", "    ✔  k3s is running", "    ✗  port 443 serves", "    ⚠  not checked", "1 problem found"} {
		if !strings.Contains(text, want) {
			t.Errorf("output lacks %q:\n%s", want, text)
		}
	}
}

func TestRenderDoctor_SaysAllIsWellWithoutFailures(t *testing.T) {
	var out bytes.Buffer
	if failures := renderDoctor(&out, "lab", []doctorGroup{{"Host", []doctor.Result{{ID: "k3s", Status: doctor.Pass, Message: "k3s is running"}}}}); failures != 0 {
		t.Fatalf("failures = %d", failures)
	}
	if !strings.Contains(out.String(), "No problems found") {
		t.Errorf("output:\n%s", out.String())
	}
}

func TestDoctorIssueLink_CarriesOnlyCheckIdsAndProgramValues(t *testing.T) {
	link := doctorIssueLink(rebootedCluster(), issueContext{KipVersion: "v0.32.0", Client: "darwin/arm64", Flags: []string{"--ssh-key"}},
		installer.HostOS{ID: "ubuntu", VersionID: "26.04"}, "small")
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()

	if got := q.Get("title"); got != "kip doctor: served-cert failed" {
		t.Errorf("title = %q", got)
	}
	for _, want := range []string{"served-cert: fail", "heartbeat: not checked", "k3s: pass"} {
		if !strings.Contains(q.Get("what-happened"), want) {
			t.Errorf("what-happened lacks %q: %q", want, q.Get("what-happened"))
		}
	}
	if got := q.Get("environment"); got != "kip on darwin/arm64, server ubuntu 26.04, profile small" {
		t.Errorf("environment = %q", got)
	}
	if !strings.Contains(q.Get("repro"), "kip doctor` with --ssh-key") {
		t.Errorf("repro = %q", q.Get("repro"))
	}
	decoded, _ := url.QueryUnescape(link)
	for _, private := range []string{"Traefik", "203.0.113.10", "lab.example.org", "ops@example.org"} {
		if strings.Contains(decoded, private) {
			t.Errorf("link carries %q: %s", private, decoded)
		}
	}
}

func TestDoctorIssueLink_EmptyWithoutFailures(t *testing.T) {
	healthy := []doctorGroup{{"Host", []doctor.Result{{ID: "k3s", Status: doctor.Pass}}}}
	if link := doctorIssueLink(healthy, issueContext{}, installer.HostOS{}, ""); link != "" {
		t.Errorf("link = %q", link)
	}
}

func TestDoctorReport_ListsProblemsAndScrubsThem(t *testing.T) {
	report := doctorReport(rebootedCluster(), nil, []string{"lab.example.org"})

	for _, want := range []string{"[served-cert]", "fallback certificate", "[heartbeat]"} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	for _, private := range []string{"203.0.113.10", "lab.example.org", "ops@example.org"} {
		if strings.Contains(report, private) {
			t.Errorf("report carries %q:\n%s", private, report)
		}
	}
	if strings.Contains(report, "[k3s]") {
		t.Error("passing checks do not belong in the report")
	}
}

func TestDoctorKnownValues_CoverTheClusterRecord(t *testing.T) {
	cluster := &config.Cluster{
		Name: "lab", Host: "203.0.113.10", Domain: "lab.kipper.run", SSHUser: "deploy", SSHKey: "/home/you/.ssh/lab",
		Kubeconfig: "/home/you/.kip/clusters/lab.yaml", GatewayToken: "gw-not-real", Org: "acme", MachineID: "m-not-real",
	}
	secrets, names := doctorKnownValues(cluster, "/home/you/.ssh/other", "ops@example.org", []string{"node-a"})

	assertHolds(t, "secrets", secrets, "gw-not-real")
	assertHolds(t, "names", names, "lab", "203.0.113.10", "lab.kipper.run", "deploy", "/home/you/.ssh/lab",
		"/home/you/.kip/clusters/lab.yaml", "acme", "m-not-real", cluster.ConsoleHost(), cluster.DexHost(),
		"/home/you/.ssh/other", "ops@example.org", "node-a")
}

func TestNodesUnavailableError_PointsAtKipDoctor(t *testing.T) {
	err := nodesUnavailableError(errors.New("Unauthorized"))

	if !strings.Contains(err.Error(), "listing nodes: Unauthorized") || !strings.Contains(err.Error(), "kip doctor") {
		t.Errorf("error = %q", err)
	}
}

func fakeLogin(t *testing.T, result installer.ProofResult, detail string, err error) {
	t.Helper()
	prev := verifyOperatorIdentity
	verifyOperatorIdentity = func(context.Context, string, string, string, string, []byte) (installer.ProofResult, string, error) {
		return result, detail, err
	}
	t.Cleanup(func() { verifyOperatorIdentity = prev })
}

func TestCheckLogin(t *testing.T) {
	cluster := &config.Cluster{Domain: "lab.kipper.run"}
	for _, tc := range []struct {
		name   string
		result installer.ProofResult
		err    error
		status doctor.Status
		want   string
	}{
		{"accepted", installer.ProofPass, nil, doctor.Pass, "accepts your login"},
		{"no session", installer.ProofAuthnRejected, errors.New("not authenticated. Run: kip auth login"), doctor.Warn, "kip auth login"},
		{"dex unreachable", installer.ProofTransportError, auth.ErrDexUnreachable, doctor.Fail, "cannot refresh"},
		{"rejected", installer.ProofAuthnRejected, nil, doctor.Fail, "rejects your login"},
	} {
		fakeLogin(t, tc.result, "", tc.err)
		r := checkLogin(context.Background(), cluster, clusterEndpoint{})
		if r.Status != tc.status || !strings.Contains(r.Message, tc.want) {
			t.Errorf("%s: got %v %q, want %v containing %q", tc.name, r.Status, r.Message, tc.status, tc.want)
		}
	}
}

func TestCheckLogin_NamesBothCausesWhenTheDeadlinePasses(t *testing.T) {
	fakeLogin(t, installer.ProofTransportError, "context deadline exceeded", nil)

	r := checkLogin(context.Background(), &config.Cluster{Domain: "lab.kipper.run"}, clusterEndpoint{})
	if r.Status != doctor.Fail || !strings.Contains(r.Message, "refreshing your session or checking it with the API server did not finish") {
		t.Errorf("got %v %q", r.Status, r.Message)
	}
}

func TestCheckLogin_StopsWaitingAtTheDeadline(t *testing.T) {
	prevTimeout := doctorLoginTimeout
	doctorLoginTimeout = 50 * time.Millisecond
	t.Cleanup(func() { doctorLoginTimeout = prevTimeout })
	prev := verifyOperatorIdentity
	verifyOperatorIdentity = func(context.Context, string, string, string, string, []byte) (installer.ProofResult, string, error) {
		time.Sleep(2 * time.Second) // Simulate a token refresh that ignores the context.
		return installer.ProofPass, "", nil
	}
	t.Cleanup(func() { verifyOperatorIdentity = prev })

	start := time.Now()
	r := checkLogin(context.Background(), &config.Cluster{Domain: "lab.kipper.run"}, clusterEndpoint{})
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("waited %s", elapsed)
	}
	if r.Status != doctor.Fail || !strings.Contains(r.Message, "within") {
		t.Errorf("got %v %q", r.Status, r.Message)
	}
}

func TestDoctorKnownValues_IncludesTheResolvedKeyPath(t *testing.T) {
	t.Setenv("KIP_SSH_KEY", "/srv/keys/lab-key")
	_, names := doctorKnownValues(&config.Cluster{Name: "lab"}, "", "", nil)
	assertHolds(t, "names", names, "/srv/keys/lab-key")
}

func TestKubernetesAPIChecks_ShowAsNotCheckedWithoutAKubeconfig(t *testing.T) {
	results := kubernetesAPIChecks(context.Background(), &config.Cluster{Kubeconfig: "/nonexistent/kubeconfig.yaml"})

	if len(results) != 2 {
		t.Fatalf("got %d results, want the api and login checks", len(results))
	}
	for _, r := range results {
		if r.Status != doctor.Skip || !strings.Contains(r.Message, "not checked") {
			t.Errorf("%s: got %v %q", r.ID, r.Status, r.Message)
		}
	}
}
