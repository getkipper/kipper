package cmd

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/md5" //nolint:gosec // S3 checks an SSE-C key against its MD5
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func describedBackup(phase string, errs, warnings int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "Backup",
		"metadata": map[string]any{"name": "velero-daily-apps-20261021030000", "namespace": "velero",
			"labels": map[string]any{"velero.io/schedule-name": "velero-daily-apps"}},
		"spec": map[string]any{"excludedNamespaces": []any{"kube-system"}},
		"status": map[string]any{
			"phase":               phase,
			"startTimestamp":      "2026-10-21T03:00:04Z",
			"completionTimestamp": "2026-10-21T03:12:40Z",
			"expiration":          "2026-10-28T03:00:04Z",
			"errors":              errs,
			"warnings":            warnings,
			"progress":            map[string]any{"itemsBackedUp": int64(810), "totalItems": int64(812)},
		},
	}}
}

func describedVolumeBackup(name, ns, pod, volume, phase, message string, bytesDone int64) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "PodVolumeBackup",
		"metadata": map[string]any{"name": name, "namespace": "velero",
			"labels": map[string]any{"velero.io/backup-name": "velero-daily-apps-20261021030000"}},
		"spec":   map[string]any{"pod": map[string]any{"namespace": ns, "name": pod}, "volume": volume},
		"status": map[string]any{"phase": phase, "message": message, "progress": map[string]any{"bytesDone": bytesDone, "totalBytes": bytesDone}},
	}}
}

func TestWriteBackupDescription(t *testing.T) {
	pvbs := []unstructured.Unstructured{
		*describedVolumeBackup("a", "shop", "db-0", "data", "Completed", "", 3<<30),
		*describedVolumeBackup("b", "shop", "files-holder", "shared", "Failed", "pod volume not found", 0),
	}
	results := backupResults{
		Errors:   backupResultSet{Velero: []string{"timed out waiting for volume backups"}, Namespaces: map[string][]string{"shop": {"pod volume backup failed: pod volume not found"}}},
		Warnings: backupResultSet{Cluster: []string{"a cluster-scoped warning"}},
	}
	var out bytes.Buffer

	writeBackupDescription(&out, *describedBackup("PartiallyFailed", 2, 1), pvbs, nil, &results, nil)

	text := out.String()
	for _, want := range []string{
		"Backup:     velero-daily-apps-20261021030000",
		"Schedule:   velero-daily-apps",
		"Status:     PartiallyFailed (2 errors, 1 warning)",
		"Started:    2026-10-21 03:00:04 UTC",
		"Completed:  2026-10-21 03:12:40 UTC",
		"Expires:    2026-10-28 03:00:04 UTC",
		"Namespaces: all except system",
		"Items:      810 of 812 backed up",
		"✔  shop/db-0 data  3.0 GiB",
		"✗  shop/files-holder shared  Failed: pod volume not found",
		"Errors:",
		"Velero: timed out waiting for volume backups",
		"shop: pod volume backup failed: pod volume not found",
		"Warnings:",
		"cluster: a cluster-scoped warning",
	} {
		assert.Contains(t, text, want)
	}
}

func TestWriteBackupDescriptionWithoutMessages(t *testing.T) {
	var out bytes.Buffer

	writeBackupDescription(&out, *describedBackup("PartiallyFailed", 2, 0), nil, errors.New("forbidden"), nil, errors.New("dial tcp: lookup minio.velero.svc: no such host"))

	assert.Contains(t, out.String(), "Volume backups: not read (forbidden)")
	assert.Contains(t, out.String(), "kip could not read the error and warning messages: dial tcp: lookup minio.velero.svc: no such host")
}

func TestWriteBackupDescriptionNamesTheFailureReason(t *testing.T) {
	b := describedBackup("Failed", 0, 0)
	require.NoError(t, unstructured.SetNestedField(b.Object, "the backup storage location is unavailable", "status", "failureReason"))
	var out bytes.Buffer

	writeBackupDescription(&out, *b, nil, nil, nil, nil)

	assert.Contains(t, out.String(), "Failure:    the backup storage location is unavailable")
}

func gzipJSON(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	require.NoError(t, json.NewEncoder(zw).Encode(v))
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

// describeClient returns a fake dynamic client in which Velero answers every
// DownloadRequest with url.
func describeClient(url string, objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		backupGVR:                "BackupList",
		podVolumeBackupGVR:       "PodVolumeBackupList",
		downloadRequestGVR:       "DownloadRequestList",
		backupStorageLocationGVR: "BackupStorageLocationList",
	}, objects...)
	dyn.PrependReactor("get", "downloadrequests", func(action k8stesting.Action) (bool, runtime.Object, error) {
		name := action.(k8stesting.GetAction).GetName()
		obj, err := dyn.Tracker().Get(downloadRequestGVR, "velero", name)
		if err != nil {
			return true, nil, err
		}
		u := obj.(*unstructured.Unstructured).DeepCopy()
		_ = unstructured.SetNestedField(u.Object, "Processed", "status", "phase")
		_ = unstructured.SetNestedField(u.Object, url, "status", "downloadURL")
		return true, u, nil
	})
	return dyn
}

func TestDescribeBackupDownloadsTheMessages(t *testing.T) {
	noDownloadWait(t)
	body := gzipJSON(t, map[string]any{
		"errors":   map[string]any{"namespaces": map[string]any{"shop": []string{"pod volume backup failed"}}},
		"warnings": map[string]any{},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	dyn := describeClient(srv.URL, describedBackup("PartiallyFailed", 1, 0))
	var out bytes.Buffer

	require.NoError(t, describeBackup(context.Background(), dyn, k8sfake.NewClientset(), "velero-daily-apps-20261021030000", &out, srv.Client()))

	assert.Contains(t, out.String(), "shop: pod volume backup failed")
	left, err := dyn.Resource(downloadRequestGVR).Namespace("velero").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, left.Items, "kip removes its download request")
	var created string
	for _, a := range dyn.Actions() {
		if a.GetVerb() == "create" && a.GetResource() == downloadRequestGVR {
			obj := a.(k8stesting.CreateAction).GetObject().(*unstructured.Unstructured)
			created, _, _ = unstructured.NestedString(obj.Object, "spec", "target", "kind")
		}
	}
	assert.Equal(t, "BackupResults", created)
}

func TestDescribeBackupSkipsTheDownloadForACleanBackup(t *testing.T) {
	dyn := describeClient("http://unused.invalid", describedBackup("Completed", 0, 0))
	var out bytes.Buffer

	require.NoError(t, describeBackup(context.Background(), dyn, k8sfake.NewClientset(), "velero-daily-apps-20261021030000", &out, http.DefaultClient))

	for _, a := range dyn.Actions() {
		assert.False(t, a.GetVerb() == "create", "a backup without errors or warnings needs no download")
	}
	assert.Contains(t, out.String(), "Status:     Completed")
}

func TestDescribeBackupNamesAMissingBackup(t *testing.T) {
	dyn := describeClient("http://unused.invalid")

	err := describeBackup(context.Background(), dyn, k8sfake.NewClientset(), "nope", io.Discard, http.DefaultClient)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kip backup list")
}

func TestDescribeBackupReportsAnUnreachableDownload(t *testing.T) {
	noDownloadWait(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer srv.Close()
	dyn := describeClient(srv.URL, describedBackup("PartiallyFailed", 1, 0))
	var out bytes.Buffer

	require.NoError(t, describeBackup(context.Background(), dyn, k8sfake.NewClientset(), "velero-daily-apps-20261021030000", &out, srv.Client()))

	assert.True(t, strings.Contains(out.String(), "kip could not read the error and warning messages") && strings.Contains(out.String(), "403"), out.String())
}

func TestDescribeBackupKeepsThePresignedLinkOutOfATransportError(t *testing.T) {
	noDownloadWait(t)
	srv := httptest.NewServer(http.NotFoundHandler())
	link := srv.URL + "/backups/results.json.gz?X-Amz-Credential=sentinel-credential&X-Amz-Signature=sentinel-signature"
	srv.Close()
	dyn := describeClient(link, describedBackup("PartiallyFailed", 1, 0))
	var out bytes.Buffer

	require.NoError(t, describeBackup(context.Background(), dyn, k8sfake.NewClientset(), "velero-daily-apps-20261021030000", &out, &http.Client{Timeout: 5 * time.Second}))

	text := out.String()
	assert.Contains(t, text, "kip could not read the error and warning messages: downloading from the backup storage at "+strings.TrimPrefix(srv.URL, "http://"))
	assert.Contains(t, text, "connect", "the underlying failure reason stays")
	assert.NotContains(t, text, "sentinel")
	assert.NotContains(t, text, "results.json.gz")
}

// The client quotes a malformed redirect target in full, so a storage proxy
// that carries the signature into Location must not get it printed.
func TestDescribeBackupKeepsASignedRedirectOutOfTheError(t *testing.T) {
	noDownloadWait(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "https://storage.example:bad/results?X-Amz-Credential=sentinel-credential&X-Amz-Signature=sentinel-signature")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	dyn := describeClient(srv.URL+"/results?X-Amz-Signature=outer", describedBackup("PartiallyFailed", 1, 0))
	var out bytes.Buffer

	require.NoError(t, describeBackup(context.Background(), dyn, k8sfake.NewClientset(), "velero-daily-apps-20261021030000", &out, srv.Client()))

	text := out.String()
	assert.Contains(t, text, "kip could not read the error and warning messages: downloading from the backup storage")
	assert.Contains(t, text, "Location", "the reason stays readable")
	assert.NotContains(t, text, "sentinel")
	assert.NotContains(t, text, "outer")
}

func TestDescribeBackupRefusesOversizedMessages(t *testing.T) {
	noDownloadWait(t)
	saved := maxBackupResultsSize
	maxBackupResultsSize = 1 << 20
	t.Cleanup(func() { maxBackupResultsSize = saved })
	body := gzipJSON(t, map[string]any{
		"errors":   map[string]any{"velero": []string{strings.Repeat("x", 2<<20)}},
		"warnings": map[string]any{},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	defer srv.Close()
	dyn := describeClient(srv.URL, describedBackup("PartiallyFailed", 1, 0))
	var out bytes.Buffer

	require.NoError(t, describeBackup(context.Background(), dyn, k8sfake.NewClientset(), "velero-daily-apps-20261021030000", &out, srv.Client()))

	assert.Contains(t, out.String(), "kip could not read the error and warning messages: the messages are larger than 1 MiB")
	assert.NotContains(t, out.String(), strings.Repeat("x", 100))
}

func noDownloadWait(t *testing.T) {
	t.Helper()
	saved := downloadPollInterval
	downloadPollInterval = 0
	t.Cleanup(func() { downloadPollInterval = saved })
}

func TestBackupNamespaces(t *testing.T) {
	for name, tc := range map[string]struct {
		spec map[string]any
		want string
	}{
		"no restriction":     {spec: map[string]any{}, want: "all"},
		"wildcard":           {spec: map[string]any{"includedNamespaces": []any{"*"}}, want: "all"},
		"named namespaces":   {spec: map[string]any{"includedNamespaces": []any{"shop", "blog"}}, want: "shop, blog"},
		"system left out":    {spec: map[string]any{"excludedNamespaces": []any{"kube-system"}}, want: "all except system"},
		"wildcard minus one": {spec: map[string]any{"includedNamespaces": []any{"*"}, "excludedNamespaces": []any{"kube-system"}}, want: "all except system"},
	} {
		t.Run(name, func(t *testing.T) {
			b := unstructured.Unstructured{Object: map[string]any{"spec": tc.spec}}
			assert.Equal(t, tc.want, backupNamespaces(b))
		})
	}
}

func TestDescribeBackupGivesUpWhenVeleroNeverAnswers(t *testing.T) {
	noDownloadWait(t)
	savedWait := downloadWait
	downloadWait = 20 * time.Millisecond
	t.Cleanup(func() { downloadWait = savedWait })
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		backupGVR:          "BackupList",
		podVolumeBackupGVR: "PodVolumeBackupList",
		downloadRequestGVR: "DownloadRequestList",
	}, describedBackup("PartiallyFailed", 1, 0))
	var out bytes.Buffer

	require.NoError(t, describeBackup(context.Background(), dyn, k8sfake.NewClientset(), "velero-daily-apps-20261021030000", &out, http.DefaultClient))

	assert.Contains(t, out.String(), "kip could not read the error and warning messages: velero did not provide a download link")
	left, err := dyn.Resource(downloadRequestGVR).Namespace("velero").List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, left.Items, "kip removes its download request after giving up")
}

func TestWriteBackupDescriptionNamesValidationErrors(t *testing.T) {
	b := describedBackup("FailedValidation", 0, 0)
	require.NoError(t, unstructured.SetNestedStringSlice(b.Object,
		[]string{"backup storage location 'default' is unavailable"}, "status", "validationErrors"))
	var out bytes.Buffer

	writeBackupDescription(&out, *b, nil, nil, nil, nil)

	assert.Contains(t, out.String(), "Validation: backup storage location 'default' is unavailable")
}

// sseCFixtures returns a backup location that encrypts with SSE-C and the
// Secret holding its key.
func sseCFixtures(key []byte) (*unstructured.Unstructured, *corev1.Secret) {
	location := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation",
		"metadata": map[string]any{"name": "default", "namespace": "velero"},
		"spec":     map[string]any{"config": map[string]any{"customerKeyEncryptionSecret": "kipper-backup-sse-key/customer-key"}},
	}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-backup-sse-key", Namespace: "velero"},
		Data:       map[string][]byte{"customer-key": key},
	}
	return location, secret
}

var testSSECKey = []byte("0123456789abcdef0123456789abcdef")

// With SSE-C on, the storage refuses to hand out an object unless the request
// carries the key it was written with, presigned link or not.
func TestDescribeBackupSendsTheSSECKey(t *testing.T) {
	noDownloadWait(t)
	sum := md5.Sum(testSSECKey) //nolint:gosec // S3 checks an SSE-C key against its MD5
	body := gzipJSON(t, map[string]any{
		"errors":   map[string]any{"namespaces": map[string]any{"shop": []string{"pod volume backup failed"}}},
		"warnings": map[string]any{},
	})
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Amz-Server-Side-Encryption-Customer-Algorithm") != "AES256" ||
			r.Header.Get("X-Amz-Server-Side-Encryption-Customer-Key") != base64.StdEncoding.EncodeToString(testSSECKey) ||
			r.Header.Get("X-Amz-Server-Side-Encryption-Customer-Key-Md5") != base64.StdEncoding.EncodeToString(sum[:]) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	location, secret := sseCFixtures(testSSECKey)
	dyn := describeClient(srv.URL, describedBackup("PartiallyFailed", 1, 0), location)
	var out bytes.Buffer

	require.NoError(t, describeBackup(context.Background(), dyn, k8sfake.NewClientset(secret), "velero-daily-apps-20261021030000", &out, srv.Client()))

	assert.Contains(t, out.String(), "shop: pod volume backup failed")
	assert.NotContains(t, out.String(), base64.StdEncoding.EncodeToString(testSSECKey))
}

// The key decrypts every backup, so it travels only over TLS and only to the
// address Velero signed, never to wherever a redirect points.
func TestDescribeBackupKeepsTheSSECKeyFromAnywhereElse(t *testing.T) {
	noDownloadWait(t)
	keyHeader := "X-Amz-Server-Side-Encryption-Customer-Key"
	var leaked []string
	elsewhere := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get(keyHeader) != "" {
			leaked = append(leaked, "redirect target")
		}
	}))
	defer elsewhere.Close()
	redirecting := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/results", http.StatusFound)
	}))
	defer redirecting.Close()
	plain := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if r.Header.Get(keyHeader) != "" {
			leaked = append(leaked, "plain HTTP link")
		}
	}))
	defer plain.Close()
	location, secret := sseCFixtures(testSSECKey)

	for _, tc := range []struct {
		name, link string
		client     *http.Client
	}{
		{"a redirect", redirecting.URL, redirecting.Client()},
		{"a plain HTTP link", plain.URL, plain.Client()},
	} {
		dyn := describeClient(tc.link, describedBackup("PartiallyFailed", 1, 0), location)
		var out bytes.Buffer

		require.NoError(t, describeBackup(context.Background(), dyn, k8sfake.NewClientset(secret), "velero-daily-apps-20261021030000", &out, tc.client), tc.name)

		assert.Contains(t, out.String(), "SSE-C key", tc.name)
	}
	assert.Empty(t, leaked)
}
