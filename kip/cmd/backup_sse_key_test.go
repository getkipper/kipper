package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func sseKeySecretNamed(name string, key []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "velero"},
		Data:       map[string][]byte{"customer-key": key},
	}
}

func TestSSEKeyExportReadsTheCurrentKey(t *testing.T) {
	current := bytes.Repeat([]byte{1}, 32)
	cs := fake.NewSimpleClientset(sseKeySecretNamed("kipper-backup-sse-key", current))

	key, err := sseKeyToExport(context.Background(), cs, "")

	require.NoError(t, err)
	assert.Equal(t, current, key)
}

func TestSSEKeyExportReadsAKeyMovedAside(t *testing.T) {
	previous := bytes.Repeat([]byte{2}, 32)
	cs := fake.NewSimpleClientset(
		sseKeySecretNamed("kipper-backup-sse-key", bytes.Repeat([]byte{1}, 32)),
		sseKeySecretNamed("kipper-backup-sse-key-previous-20261101120000", previous),
	)

	key, err := sseKeyToExport(context.Background(), cs, "kipper-backup-sse-key-previous-20261101120000")

	require.NoError(t, err)
	assert.Equal(t, previous, key, "backups written before the key was replaced need the old key")
}

func TestSSEKeyExportRefusesASecretThatIsNotAMovedAsideKey(t *testing.T) {
	cs := fake.NewSimpleClientset(sseKeySecretNamed("cloud-credentials", bytes.Repeat([]byte{3}, 32)))

	_, err := sseKeyToExport(context.Background(), cs, "cloud-credentials")

	require.Error(t, err, "the flag must not turn the command into a reader for any Secret in velero")
	assert.Contains(t, err.Error(), "kipper-backup-sse-key-previous-")
}

func TestSSEKeyExportNamesAMissingMovedAsideKey(t *testing.T) {
	cs := fake.NewSimpleClientset()

	_, err := sseKeyToExport(context.Background(), cs, "kipper-backup-sse-key-previous-20261101120000")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "kipper-backup-sse-key-previous-20261101120000")
}

func TestSSEKeyExportRefusesTheBarePrefix(t *testing.T) {
	cs := fake.NewSimpleClientset()

	_, err := sseKeyToExport(context.Background(), cs, "kipper-backup-sse-key-previous-")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--previous takes the name")
}

func TestSSEKeyExportSaysWhenBackupsDoNotUseSSEC(t *testing.T) {
	cs := fake.NewSimpleClientset()

	_, err := sseKeyToExport(context.Background(), cs, "")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "do not use SSE-C")
}

func TestSSEKeyExportNamesTheSecretWithAKeyOfTheWrongLength(t *testing.T) {
	cs := fake.NewSimpleClientset(sseKeySecretNamed("kipper-backup-sse-key-previous-20261101120000", []byte("short")))

	_, err := sseKeyToExport(context.Background(), cs, "kipper-backup-sse-key-previous-20261101120000")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "velero/kipper-backup-sse-key-previous-20261101120000 is not 32 bytes")
}

func TestSSEKeyExportNamesTheSecretItFailedToRead(t *testing.T) {
	cs := fake.NewSimpleClientset()
	cs.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("forbidden")
	})

	_, err := sseKeyToExport(context.Background(), cs, "kipper-backup-sse-key-previous-20261101120000")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "velero/kipper-backup-sse-key-previous-20261101120000",
		"an operator exporting an old key must not be pointed at the current one")
}
