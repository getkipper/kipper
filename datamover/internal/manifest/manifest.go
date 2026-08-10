// Package manifest builds and encodes transfer manifests: the sorted list of
// filesystem entries (or S3 objects) that make up a transfer, with per-entry
// metadata and SHA-256 hashes for regular files. The encoded manifest bytes
// are the unit both sides hash to agree on transfer identity.
package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/klauspost/compress/zstd"
)

// DefaultChunkSize is the default chunk size: 128Mi.
const DefaultChunkSize int64 = 128 * 1024 * 1024

// Entry types. Directories and symlinks carry no content: they exist so the
// target reproduces the full filesystem layout, including empty directories
// (load-bearing for MinIO bucket dirs) and links.
const (
	TypeFile    = "file"
	TypeDir     = "dir"
	TypeSymlink = "symlink"
)

// Entry describes one transfer unit: a regular file, directory, or symlink
// relative to the transfer root, or an S3 object key (always TypeFile).
type Entry struct {
	// Path is slash-separated and relative to the transfer root.
	Path string `json:"path"`
	// Type is one of TypeFile, TypeDir, TypeSymlink.
	Type string `json:"type"`
	// Size is the content length; zero for directories and symlinks.
	Size int64 `json:"size"`
	// Mode is the Unix permission bits.
	Mode uint32 `json:"mode"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
	// MtimeUnixNano is the modification time in nanoseconds since the epoch.
	MtimeUnixNano int64 `json:"mtimeUnixNano"`
	// SHA256 is the hex digest of the file content; empty for non-files.
	SHA256 string `json:"sha256,omitempty"`
	// LinkTarget is the symlink target, recorded verbatim and never followed.
	LinkTarget string `json:"linkTarget,omitempty"`
}

// Manifest is the complete description of a transfer.
type Manifest struct {
	ChunkSize int64   `json:"chunkSizeBytes"`
	Entries   []Entry `json:"entries"`
}

// TotalBytes returns the sum of all content sizes.
func (m *Manifest) TotalBytes() int64 {
	var t int64
	for _, e := range m.Entries {
		t += e.Size
	}
	return t
}

// Validate checks the manifest for malformed entries: unsafe paths, negative
// sizes, per-type field rules, duplicate paths, and a non-positive chunk size.
func (m *Manifest) Validate() error {
	if m.ChunkSize <= 0 {
		return fmt.Errorf("chunk size must be positive, got %d", m.ChunkSize)
	}
	seen := make(map[string]struct{}, len(m.Entries))
	for _, e := range m.Entries {
		if err := validateEntry(e); err != nil {
			return err
		}
		if _, dup := seen[e.Path]; dup {
			return fmt.Errorf("duplicate manifest path %q", e.Path)
		}
		seen[e.Path] = struct{}{}
	}
	return nil
}

func validateEntry(e Entry) error {
	if err := validatePath(e.Path); err != nil {
		return err
	}
	switch e.Type {
	case TypeFile:
		if e.Size < 0 {
			return fmt.Errorf("file %q has negative size %d", e.Path, e.Size)
		}
		if len(e.SHA256) != hex.EncodedLen(sha256.Size) {
			return fmt.Errorf("file %q has malformed sha256 %q", e.Path, e.SHA256)
		}
		if e.LinkTarget != "" {
			return fmt.Errorf("file %q must not carry a link target", e.Path)
		}
	case TypeDir:
		if e.Size != 0 || e.SHA256 != "" || e.LinkTarget != "" {
			return fmt.Errorf("directory %q must carry only metadata", e.Path)
		}
	case TypeSymlink:
		if e.LinkTarget == "" {
			return fmt.Errorf("symlink %q has no link target", e.Path)
		}
		if e.Size != 0 || e.SHA256 != "" {
			return fmt.Errorf("symlink %q must not carry content fields", e.Path)
		}
	default:
		return fmt.Errorf("entry %q has unknown type %q", e.Path, e.Type)
	}
	return nil
}

func validatePath(p string) error {
	if p == "" {
		return fmt.Errorf("empty manifest path")
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("absolute manifest path %q", p)
	}
	if p != path.Clean(p) {
		return fmt.Errorf("manifest path %q is not clean", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || seg == "." {
			return fmt.Errorf("manifest path %q escapes the transfer root", p)
		}
	}
	return nil
}

// BuildDir walks root and returns a manifest of every regular file, directory,
// and symlink beneath it, sorted by path. Symlinks are recorded with their
// target and never followed; other non-regular entries are skipped.
func BuildDir(root string, chunkSize int64) (*Manifest, error) {
	m := &Manifest{ChunkSize: chunkSize}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("relativising %q: %w", p, err)
		}
		if rel == "." {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		switch {
		case d.IsDir():
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("stating directory %s: %w", relSlash, err)
			}
			m.Entries = append(m.Entries, entryFromInfo(relSlash, TypeDir, info))
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return fmt.Errorf("reading symlink %s: %w", relSlash, err)
			}
			// DirEntry.Info reports the link itself, not its target.
			info, err := d.Info()
			if err != nil {
				return fmt.Errorf("stating symlink %s: %w", relSlash, err)
			}
			e := entryFromInfo(relSlash, TypeSymlink, info)
			e.LinkTarget = target
			m.Entries = append(m.Entries, e)
		case d.Type().IsRegular():
			e, err := statAndHash(p, relSlash)
			if err != nil {
				return err
			}
			m.Entries = append(m.Entries, e)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", root, err)
	}
	sort.Slice(m.Entries, func(i, j int) bool { return m.Entries[i].Path < m.Entries[j].Path })
	return m, nil
}

// BuildFile returns a single-entry manifest for one file. The manifest path is
// the file's base name.
func BuildFile(p string, chunkSize int64) (*Manifest, error) {
	e, err := statAndHash(p, filepath.Base(p))
	if err != nil {
		return nil, err
	}
	return &Manifest{ChunkSize: chunkSize, Entries: []Entry{e}}, nil
}

func entryFromInfo(relPath, entryType string, info fs.FileInfo) Entry {
	e := Entry{
		Path:          relPath,
		Type:          entryType,
		Mode:          uint32(info.Mode().Perm()),
		MtimeUnixNano: info.ModTime().UnixNano(),
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		e.UID = int(st.Uid)
		e.GID = int(st.Gid)
	}
	return e
}

func statAndHash(p, relPath string) (Entry, error) {
	fh, err := os.Open(p)
	if err != nil {
		return Entry{}, fmt.Errorf("opening %s: %w", p, err)
	}
	defer func() { _ = fh.Close() }()
	info, err := fh.Stat()
	if err != nil {
		return Entry{}, fmt.Errorf("stating %s: %w", p, err)
	}
	if !info.Mode().IsRegular() {
		return Entry{}, fmt.Errorf("%s is not a regular file", p)
	}
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		return Entry{}, fmt.Errorf("hashing %s: %w", p, err)
	}
	e := entryFromInfo(relPath, TypeFile, info)
	e.Size = info.Size()
	e.SHA256 = hex.EncodeToString(h.Sum(nil))
	return e, nil
}

// Encode returns the canonical JSON encoding of the manifest. Digest is
// computed over exactly these bytes on both sides.
func Encode(m *Manifest) ([]byte, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("encoding manifest: %w", err)
	}
	return raw, nil
}

// Decode parses manifest JSON produced by Encode and validates it.
func Decode(raw []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decoding manifest: %w", err)
	}
	if err := m.Validate(); err != nil {
		return nil, fmt.Errorf("invalid manifest: %w", err)
	}
	return &m, nil
}

// Digest returns the hex SHA-256 of the encoded manifest bytes.
func Digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Compress zstd-compresses encoded manifest bytes for the wire.
func Compress(raw []byte) ([]byte, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, fmt.Errorf("creating zstd encoder: %w", err)
	}
	defer func() { _ = enc.Close() }()
	return enc.EncodeAll(raw, nil), nil
}

// Decompress streams a zstd-compressed manifest from r, refusing to expand
// beyond maxBytes.
func Decompress(r io.Reader, maxBytes int64) ([]byte, error) {
	dec, err := zstd.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("creating zstd decoder: %w", err)
	}
	defer dec.Close()
	raw, err := io.ReadAll(io.LimitReader(dec, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("decompressing manifest: %w", err)
	}
	if int64(len(raw)) > maxBytes {
		return nil, fmt.Errorf("manifest exceeds %d bytes decompressed", maxBytes)
	}
	return raw, nil
}
