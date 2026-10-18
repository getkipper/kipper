package installer

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const sseEndpoint = "https://hel1.example.com"

var sseTarget = backupTarget{bucket: "backups", s3URL: sseEndpoint, mcHost: "https://ak:sk@hel1.example.com"} //nolint:gosec // G101: a synthetic credentials fixture

func sseConfig(on bool, key []byte) *BackupStorageConfig {
	return &BackupStorageConfig{Bucket: "backups", Region: "hel1", Endpoint: sseEndpoint, SSEC: on, SSECKey: key}
}

func prepareSSEC(h *fakeRepoHost, cfg *BackupStorageConfig) (bool, error) {
	return prepareBackupSSEC(h, cfg, sseTarget, &bucketCheck{client: h, target: sseTarget})
}

var suppliedSSEKey = []byte("supplied-sse-key-of-thirty-two-b")

func TestSSECStaysOffUnlessAsked(t *testing.T) {
	h := &fakeRepoHost{bucket: "populated", liveBSL: "backups|" + sseEndpoint}

	on, err := prepareSSEC(h, sseConfig(false, nil))

	require.NoError(t, err)
	assert.False(t, on)
	assert.False(t, h.ran(bucketCheckJob), "a cluster without SSE-C never lists its bucket for it")
}

func TestSSECKeyIsGeneratedOnTheServerForAnEmptyBucket(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseWrite: sseWriteOKMarker}

	on, err := prepareSSEC(h, sseConfig(true, nil))

	require.NoError(t, err)
	assert.True(t, on)
	assert.NotEmpty(t, h.sseKey)
	for _, c := range h.cmds {
		if strings.Contains(c, "/dev/urandom") {
			assert.Contains(t, c, "kubectl create -f -", "create refuses to replace a key that appeared meanwhile")
			assert.Contains(t, c, "velero.io/exclude-from-backup=true", "the key stays out of the backups it protects")
		}
	}
	assert.Contains(t, h.lastSSEJob, "secretName: "+sseKeySecret, "the write test uses the key Velero will use")
}

func TestSSECIsRefusedOnABucketWithBackups(t *testing.T) {
	h := &fakeRepoHost{bucket: "populated", sseWrite: sseWriteOKMarker}

	_, err := prepareSSEC(h, sseConfig(true, nil))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--backup-storage-sse-c-key-file")
	assert.Empty(t, h.sseKey, "objects written without SSE-C would become unreadable to Velero")
}

func TestSSECIsRefusedWhenTheBucketCannotBeChecked(t *testing.T) {
	h := &fakeRepoHost{bucket: "fail", sseWrite: sseWriteOKMarker}

	_, err := prepareSSEC(h, sseConfig(true, nil))

	require.Error(t, err)
	assert.Empty(t, h.sseKey)
}

func TestAFailedWriteTestRemovesTheNewKey(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseWrite: "mc: <ERROR> 400 Bad Request"}

	_, err := prepareSSEC(h, sseConfig(true, nil))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "SSE-C test")
	assert.Empty(t, h.sseKey, "a store that cannot do SSE-C must not leave SSE-C switched on for the next run")
}

func TestAnExistingKeyKeepsSSECOnWithoutAFlag(t *testing.T) {
	h := &fakeRepoHost{sseKey: string(suppliedSSEKey), liveBSL: "backups|" + sseEndpoint + "|" + sseKeySecret + "/" + sseKeyField}

	on, err := prepareSSEC(h, &BackupStorageConfig{Bucket: "backups", Region: "hel1", Endpoint: sseEndpoint})

	require.NoError(t, err)
	assert.True(t, on, "an upgrade carries no SSE-C flag and must keep SSE-C")
	assert.False(t, h.ran("name: "+sseCheckJob), "an unchanged location needs no new test")
	assert.True(t, h.ran("label secret "+sseKeySecret+" velero.io/exclude-from-backup=true"))
}

func TestMovingAnSSECClusterTestsTheNewBucket(t *testing.T) {
	h := &fakeRepoHost{sseKey: string(suppliedSSEKey), liveBSL: "old-bucket|" + sseEndpoint + "|" + sseKeySecret + "/" + sseKeyField, sseWrite: sseWriteOKMarker}

	on, err := prepareSSEC(h, sseConfig(false, nil))

	require.NoError(t, err)
	assert.True(t, on)
	assert.True(t, h.ran("name: "+sseCheckJob))
}

func TestASuppliedKeyMustMatchTheLiveOne(t *testing.T) {
	h := &fakeRepoHost{sseKey: "another-sse-key-of-thirty-two-by", liveBSL: "backups|" + sseEndpoint}

	_, err := prepareSSEC(h, sseConfig(true, suppliedSSEKey))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "differs")
	assert.Equal(t, "another-sse-key-of-thirty-two-by", h.sseKey)
}

func TestASuppliedKeyIsCheckedAgainstTheBucketsBackups(t *testing.T) {
	for _, tc := range []struct {
		log     string
		wantErr string
	}{
		{sseKeyMatchMarker, ""},
		{sseKeyWrongMarker, "does not open"},
		{ssePlainMarker, "written without SSE-C"},
		{"mc: <ERROR> Access Denied.", "failed"},
	} {
		t.Run(tc.log, func(t *testing.T) {
			h := &fakeRepoHost{bucket: "populated", sseWrite: sseWriteOKMarker, sseCheck: tc.log}

			on, err := prepareSSEC(h, sseConfig(false, suppliedSSEKey))

			if tc.wantErr == "" {
				require.NoError(t, err)
				assert.True(t, on)
				assert.Equal(t, string(suppliedSSEKey), h.sseKey)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Empty(t, h.sseKey, "a key that does not fit the backups is removed again")
		})
	}
}

func TestTheSuppliedKeyTravelsOnlyOverStdin(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseWrite: sseWriteOKMarker}

	_, err := prepareSSEC(h, sseConfig(false, suppliedSSEKey))

	require.NoError(t, err)
	assert.Contains(t, h.stdins, string(suppliedSSEKey))
	for _, c := range h.cmds {
		assert.NotContains(t, c, string(suppliedSSEKey))
		assert.NotContains(t, c, EncodeSSECKey(suppliedSSEKey))
	}
}

func TestSSECNeedsAnHTTPSCompatibleStore(t *testing.T) {
	for name, cfg := range map[string]*BackupStorageConfig{
		"in-cluster MinIO": nil,
		"native AWS S3":    {Bucket: "backups", Region: "eu-central-1", SSEC: true},
		"plain HTTP":       {Bucket: "backups", Region: "x", Endpoint: "http://s3.example.com", SSEC: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := &fakeRepoHost{bucket: "empty", sseKey: map[bool]string{true: "existing-sse-key-of-thirty-two-b"}[cfg == nil], sseWrite: sseWriteOKMarker}
			_, err := prepareBackupSSEC(h, cfg, sseTarget, &bucketCheck{client: h, target: sseTarget})
			require.Error(t, err)
		})
	}
}

func TestVeleroIsToldToUseTheKeySecret(t *testing.T) {
	cfg := sseConfig(true, nil)

	block := veleroBSLBlock(cfg)

	assert.Contains(t, block, "customerKeyEncryptionSecret: "+sseKeySecret+"/"+sseKeyField)
	assert.NotContains(t, block, "serverSideEncryption", "the AWS plugin refuses SSE-S3 and SSE-C together")
	assert.NotContains(t, veleroBSLBlock(sseConfig(false, nil)), "customerKeyEncryptionSecret")
}

func TestReadSSECKeyFile(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	dir := t.TempDir()
	write := func(name, content string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), mode))
		require.NoError(t, os.Chmod(p, mode))
		return p
	}

	got, err := ReadSSECKeyFile(write("ok", EncodeSSECKey(key)+"\n", 0o600))
	require.NoError(t, err)
	assert.Equal(t, key, got)

	_, err = ReadSSECKeyFile(write("short", EncodeSSECKey(key[:16]), 0o600))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), EncodeSSECKey(key[:16]), "errors never echo key material")

	_, err = ReadSSECKeyFile(write("raw", string(key), 0o600))
	require.Error(t, err, "raw bytes are not the exported format")

	if runtime.GOOS != "windows" {
		_, err = ReadSSECKeyFile(write("open", EncodeSSECKey(key), 0o644))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "chmod 600")
	}
}

const liveSSE = "backups|" + sseEndpoint + "|" + sseKeySecret + "/" + sseKeyField

func TestALostKeySecretIsRefusedRatherThanTurningSSECOff(t *testing.T) {
	h := &fakeRepoHost{liveBSL: liveSSE}

	_, err := prepareSSEC(h, &BackupStorageConfig{Bucket: "backups", Region: "hel1", Endpoint: sseEndpoint})

	require.Error(t, err, "an upgrade without the key must not switch Velero to plain objects next to encrypted ones")
	assert.Contains(t, err.Error(), "--backup-storage-sse-c-key-file")
}

func TestAKeyLeftByAnInterruptedRunIsCheckedBeforeUse(t *testing.T) {
	h := &fakeRepoHost{sseKey: string(suppliedSSEKey), liveBSL: "backups|" + sseEndpoint, bucket: "populated",
		sseWrite: sseWriteOKMarker, sseCheck: ssePlainMarker}

	_, err := prepareSSEC(h, sseConfig(false, nil))

	require.Error(t, err, "Velero kept writing plain backups while the key waited")
	assert.Contains(t, err.Error(), "written without SSE-C")
	assert.Empty(t, h.sseKey, "the unused key is removed so the next run starts clean")
}

func TestAKeyLeftByAnInterruptedRunIsActivatedOnAnEmptyBucket(t *testing.T) {
	h := &fakeRepoHost{sseKey: string(suppliedSSEKey), liveBSL: "backups|" + sseEndpoint, bucket: "empty", sseWrite: sseWriteOKMarker}

	on, err := prepareSSEC(h, sseConfig(false, nil))

	require.NoError(t, err)
	assert.True(t, on)
	assert.True(t, h.ran("name: "+sseCheckJob), "the write test runs although the key already existed")
}

func TestARecoveryWithoutTheSSECKeyIsRefused(t *testing.T) {
	for _, tc := range []struct {
		probe   string
		wantErr bool
	}{
		{sseEncryptedMarker, true},
		{ssePlainMarker, false},
		{sseNoBackupsMarker, false},
		{sseFailedMarker + " mc: <ERROR> 503", true},
	} {
		t.Run(tc.probe, func(t *testing.T) {
			h := &fakeRepoHost{bucket: "populated", sseProbe: tc.probe}

			on, err := prepareSSEC(h, sseConfig(false, nil))

			assert.False(t, on)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestAnUpgradeWithoutSSECDoesNotProbeTheBucket(t *testing.T) {
	h := &fakeRepoHost{liveBSL: "backups|" + sseEndpoint, bucket: "populated"}

	_, err := prepareSSEC(h, sseConfig(false, nil))

	require.NoError(t, err)
	assert.False(t, h.ran("name: "+sseCheckJob), "a running cluster's own bucket needs no SSE-C probe")
}

func TestARecoveryIntoABucketKipCannotCheckIsRefused(t *testing.T) {
	h := &fakeRepoHost{bucket: "fail"}

	_, err := prepareSSEC(h, sseConfig(false, nil))

	require.Error(t, err, "a supplied repository key says nothing about whether the backups need an SSE-C key")
}
