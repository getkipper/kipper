package mirror

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testNow = time.Date(2026, time.October, 20, 3, 0, 0, 0, time.UTC)

// fakeStore supplies configurable S3 responses. readOnlySource refuses
// writes, and lockedTarget supplies a correctly locked target.
type fakeStore struct {
	versioning      string
	versioningErr   error
	lock            Lock
	lockErr         error
	putErr          error
	putVersion      string
	retentionMode   string
	retentionUntil  time.Time
	retentionErr    error
	deleteErr       error
	markerErr       error
	goneAfterDelete bool

	puts    []string
	deletes []string
}

func (f *fakeStore) BucketVersioning(context.Context, string) (string, error) {
	return f.versioning, f.versioningErr
}

func (f *fakeStore) BucketLock(context.Context, string) (Lock, error) { return f.lock, f.lockErr }

func (f *fakeStore) Put(_ context.Context, bucket, key string) (string, error) {
	if f.putErr != nil {
		return "", f.putErr
	}
	f.puts = append(f.puts, bucket+"/"+key)
	return f.putVersion, nil
}

func (f *fakeStore) Retention(context.Context, string, string, string) (string, time.Time, error) {
	if f.goneAfterDelete && len(f.deletes) > 0 {
		return "", time.Time{}, errors.New("NoSuchVersion")
	}
	return f.retentionMode, f.retentionUntil, f.retentionErr
}

func (f *fakeStore) Delete(_ context.Context, bucket, key string) error {
	f.deletes = append(f.deletes, bucket+"/"+key)
	return f.markerErr
}

func (f *fakeStore) DeleteVersion(_ context.Context, bucket, key, version string) error {
	f.deletes = append(f.deletes, bucket+"/"+key+"@"+version)
	return f.deleteErr
}

func readOnlySource() *fakeStore {
	denied := fmt.Errorf("%w: Access Denied", ErrDenied)
	return &fakeStore{versioning: "Enabled", putErr: denied, markerErr: denied, deleteErr: denied}
}

func lockedTarget() *fakeStore {
	return &fakeStore{
		lock:           Lock{Enabled: true, Mode: "COMPLIANCE", Days: 30},
		putVersion:     "v1",
		retentionMode:  "COMPLIANCE",
		retentionUntil: testNow.Add(30 * 24 * time.Hour),
		deleteErr:      fmt.Errorf("%w: object is locked", ErrDenied),
	}
}

func runPreflight(spec Spec, source, target Store) Result {
	return Preflight(context.Background(), spec, source, target, testNow)
}

func TestPreflightPassesOnAVersionedSourceAndALockedTarget(t *testing.T) {
	source, target := readOnlySource(), lockedTarget()
	res := runPreflight(validSpec(), source, target)

	require.True(t, res.Passed(), "%+v", res.Checks)
	assert.Nil(t, res.Failure())
	var names []string
	for _, c := range res.Checks {
		names = append(names, c.Name)
	}
	assert.Equal(t, []string{
		"endpoints differ", "source versioning", "target object lock", "canary write",
		"canary retention", "canary delete refused", "source write refused",
	}, names)
	assert.Equal(t, []string{"app-backup/" + CanaryKey}, target.puts)
	assert.Equal(t, []string{"app-backup/" + CanaryKey + "@v1"}, target.deletes)
	assert.Empty(t, source.puts)
	assert.Equal(t, []string{"app/" + CanaryKey, "app/" + CanaryKey + "@null"}, source.deletes,
		"the source probes touch only the canary key")
}

func TestPreflightAcceptsALongerLockThanTheRetention(t *testing.T) {
	target := lockedTarget()
	target.lock.Days = 365
	target.retentionUntil = testNow.Add(365 * 24 * time.Hour)
	res := runPreflight(validSpec(), readOnlySource(), target)
	assert.True(t, res.Passed())
	assert.Equal(t, 365, res.LockDays, "the run locks its uploads for the bucket's own default")
}

func TestLockDaysIgnoreDaylightSavingTime(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	require.NoError(t, err)
	beforeDST := time.Date(2026, time.March, 28, 12, 0, 0, 0, berlin)
	assert.Equal(t, 365, Lock{Years: 1}.daysFrom(beforeDST))
}

func TestPreflightCountsAYearsRetentionInCalendarDays(t *testing.T) {
	target := lockedTarget()
	target.lock = Lock{Enabled: true, Mode: "COMPLIANCE", Years: 10}
	target.retentionUntil = testNow.AddDate(10, 0, 0)
	res := runPreflight(validSpec(), readOnlySource(), target)
	require.True(t, res.Passed(), "%+v", res.Checks)
	assert.Equal(t, 3654, res.LockDays, "ten calendar years from 2026-10-20 hold three leap days, plus one day so no upload is locked for less")

	spec := validSpec()
	spec.RetentionDays = 366
	target.lock.Years, target.retentionUntil = 1, testNow.AddDate(1, 0, 0)
	assert.False(t, runPreflight(spec, readOnlySource(), target).Passed(), "the year from 2026-10-20 has 365 days")
	target.retentionUntil = testNow.AddDate(1, 0, 0)
	spec.RetentionDays = 365
	assert.True(t, runPreflight(spec, readOnlySource(), target).Passed())
}

func TestPreflightFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		spec    func(*Spec)
		source  func(*fakeStore)
		target  func(*fakeStore)
		failing string
	}{
		{name: "same endpoint", spec: func(s *Spec) { s.Target.Endpoint = s.Source.Endpoint }, failing: "endpoints differ"},
		{name: "source unversioned", source: func(f *fakeStore) { f.versioning = "" }, failing: "source versioning"},
		{name: "source versioning suspended", source: func(f *fakeStore) { f.versioning = "Suspended" }, failing: "source versioning"},
		{name: "source versioning unreadable", source: func(f *fakeStore) { f.versioningErr = errors.New("403") }, failing: "source versioning"},
		{name: "target without object lock", target: func(f *fakeStore) { f.lock = Lock{} }, failing: "target object lock"},
		{name: "target lock unreadable", target: func(f *fakeStore) { f.lockErr = errors.New("no lock configuration") }, failing: "target object lock"},
		{name: "governance mode", target: func(f *fakeStore) { f.lock.Mode = "GOVERNANCE" }, failing: "target object lock"},
		{name: "no default retention", target: func(f *fakeStore) { f.lock = Lock{Enabled: true} }, failing: "target object lock"},
		{name: "retention too short", target: func(f *fakeStore) { f.lock.Days = 29 }, failing: "target object lock"},
		{name: "canary write refused", target: func(f *fakeStore) { f.putErr = errors.New("InvalidRequest: Content-MD5 required") }, failing: "canary write"},
		{name: "canary without version", target: func(f *fakeStore) { f.putVersion = "" }, failing: "canary write"},
		{name: "canary not locked", target: func(f *fakeStore) { f.retentionMode = "" }, failing: "canary retention"},
		{name: "canary governance", target: func(f *fakeStore) { f.retentionMode = "GOVERNANCE" }, failing: "canary retention"},
		{name: "canary retention short", target: func(f *fakeStore) { f.retentionUntil = testNow.Add(2 * 24 * time.Hour) }, failing: "canary retention"},
		{name: "canary retention far beyond the bucket default", target: func(f *fakeStore) { f.retentionUntil = testNow.AddDate(10, 0, 0) }, failing: "canary retention"},
		{name: "canary head failed", target: func(f *fakeStore) { f.retentionErr = errors.New("timeout") }, failing: "canary retention"},
		{name: "canary deleted", target: func(f *fakeStore) { f.deleteErr = nil }, failing: "canary delete refused"},
		{name: "canary delete failed otherwise", target: func(f *fakeStore) { f.deleteErr = errors.New("503 SlowDown") }, failing: "canary delete refused"},
		{name: "canary gone despite the refusal", target: func(f *fakeStore) { f.goneAfterDelete = true }, failing: "canary delete refused"},
		{name: "source accepts writes", source: func(f *fakeStore) { f.putErr = nil; f.putVersion = "s1" }, failing: "source write refused"},
		{name: "source write failed otherwise", source: func(f *fakeStore) { f.putErr = errors.New("connection reset") }, failing: "source write refused"},
		{name: "source accepts deletes", source: func(f *fakeStore) { f.markerErr = nil }, failing: "source write refused"},
		{name: "source accepts version deletes", source: func(f *fakeStore) { f.deleteErr = nil }, failing: "source write refused"},
		{name: "source version delete failed otherwise", source: func(f *fakeStore) { f.deleteErr = errors.New("NoSuchVersion") }, failing: "source write refused"},
		{name: "canary delete refused for another reason", target: func(f *fakeStore) { f.deleteErr = errors.New("InvalidRequest: missing header") }, failing: "canary delete refused"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, source, target := validSpec(), readOnlySource(), lockedTarget()
			if tc.spec != nil {
				tc.spec(&spec)
			}
			if tc.source != nil {
				tc.source(source)
			}
			if tc.target != nil {
				tc.target(target)
			}
			res := runPreflight(spec, source, target)
			require.False(t, res.Passed())
			require.NotNil(t, res.Failure())
			assert.Equal(t, tc.failing, res.Failure().Name)
			assert.NotEmpty(t, res.Failure().Detail)
			assert.Equal(t, res.Failure(), &res.Checks[len(res.Checks)-1], "the preflight stops at the first failure")
		})
	}
}

func TestPreflightAcceptsALockRefusalWhileTheVersionStays(t *testing.T) {
	target := lockedTarget()
	target.deleteErr = fmt.Errorf("%w: InvalidRequest: Object is WORM protected and cannot be overwritten", ErrLocked)
	res := runPreflight(validSpec(), readOnlySource(), target)
	require.True(t, res.Passed(), "%+v", res.Checks)
	assert.Contains(t, res.Checks[5].Detail, "WORM protected")
}

func TestPreflightRequiresAccessDeniedFromTheSource(t *testing.T) {
	source := readOnlySource()
	source.putErr = fmt.Errorf("%w: Object is WORM protected", ErrLocked)
	res := runPreflight(validSpec(), source, lockedTarget())
	require.False(t, res.Passed())
	assert.Equal(t, "source write refused", res.Failure().Name, "only an authorization refusal shows the key's rights")
}

func TestPreflightRemovesACanaryTheSourceAccepted(t *testing.T) {
	source := readOnlySource()
	source.putErr, source.putVersion = nil, "s1"
	res := runPreflight(validSpec(), source, lockedTarget())

	require.False(t, res.Passed())
	assert.Equal(t, []string{"app/" + CanaryKey + "@s1"}, source.deletes)
	assert.Contains(t, res.Failure().Detail, "read-only")
}

func TestPreflightAllowsTheSameKeyOnBothSides(t *testing.T) {
	spec := validSpec()
	spec.Target.AccessKeyID, spec.Target.SecretAccessKey = spec.Source.AccessKeyID, spec.Source.SecretAccessKey
	assert.True(t, runPreflight(spec, readOnlySource(), lockedTarget()).Passed(),
		"one mirror key that reads the source through a grant and writes the target is the recommended setup")
}
