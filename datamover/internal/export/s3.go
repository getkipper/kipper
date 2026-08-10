package export

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"sort"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/getkipper/kipper/datamover/internal/manifest"
)

// objectInfo is the listing metadata needed to build a manifest entry.
type objectInfo struct {
	Key           string
	Size          int64
	MtimeUnixNano int64
}

// objectLister lists and reads objects; satisfied by S3Source and by test
// fakes. Manifest construction runs against this seam.
type objectLister interface {
	Source
	list(ctx context.Context) ([]objectInfo, error)
}

// s3ObjectMode is the permission recorded for objects, so a later
// volume-import of an S3 export produces readable files.
const s3ObjectMode = 0o644

// buildObjectManifest lists all objects, hashes each with ranged reads, and
// returns a manifest sorted by key.
func buildObjectManifest(ctx context.Context, l objectLister, chunkSize int64) (*manifest.Manifest, error) {
	objects, err := l.list(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing objects: %w", err)
	}
	sort.Slice(objects, func(i, j int) bool { return objects[i].Key < objects[j].Key })
	m := &manifest.Manifest{ChunkSize: chunkSize}
	for _, o := range objects {
		sum, err := hashObject(ctx, l, o)
		if err != nil {
			return nil, err
		}
		m.Entries = append(m.Entries, manifest.Entry{
			Path:          o.Key,
			Type:          manifest.TypeFile,
			Size:          o.Size,
			Mode:          s3ObjectMode,
			MtimeUnixNano: o.MtimeUnixNano,
			SHA256:        sum,
		})
	}
	return m, nil
}

func hashObject(ctx context.Context, src Source, o objectInfo) (string, error) {
	h := sha256.New()
	if o.Size > 0 {
		r, err := src.OpenRange(ctx, o.Key, 0, o.Size)
		if err != nil {
			return "", fmt.Errorf("opening object %s: %w", o.Key, err)
		}
		defer func() { _ = r.Close() }()
		n, err := io.Copy(h, r)
		if err != nil {
			return "", fmt.Errorf("hashing object %s: %w", o.Key, err)
		}
		if n != o.Size {
			return "", fmt.Errorf("short read on object %s: got %d of %d bytes", o.Key, n, o.Size)
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// S3Source reads a bucket through the S3 API. It implements Source with
// ranged GetObject requests.
type S3Source struct {
	client *minio.Client
	bucket string
}

// NewS3Source connects to an S3-compatible endpoint URL (scheme decides TLS).
func NewS3Source(endpoint, bucket, accessKey, secretKey string) (*S3Source, error) {
	client, err := newMinioClient(endpoint, accessKey, secretKey)
	if err != nil {
		return nil, err
	}
	return &S3Source{client: client, bucket: bucket}, nil
}

func newMinioClient(endpoint, accessKey, secretKey string) (*minio.Client, error) {
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
	return client, nil
}

// BuildManifest lists the bucket and hashes every object.
func (s *S3Source) BuildManifest(ctx context.Context, chunkSize int64) (*manifest.Manifest, error) {
	return buildObjectManifest(ctx, s, chunkSize)
}

func (s *S3Source) list(ctx context.Context) ([]objectInfo, error) {
	var objects []objectInfo
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Recursive: true}) {
		if obj.Err != nil {
			return nil, fmt.Errorf("listing bucket %s: %w", s.bucket, obj.Err)
		}
		objects = append(objects, objectInfo{
			Key:           obj.Key,
			Size:          obj.Size,
			MtimeUnixNano: obj.LastModified.UnixNano(),
		})
	}
	return objects, nil
}

// OpenRange implements Source with a ranged GetObject.
func (s *S3Source) OpenRange(ctx context.Context, path string, offset, length int64) (io.ReadCloser, error) {
	opts := minio.GetObjectOptions{}
	if err := opts.SetRange(offset, offset+length-1); err != nil {
		return nil, fmt.Errorf("setting range for object %s: %w", path, err)
	}
	obj, err := s.client.GetObject(ctx, s.bucket, path, opts)
	if err != nil {
		return nil, fmt.Errorf("getting object %s: %w", path, err)
	}
	return obj, nil
}
