package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/getkipper/kipper/kip/internal/installjob"
)

func TestInstallSummaryWritesCredentialsToTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.txt")
	var out bytes.Buffer
	printInstallSummary(&out, finishedInstall(generated, false), nil, path)

	if strings.Contains(out.String(), generated) {
		t.Errorf("password reached stdout despite --credentials-file:\n%s", out.String())
	}
	if !strings.Contains(out.String(), path) {
		t.Errorf("summary does not say where the credentials went:\n%s", out.String())
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), generated) {
		t.Errorf("file does not hold the password (err %v):\n%s", err, body)
	}
}

func TestCollectedCredentialsDefaultOutputIsUnchanged(t *testing.T) {
	var out bytes.Buffer
	announceCollectedCredentials(&out, "", installjob.Result{AdminEmail: "admin@storefront.example", AdminPassword: generated})

	want := "\n  Admin email: admin@storefront.example\n  Password:    " + generated +
		"\n  Save the password now; it is cleared from the server result after collection.\n"
	if out.String() != want {
		t.Errorf("output changed:\n got %q\nwant %q", out.String(), want)
	}
}

func TestCollectedCredentialsGoToTheLocalFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.txt")
	var out bytes.Buffer
	announceCollectedCredentials(&out, path, installjob.Result{AdminEmail: "admin@storefront.example", AdminPassword: generated})

	if strings.Contains(out.String(), generated) {
		t.Errorf("password reached stdout despite --credentials-file:\n%s", out.String())
	}
	body, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(body), generated) {
		t.Errorf("file does not hold the password (err %v):\n%s", err, body)
	}
}

func TestCredentialsFileMustDifferFromTheResultFile(t *testing.T) {
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	target := filepath.Join(dir, "install-output")

	for name, result := range map[string]string{
		"same path":         target,
		"through a symlink": filepath.Join(alias, "install-output"),
	} {
		if err := checkOutputPathsDiffer(target, result); err == nil {
			t.Errorf("%s: accepted --result %s for --credentials-file %s", name, result, target)
		}
	}
	// The symlink is followed before "..", so this names dir/install-output,
	// although a cleaned spelling of it points elsewhere.
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatal(err)
	}
	traversal := link + string(filepath.Separator) + ".." + string(filepath.Separator) + "install-output"
	if err := checkOutputPathsDiffer(target, traversal); err == nil {
		t.Errorf("accepted --result %s, which reaches %s through the symlink", traversal, target)
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if err := checkOutputPathsDiffer(target, "install-output"); err == nil {
		t.Error("accepted a relative --result naming the credentials file")
	}
	t.Chdir(wd)

	if err := checkOutputPathsDiffer(target, filepath.Join(dir, "cluster-result.kip")); err != nil {
		t.Errorf("rejected two different files: %v", err)
	}
	if err := checkOutputPathsDiffer("", target); err != nil {
		t.Errorf("rejected --result without --credentials-file: %v", err)
	}
}
