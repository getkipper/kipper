package installer

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const generatedValue = "b3d4f6a8c0e2"

func TestSaveAdminCredentialsWritesAPrivateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.txt")
	var out bytes.Buffer

	if !SaveAdminCredentials(&out, path, "admin@storefront.example", generatedValue) {
		t.Fatalf("credentials were not saved:\n%s", out.String())
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{generatedValue, "admin@storefront.example", "kip auth reset-password"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("file omits %q:\n%s", want, body)
		}
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("file mode = %o, want 600", mode)
		}
	}
	if strings.Contains(out.String(), generatedValue) {
		t.Errorf("password reached the terminal:\n%s", out.String())
	}
	if !strings.Contains(out.String(), path) || strings.Count(out.String(), "\n") > 2 {
		t.Errorf("want one line naming %s, got:\n%s", path, out.String())
	}
}

func TestSaveAdminCredentialsKeepsAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.txt")
	if err := os.WriteFile(path, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer

	if SaveAdminCredentials(&out, path, "admin@storefront.example", generatedValue) {
		t.Fatal("reported an existing file as saved")
	}

	body, _ := os.ReadFile(path)
	if string(body) != "keep me\n" {
		t.Errorf("existing file was overwritten:\n%s", body)
	}
	if strings.Contains(out.String(), generatedValue) {
		t.Errorf("the warning carries the password; the caller shows it:\n%s", out.String())
	}
	if !strings.Contains(out.String(), path) {
		t.Errorf("warning does not name the file:\n%s", out.String())
	}
}

func TestSaveAdminCredentialsDoesNotFollowSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks needs extra privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target.txt")
	if err := os.WriteFile(target, []byte("keep me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dest := range map[string]string{
		"symlink to a file": target,
		"dangling symlink":  filepath.Join(dir, "nowhere.txt"),
	} {
		link := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
		if err := os.Symlink(dest, link); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if SaveAdminCredentials(&out, link, "admin@storefront.example", generatedValue) {
			t.Errorf("%s: wrote through the link", name)
		}
		if err := CheckCredentialsFile(link); err == nil {
			t.Errorf("%s: the early check accepted the link", name)
		}
	}
	body, _ := os.ReadFile(target)
	if string(body) != "keep me\n" {
		t.Errorf("the link target was overwritten:\n%s", body)
	}
	if _, err := os.Lstat(filepath.Join(dir, "nowhere.txt")); !os.IsNotExist(err) {
		t.Error("a file was created at the dangling link's target")
	}
}

func TestSaveAdminCredentialsLeavesTheDefaultAlone(t *testing.T) {
	var out bytes.Buffer
	if SaveAdminCredentials(&out, "", "admin@storefront.example", generatedValue) {
		t.Error("reported saved without a path")
	}
	if SaveAdminCredentials(&out, filepath.Join(t.TempDir(), "admin.txt"), "admin@storefront.example", "") {
		t.Error("reported saved without a generated password")
	}
	if out.Len() != 0 {
		t.Errorf("printed without anything to save:\n%s", out.String())
	}
}

func TestCheckCredentialsFileCatchesBadPathsBeforeTheInstall(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "taken.txt")
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"existing file":     existing,
		"missing directory": filepath.Join(dir, "missing", "admin.txt"),
		"directory":         dir,
	} {
		if err := CheckCredentialsFile(path); err == nil {
			t.Errorf("%s: accepted %s", name, path)
		}
	}

	fresh := filepath.Join(dir, "admin.txt")
	if err := CheckCredentialsFile(fresh); err != nil {
		t.Fatalf("rejected a fresh path: %v", err)
	}
	if _, err := os.Lstat(fresh); !os.IsNotExist(err) {
		t.Error("the check created the file; a failed install would then block its own rerun")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("the check left files behind: %v", entries)
	}
}
