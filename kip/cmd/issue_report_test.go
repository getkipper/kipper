package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestSaveIssueReport_WritesANewPrivateFile(t *testing.T) {
	dir := t.TempDir()

	path, err := saveIssueReport(dir, "install-error", "E: Could not get lock")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir || !strings.HasPrefix(filepath.Base(path), "install-error-") || filepath.Ext(path) != ".txt" {
		t.Errorf("path = %q", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows reports 0666 for any writable file and protects it with ACLs instead.
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", info.Mode().Perm())
	}
	if body, _ := os.ReadFile(path); string(body) != "E: Could not get lock\n" {
		t.Errorf("body = %q", body)
	}
}

func TestSaveIssueReport_GivesEveryReportItsOwnFile(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "install-error.txt")
	if err := os.WriteFile(existing, []byte("admin password"), 0o600); err != nil {
		t.Fatal(err)
	}

	first, err := saveIssueReport(dir, "install-error", "first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := saveIssueReport(dir, "install-error", "second")
	if err != nil {
		t.Fatal(err)
	}

	if first == second || first == existing || second == existing {
		t.Errorf("reports reused a path: %q, %q", first, second)
	}
	for path, want := range map[string]string{existing: "admin password", first: "first\n", second: "second\n"} {
		if body, _ := os.ReadFile(path); string(body) != want {
			t.Errorf("%s holds %q, want %q", path, body, want)
		}
	}
}

func TestSaveIssueReport_LeavesALinkedFileAlone(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "credentials.txt")
	if err := os.WriteFile(target, []byte("admin password"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "install-error.txt")); err != nil {
		t.Skipf("this account cannot create symlinks: %v", err)
	}

	if _, err := saveIssueReport(dir, "install-error", "report"); err != nil {
		t.Fatal(err)
	}

	if body, _ := os.ReadFile(target); string(body) != "admin password" {
		t.Errorf("the linked file was overwritten: %q", body)
	}
}

func TestPrintIssueReport_SaysWhenTheReportCouldNotBeSaved(t *testing.T) {
	var out bytes.Buffer
	printIssueReport(&out, "Think this is a Kipper bug?", "https://github.com/getkipper/kipper/issues/new", "E: boom", "")

	if strings.Contains(out.String(), "Also saved") {
		t.Errorf("claimed a saved file:\n%s", out.String())
	}
}

func TestPrintIssueReport_ShowsTheLinkAndTheErrorToPaste(t *testing.T) {
	var out bytes.Buffer
	printIssueReport(&out, "Think this is a Kipper bug?", "https://github.com/getkipper/kipper/issues/new?template=bug_report.yml",
		"E: Could not get lock", "/home/you/.kip/install-error.txt")

	for _, want := range []string{
		"https://github.com/getkipper/kipper/issues/new",
		"Logs",
		"E: Could not get lock",
		"/home/you/.kip/install-error.txt",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestPrintIssueReport_PrintsNothingWithoutALink(t *testing.T) {
	var out bytes.Buffer
	printIssueReport(&out, "Think this is a Kipper bug?", "", "E: boom", "")

	if out.Len() != 0 {
		t.Errorf("printed %q", out.String())
	}
}
