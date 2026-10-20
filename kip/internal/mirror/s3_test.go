package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeS3 answers the handful of S3 requests the preflight makes, the way a
// locked, versioned bucket would.
type fakeS3 struct {
	mu       sync.Mutex
	requests []string
	auth     []string

	versioning string
	lockXML    string
	lockStatus int
	putStatus  int
	putCode    string
	delStatus  int
	delCode    string
	delMessage string
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	f := &fakeS3{
		versioning: "Enabled",
		lockXML: `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled>` +
			`<Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Days>30</Days></DefaultRetention></Rule></ObjectLockConfiguration>`,
		lockStatus: http.StatusOK,
		putStatus:  http.StatusOK,
		putCode:    "AccessDenied",
		delStatus:  http.StatusForbidden,
		delCode:    "AccessDenied",
		delMessage: "Access Denied because object protected by object lock.",
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func s3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message><RequestId>TX1</RequestId></Error>`, code, message) //nolint:gosec // the test server answers with fixed strings
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	f.mu.Unlock()
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodGet && q.Has("versioning"):
		_, _ = fmt.Fprintf(w, `<VersioningConfiguration><Status>%s</Status></VersioningConfiguration>`, f.versioning)
	case r.Method == http.MethodGet && q.Has("object-lock"):
		if f.lockStatus != http.StatusOK {
			s3Error(w, f.lockStatus, "ObjectLockConfigurationNotFoundError", "Object Lock configuration does not exist for this bucket")
			return
		}
		_, _ = fmt.Fprint(w, f.lockXML)
	case r.Method == http.MethodPut:
		if r.Header.Get("Content-MD5") == "" {
			s3Error(w, http.StatusBadRequest, "InvalidRequest", "Content-MD5 HTTP header is required for Put Object requests with Object Lock parameters")
			return
		}
		if f.putStatus != http.StatusOK {
			s3Error(w, f.putStatus, f.putCode, "refused")
			return
		}
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		w.Header().Set("x-amz-version-id", "v1")
	case r.Method == http.MethodHead:
		w.Header().Set("ETag", `"d41d8cd98f00b204e9800998ecf8427e"`)
		w.Header().Set("Content-Length", "1")
		w.Header().Set("Last-Modified", testNow.Format(http.TimeFormat))
		w.Header().Set("x-amz-version-id", q.Get("versionId"))
		w.Header().Set("x-amz-object-lock-mode", "COMPLIANCE")
		w.Header().Set("x-amz-object-lock-retain-until-date", "2026-11-19T03:00:00.123Z")
	case r.Method == http.MethodDelete:
		if f.delStatus != http.StatusNoContent {
			s3Error(w, f.delStatus, f.delCode, f.delMessage)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		s3Error(w, http.StatusBadRequest, "NotImplemented", r.Method)
	}
}

func testStore(t *testing.T, srv *httptest.Server) Store {
	t.Helper()
	s, err := newStore(Remote{
		Endpoint: strings.TrimPrefix(srv.URL, "http://"), Bucket: "app-backup", Region: "r2",
		AccessKeyID: "AKEXAMPLE", SecretAccessKey: "secret",
	}, false)
	require.NoError(t, err)
	return s
}

func TestStoreReadsVersioningAndLock(t *testing.T) {
	f, srv := newFakeS3(t)
	s := testStore(t, srv)
	ctx := context.Background()

	status, err := s.BucketVersioning(ctx, "app-backup")
	require.NoError(t, err)
	assert.Equal(t, "Enabled", status)

	lock, err := s.BucketLock(ctx, "app-backup")
	require.NoError(t, err)
	assert.Equal(t, Lock{Enabled: true, Mode: "COMPLIANCE", Days: 30}, lock)

	for _, a := range f.auth {
		assert.True(t, strings.HasPrefix(a, "AWS4-HMAC-SHA256 Credential=AKEXAMPLE/"), "requests are signed with the remote's key and region: %s", a)
		assert.Contains(t, a, "/r2/s3/aws4_request")
	}
}

func TestStoreKeepsARetentionInYears(t *testing.T) {
	f, srv := newFakeS3(t)
	f.lockXML = `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled>` +
		`<Rule><DefaultRetention><Mode>COMPLIANCE</Mode><Years>1</Years></DefaultRetention></Rule></ObjectLockConfiguration>`
	lock, err := testStore(t, srv).BucketLock(context.Background(), "app-backup")
	require.NoError(t, err)
	assert.Equal(t, Lock{Enabled: true, Mode: "COMPLIANCE", Years: 1}, lock)
}

func TestStoreReportsABucketWithoutLockAsUnlocked(t *testing.T) {
	f, srv := newFakeS3(t)
	f.lockStatus = http.StatusNotFound
	lock, err := testStore(t, srv).BucketLock(context.Background(), "app-backup")
	require.NoError(t, err)
	assert.Equal(t, Lock{}, lock)

	f.lockStatus, f.lockXML = http.StatusOK, `<ObjectLockConfiguration><ObjectLockEnabled>Enabled</ObjectLockEnabled></ObjectLockConfiguration>`
	lock, err = testStore(t, srv).BucketLock(context.Background(), "app-backup")
	require.NoError(t, err)
	assert.Equal(t, Lock{Enabled: true}, lock, "lock without a default retention")
}

func TestStoreWritesReadsAndDeletesTheCanary(t *testing.T) {
	f, srv := newFakeS3(t)
	s := testStore(t, srv)
	ctx := context.Background()

	version, err := s.Put(ctx, "app-backup", CanaryKey)
	require.NoError(t, err)
	assert.Equal(t, "v1", version)

	mode, until, err := s.Retention(ctx, "app-backup", CanaryKey, version)
	require.NoError(t, err)
	assert.Equal(t, "COMPLIANCE", mode)
	assert.Equal(t, time.Date(2026, time.November, 19, 3, 0, 0, 123e6, time.UTC), until.UTC())

	err = s.DeleteVersion(ctx, "app-backup", CanaryKey, version)
	require.ErrorIs(t, err, ErrDenied)

	f.delStatus = http.StatusNoContent
	require.NoError(t, s.DeleteVersion(ctx, "app-backup", CanaryKey, version))
	require.NoError(t, s.Delete(ctx, "app-backup", CanaryKey))

	assert.Contains(t, f.requests, "PUT /app-backup/.kipper-mirror/canary?")
	assert.Contains(t, f.requests, "HEAD /app-backup/.kipper-mirror/canary?versionId=v1")
	assert.Contains(t, f.requests, "DELETE /app-backup/.kipper-mirror/canary?versionId=v1")
}

func TestStoreMarksARefusedWriteAsDenied(t *testing.T) {
	f, srv := newFakeS3(t)
	f.putStatus = http.StatusForbidden
	_, err := testStore(t, srv).Put(context.Background(), "app", CanaryKey)
	require.ErrorIs(t, err, ErrDenied)
}

func TestStoreRecognizesAWORMRefusal(t *testing.T) {
	f, srv := newFakeS3(t)
	f.delStatus, f.delCode, f.delMessage = http.StatusBadRequest, "InvalidRequest", "Object is WORM protected and cannot be overwritten"
	err := testStore(t, srv).DeleteVersion(context.Background(), "app-backup", CanaryKey, "v1")
	require.ErrorIs(t, err, ErrLocked)
	assert.False(t, errors.Is(err, ErrDenied))
}

func TestStoreDoesNotMistakeOtherRefusals(t *testing.T) {
	f, srv := newFakeS3(t)
	s := testStore(t, srv)
	ctx := context.Background()

	f.delStatus, f.delCode, f.delMessage = http.StatusBadRequest, "InvalidRequest", "Missing required header for this request: Content-MD5"
	err := s.DeleteVersion(ctx, "app-backup", CanaryKey, "v1")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrLocked), "a malformed request proves no lock")
	assert.False(t, errors.Is(err, ErrDenied))

	f.putStatus, f.putCode = http.StatusForbidden, "SignatureDoesNotMatch"
	_, err = s.Put(ctx, "app", CanaryKey)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrDenied), "a signing failure says nothing about the key's rights")

	f.putCode = "InvalidAccessKeyId"
	_, err = s.Put(ctx, "app", CanaryKey)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrDenied))
}

func TestStoreDoesNotTrustARefusalThatDidNotComeFromS3(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, "proxy rejected request")
	}))
	t.Cleanup(proxy.Close)
	s := testStore(t, proxy)
	ctx := context.Background()

	_, err := s.Put(ctx, "app", CanaryKey)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrDenied), "a 403 without an S3 error response proves nothing")
	err = s.DeleteVersion(ctx, "app-backup", CanaryKey, "v1")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrDenied))
	assert.False(t, errors.Is(err, ErrLocked))
}

func TestStoreRequiresA403ForAccessDenied(t *testing.T) {
	f, srv := newFakeS3(t)
	f.putStatus, f.putCode = http.StatusBadRequest, "AccessDenied"
	_, err := testStore(t, srv).Put(context.Background(), "app", CanaryKey)
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrDenied), "an AccessDenied code with another status is contradictory")
}

func TestStoreSendsAPlainDeleteAndANullVersionDelete(t *testing.T) {
	f, srv := newFakeS3(t)
	s := testStore(t, srv)
	ctx := context.Background()
	require.ErrorIs(t, s.Delete(ctx, "app", CanaryKey), ErrDenied)
	require.ErrorIs(t, s.DeleteVersion(ctx, "app", CanaryKey, "null"), ErrDenied)
	assert.Contains(t, f.requests, "DELETE /app/.kipper-mirror/canary?")
	assert.Contains(t, f.requests, "DELETE /app/.kipper-mirror/canary?versionId=null")
}

func TestStoreKeepsOtherErrorsDistinct(t *testing.T) {
	_, srv := newFakeS3(t)
	s := testStore(t, srv)
	srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := s.BucketVersioning(ctx, "app")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrDenied))
	assert.False(t, errors.Is(err, ErrLocked))
}

func TestPreflightPassesAgainstTheFakeS3(t *testing.T) {
	target, targetSrv := newFakeS3(t)
	source, sourceSrv := newFakeS3(t)
	source.putStatus = http.StatusForbidden

	spec := validSpec()
	spec.Source.Endpoint = strings.TrimPrefix(sourceSrv.URL, "http://")
	spec.Target.Endpoint = strings.TrimPrefix(targetSrv.URL, "http://")
	src, err := newStore(spec.Source, false)
	require.NoError(t, err)
	dst, err := newStore(spec.Target, false)
	require.NoError(t, err)

	res := Preflight(context.Background(), spec, src, dst, testNow)
	require.True(t, res.Passed(), "%+v", res.Checks)
	assert.Contains(t, target.requests, "DELETE /app-backup/.kipper-mirror/canary?versionId=v1")
	assert.Contains(t, source.requests, "PUT /app/.kipper-mirror/canary?")
}
