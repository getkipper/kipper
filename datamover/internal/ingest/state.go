// Package ingest implements the receiving side of a transfer: the HTTP API,
// crash-safe resume state on disk, chunk verification, and the finalize
// committers for filesystem and S3 targets.
package ingest

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/getkipper/kipper/datamover/internal/chunk"
	"github.com/getkipper/kipper/datamover/internal/manifest"
	"github.com/getkipper/kipper/datamover/internal/wire"
)

const (
	manifestFileName = "manifest.json"
	idFileName       = "transfer-id"
	bitmapFileName   = "bitmap"
	chunksDirName    = "chunks"
)

// maxManifestBytes caps the decompressed manifest size (1GiB covers well over
// a million files).
const maxManifestBytes = 1 << 30

// DefaultStateDir returns where transfer state lives when no explicit state
// dir is configured: under the import root itself.
func DefaultStateDir(root string) string {
	return filepath.Join(root, wire.StateDirName)
}

// State is the durable per-transfer state: the manifest, the completed-chunk
// bitmap, and received chunk data as flat chunk-indexed files. It survives
// process restarts so transfers resume instead of starting over.
//
// The state dir is configurable and independent of the import root. Chunk
// data is staged flat (chunks/<n>), never mirroring the source tree: NFS
// backed roots (Longhorn RWX) wedge on many-small-files nested-dir churn, so
// transient state must be placeable off the data volume. The caller chooses
// the crash-resume tradeoff through that placement: node-local ephemeral
// storage loses state when the import pod's node is gone, persistent storage
// survives. The datamover does not decide this; the --state-dir flag does.
//
// Resume covers the export side's own retries against a live import server
// (a dropped connection, a chunk that failed its hash). It does NOT recover
// a lost import pod: the pod is created once per transfer and is not
// rescheduled, so if it dies the transfer fails cleanly and the operator
// re-runs the migration. Resume within a run, not survival of the receiver.
type State struct {
	root string
	dir  string

	// TransferID is the id this state was created for.
	TransferID string
	// Raw is the encoded manifest exactly as received.
	Raw []byte
	// Manifest is the decoded manifest.
	Manifest *manifest.Manifest
	// Plan is the chunk plan derived from the manifest.
	Plan *chunk.Plan

	mu     sync.Mutex
	bitmap wire.Bitmap
}

// LoadState restores state from stateDir. It returns (nil, nil) when no prior
// state exists.
func LoadState(root, stateDir string) (*State, error) {
	raw, err := os.ReadFile(filepath.Join(stateDir, manifestFileName))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading stored manifest: %w", err)
	}
	m, err := manifest.Decode(raw)
	if err != nil {
		return nil, fmt.Errorf("stored manifest: %w", err)
	}
	id, err := os.ReadFile(filepath.Join(stateDir, idFileName))
	if err != nil {
		return nil, fmt.Errorf("reading stored transfer id: %w", err)
	}
	plan := chunk.NewPlan(m)
	bitmap, err := os.ReadFile(filepath.Join(stateDir, bitmapFileName))
	if err != nil {
		return nil, fmt.Errorf("reading chunk bitmap: %w", err)
	}
	if len(bitmap) != len(wire.NewBitmap(plan.NumChunks())) {
		return nil, fmt.Errorf("chunk bitmap has %d bytes, want %d", len(bitmap), len(wire.NewBitmap(plan.NumChunks())))
	}
	return &State{
		root:       root,
		dir:        stateDir,
		TransferID: string(id),
		Raw:        raw,
		Manifest:   m,
		Plan:       plan,
		bitmap:     bitmap,
	}, nil
}

// InitState discards any prior state under stateDir and creates fresh state
// for a manifest: stores the manifest and id, zeroes the bitmap, and creates
// the flat chunk staging dir.
func InitState(root, stateDir, transferID string, raw []byte, m *manifest.Manifest) (*State, error) {
	if err := os.RemoveAll(stateDir); err != nil {
		return nil, fmt.Errorf("clearing previous state: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(stateDir, chunksDirName), 0o700); err != nil {
		return nil, fmt.Errorf("creating state dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, manifestFileName), raw, 0o600); err != nil {
		return nil, fmt.Errorf("storing manifest: %w", err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, idFileName), []byte(transferID), 0o600); err != nil { //nolint:gosec // G703: the id is file content; the path is the fixed state-dir layout
		return nil, fmt.Errorf("storing transfer id: %w", err)
	}
	st := &State{
		root:       root,
		dir:        stateDir,
		TransferID: transferID,
		Raw:        raw,
		Manifest:   m,
		Plan:       chunk.NewPlan(m),
		bitmap:     wire.NewBitmap(chunk.NewPlan(m).NumChunks()),
	}
	if err := st.persistBitmap(); err != nil {
		return nil, err
	}
	return st, nil
}

// Root returns the import root directory.
func (s *State) Root() string { return s.root }

// StateDir returns the directory holding all transient transfer state.
func (s *State) StateDir() string { return s.dir }

// Digest returns the hex SHA-256 of the stored manifest bytes.
func (s *State) Digest() string { return manifest.Digest(s.Raw) }

// ChunkPath returns where chunk n's verified payload is staged.
func (s *State) ChunkPath(n int) string {
	return filepath.Join(s.dir, chunksDirName, strconv.Itoa(n))
}

// AssembleEntry streams the content of manifest entry i into w by reading the
// staged chunk files that cover its byte range, in order.
func (s *State) AssembleEntry(i int, w io.Writer) error {
	e := s.Manifest.Entries[i]
	if e.Size == 0 {
		return nil
	}
	start := s.Plan.EntryOffset(i)
	chunkSize := s.Manifest.ChunkSize
	var written int64
	for written < e.Size {
		pos := start + written
		n := int(pos / chunkSize)
		inOff := pos - int64(n)*chunkSize
		length := min(e.Size-written, s.Plan.ChunkLength(n)-inOff)
		fh, err := os.Open(s.ChunkPath(n))
		if err != nil {
			return fmt.Errorf("opening staged chunk %d: %w", n, err)
		}
		copied, err := io.Copy(w, io.NewSectionReader(fh, inOff, length))
		_ = fh.Close()
		if err != nil {
			return fmt.Errorf("reading staged chunk %d: %w", n, err)
		}
		if copied != length {
			return fmt.Errorf("staged chunk %d is short: got %d of %d bytes", n, copied, length)
		}
		written += copied
	}
	return nil
}

// BitmapSnapshot returns a copy of the completed-chunk bitmap.
func (s *State) BitmapSnapshot() wire.Bitmap {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(wire.Bitmap, len(s.bitmap))
	copy(out, s.bitmap)
	return out
}

// ChunkDone reports whether chunk n is already committed.
func (s *State) ChunkDone(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bitmap.Get(n)
}

// MarkChunk records chunk n as committed and persists the bitmap. When the
// persist fails the in-memory bit is reverted too: GET /state must never
// claim a chunk that is not durable, or the exporter would skip re-sending
// it.
func (s *State) MarkChunk(n int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.bitmap.Set(n)
	if err := s.persistBitmap(); err != nil {
		s.bitmap.Clear(n)
		return err
	}
	return nil
}

// AllChunksDone reports whether every chunk has been committed.
func (s *State) AllChunksDone() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bitmap.Count(s.Plan.NumChunks()) == s.Plan.NumChunks()
}

// syncFile flushes a file to stable storage. It is a package variable so the
// durability-ordering regression test can observe the sync sequence.
var syncFile = func(f *os.File) error { return f.Sync() }

// syncDir flushes a directory so a completed rename survives a crash.
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("opening dir for sync: %w", err)
	}
	err = syncFile(d)
	if cerr := d.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("syncing dir: %w", err)
	}
	return nil
}

// persistBitmap writes the bitmap durably: tmp file, fsync, rename, then
// fsync of the state dir. Chunk data must already be synced before the bit is
// persisted, or a crash could leave the bitmap claiming data that never hit
// disk and resume would skip it forever. Callers hold s.mu, except InitState
// before the state is shared.
func (s *State) persistBitmap() error {
	tmp := filepath.Join(s.dir, bitmapFileName+".tmp")
	fh, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("writing chunk bitmap: %w", err)
	}
	if _, err := fh.Write(s.bitmap); err != nil {
		_ = fh.Close()
		return fmt.Errorf("writing chunk bitmap: %w", err)
	}
	if err := syncFile(fh); err != nil {
		_ = fh.Close()
		return fmt.Errorf("syncing chunk bitmap: %w", err)
	}
	if err := fh.Close(); err != nil {
		return fmt.Errorf("closing chunk bitmap: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(s.dir, bitmapFileName)); err != nil {
		return fmt.Errorf("committing chunk bitmap: %w", err)
	}
	return syncDir(s.dir)
}

// Destroy removes all on-disk state for the transfer.
func (s *State) Destroy() error {
	if err := os.RemoveAll(s.dir); err != nil {
		return fmt.Errorf("removing transfer state: %w", err)
	}
	return nil
}
