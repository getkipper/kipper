package mirror

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// CanaryKey names the preflight probe. CanaryFilter excludes its reserved
// prefix from sync and check, including sync deletions.
const (
	CanaryKey    = ".kipper-mirror/canary"
	CanaryFilter = "/.kipper-mirror/**"
)

// ErrDenied marks a request the store refused with the AccessDenied error
// code. ErrLocked marks a refusal another store gives for deleting a locked
// version, such as MinIO's "Object is WORM protected".
var (
	ErrDenied = errors.New("access denied")
	ErrLocked = errors.New("object locked")
)

// Lock is a bucket's Object Lock configuration. The default retention is
// given in Days or in Years, and both are zero without a default retention.
type Lock struct {
	Enabled bool
	Mode    string
	Days    int
	Years   int
}

// daysFrom returns the default retention in days for a version written at
// now, counting a retention in years by the calendar.
func (l Lock) daysFrom(now time.Time) int {
	if l.Years == 0 {
		return l.Days
	}
	now = now.UTC()
	return int(now.AddDate(l.Years, 0, 0).Sub(now).Hours() / 24)
}

// Store provides the S3 operations used by Preflight. Implementations wrap
// access and lock refusals in ErrDenied or ErrLocked and preserve other errors.
type Store interface {
	// BucketVersioning returns the S3 status, or an empty string if unset.
	BucketVersioning(ctx context.Context, bucket string) (string, error)
	// BucketLock returns a zero Lock when Object Lock is not configured.
	BucketLock(ctx context.Context, bucket string) (Lock, error)
	// Put writes a one-byte object without lock headers and returns its version ID.
	Put(ctx context.Context, bucket, key string) (string, error)
	// Retention reads the version's lock mode and retain-until date using HEAD.
	// A missing date returns a zero time, and a malformed date returns an error.
	Retention(ctx context.Context, bucket, key, versionID string) (string, time.Time, error)
	// Delete deletes the key without a version ID, which leaves a delete
	// marker in a versioned bucket.
	Delete(ctx context.Context, bucket, key string) error
	// DeleteVersion permanently deletes the specified version if permitted.
	DeleteVersion(ctx context.Context, bucket, key, versionID string) error
}

// Check is the outcome of one preflight step.
type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

// Result lists the checks that ran, in order. A failed check is always the
// last one, because the preflight stops there. LockDays is the target's
// default retention in days once the lock check passed, one day more for a
// retention in years, so a run that crosses a leap day locks no upload for
// less than the bucket would.
type Result struct {
	Checks   []Check `json:"checks"`
	LockDays int     `json:"-"`
}

// Passed reports whether every check ran and passed.
func (r Result) Passed() bool {
	return len(r.Checks) == len(checkNames) && r.Failure() == nil
}

// Failure returns the first failed check, or nil if none failed.
func (r Result) Failure() *Check {
	for i := range r.Checks {
		if !r.Checks[i].Passed {
			return &r.Checks[i]
		}
	}
	return nil
}

var checkNames = []string{
	"endpoints differ", "source versioning", "target object lock", "canary write",
	"canary retention", "canary delete refused", "source write refused",
}

// The canary's retain-until date may sit a little before the expected date
// through clock skew, and later through providers that round it up to a day.
const (
	retainUntilEarlySlack = time.Hour
	retainUntilLateSlack  = 48 * time.Hour
)

// Preflight checks endpoint separation, versioning, target retention and
// source permissions, stopping at the first failure. It writes a one-byte
// target canary and probes deletion of that version. Source write and delete
// probes use only CanaryKey, so permissions on other keys are not tested.
// If the source accepts the write, cleanup by version is best effort.
// now is the run start time used to check retention.
func Preflight(ctx context.Context, spec Spec, source, target Store, now time.Time) Result {
	var res Result
	pass := func(name, detail string) {
		res.Checks = append(res.Checks, Check{Name: name, Passed: true, Detail: detail})
	}
	fail := func(name, format string, args ...any) Result {
		res.Checks = append(res.Checks, Check{Name: name, Detail: fmt.Sprintf(format, args...)})
		return res
	}
	src, dst := spec.Source, spec.Target

	if src.Endpoint == dst.Endpoint {
		return fail(checkNames[0], "the source and target both use %s. Put the mirror at a different endpoint, such as another location", src.Endpoint)
	}
	pass(checkNames[0], src.Endpoint+" and "+dst.Endpoint)

	status, err := source.BucketVersioning(ctx, src.Bucket)
	if err != nil {
		return fail(checkNames[1], "reading versioning on %s: %v", src.URL(), err)
	}
	if status != "Enabled" {
		return fail(checkNames[1], "versioning on %s is %q. Enable it so the source keeps deleted and overwritten objects", src.URL(), statusOrOff(status))
	}
	pass(checkNames[1], "enabled on "+src.URL())

	lock, err := target.BucketLock(ctx, dst.Bucket)
	if err != nil {
		return fail(checkNames[2], "reading the Object Lock configuration of %s: %v", dst.URL(), err)
	}
	lockDays := lock.daysFrom(now)
	switch {
	case !lock.Enabled:
		return fail(checkNames[2], "%s has no Object Lock. Create the mirror bucket with Object Lock enabled", dst.URL())
	case lock.Mode != "COMPLIANCE":
		return fail(checkNames[2], "the default retention of %s is %q. It must be COMPLIANCE", dst.URL(), statusOrOff(lock.Mode))
	case lockDays < spec.RetentionDays:
		return fail(checkNames[2], "the default retention of %s is %d days. It must be at least %d", dst.URL(), lockDays, spec.RetentionDays)
	}
	pass(checkNames[2], fmt.Sprintf("COMPLIANCE, %d days", lockDays))
	res.LockDays = lockDays
	if lock.Years > 0 {
		res.LockDays++
	}

	version, err := target.Put(ctx, dst.Bucket, CanaryKey)
	if err != nil {
		return fail(checkNames[3], "writing %s to %s: %v", CanaryKey, dst.URL(), err)
	}
	if version == "" {
		return fail(checkNames[3], "%s returned no version ID for %s, so it is not versioned", dst.URL(), CanaryKey)
	}
	pass(checkNames[3], "version "+version)

	mode, until, err := target.Retention(ctx, dst.Bucket, CanaryKey, version)
	if err != nil {
		return fail(checkNames[4], "reading the lock of %s: %v", CanaryKey, err)
	}
	earliest := now.Add(time.Duration(spec.RetentionDays)*24*time.Hour - retainUntilEarlySlack)
	latest := now.Add(time.Duration(lockDays)*24*time.Hour + retainUntilLateSlack)
	switch {
	case mode != "COMPLIANCE":
		return fail(checkNames[4], "the canary's lock mode is %q. The default retention did not apply to a plain write", statusOrOff(mode))
	case until.Before(earliest) || until.After(latest):
		return fail(checkNames[4], "the canary is locked until %s. Expected about %d days from now", until.UTC().Format(time.RFC3339), lockDays)
	}
	pass(checkNames[4], "locked until "+until.UTC().Format(time.RFC3339))

	err = target.DeleteVersion(ctx, dst.Bucket, CanaryKey, version)
	switch {
	case err == nil:
		return fail(checkNames[5], "deleting the canary version succeeded, so versions in %s are not protected", dst.URL())
	case !errors.Is(err, ErrDenied) && !errors.Is(err, ErrLocked):
		return fail(checkNames[5], "deleting the canary version failed with %v instead of a lock refusal", err)
	}
	if _, _, headErr := target.Retention(ctx, dst.Bucket, CanaryKey, version); headErr != nil {
		return fail(checkNames[5], "the delete was refused with %v, but the canary version cannot be read afterwards: %v", err, headErr)
	}
	pass(checkNames[5], err.Error())

	srcVersion, err := source.Put(ctx, src.Bucket, CanaryKey)
	if err == nil {
		_ = source.DeleteVersion(ctx, src.Bucket, CanaryKey, srcVersion)
	}
	if detail, ok := sourceRefused(src, "write to", err); !ok {
		return fail(checkNames[6], "%s", detail)
	}
	// Restricting probes to the canary key protects user data outside the
	// reserved prefix even if deletion is allowed. "null" probes deletion
	// of a version created before versioning was enabled.
	if detail, ok := sourceRefused(src, "delete from", source.Delete(ctx, src.Bucket, CanaryKey)); !ok {
		return fail(checkNames[6], "%s", detail)
	}
	if detail, ok := sourceRefused(src, "delete versions in", source.DeleteVersion(ctx, src.Bucket, CanaryKey, "null")); !ok {
		return fail(checkNames[6], "%s", detail)
	}
	pass(checkNames[6], "write, delete and version delete of "+CanaryKey+" denied")
	return res
}

// sourceRefused reports whether a probe against the source was denied, and
// otherwise says what went wrong.
func sourceRefused(src Remote, action string, err error) (string, bool) {
	switch {
	case err == nil:
		return fmt.Sprintf("the source key can %s %s. Give the mirror read-only access to the source", action, src.URL()), false
	case !errors.Is(err, ErrDenied):
		return fmt.Sprintf("trying to %s %s failed with %v instead of access denied", action, src.URL(), err), false
	}
	return "", true
}

func statusOrOff(s string) string {
	if s == "" {
		return "off"
	}
	return s
}
