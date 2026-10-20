package mirror

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type s3Store struct {
	client *minio.Client
}

// NewStore returns a Store that talks to the remote's endpoint over HTTPS
// with the remote's key and region. It validates client configuration without
// contacting the endpoint and returns an error if that configuration is invalid.
func NewStore(r Remote) (Store, error) {
	return newStore(r, true)
}

func newStore(r Remote, secure bool) (*s3Store, error) {
	client, err := minio.New(r.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(r.AccessKeyID, r.SecretAccessKey, ""),
		Secure: secure,
		Region: r.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", r.Endpoint, err)
	}
	return &s3Store{client: client}, nil
}

// classify wraps an AccessDenied error in ErrDenied and a recognized lock
// refusal in ErrLocked. Both require an S3 request ID to distinguish S3
// responses from generic proxy or firewall refusals. Other errors, including
// 403 codes such as SignatureDoesNotMatch, are returned unchanged.
func classify(err error) error {
	if err == nil {
		return nil
	}
	resp := minio.ToErrorResponse(err)
	if resp.RequestID == "" {
		return err
	}
	switch {
	case resp.StatusCode == http.StatusForbidden && resp.Code == "AccessDenied":
		return fmt.Errorf("%w: %s", ErrDenied, resp.Message)
	case resp.StatusCode >= 400 && resp.StatusCode < 500 && (resp.Code == "ObjectLocked" ||
		resp.Code == "InvalidRequest" && strings.Contains(strings.ToLower(resp.Message), "worm protected")):
		return fmt.Errorf("%w: %s: %s", ErrLocked, resp.Code, resp.Message)
	}
	return err
}

func (s *s3Store) BucketVersioning(ctx context.Context, bucket string) (string, error) {
	cfg, err := s.client.GetBucketVersioning(ctx, bucket)
	if err != nil {
		return "", classify(err)
	}
	return cfg.Status, nil
}

func (s *s3Store) BucketLock(ctx context.Context, bucket string) (Lock, error) {
	enabled, mode, validity, unit, err := s.client.GetObjectLockConfig(ctx, bucket)
	if err != nil {
		if minio.ToErrorResponse(err).Code == "ObjectLockConfigurationNotFoundError" {
			return Lock{}, nil
		}
		return Lock{}, classify(err)
	}
	lock := Lock{Enabled: enabled == "Enabled"}
	if mode != nil {
		lock.Mode = string(*mode)
	}
	if validity != nil {
		if unit != nil && *unit == minio.Years {
			lock.Years = int(*validity)
		} else {
			lock.Days = int(*validity)
		}
	}
	return lock, nil
}

func (s *s3Store) Put(ctx context.Context, bucket, key string) (string, error) {
	// Content-MD5 satisfies the checksum requirement for locked uploads.
	// Omitting lock headers tests whether the bucket's default retention applies.
	info, err := s.client.PutObject(ctx, bucket, key, bytes.NewReader([]byte{'k'}), 1,
		minio.PutObjectOptions{ContentType: "application/octet-stream", SendContentMd5: true})
	if err != nil {
		return "", classify(err)
	}
	return info.VersionID, nil
}

func (s *s3Store) Retention(ctx context.Context, bucket, key, versionID string) (string, time.Time, error) {
	info, err := s.client.StatObject(ctx, bucket, key, minio.StatObjectOptions{VersionID: versionID})
	if err != nil {
		return "", time.Time{}, classify(err)
	}
	mode := info.Metadata.Get("X-Amz-Object-Lock-Mode")
	raw := info.Metadata.Get("X-Amz-Object-Lock-Retain-Until-Date")
	if raw == "" {
		return mode, time.Time{}, nil
	}
	until, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("unreadable retain-until date %q: %w", raw, err)
	}
	return mode, until, nil
}

func (s *s3Store) Delete(ctx context.Context, bucket, key string) error {
	return classify(s.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{}))
}

func (s *s3Store) DeleteVersion(ctx context.Context, bucket, key, versionID string) error {
	return classify(s.client.RemoveObject(ctx, bucket, key, minio.RemoveObjectOptions{VersionID: versionID}))
}
