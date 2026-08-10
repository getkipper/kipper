package export

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Source provides ranged read access to the transfer units named by a
// manifest. Implementations exist for the local filesystem and S3.
type Source interface {
	// OpenRange returns a reader over length bytes of the unit at the given
	// manifest path, starting at offset.
	OpenRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error)
}

// FSSource reads manifest paths relative to a root directory.
type FSSource struct {
	// Root is the directory manifest paths are resolved against.
	Root string
}

// OpenRange implements Source.
func (s *FSSource) OpenRange(_ context.Context, path string, offset, length int64) (io.ReadCloser, error) {
	f, err := os.Open(filepath.Join(s.Root, filepath.FromSlash(path)))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	return &sectionReadCloser{r: io.NewSectionReader(f, offset, length), c: f}, nil
}

type sectionReadCloser struct {
	r *io.SectionReader
	c io.Closer
}

func (s *sectionReadCloser) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *sectionReadCloser) Close() error               { return s.c.Close() }
