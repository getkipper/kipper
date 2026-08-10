// Package wire defines the datamover transfer protocol: URL layout, headers,
// and the JSON types exchanged between the export client and the import server.
package wire

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
)

// HeaderChunkSHA256 carries the hex SHA-256 of a chunk's uncompressed payload.
const HeaderChunkSHA256 = "X-Chunk-SHA256"

// StateDirName is the directory under the import root that holds resume state
// (manifest, chunk bitmap, partial file data). It is excluded from the
// deletion pass and removed after a successful finalize.
const StateDirName = ".kipper-transfer-state"

// PathPrefix is the URL prefix all transfer endpoints live under.
const PathPrefix = "/kipper-transfer"

// URL builds the full endpoint URL for an operation of a transfer,
// e.g. URL("https://host", "t1", "manifest") → "https://host/kipper-transfer/t1/manifest".
func URL(base, transferID, op string) string {
	return strings.TrimRight(base, "/") + PathPrefix + "/" + transferID + "/" + op
}

// StateResponse is the body of GET /state: the manifest the server holds and
// which chunks it has already committed, so the exporter can resume.
type StateResponse struct {
	ManifestDigest  string `json:"manifestDigest"`
	TotalChunks     int    `json:"totalChunks"`
	CompletedChunks Bitmap `json:"completedChunks"`
}

// FileResult is the per-file outcome of finalize verification on the target.
type FileResult struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Match  bool   `json:"match"`
	Error  string `json:"error,omitempty"`
}

// Report is the body of the finalize response: verification results plus the
// deletion pass (target entries absent from the manifest).
type Report struct {
	Files   []FileResult `json:"files"`
	Deleted []string     `json:"deleted,omitempty"`
}

// Progress is the body of GET /progress, polled by the reconciler.
type Progress struct {
	BytesDone   int64  `json:"bytesDone"`
	TotalBytes  int64  `json:"totalBytes"`
	ChunksDone  int    `json:"chunksDone"`
	TotalChunks int    `json:"totalChunks"`
	Phase       string `json:"phase"`
}

// Transfer phases reported by GET /progress.
const (
	PhaseWaiting    = "waiting"
	PhaseReceiving  = "receiving"
	PhaseFinalizing = "finalizing"
	PhaseCompleted  = "completed"
	PhaseFailed     = "failed"
)

// Bitmap tracks completed chunks, one bit per chunk. It marshals to a base64
// string in JSON (via the default []byte encoding).
type Bitmap []byte

// NewBitmap returns a zeroed bitmap sized for n chunks.
func NewBitmap(n int) Bitmap {
	return make(Bitmap, (n+7)/8)
}

// Get reports whether chunk n is marked complete.
func (b Bitmap) Get(n int) bool {
	i := n / 8
	if i < 0 || i >= len(b) {
		return false
	}
	return b[i]&(1<<uint(n%8)) != 0
}

// Set marks chunk n complete.
func (b Bitmap) Set(n int) {
	b[n/8] |= 1 << uint(n%8)
}

// Clear marks chunk n incomplete.
func (b Bitmap) Clear(n int) {
	b[n/8] &^= 1 << uint(n%8)
}

// Count returns the number of complete chunks among the first total.
func (b Bitmap) Count(total int) int {
	c := 0
	for n := 0; n < total; n++ {
		if b.Get(n) {
			c++
		}
	}
	return c
}

// TokenEqual compares two bearer tokens in constant time. Both sides are
// hashed first so the comparison leaks neither content nor length.
func TokenEqual(a, b string) bool {
	ha := sha256.Sum256([]byte(a))
	hb := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(ha[:], hb[:]) == 1
}

// StatusError is an HTTP-level protocol failure carrying the response status.
type StatusError struct {
	Status int
	Body   string
}

// Error implements the error interface.
func (e *StatusError) Error() string {
	return fmt.Sprintf("unexpected status %d: %s", e.Status, e.Body)
}
