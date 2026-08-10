package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/getkipper/kipper/datamover/internal/manifest"
	"github.com/getkipper/kipper/datamover/internal/wire"
)

// eremoteio is Linux's EREMOTEIO (stale NFS/ganesha handle). The stdlib has no
// constant for it, so the numeric errno is matched directly. Longhorn RWX
// volumes surface it transiently on the share root even while every child
// entry reads fine.
const eremoteio = syscall.Errno(121)

// ioRetryAttempts and ioRetryBackoff govern transient-IO retries. Backoff runs
// 200ms, 400ms, 800ms, 1.6s between the five attempts.
const ioRetryAttempts = 5

var ioRetryBackoff = []time.Duration{
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
	1600 * time.Millisecond,
}

// Seams, overridable in tests to inject transient failures. Production code
// always binds them to the real os functions.
var (
	lstatFn   = os.Lstat
	readDirFn = os.ReadDir
	ioSleep   = time.Sleep
)

// isTransientIO reports whether err is a transient filesystem I/O error worth
// retrying: EIO, EREMOTEIO, or either wrapped in an *os.PathError. Matching is
// by errno value, never by message text.
func isTransientIO(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.EIO) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == eremoteio || errno == syscall.EIO
	}
	return false
}

// retryIO runs fn, retrying on transient I/O errors with backoff. A non-I/O
// error (including os.IsNotExist) returns immediately; a persistent I/O error
// returns after the attempt budget is spent.
func retryIO(fn func() error) error {
	var err error
	for attempt := 0; attempt < ioRetryAttempts; attempt++ {
		err = fn()
		if err == nil || !isTransientIO(err) {
			return err
		}
		if attempt < len(ioRetryBackoff) {
			ioSleep(ioRetryBackoff[attempt])
		}
	}
	return err
}

func lstatRetry(p string) (os.FileInfo, error) {
	var info os.FileInfo
	err := retryIO(func() error {
		var e error
		info, e = lstatFn(p)
		return e
	})
	return info, err
}

func readDirRetry(p string) ([]os.DirEntry, error) {
	var entries []os.DirEntry
	err := retryIO(func() error {
		var e error
		entries, e = readDirFn(p)
		return e
	})
	return entries, err
}

// FSCommitter finalizes a transfer onto the local filesystem: it verifies
// every assembled file with a full re-read, recreates directories and
// symlinks with their metadata, renames files into place atomically, and
// deletes target entries absent from the manifest (full-sync semantics).
// Symlinks are never followed, neither when applying metadata nor when
// deleting. Metadata and tree-walking operations are retried on transient
// NFS I/O errors.
type FSCommitter struct{}

// Commit implements Committer. Directories are created first (the manifest is
// sorted, so parents precede children), then files, then symlinks; the
// deletion pass runs last, followed by directory mtimes, which every earlier
// phase would otherwise disturb.
func (c FSCommitter) Commit(ctx context.Context, st *State) (*wire.Report, error) {
	results := make(map[string]wire.FileResult, len(st.Manifest.Entries))
	for i, e := range st.Manifest.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		switch e.Type {
		case manifest.TypeDir:
			results[e.Path] = commitDir(st, e)
		case manifest.TypeFile:
			results[e.Path] = commitFile(st, i, e)
		}
	}
	for _, e := range st.Manifest.Entries {
		if e.Type == manifest.TypeSymlink {
			results[e.Path] = commitSymlink(st, e)
		}
	}
	deleted, err := c.deletionPass(ctx, st.Root(), st.StateDir(), st.Manifest)
	if err != nil {
		return nil, err
	}
	applyDirTimes(st, results)

	report := &wire.Report{Files: make([]wire.FileResult, 0, len(st.Manifest.Entries)), Deleted: deleted}
	for _, e := range st.Manifest.Entries {
		report.Files = append(report.Files, results[e.Path])
	}
	return report, nil
}

// ensureRealDir walks the relative directory path under root component by
// component, replacing any symlink or non-directory it meets, so no write
// ever traverses a symlink out of the transfer root.
func ensureRealDir(root, relDir string) error {
	if relDir == "." || relDir == "" {
		return nil
	}
	current := root
	for _, seg := range strings.Split(relDir, "/") {
		current = filepath.Join(current, seg)
		info, err := lstatRetry(current)
		switch {
		case os.IsNotExist(err):
			if err := mkdirRetry(current); err != nil {
				return fmt.Errorf("creating directory: %v", err)
			}
		case err != nil:
			return fmt.Errorf("stating path component: %v", err)
		case !info.IsDir():
			// A stale file or symlink sits where a directory belongs.
			if err := retryIO(func() error { return os.Remove(current) }); err != nil {
				return fmt.Errorf("replacing stale entry: %v", err)
			}
			if err := mkdirRetry(current); err != nil {
				return fmt.Errorf("creating directory: %v", err)
			}
		}
	}
	return nil
}

func mkdirRetry(p string) error {
	return retryIO(func() error {
		return os.Mkdir(p, 0o755) //nolint:gosec // G301: app data dirs must stay traversable by the app user
	})
}

func commitDir(st *State, e manifest.Entry) wire.FileResult {
	res := wire.FileResult{Path: e.Path, Match: true}
	if err := ensureRealDir(st.Root(), e.Path); err != nil {
		res.Match = false
		res.Error = err.Error()
		return res
	}
	final := filepath.Join(st.Root(), filepath.FromSlash(e.Path))
	if err := retryIO(func() error { return os.Chmod(final, fs.FileMode(e.Mode)) }); err != nil {
		res.Error = fmt.Sprintf("applying mode: %v", err)
		return res
	}
	if os.Geteuid() == 0 {
		if err := retryIO(func() error { return os.Chown(final, e.UID, e.GID) }); err != nil {
			res.Error = fmt.Sprintf("applying ownership: %v", err)
		}
	}
	return res
}

// commitFile assembles one file from the flat staged chunks into a staging
// file beside its final location (same filesystem, so the last rename stays
// atomic), verifies it with a full re-read, applies metadata, and moves it
// into place. The hash is always computed by re-reading the assembled file.
func commitFile(st *State, i int, e manifest.Entry) wire.FileResult {
	res := wire.FileResult{Path: e.Path}
	if err := ensureRealDir(st.Root(), parentDir(e.Path)); err != nil {
		res.Error = err.Error()
		return res
	}
	final := filepath.Join(st.Root(), filepath.FromSlash(e.Path))
	part := final + ".kipper-part"
	if err := retryIO(func() error { return assembleTo(st, i, part) }); err != nil {
		res.Error = err.Error()
		return res
	}
	var sum string
	if err := retryIO(func() error {
		var e error
		sum, e = hashFile(part)
		return e
	}); err != nil {
		res.Error = err.Error()
		return res
	}
	res.SHA256 = sum
	// Verification is correctness: a real mismatch always hard-fails and is
	// never retried away.
	if sum != e.SHA256 {
		_ = os.Remove(part) //nolint:errcheck // best-effort cleanup of a failed part
		res.Error = "sha256 mismatch after assembly"
		return res
	}
	res.Match = true
	if err := applyAndRename(e, part, final); err != nil {
		res.Error = err.Error()
	}
	return res
}

func assembleTo(st *State, i int, part string) error {
	f, err := os.OpenFile(part, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("creating assembly file: %v", err)
	}
	err = st.AssembleEntry(i, f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(part) //nolint:errcheck // best-effort cleanup of a failed part
		return fmt.Errorf("assembling: %v", err)
	}
	return nil
}

func applyAndRename(e manifest.Entry, part, final string) error {
	if err := retryIO(func() error { return os.Chmod(part, fs.FileMode(e.Mode)) }); err != nil {
		return fmt.Errorf("applying mode: %v", err)
	}
	// Preserving ownership requires root; a non-root mover leaves files
	// owned by itself, matching rsync's behaviour without --owner.
	if os.Geteuid() == 0 {
		if err := retryIO(func() error { return os.Chown(part, e.UID, e.GID) }); err != nil {
			return fmt.Errorf("applying ownership: %v", err)
		}
	}
	mtime := time.Unix(0, e.MtimeUnixNano)
	if err := retryIO(func() error { return os.Chtimes(part, mtime, mtime) }); err != nil {
		return fmt.Errorf("applying mtime: %v", err)
	}
	if err := replaceIfDir(final); err != nil {
		return err
	}
	if err := retryIO(func() error { return os.Rename(part, final) }); err != nil {
		return fmt.Errorf("renaming into place: %v", err)
	}
	return nil
}

func commitSymlink(st *State, e manifest.Entry) wire.FileResult {
	res := wire.FileResult{Path: e.Path, Match: true}
	fail := func(err error) wire.FileResult {
		res.Match = false
		res.Error = err.Error()
		return res
	}
	if err := ensureRealDir(st.Root(), parentDir(e.Path)); err != nil {
		return fail(err)
	}
	final := filepath.Join(st.Root(), filepath.FromSlash(e.Path))
	if err := replaceIfDir(final); err != nil {
		return fail(err)
	}
	// The target string is reproduced verbatim and never resolved; pointing
	// outside the root is legitimate, following it would not be. Rename over
	// any stale file or link makes the swap atomic.
	tmp := final + ".kipper-link"
	if err := retryIO(func() error { return os.Remove(tmp) }); err != nil && !os.IsNotExist(err) {
		return fail(fmt.Errorf("clearing temp link: %v", err))
	}
	if err := retryIO(func() error { return os.Symlink(e.LinkTarget, tmp) }); err != nil {
		return fail(fmt.Errorf("creating symlink: %v", err))
	}
	if os.Geteuid() == 0 {
		if err := retryIO(func() error { return os.Lchown(tmp, e.UID, e.GID) }); err != nil {
			return fail(fmt.Errorf("applying link ownership: %v", err))
		}
	}
	// Link mtime is not applied: the stdlib offers no lutimes, and link
	// timestamps carry no application meaning.
	if err := retryIO(func() error { return os.Rename(tmp, final) }); err != nil {
		return fail(fmt.Errorf("renaming link into place: %v", err))
	}
	return res
}

// parentDir returns the slash-separated parent of a manifest path.
func parentDir(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[:i]
	}
	return ""
}

// replaceIfDir removes a stale directory occupying a path where a file or
// symlink belongs. Rename cannot atomically replace a directory.
func replaceIfDir(final string) error {
	info, err := lstatRetry(final)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stating target path: %v", err)
	}
	if info.IsDir() {
		if err := retryIO(func() error { return os.RemoveAll(final) }); err != nil {
			return fmt.Errorf("removing stale directory: %v", err)
		}
	}
	return nil
}

func hashFile(p string) (string, error) {
	fh, err := os.Open(p)
	if err != nil {
		return "", fmt.Errorf("opening assembled file: %v", err)
	}
	defer func() { _ = fh.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return "", fmt.Errorf("hashing assembled file: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// deletionPass removes entries under root that are absent from the manifest,
// then prunes emptied directories the manifest does not list. Manifest
// directories and symlinks count as present; the state dir is excluded when it
// lives under root. Symlinks are never followed: each entry is classified by
// lstat, so a link is removed as a link and its target directory is never
// descended into.
//
// The full-sync contract is correctness, not cleanup: a completed transfer
// means the target equals the source, so a stray file that cannot be removed
// fails the transfer rather than being reported as a clean sync. Every walk,
// stat, and remove retries transient NFS I/O errors (EIO/EREMOTEIO) first,
// since those clear on retry; only a persistent failure, after retries, is
// returned as an error. The caller then keeps resume state and the transfer
// re-runs instead of committing to a target tree that is neither the source
// nor a known snapshot. Context cancellation also aborts.
func (c FSCommitter) deletionPass(ctx context.Context, root, stateDir string, m *manifest.Manifest) ([]string, error) {
	keep := make(map[string]struct{}, len(m.Entries))
	for _, e := range m.Entries {
		keep[e.Path] = struct{}{}
	}
	var deleted []string
	var pruneCandidates []string

	var walk func(dir, rel string) error
	walk = func(dir, rel string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := readDirRetry(dir)
		if err != nil {
			return fmt.Errorf("deletion pass: reading %s: %w", dir, err)
		}
		for _, ent := range entries {
			if err := ctx.Err(); err != nil {
				return err
			}
			child := filepath.Join(dir, ent.Name())
			if child == stateDir {
				continue
			}
			childRel := path.Join(rel, ent.Name())
			info, err := lstatRetry(child)
			if err != nil {
				return fmt.Errorf("deletion pass: stat %s: %w", child, err)
			}
			if info.IsDir() {
				if err := walk(child, childRel); err != nil {
					return err
				}
				if _, ok := keep[childRel]; !ok {
					pruneCandidates = append(pruneCandidates, child)
				}
				continue
			}
			if _, ok := keep[childRel]; !ok {
				// The mover is the volume's only writer during finalize, so
				// the walk/remove window has no concurrent renames to race.
				if err := retryIO(func() error { return os.Remove(child) }); err != nil { //nolint:gosec // G122
					return fmt.Errorf("deletion pass: removing stray %s: %w", childRel, err)
				}
				deleted = append(deleted, childRel)
			}
		}
		return nil
	}
	if err := walk(root, ""); err != nil {
		return deleted, err
	}
	// Deepest first, so emptied trees prune bottom-up. Manifest-listed dirs
	// are never candidates: empty directories in the manifest are deliberate.
	sort.Slice(pruneCandidates, func(i, j int) bool { return len(pruneCandidates[i]) > len(pruneCandidates[j]) })
	for _, dir := range pruneCandidates {
		entries, err := readDirRetry(dir)
		if err != nil {
			return deleted, fmt.Errorf("deletion pass: reading %s: %w", dir, err)
		}
		if len(entries) == 0 {
			// A non-empty or busy dir simply stays.
			_ = retryIO(func() error { return os.Remove(dir) }) //nolint:errcheck // best-effort prune
		}
	}
	return deleted, nil
}

// applyDirTimes restores directory mtimes deepest-first after all renames and
// deletions, which bump the mtime of every touched parent.
func applyDirTimes(st *State, results map[string]wire.FileResult) {
	var dirs []manifest.Entry
	for _, e := range st.Manifest.Entries {
		if e.Type == manifest.TypeDir && results[e.Path].Match {
			dirs = append(dirs, e)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return len(dirs[i].Path) > len(dirs[j].Path) })
	for _, e := range dirs {
		final := filepath.Join(st.Root(), filepath.FromSlash(e.Path))
		mtime := time.Unix(0, e.MtimeUnixNano)
		if err := retryIO(func() error { return os.Chtimes(final, mtime, mtime) }); err != nil {
			res := results[e.Path]
			res.Error = fmt.Sprintf("applying mtime: %v", err)
			results[e.Path] = res
		}
	}
}
