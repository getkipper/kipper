package ingest

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/getkipper/kipper/datamover/internal/manifest"

	"github.com/getkipper/kipper/datamover/internal/wire"
)

// ObjectStore is the write-side S3 abstraction the object committer uses.
// The minio adapter implements it against a real endpoint; tests use an
// in-memory fake.
type ObjectStore interface {
	// EnsureBucket creates the target bucket when it does not exist.
	EnsureBucket(ctx context.Context) error
	// Put stores an object of a known size.
	Put(ctx context.Context, key string, r io.Reader, size int64) error
	// List returns all object keys in the bucket.
	List(ctx context.Context) ([]string, error)
	// Remove deletes one object.
	Remove(ctx context.Context, key string) error
}

// ObjectCommitter finalizes a transfer into an object store: verify each
// assembled object file with a full re-read, upload it, then remove bucket
// objects absent from the manifest.
type ObjectCommitter struct {
	// Store is the target bucket.
	Store ObjectStore
}

// Commit implements Committer. Each object is assembled from the flat staged
// chunks into one scratch file under the state dir, verified with a full
// re-read, then uploaded.
func (c ObjectCommitter) Commit(ctx context.Context, st *State) (*wire.Report, error) {
	if err := c.Store.EnsureBucket(ctx); err != nil {
		return nil, fmt.Errorf("ensuring target bucket: %w", err)
	}
	scratch := filepath.Join(st.StateDir(), "assemble.tmp")
	defer func() { _ = os.Remove(scratch) }() //nolint:errcheck // best-effort scratch cleanup
	report := &wire.Report{Files: make([]wire.FileResult, 0, len(st.Manifest.Entries))}
	for i, e := range st.Manifest.Entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res := wire.FileResult{Path: e.Path}
		if e.Type != manifest.TypeFile {
			// Object stores have no directories or symlinks; prefixes are
			// implicit, so non-file entries are complete by construction.
			res.Match = true
			report.Files = append(report.Files, res)
			continue
		}
		if err := assembleTo(st, i, scratch); err != nil {
			res.Error = err.Error()
			report.Files = append(report.Files, res)
			continue
		}
		sum, err := hashFile(scratch)
		switch {
		case err != nil:
			res.Error = err.Error()
		case sum != e.SHA256:
			res.SHA256 = sum
			res.Error = "sha256 mismatch after assembly"
		default:
			res.SHA256 = sum
			res.Match = true
			if err := c.putObject(ctx, scratch, e.Path, e.Size); err != nil {
				res.Error = err.Error()
			}
		}
		report.Files = append(report.Files, res)
	}
	keys, err := c.Store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing target bucket: %w", err)
	}
	keep := make(map[string]struct{}, len(st.Manifest.Entries))
	for _, e := range st.Manifest.Entries {
		if e.Type == manifest.TypeFile {
			keep[e.Path] = struct{}{}
		}
	}
	for _, key := range keys {
		if _, ok := keep[key]; ok {
			continue
		}
		if err := c.Store.Remove(ctx, key); err != nil {
			return nil, fmt.Errorf("deleting object %s: %w", key, err)
		}
		report.Deleted = append(report.Deleted, key)
	}
	return report, nil
}

func (c ObjectCommitter) putObject(ctx context.Context, scratch, key string, size int64) error {
	fh, err := os.Open(scratch)
	if err != nil {
		return fmt.Errorf("opening assembled object: %v", err)
	}
	defer func() { _ = fh.Close() }()
	if err := c.Store.Put(ctx, key, fh, size); err != nil {
		return fmt.Errorf("uploading object: %v", err)
	}
	return nil
}

// MinioStore implements ObjectStore against an S3-compatible endpoint.
type MinioStore struct {
	client *minio.Client
	bucket string
}

// NewMinioStore connects to an S3-compatible endpoint URL (scheme decides TLS).
func NewMinioStore(endpoint, bucket, accessKey, secretKey string) (*MinioStore, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid s3 endpoint %q", endpoint)
	}
	client, err := minio.New(u.Host, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: u.Scheme == "https",
	})
	if err != nil {
		return nil, fmt.Errorf("creating s3 client: %w", err)
	}
	return &MinioStore{client: client, bucket: bucket}, nil
}

// EnsureBucket implements ObjectStore.
func (s *MinioStore) EnsureBucket(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("checking bucket %s: %w", s.bucket, err)
	}
	if exists {
		return nil
	}
	if err := s.client.MakeBucket(ctx, s.bucket, minio.MakeBucketOptions{}); err != nil {
		return fmt.Errorf("creating bucket %s: %w", s.bucket, err)
	}
	return nil
}

// Put implements ObjectStore.
func (s *MinioStore) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	_, err := s.client.PutObject(ctx, s.bucket, key, r, size, minio.PutObjectOptions{})
	return err
}

// List implements ObjectStore.
func (s *MinioStore) List(ctx context.Context) ([]string, error) {
	var keys []string
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		keys = append(keys, obj.Key)
	}
	return keys, nil
}

// Remove implements ObjectStore.
func (s *MinioStore) Remove(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{})
}
