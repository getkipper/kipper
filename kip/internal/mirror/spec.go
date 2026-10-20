// Package mirror copies one S3 bucket into another bucket that is versioned
// and protected by an Object Lock in compliance mode. It holds the checks
// that refuse to mirror into an unprotected bucket, the rclone invocation,
// and the Kubernetes objects that schedule the copy.
package mirror

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7/pkg/s3utils"
	"github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/util/validation"
)

// maxNameLength keeps "backup-mirror-<name>" within the 52 characters
// Kubernetes allows for a CronJob name.
const maxNameLength = 52 - len(ObjectPrefix)

// Remote is one side of a mirror: a bucket at an S3 endpoint and the key
// used to reach it.
type Remote struct {
	Endpoint        string `json:"endpoint"`
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
}

// URL returns the remote in the s3://endpoint/bucket form the CLI accepts.
func (r Remote) URL() string {
	return "s3://" + r.Endpoint + "/" + r.Bucket
}

// Spec holds the mirror job's configuration and credentials, so it is stored
// in a Secret. MaxDelete limits target deletions per run, and a negative
// value disables that limit.
type Spec struct {
	Name          string `json:"name"`
	Source        Remote `json:"source"`
	Target        Remote `json:"target"`
	RetentionDays int    `json:"retentionDays"`
	MaxDelete     int    `json:"maxDelete"`
}

// Validate checks the name, a positive retention period and required remote
// fields, returning the first error. It does not validate endpoint or bucket
// syntax, credential validity, or MaxDelete.
func (s Spec) Validate() error {
	if err := ValidateName(s.Name); err != nil {
		return err
	}
	if s.RetentionDays < 1 {
		return errors.New("the retention must be at least one day")
	}
	for _, side := range []struct {
		label string
		r     Remote
	}{{"source", s.Source}, {"target", s.Target}} {
		switch {
		case side.r.Endpoint == "" || side.r.Bucket == "":
			return fmt.Errorf("the %s needs an endpoint and a bucket", side.label)
		case side.r.Region == "":
			return fmt.Errorf("the %s needs a region", side.label)
		case side.r.AccessKeyID == "" || side.r.SecretAccessKey == "":
			return fmt.Errorf("the %s needs an access key and a secret key", side.label)
		}
	}
	return nil
}

// ValidateName accepts a lowercase DNS label short enough to name the
// mirror's CronJob.
func ValidateName(name string) error {
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return fmt.Errorf("invalid mirror name %q: %s", name, strings.Join(errs, "; "))
	}
	if len(name) > maxNameLength {
		return fmt.Errorf("invalid mirror name %q: use at most %d characters", name, maxNameLength)
	}
	return nil
}

// ParseURL splits s3://endpoint/bucket into a lowercase endpoint, with its
// port unless it is the HTTPS default, and a valid S3 bucket name.
// It rejects key prefixes, credentials, queries and fragments because the
// mirror accepts whole-bucket addresses.
func ParseURL(raw string) (endpoint, bucket string, err error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "s3" {
		return "", "", fmt.Errorf("%q is not an s3://endpoint/bucket URL", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Host == "" {
		return "", "", fmt.Errorf("%q is not an s3://endpoint/bucket URL", raw)
	}
	bucket = strings.TrimSuffix(strings.TrimPrefix(u.Path, "/"), "/")
	if bucket == "" || strings.Contains(bucket, "/") {
		return "", "", fmt.Errorf("%q must name exactly one bucket and no key prefix, for example s3://objects.example.com/my-bucket", raw)
	}
	if err := s3utils.CheckValidBucketNameStrict(bucket); err != nil {
		return "", "", fmt.Errorf("%q: %w", raw, err)
	}
	return strings.TrimSuffix(strings.ToLower(u.Host), ":443"), bucket, nil
}

// ScheduleInterval estimates the longest gap between runs by evaluating a
// five-field cron schedule or descriptor such as @daily over 2001 in UTC.
// It returns an error for invalid schedules or when no gap is found.
// Time zone prefixes are rejected because the CronJob uses the cluster's time zone.
func ScheduleInterval(schedule string) (time.Duration, error) {
	if strings.Contains(schedule, "TZ=") {
		return 0, errors.New("the schedule uses the cluster's time zone, so omit the time zone prefix")
	}
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		return 0, fmt.Errorf("invalid schedule %q: %w", schedule, err)
	}
	start := time.Date(2001, time.January, 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(1, 0, 0)
	prev := sched.Next(start)
	var longest time.Duration
	for prev.Before(end) {
		next := sched.Next(prev)
		if next.IsZero() {
			break
		}
		if gap := next.Sub(prev); gap > longest {
			longest = gap
		}
		prev = next
	}
	if longest == 0 {
		return 0, fmt.Errorf("the schedule %q does not repeat within a year", schedule)
	}
	return longest, nil
}
