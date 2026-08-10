package manifest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeFile(t *testing.T, root, rel string, content []byte, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, content, mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildDirDeterminism(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "b/two.txt", []byte("second"), 0o644)
	writeFile(t, root, "a/one.txt", []byte("first"), 0o600)
	writeFile(t, root, "empty.bin", nil, 0o644)

	m1, err := BuildDir(root, DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := BuildDir(root, DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	raw1, err := Encode(m1)
	if err != nil {
		t.Fatal(err)
	}
	raw2, err := Encode(m2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw1, raw2) {
		t.Error("two builds of the same tree produced different encodings")
	}
	if Digest(raw1) != Digest(raw2) {
		t.Error("digests differ between identical builds")
	}
	wantOrder := []struct {
		path      string
		entryType string
	}{
		{"a", TypeDir},
		{"a/one.txt", TypeFile},
		{"b", TypeDir},
		{"b/two.txt", TypeFile},
		{"empty.bin", TypeFile},
	}
	if len(m1.Entries) != len(wantOrder) {
		t.Fatalf("got %d entries, want %d: %+v", len(m1.Entries), len(wantOrder), m1.Entries)
	}
	for i, want := range wantOrder {
		if m1.Entries[i].Path != want.path || m1.Entries[i].Type != want.entryType {
			t.Errorf("entry %d: got %s %q, want %s %q",
				i, m1.Entries[i].Type, m1.Entries[i].Path, want.entryType, want.path)
		}
	}
}

func TestBuildDirMetadata(t *testing.T) {
	root := t.TempDir()
	p := writeFile(t, root, "script.sh", []byte("#!/bin/sh\n"), 0o755)
	mtime := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}

	m, err := BuildDir(root, DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	f := m.Entries[0]
	if f.Type != TypeFile {
		t.Errorf("got type %q, want file", f.Type)
	}
	if f.Mode != 0o755 {
		t.Errorf("got mode %o, want 755", f.Mode)
	}
	if f.MtimeUnixNano != mtime.UnixNano() {
		t.Errorf("got mtime %d, want %d", f.MtimeUnixNano, mtime.UnixNano())
	}
	if f.Size != 10 {
		t.Errorf("got size %d, want 10", f.Size)
	}
	if f.UID != os.Getuid() || f.GID != os.Getgid() {
		t.Errorf("got uid/gid %d/%d, want %d/%d", f.UID, f.GID, os.Getuid(), os.Getgid())
	}
	// sha256 of "#!/bin/sh\n"
	if f.SHA256 != "a8076d3d28d21e02012b20eaf7dbf75409a6277134439025f282e368e3305abf" {
		t.Errorf("unexpected sha256 %s", f.SHA256)
	}
}

func TestBuildDirRecordsSymlinks(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "real.txt", []byte("data"), 0o644)
	if err := os.Symlink("real.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hosts", filepath.Join(root, "outside.txt")); err != nil {
		t.Fatal(err)
	}
	m, err := BuildDir(root, DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(m.Entries), m.Entries)
	}
	link := m.Entries[0]
	if link.Path != "link.txt" || link.Type != TypeSymlink || link.LinkTarget != "real.txt" {
		t.Errorf("relative link recorded wrong: %+v", link)
	}
	if link.SHA256 != "" || link.Size != 0 {
		t.Errorf("symlink must not carry content fields: %+v", link)
	}
	outside := m.Entries[1]
	if outside.Path != "outside.txt" || outside.Type != TypeSymlink || outside.LinkTarget != "/etc/hosts" {
		t.Errorf("absolute link target must be recorded verbatim: %+v", outside)
	}
}

func TestBuildDirRecordsEmptyDirs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "buckets", "invoices"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(root, "buckets", "invoices"), 0o500); err != nil { //nolint:gosec // G302: directories need the traversal bit
		t.Fatal(err)
	}
	m, err := BuildDir(root, DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(m.Entries), m.Entries)
	}
	if m.Entries[0].Path != "buckets" || m.Entries[0].Type != TypeDir {
		t.Errorf("parent dir not recorded: %+v", m.Entries[0])
	}
	leaf := m.Entries[1]
	if leaf.Path != "buckets/invoices" || leaf.Type != TypeDir {
		t.Fatalf("empty dir not recorded: %+v", leaf)
	}
	if leaf.Mode != 0o500 {
		t.Errorf("empty dir mode = %o, want 500", leaf.Mode)
	}
}

func TestBuildFile(t *testing.T) {
	root := t.TempDir()
	p := writeFile(t, root, "dump.sql.zst", []byte("compressed dump"), 0o600)
	m, err := BuildFile(p, DefaultChunkSize)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(m.Entries))
	}
	if m.Entries[0].Path != "dump.sql.zst" {
		t.Errorf("got path %q, want base name", m.Entries[0].Path)
	}
	if m.Entries[0].Type != TypeFile {
		t.Errorf("got type %q, want file", m.Entries[0].Type)
	}
	if m.Entries[0].Size != 15 {
		t.Errorf("got size %d, want 15", m.Entries[0].Size)
	}
}

func TestValidate(t *testing.T) {
	goodSHA := "40b642cdc4c2a5623c78ae2c6e3fd8f0339102a2b0adb5992adafa4c07b431ff"
	file := func(path string) Entry {
		return Entry{Path: path, Type: TypeFile, Size: 1, SHA256: goodSHA}
	}
	dir := func(path string) Entry {
		return Entry{Path: path, Type: TypeDir, Mode: 0o755}
	}
	link := func(path, target string) Entry {
		return Entry{Path: path, Type: TypeSymlink, LinkTarget: target}
	}
	tests := []struct {
		name    string
		m       Manifest
		wantErr bool
	}{
		{"valid mixed", Manifest{ChunkSize: 1024, Entries: []Entry{dir("a"), file("a/b.txt"), link("a/l", "b.txt")}}, false},
		{"symlink with absolute target", Manifest{ChunkSize: 1024, Entries: []Entry{link("l", "/var/data")}}, false},
		{"empty entry list", Manifest{ChunkSize: 1024}, false},
		{"zero chunk size", Manifest{ChunkSize: 0}, true},
		{"negative chunk size", Manifest{ChunkSize: -1}, true},
		{"absolute path", Manifest{ChunkSize: 1024, Entries: []Entry{file("/etc/passwd")}}, true},
		{"parent escape", Manifest{ChunkSize: 1024, Entries: []Entry{file("../../etc/passwd")}}, true},
		{"inner parent escape", Manifest{ChunkSize: 1024, Entries: []Entry{file("a/../../b")}}, true},
		{"unclean path", Manifest{ChunkSize: 1024, Entries: []Entry{file("a//b.txt")}}, true},
		{"dot segment", Manifest{ChunkSize: 1024, Entries: []Entry{file("./a.txt")}}, true},
		{"empty path", Manifest{ChunkSize: 1024, Entries: []Entry{file("")}}, true},
		{"negative size", Manifest{ChunkSize: 1024, Entries: []Entry{{Path: "a", Type: TypeFile, Size: -1, SHA256: goodSHA}}}, true},
		{"malformed sha256", Manifest{ChunkSize: 1024, Entries: []Entry{{Path: "a", Type: TypeFile, Size: 1, SHA256: "abc"}}}, true},
		{"duplicate path", Manifest{ChunkSize: 1024, Entries: []Entry{file("a.txt"), file("a.txt")}}, true},
		{"unknown type", Manifest{ChunkSize: 1024, Entries: []Entry{{Path: "a", Type: "device"}}}, true},
		{"missing type", Manifest{ChunkSize: 1024, Entries: []Entry{{Path: "a", Size: 1, SHA256: goodSHA}}}, true},
		{"dir with sha", Manifest{ChunkSize: 1024, Entries: []Entry{{Path: "a", Type: TypeDir, SHA256: goodSHA}}}, true},
		{"dir with size", Manifest{ChunkSize: 1024, Entries: []Entry{{Path: "a", Type: TypeDir, Size: 4}}}, true},
		{"dir with link target", Manifest{ChunkSize: 1024, Entries: []Entry{{Path: "a", Type: TypeDir, LinkTarget: "b"}}}, true},
		{"symlink without target", Manifest{ChunkSize: 1024, Entries: []Entry{{Path: "a", Type: TypeSymlink}}}, true},
		{"symlink with size", Manifest{ChunkSize: 1024, Entries: []Entry{{Path: "a", Type: TypeSymlink, LinkTarget: "b", Size: 2}}}, true},
		{"symlink with sha", Manifest{ChunkSize: 1024, Entries: []Entry{{Path: "a", Type: TypeSymlink, LinkTarget: "b", SHA256: goodSHA}}}, true},
		{"file with link target", Manifest{ChunkSize: 1024, Entries: []Entry{{Path: "a", Type: TypeFile, Size: 1, SHA256: goodSHA, LinkTarget: "b"}}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.m.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestEncodeDecodeCompressRoundTrip(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "a.txt", []byte("hello"), 0o644)
	if err := os.Symlink("a.txt", filepath.Join(root, "b.lnk")); err != nil {
		t.Fatal(err)
	}
	m, err := BuildDir(root, 1024)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := Encode(m)
	if err != nil {
		t.Fatal(err)
	}
	compressed, err := Compress(raw)
	if err != nil {
		t.Fatal(err)
	}
	back, err := Decompress(bytes.NewReader(compressed), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, raw) {
		t.Error("compress/decompress round trip changed bytes")
	}
	m2, err := Decode(back)
	if err != nil {
		t.Fatal(err)
	}
	if len(m2.Entries) != 2 || m2.Entries[0].SHA256 != m.Entries[0].SHA256 {
		t.Error("decode round trip lost data")
	}
	if m2.Entries[1].LinkTarget != "a.txt" {
		t.Error("decode round trip lost the link target")
	}
}

func TestDecompressLimit(t *testing.T) {
	raw := bytes.Repeat([]byte("x"), 2048)
	compressed, err := Compress(raw)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Decompress(bytes.NewReader(compressed), 1024); err == nil {
		t.Error("expected error when decompressed size exceeds the limit")
	}
}

func TestDecodeRejectsInvalid(t *testing.T) {
	if _, err := Decode([]byte("{not json")); err == nil {
		t.Error("expected error for malformed json")
	}
	if _, err := Decode([]byte(`{"chunkSizeBytes":0,"entries":[]}`)); err == nil {
		t.Error("expected validation error for zero chunk size")
	}
}
