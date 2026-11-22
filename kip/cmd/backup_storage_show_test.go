package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

var showNow = time.Date(2026, 11, 22, 1, 0, 0, 0, time.UTC)

func showPlatformConfig(backup string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kipper.run/v1alpha1", "kind": "PlatformConfig",
		"metadata": map[string]any{"name": "platform"},
		"spec":     map[string]any{"backup": backup},
	}}
}

func showLocation(spec, status map[string]any) *unstructured.Unstructured {
	obj := map[string]any{
		"apiVersion": "velero.io/v1", "kind": "BackupStorageLocation",
		"metadata": map[string]any{"name": "default", "namespace": "velero"},
		"spec":     spec,
	}
	if status != nil {
		obj["status"] = status
	}
	return &unstructured.Unstructured{Object: obj}
}

func showDynamic(objects ...runtime.Object) *dynamicfake.FakeDynamicClient {
	scheme := runtime.NewScheme()
	for _, gvk := range []struct{ gv, kind string }{
		{"kipper.run/v1alpha1", "PlatformConfig"}, {"velero.io/v1", "BackupStorageLocation"},
	} {
		scheme.AddKnownTypeWithName(schema.FromAPIVersionAndKind(gvk.gv, gvk.kind), &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(schema.FromAPIVersionAndKind(gvk.gv, gvk.kind+"List"), &unstructured.UnstructuredList{})
	}
	return dynamicfake.NewSimpleDynamicClient(scheme, objects...)
}

func showSecret(name string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "velero"}, Data: data}
}

func showForTest(t *testing.T, dyn *dynamicfake.FakeDynamicClient, cs *fake.Clientset) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := showBackupStorage(context.Background(), dyn, cs, &out, showNow)
	return out.String(), err
}

func TestBackupStorageShowNamesTheExternalBucketWithoutItsSecrets(t *testing.T) {
	dyn := showDynamic(showPlatformConfig("external"), showLocation(
		map[string]any{
			"provider":      "aws",
			"objectStorage": map[string]any{"bucket": "team-backups"},
			"config": map[string]any{
				"region": "eu-1", "s3Url": "https://s3.example.com", "s3ForcePathStyle": "true",
				"customerKeyEncryptionSecret": "kipper-backup-sse-key/customer-key",
			},
		},
		map[string]any{"phase": "Available", "lastValidationTime": "2026-11-22T00:58:00Z"},
	))
	cs := fake.NewClientset(
		showSecret("cloud-credentials", map[string][]byte{"cloud": []byte("[default]\naws_secret_access_key = SECRET-CREDENTIAL-VALUE\n")}),
		showSecret("kipper-backup-sse-key", map[string][]byte{"customer-key": []byte("SECRET-SSE-KEY-VALUE")}),
	)

	out, err := showForTest(t, dyn, cs)
	require.NoError(t, err)
	assert.Contains(t, out, "team-backups")
	assert.Contains(t, out, "eu-1")
	assert.Contains(t, out, "https://s3.example.com")
	assert.Contains(t, out, "SSE-C, key in Secret velero/kipper-backup-sse-key present")
	assert.Contains(t, out, "Available")
	assert.Contains(t, out, "last checked 2m ago")
	assert.Contains(t, out, "velero/cloud-credentials")
	assert.NotContains(t, out, "SECRET-CREDENTIAL-VALUE")
	assert.NotContains(t, out, "SECRET-SSE-KEY-VALUE")
	assert.NotContains(t, out, "missing")
}

func TestBackupStorageShowNamesAWSS3AndItsServerSideEncryption(t *testing.T) {
	dyn := showDynamic(showPlatformConfig("external"), showLocation(
		map[string]any{
			"provider":      "aws",
			"objectStorage": map[string]any{"bucket": "aws-backups"},
			"config":        map[string]any{"region": "eu-central-1", "serverSideEncryption": "AES256"},
		}, nil,
	))
	out, err := showForTest(t, dyn, fake.NewClientset(showSecret("cloud-credentials", nil)))
	require.NoError(t, err)
	assert.Contains(t, out, "aws-backups")
	assert.Contains(t, out, "eu-central-1")
	assert.Contains(t, out, "AWS S3")
	assert.Contains(t, out, "SSE-S3")
	assert.Contains(t, out, "not validated yet")
}

func TestBackupStorageShowNamesInClusterMinIO(t *testing.T) {
	dyn := showDynamic(showPlatformConfig("in-cluster"), showLocation(
		map[string]any{
			"provider":      "aws",
			"objectStorage": map[string]any{"bucket": "velero"},
			"config":        map[string]any{"region": "minio", "s3Url": "http://minio.velero.svc:9000", "s3ForcePathStyle": "true"},
		},
		map[string]any{"phase": "Available", "lastValidationTime": "2026-11-22T00:59:30Z"},
	))
	out, err := showForTest(t, dyn, fake.NewClientset(showSecret("cloud-credentials", nil)))
	require.NoError(t, err)
	assert.Contains(t, out, "in-cluster MinIO")
	assert.NotContains(t, out, "Region")
	assert.Contains(t, out, "Objects       MinIO stores objects as written and adds no encryption. Volume data in these objects is encrypted with the backup repository key")
	assert.NotContains(t, out, "the provider's default", "kip sets no object encryption on in-cluster MinIO")
	assert.NotContains(t, out, "Encryption", "the line describes the objects, not every layer of a backup")
}

func TestBackupStorageShowPointsAnUnavailableMinIOAtItsHealthAndVolume(t *testing.T) {
	dyn := showDynamic(showPlatformConfig("in-cluster"), showLocation(
		map[string]any{
			"provider":      "aws",
			"objectStorage": map[string]any{"bucket": "velero"},
			"config":        map[string]any{"region": "minio", "s3Url": "http://minio.velero.svc:9000", "s3ForcePathStyle": "true"},
		},
		map[string]any{"phase": "Unavailable"},
	))
	out, err := showForTest(t, dyn, fake.NewClientset(showSecret("cloud-credentials", nil)))
	require.NoError(t, err)
	assert.Contains(t, out, "Velero cannot use the in-cluster MinIO")
	assert.Contains(t, out, "kip backup storage resize")
	assert.NotContains(t, out, "Check that the bucket exists", "kip manages the in-cluster MinIO bucket and its credentials")
}

func TestBackupStorageShowWarnsAboutAMissingCredentialsSecretAndAnUnavailableBucket(t *testing.T) {
	dyn := showDynamic(showPlatformConfig("external"), showLocation(
		map[string]any{
			"provider":      "aws",
			"accessMode":    "ReadOnly",
			"objectStorage": map[string]any{"bucket": "old-backups", "prefix": "cluster-a"},
			"config":        map[string]any{"region": "auto", "s3Url": "https://example.r2.cloudflarestorage.com"},
		},
		map[string]any{"phase": "Unavailable", "message": "BackupStorageLocation \"default\" is unavailable: access denied"},
	))
	out, err := showForTest(t, dyn, fake.NewClientset())
	require.NoError(t, err)
	assert.Contains(t, out, "old-backups")
	assert.Contains(t, out, "cluster-a")
	assert.Contains(t, out, "read-only")
	assert.Contains(t, out, "Unavailable")
	assert.Contains(t, out, "Velero cannot use this location")
	assert.NotContains(t, out, "access denied", "Velero's free-form message can quote provider errors")
	assert.Contains(t, out, "velero/cloud-credentials missing")
}

func TestBackupStorageShowIncludesAMoveAndThePreviousBucket(t *testing.T) {
	dyn := showDynamic(showPlatformConfig("external"), showLocation(
		map[string]any{
			"provider":      "aws",
			"objectStorage": map[string]any{"bucket": "new-backups"},
			"config":        map[string]any{"region": "eu-1", "s3Url": "https://s3.example.com"},
		}, nil,
	))
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-backup-storage", Namespace: "velero"},
		Data: map[string]string{
			"move":     `{"target":{"bucket":"next-backups","endpoint":"https://s3-next.example.com"},"previous":{"bucket":"new-backups"},"phase":"preparing","startedAt":"2026-11-22T00:30:00Z"}`,
			"previous": `{"bucket":"old-backups","switchedAt":"2026-11-20T10:00:00Z"}`,
		},
	}
	out, err := showForTest(t, dyn, fake.NewClientset(showSecret("cloud-credentials", nil), record))
	require.NoError(t, err)
	assert.Contains(t, out, "next-backups at https://s3-next.example.com")
	assert.Contains(t, out, "preparing")
	assert.Contains(t, out, "kip backup storage set --abort")
	assert.Contains(t, out, "old-backups at AWS S3")
	assert.Contains(t, out, "kip backup storage retire-previous")
}

func showVelero(replicas int32) *appsv1.Deployment {
	return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "velero", Namespace: "velero"}, Spec: appsv1.DeploymentSpec{Replicas: &replicas}}
}

func TestBackupStorageShowSaysVeleroIsStoppedDuringAMoveInsteadOfItsStaleStatus(t *testing.T) {
	dyn := showDynamic(showPlatformConfig("external"), showLocation(
		map[string]any{
			"provider":      "aws",
			"objectStorage": map[string]any{"bucket": "old-backups"},
			"config":        map[string]any{"region": "eu-1", "s3Url": "https://s3.example.com"},
		},
		map[string]any{"phase": "Available", "lastValidationTime": "2026-11-22T00:10:00Z"},
	))
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-backup-storage", Namespace: "velero"},
		Data: map[string]string{
			"move": `{"target":{"bucket":"next-backups","endpoint":"https://s3-next.example.com"},"previous":{"bucket":"old-backups","endpoint":"https://s3.example.com"},"phase":"cutover","startedAt":"2026-11-22T00:30:00Z"}`,
		},
	}
	out, err := showForTest(t, dyn, fake.NewClientset(showSecret("cloud-credentials", nil), record, showVelero(0)))
	require.NoError(t, err)
	assert.Contains(t, out, "stopped, so no backups run")
	assert.Contains(t, out, "kip stopped Velero for the unfinished move")
	assert.NotContains(t, out, "canceled", "a move past preparing can only be finished")
	assert.NotContains(t, out, "Available", "the location's status is stale while Velero is stopped")

	record.Data["move"] = `{"target":{"bucket":"next-backups"},"previous":{"bucket":"old-backups"},"phase":"preparing"}`
	out, err = showForTest(t, dyn, fake.NewClientset(showSecret("cloud-credentials", nil), record, showVelero(0)))
	require.NoError(t, err)
	assert.Contains(t, out, "starts again when the move finishes or is canceled")
}

func TestBackupStorageShowSaysVeleroIsStoppedOutsideAMove(t *testing.T) {
	dyn := showDynamic(showPlatformConfig("external"), showLocation(
		map[string]any{"provider": "aws", "objectStorage": map[string]any{"bucket": "b"}, "config": map[string]any{"region": "r"}},
		map[string]any{"phase": "Unavailable"},
	))
	out, err := showForTest(t, dyn, fake.NewClientset(showSecret("cloud-credentials", nil), showVelero(0)))
	require.NoError(t, err)
	assert.Contains(t, out, "stopped, so no backups run")
	assert.Contains(t, out, "kip backup repository-key reset")
	assert.NotContains(t, out, "Velero cannot use this location", "an Unavailable status is stale while Velero is stopped")
}

func TestBackupStorageShowHoldsBackRetireDuringAMove(t *testing.T) {
	dyn := showDynamic(showPlatformConfig("external"), showLocation(
		map[string]any{
			"provider":      "aws",
			"objectStorage": map[string]any{"bucket": "old-backups"},
			"config":        map[string]any{"region": "eu-1", "s3Url": "https://s3.example.com"},
		}, nil,
	))
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-backup-storage", Namespace: "velero"},
		Data: map[string]string{
			"move":     `{"target":{"bucket":"next-backups","endpoint":"https://s3-next.example.com"},"previous":{"bucket":"old-backups","endpoint":"https://s3.example.com"},"phase":"switched","startedAt":"2026-11-22T00:30:00Z"}`,
			"previous": `{"bucket":"old-backups","endpoint":"https://s3.example.com","switchedAt":"2026-11-22T00:40:00Z"}`,
		},
	}
	out, err := showForTest(t, dyn, fake.NewClientset(showSecret("cloud-credentials", nil), record, showVelero(1)))
	require.NoError(t, err)
	assert.Contains(t, out, "next-backups at https://s3-next.example.com")
	assert.NotContains(t, out, "then run kip backup storage retire-previous", "retire-previous refuses while a move is unfinished")
	assert.NotContains(t, out, "previous bucket was old-backups", "the bucket line above already names it")
}

func TestBackupStorageShowRefusesWhenBackupsAreOff(t *testing.T) {
	_, err := showForTest(t, showDynamic(showPlatformConfig("none")), fake.NewClientset())
	require.ErrorIs(t, err, errBackupsNotConfigured)
}

func TestBackupStorageShowStatesAMissingLocation(t *testing.T) {
	_, err := showForTest(t, showDynamic(showPlatformConfig("external")), fake.NewClientset())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "velero/default")
}

func TestBackupStorageShowPointsAMissingLocationDuringAMoveAtTheMove(t *testing.T) {
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-backup-storage", Namespace: "velero"},
		Data:       map[string]string{"move": `{"target":{"bucket":"next-backups"},"previous":{"bucket":"old-backups"},"phase":"cutover"}`},
	}
	_, err := showForTest(t, showDynamic(showPlatformConfig("external")), fake.NewClientset(record))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "next-backups")
	assert.Contains(t, err.Error(), "same kip backup storage set command")
}

func TestBackupStorageShowWithholdsCredentialsInEndpoints(t *testing.T) {
	const raw = "https://key-id:PASSWORD-SENTINEL@objects.example.com/s3?token=TOKEN-SENTINEL#FRAGMENT-SENTINEL" //nolint:gosec // G101: This value is a test sentinel.
	dyn := showDynamic(showPlatformConfig("external"), showLocation(
		map[string]any{
			"provider":      "aws",
			"objectStorage": map[string]any{"bucket": "team-backups"},
			"config":        map[string]any{"region": "eu-1", "s3Url": raw},
		},
		map[string]any{"phase": "Unavailable", "message": "authentication failed for token TOKEN-SENTINEL, user key-id, password PASSWORD-SENTINEL"},
	))
	record := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-backup-storage", Namespace: "velero"},
		Data: map[string]string{
			"move":     `{"target":{"bucket":"next-backups","endpoint":"` + raw + `"},"phase":"cutover"}`,
			"previous": `{"bucket":"old-backups","endpoint":"` + raw + `","switchedAt":"2026-11-20T10:00:00Z"}`,
		},
	}
	out, err := showForTest(t, dyn, fake.NewClientset(record))
	require.NoError(t, err)
	assert.Contains(t, out, "https://objects.example.com/s3")
	for _, sentinel := range []string{"PASSWORD-SENTINEL", "TOKEN-SENTINEL", "FRAGMENT-SENTINEL", "key-id"} {
		assert.NotContains(t, out, sentinel)
	}
	assert.NotContains(t, out, "--abort", "a move past preparing can no longer be canceled")
}

func TestBackupStorageShowReportsAnUnreadableSecretWithoutTheAPIError(t *testing.T) {
	dyn := showDynamic(showPlatformConfig("external"), showLocation(
		map[string]any{"provider": "aws", "objectStorage": map[string]any{"bucket": "b"}, "config": map[string]any{"region": "r"}}, nil,
	))
	cs := fake.NewClientset()
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("ERROR-SENTINEL")
	})
	out, err := showForTest(t, dyn, cs)
	require.NoError(t, err)
	assert.Contains(t, out, "velero/cloud-credentials not readable")
	assert.NotContains(t, out, "ERROR-SENTINEL")
}
