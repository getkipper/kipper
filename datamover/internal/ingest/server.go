package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"

	"github.com/getkipper/kipper/datamover/internal/manifest"
	"github.com/getkipper/kipper/datamover/internal/wire"
)

// Committer performs the finalize step against the transfer target: verify
// every assembled file with a full re-read, commit it into place, and run the
// deletion pass.
type Committer interface {
	Commit(ctx context.Context, st *State) (*wire.Report, error)
}

// Server serves the transfer ingest API for a single transfer target.
type Server struct {
	root      string
	stateDir  string
	token     string
	committer Committer
	logf      func(format string, args ...any)

	mu         sync.RWMutex
	st         *State
	phase      string
	lastReport *wire.Report
}

// NewServer creates a server rooted at root, restoring any resume state left
// by a previous run. stateDir holds all transient state (chunk staging,
// bitmap); when empty it defaults to a dir under root. logf may be nil.
func NewServer(root, stateDir, token string, committer Committer, logf func(format string, args ...any)) (*Server, error) {
	if token == "" {
		return nil, errors.New("ingest token must not be empty")
	}
	if stateDir == "" {
		stateDir = DefaultStateDir(root)
	}
	st, err := LoadState(root, stateDir)
	if err != nil {
		return nil, err
	}
	s := &Server{root: root, stateDir: stateDir, token: token, committer: committer, logf: logf, st: st, phase: wire.PhaseWaiting}
	if st != nil {
		s.phase = wire.PhaseReceiving
		s.log("resumed transfer %s: %d of %d chunks already committed",
			st.TransferID, st.BitmapSnapshot().Count(st.Plan.NumChunks()), st.Plan.NumChunks())
	}
	return s, nil
}

func (s *Server) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// Handler returns the ingest API handler with bearer auth on every route.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	prefix := wire.PathPrefix + "/{id}"
	mux.HandleFunc("POST "+prefix+"/manifest", s.handleManifest)
	mux.HandleFunc("GET "+prefix+"/state", s.handleState)
	mux.HandleFunc("PUT "+prefix+"/chunk/{n}", s.handleChunk)
	mux.HandleFunc("POST "+prefix+"/finalize", s.handleFinalize)
	mux.HandleFunc("POST "+prefix+"/abort", s.handleAbort)
	mux.HandleFunc("GET "+prefix+"/progress", s.handleProgress)
	return s.requireAuth(mux)
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || !wire.TokenEqual(got, s.token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// state returns the current transfer state if the request's transfer id
// matches it, writing the error response otherwise.
func (s *Server) state(w http.ResponseWriter, r *http.Request) *State {
	s.mu.RLock()
	st := s.st
	s.mu.RUnlock()
	if st == nil {
		http.Error(w, "no manifest received", http.StatusConflict)
		return nil
	}
	if r.PathValue("id") != st.TransferID {
		http.Error(w, fmt.Sprintf("transfer id mismatch: server is bound to %s", st.TransferID), http.StatusConflict)
		return nil
	}
	return st
}

func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	raw, err := manifest.Decompress(r.Body, maxManifestBytes)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m, err := manifest.Decode(raw)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != nil && s.st.TransferID == id && s.st.Digest() == manifest.Digest(raw) {
		// Same transfer resuming; keep the bitmap and staged chunks.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	st, err := InitState(s.root, s.stateDir, id, raw, m)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.st = st
	s.phase = wire.PhaseReceiving
	s.lastReport = nil
	s.log("manifest accepted for transfer %s: %d files, %d bytes, %d chunks",
		id, len(m.Entries), st.Plan.TotalBytes(), st.Plan.NumChunks())
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	st := s.state(w, r)
	if st == nil {
		return
	}
	writeJSON(w, wire.StateResponse{
		ManifestDigest:  st.Digest(),
		TotalChunks:     st.Plan.NumChunks(),
		CompletedChunks: st.BitmapSnapshot(),
	})
}

func (s *Server) handleChunk(w http.ResponseWriter, r *http.Request) {
	st := s.state(w, r)
	if st == nil {
		return
	}
	n, err := strconv.Atoi(r.PathValue("n"))
	if err != nil || n < 0 || n >= st.Plan.NumChunks() {
		http.Error(w, "invalid chunk number", http.StatusBadRequest)
		return
	}
	if st.ChunkDone(n) {
		// Idempotent replay; drain so the client can reuse the connection.
		_, _ = io.Copy(io.Discard, r.Body) //nolint:errcheck // draining a duplicate
		w.WriteHeader(http.StatusOK)
		return
	}
	wantSum := strings.ToLower(r.Header.Get(wire.HeaderChunkSHA256))
	if len(wantSum) != hex.EncodedLen(sha256.Size) {
		http.Error(w, "missing or malformed "+wire.HeaderChunkSHA256+" header", http.StatusBadRequest)
		return
	}
	if err := s.receiveChunk(st, n, wantSum, r.Body); err != nil {
		var cerr *chunkError
		if errors.As(err, &cerr) {
			http.Error(w, cerr.Error(), http.StatusUnprocessableEntity)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// chunkError marks a payload problem the exporter should retry (422).
type chunkError struct{ msg string }

func (e *chunkError) Error() string { return e.msg }

// taggedReader converts read failures into chunk errors.
type taggedReader struct{ r io.Reader }

func (t taggedReader) Read(p []byte) (int, error) {
	n, err := t.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		return n, &chunkError{msg: fmt.Sprintf("reading chunk payload: %v", err)}
	}
	return n, err
}

// receiveChunk streams the zstd body into the flat staged chunk file while
// hashing the uncompressed payload, and only marks the chunk complete when
// length and hash both match. Staging is one file per chunk, never a mirror
// of the source tree, so an NFS-backed state dir sees no nested-dir churn.
func (s *Server) receiveChunk(st *State, n int, wantSum string, body io.Reader) error {
	dec, err := zstd.NewReader(body)
	if err != nil {
		return &chunkError{msg: fmt.Sprintf("chunk %d: invalid zstd stream: %v", n, err)}
	}
	defer dec.Close()

	h := sha256.New()
	// Read-side failures (bad zstd data, dropped connection) are payload
	// problems the exporter should retry, so they are tagged as chunk errors;
	// write-side failures stay internal errors.
	want := st.Plan.ChunkLength(n)
	payload := io.TeeReader(io.LimitReader(taggedReader{r: dec}, want+1), h)
	f, err := os.OpenFile(st.ChunkPath(n), os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("staging chunk %d: %w", n, err)
	}
	received, err := io.Copy(f, payload)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("writing chunk %d: %w", n, err)
	}
	if received != want {
		_ = f.Close()
		if received > want {
			return &chunkError{msg: fmt.Sprintf("chunk %d: payload longer than expected %d bytes", n, want)}
		}
		return &chunkError{msg: fmt.Sprintf("chunk %d: got %d of %d bytes", n, received, want)}
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSum {
		_ = f.Close()
		return &chunkError{msg: fmt.Sprintf("chunk %d: sha256 mismatch", n)}
	}
	// Data must reach stable storage before the completion bit does: a crash
	// after a persisted bit but before the data blocks would make resume skip
	// the chunk forever. The file is a fresh create, so its directory entry
	// is fsynced too or the bitmap could outlive a chunk the directory never
	// recorded.
	if err := syncFile(f); err != nil {
		_ = f.Close()
		return fmt.Errorf("syncing chunk %d: %w", n, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("closing chunk %d: %w", n, err)
	}
	if err := syncDir(filepath.Dir(st.ChunkPath(n))); err != nil {
		return fmt.Errorf("syncing chunk dir for %d: %w", n, err)
	}
	return st.MarkChunk(n)
}

func (s *Server) handleFinalize(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st == nil {
		if s.phase == wire.PhaseCompleted && s.lastReport != nil {
			// Finalize retried after a completed commit; replay the report.
			writeJSON(w, s.lastReport)
			return
		}
		http.Error(w, "no manifest received", http.StatusConflict)
		return
	}
	if r.PathValue("id") != s.st.TransferID {
		http.Error(w, fmt.Sprintf("transfer id mismatch: server is bound to %s", s.st.TransferID), http.StatusConflict)
		return
	}
	if !s.st.AllChunksDone() {
		http.Error(w, "cannot finalize: chunks missing", http.StatusConflict)
		return
	}
	s.phase = wire.PhaseFinalizing
	report, err := s.committer.Commit(r.Context(), s.st)
	if err != nil {
		s.phase = wire.PhaseFailed
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.lastReport = report
	if reportClean(report) {
		if err := s.st.Destroy(); err != nil {
			s.log("warning: %v", err)
		}
		s.st = nil
		s.phase = wire.PhaseCompleted
		s.log("transfer finalized: %d files verified, %d entries deleted", len(report.Files), len(report.Deleted))
	} else {
		// Keep state so the exporter can retry after fixing the source.
		s.phase = wire.PhaseFailed
		s.log("finalize verification failed for %d files", failedCount(report))
	}
	writeJSON(w, report)
}

func reportClean(r *wire.Report) bool { return failedCount(r) == 0 }

func failedCount(r *wire.Report) int {
	n := 0
	for _, f := range r.Files {
		if !f.Match || f.Error != "" {
			n++
		}
	}
	return n
}

func (s *Server) handleAbort(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.st != nil {
		if err := s.st.Destroy(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		s.st = nil
	}
	s.phase = wire.PhaseWaiting
	s.lastReport = nil
	s.log("transfer aborted, state cleaned")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleProgress(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	st, phase := s.st, s.phase
	s.mu.RUnlock()
	p := wire.Progress{Phase: phase}
	if st != nil {
		bitmap := st.BitmapSnapshot()
		total := st.Plan.NumChunks()
		p.TotalChunks = total
		p.TotalBytes = st.Plan.TotalBytes()
		for n := 0; n < total; n++ {
			if bitmap.Get(n) {
				p.ChunksDone++
				p.BytesDone += st.Plan.ChunkLength(n)
			}
		}
	}
	writeJSON(w, p)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Headers are already sent; nothing more can be reported to the peer.
		_ = err //nolint:errcheck // response already committed
	}
}
