// Package chunk derives the deterministic chunk plan from a manifest: the
// concatenation of all file contents in manifest order, cut into fixed-size
// chunks. Both sides compute the identical plan from the manifest alone.
package chunk

import (
	"sort"

	"github.com/getkipper/kipper/datamover/internal/manifest"
)

// Span is a contiguous slice of one file that falls inside a chunk.
type Span struct {
	// FileIndex is the file's position in the manifest.
	FileIndex int
	// Path is the manifest path of the file.
	Path string
	// FileOffset is the span's start offset within the file.
	FileOffset int64
	// Length is the span length in bytes; always positive.
	Length int64
}

// Plan maps chunk numbers to file spans for a manifest.
type Plan struct {
	chunkSize int64
	total     int64
	// starts[i] is the logical stream offset where file i begins.
	starts []int64
	files  []manifest.Entry
}

// NewPlan builds the chunk plan for a manifest.
func NewPlan(m *manifest.Manifest) *Plan {
	p := &Plan{
		chunkSize: m.ChunkSize,
		starts:    make([]int64, len(m.Entries)),
		files:     m.Entries,
	}
	var off int64
	for i, f := range m.Entries {
		p.starts[i] = off
		off += f.Size
	}
	p.total = off
	return p
}

// TotalBytes returns the length of the logical stream.
func (p *Plan) TotalBytes() int64 { return p.total }

// EntryOffset returns the logical stream offset where manifest entry i
// begins. Non-file entries occupy zero bytes.
func (p *Plan) EntryOffset(i int) int64 { return p.starts[i] }

// NumChunks returns how many chunks the stream cuts into.
func (p *Plan) NumChunks() int {
	if p.total == 0 {
		return 0
	}
	return int((p.total + p.chunkSize - 1) / p.chunkSize)
}

// ChunkLength returns the uncompressed payload length of chunk n.
func (p *Plan) ChunkLength(n int) int64 {
	start := int64(n) * p.chunkSize
	end := start + p.chunkSize
	if end > p.total {
		end = p.total
	}
	if end <= start {
		return 0
	}
	return end - start
}

// Spans returns the file spans covered by chunk n, in stream order.
// Zero-length files never appear in any span.
func (p *Plan) Spans(n int) []Span {
	start := int64(n) * p.chunkSize
	end := start + p.ChunkLength(n)
	if end <= start {
		return nil
	}
	// First file whose end is past the chunk start.
	i := sort.Search(len(p.files), func(i int) bool {
		return p.starts[i]+p.files[i].Size > start
	})
	var spans []Span
	for ; i < len(p.files) && p.starts[i] < end; i++ {
		f := p.files[i]
		if f.Size == 0 {
			continue
		}
		so := max(start, p.starts[i]) - p.starts[i]
		eo := min(end, p.starts[i]+f.Size) - p.starts[i]
		spans = append(spans, Span{
			FileIndex:  i,
			Path:       f.Path,
			FileOffset: so,
			Length:     eo - so,
		})
	}
	return spans
}
