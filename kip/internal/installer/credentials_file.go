package installer

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CheckCredentialsFile checks that the destination is unused and its directory
// is writable. It leaves the destination uncreated so installation can be retried.
func CheckCredentialsFile(path string) error {
	if path == "" {
		return nil
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("--credentials-file %s already exists; choose a new path", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("--credentials-file %s: %w", path, err)
	}
	dir := filepath.Dir(path)
	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("--credentials-file: directory %s: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("--credentials-file: %s is not a directory", dir)
	}
	probe, err := os.CreateTemp(dir, ".kip-credentials-check-*")
	if err != nil {
		return fmt.Errorf("--credentials-file: cannot write to %s: %w", dir, err)
	}
	_ = probe.Close()
	_ = os.Remove(probe.Name())
	return nil
}

// SaveAdminCredentials creates a credentials file with mode 0600 and prints its path.
// Windows uses the directory's inherited permissions.
// It returns false if the path or password is empty or saving fails, so the caller
// can print the credentials. Save failures also produce a warning.
func SaveAdminCredentials(out io.Writer, path, email, password string) bool {
	if path == "" || password == "" {
		return false
	}
	if err := writeCredentialsFile(path, email, password); err != nil {
		_, _ = fmt.Fprintf(out, "  ⚠  Could not write the admin credentials to %s: %v\n", path, err)
		_, _ = fmt.Fprintf(out, "     Showing them here instead.\n\n")
		return false
	}
	_, _ = fmt.Fprintf(out, "  ✔  Admin credentials written to %s\n\n", path)
	return true
}

func writeCredentialsFile(path, email, password string) error {
	// Exclusive creation protects existing files, including symlinks.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // the operator chose this path
	if err != nil {
		return err
	}
	_, werr := fmt.Fprintf(f, "Kipper admin sign-in\nEmail:    %s\nPassword: %s\n\nIf lost, run: kip auth reset-password\n", email, password)
	cerr := f.Close()
	if err := errors.Join(werr, cerr); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}
