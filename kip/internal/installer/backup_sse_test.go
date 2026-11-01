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
	on, _, err := prepareBackupSSEC(h, cfg, sseTarget, &bucketCheck{client: h, target: sseTarget})
	return on, err
}

var suppliedSSEKey = []byte("supplied-sse-key-of-thirty-two-b")

func TestSSECStaysOffWithoutCredentials(t *testing.T) {
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
	assert.NotContains(t, err.Error(), "--backup-storage-sse-c=false", "SSE-C was asked for, so turning it off is not the advice")
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
	h := &fakeRepoHost{sseKey: "another-sse-key-of-thirty-two-by", liveBSL: liveSSE}

	_, err := prepareSSEC(h, sseConfig(true, suppliedSSEKey))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "differs from the SSE-C key this cluster's backup location uses")
	assert.Equal(t, "another-sse-key-of-thirty-two-by", h.sseKey)
}

const unusedSSEKey = "another-sse-key-of-thirty-two-by"

// TestAKeyFileMovesAnUnusedKeyAside preserves the old key because it may still
// protect backups elsewhere, even when the imported key opens this bucket.
func TestAKeyFileMovesAnUnusedKeyAside(t *testing.T) {
	h := &fakeRepoHost{sseKey: unusedSSEKey, bucket: "populated", sseWrite: sseWriteOKMarker, sseCheck: sseKeyMatchMarker}

	on, notice, err := prepareSSECNotice(h, sseConfig(false, suppliedSSEKey))

	require.NoError(t, err)
	assert.True(t, on)
	assert.Equal(t, string(suppliedSSEKey), h.sseKey)
	require.Len(t, h.ssePrevious, 1)
	for name, key := range h.ssePrevious {
		assert.Regexp(t, `^`+sseKeySecret+`-previous-\d{14}$`, name)
		assert.Equal(t, unusedSSEKey, key)
		assert.Contains(t, notice, "velero/"+name)
	}
	assert.Contains(t, h.lastSSEJob, "secretName: "+sseCandidateSecret, "the file's key is checked before the old one moves")
	assert.Empty(t, h.sseCandidate)
}

func TestAKeyFileThatDoesNotOpenTheBucketLeavesTheUnusedKey(t *testing.T) {
	h := &fakeRepoHost{sseKey: unusedSSEKey, bucket: "populated", sseWrite: sseWriteOKMarker, sseCheck: sseKeyWrongMarker}

	_, _, err := prepareSSECNotice(h, sseConfig(false, suppliedSSEKey))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not open")
	assert.Equal(t, unusedSSEKey, h.sseKey)
	assert.Empty(t, h.ssePrevious)
	assert.Empty(t, h.sseCandidate)
}

// TestAFailedCandidateRemovalIsNamed ensures users can find the copied key
// when cleanup fails, whether activation succeeds or fails.
func TestAFailedCandidateRemovalIsNamed(t *testing.T) {
	for _, tc := range []struct {
		check   string
		wantErr bool
	}{
		{sseKeyMatchMarker, false},
		{sseKeyWrongMarker, true},
	} {
		t.Run(tc.check, func(t *testing.T) {
			h := &fakeRepoHost{sseKey: unusedSSEKey, bucket: "populated", sseWrite: sseWriteOKMarker, sseCheck: tc.check, failCandidateRm: true}

			_, notice, err := prepareSSECNotice(h, sseConfig(false, suppliedSSEKey))

			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "does not open")
				assert.Contains(t, err.Error(), "velero/"+sseCandidateSecret)
				return
			}
			require.NoError(t, err)
			assert.Contains(t, notice, "velero/"+sseCandidateSecret)
		})
	}
}

func TestAKeyFileIsRefusedWhenTheUnusedKeyCannotMove(t *testing.T) {
	h := &fakeRepoHost{sseKey: unusedSSEKey, bucket: "populated", sseWrite: sseWriteOKMarker, sseCheck: sseKeyMatchMarker, failSSECopy: true}

	_, _, err := prepareSSECNotice(h, sseConfig(false, suppliedSSEKey))

	require.Error(t, err)
	assert.Equal(t, unusedSSEKey, h.sseKey, "the old key stays where it is until a copy of it exists")
	assert.Empty(t, h.sseCandidate)
	assert.NotContains(t, err.Error(), EncodeSSECKey([]byte(unusedSSEKey)), "errors never echo key material")
}

func TestAFailedKeyReplacementDoesNotEchoTheKey(t *testing.T) {
	h := &fakeRepoHost{sseKey: unusedSSEKey, bucket: "populated", sseWrite: sseWriteOKMarker, sseCheck: sseKeyMatchMarker, failSSEReplace: true}

	_, _, err := prepareSSECNotice(h, sseConfig(false, suppliedSSEKey))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "velero/"+sseKeySecret+"-previous-", "the operator needs to know where the old key's copy is")
	assert.NotContains(t, err.Error(), EncodeSSECKey(suppliedSSEKey), "errors never echo key material")
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
			h := &fakeRepoHost{bucket: "empty", sseWrite: sseWriteOKMarker}
			if cfg == nil {
				// A cluster whose backups use SSE-C cannot move them to MinIO.
				h.sseKey = "existing-sse-key-of-thirty-two-b"
				h.liveBSL = "backups|" + sseEndpoint + "|" + sseKeySecret + "/" + sseKeyField
			}
			_, _, err := prepareBackupSSEC(h, cfg, sseTarget, &bucketCheck{client: h, target: sseTarget})
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

	_, err := prepareSSEC(h, sseConfig(true, nil))

	require.Error(t, err, "Velero kept writing plain backups while the key waited")
	assert.Contains(t, err.Error(), "written without SSE-C")
	assert.Equal(t, string(suppliedSSEKey), h.sseKey, "a key kip did not create in this run may protect backups elsewhere")
}

func TestAKeyLeftByAnInterruptedRunIsActivatedOnAnEmptyBucket(t *testing.T) {
	h := &fakeRepoHost{sseKey: string(suppliedSSEKey), bucket: "empty", sseWrite: sseWriteOKMarker}

	on, notice, err := prepareSSECNotice(h, installSSEConfig(sseEndpoint))

	require.NoError(t, err)
	assert.True(t, on)
	assert.True(t, h.ran("name: "+sseCheckJob), "the write test runs although the key already existed")
	assert.Equal(t, string(suppliedSSEKey), h.sseKey, "the key that was left is the one put to use")
	assert.False(t, h.ran("/dev/urandom"))
	assert.Contains(t, notice, "kip backup sse-key export")
}

// TestALeftoverKeyStaysUnusedOnARunThatWouldNotTrySSEC covers automatic
// activation. Upgrades and reruns targeting a live plain location preserve
// the unused key without activating it.
func TestALeftoverKeyStaysUnusedOnARunThatWouldNotTrySSEC(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cfg     *BackupStorageConfig
		liveBSL string
		bucket  string
	}{
		{"upgrade, empty bucket", sseConfig(false, nil), "backups|" + sseEndpoint, "empty"},
		{"upgrade, plain backups", sseConfig(false, nil), "backups|" + sseEndpoint, "populated"},
		{"rerun over a running plain location", installSSEConfig(sseEndpoint), "backups|" + sseEndpoint, "empty"},
		{"native AWS S3", installSSEConfig(""), "", "empty"},
		{"plain HTTP", installSSEConfig("http://10.0.0.5:9000"), "", "empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeRepoHost{sseKey: string(suppliedSSEKey), liveBSL: tc.liveBSL, bucket: tc.bucket,
				sseProbe: ssePlainMarker, sseWrite: sseWriteOKMarker, sseCheck: ssePlainMarker}

			on, notice, err := prepareSSECNotice(h, tc.cfg)

			require.NoError(t, err)
			assert.False(t, on)
			assert.Equal(t, string(suppliedSSEKey), h.sseKey)
			assert.Contains(t, notice, "no backup location uses")
			assert.False(t, h.ran("kipper-sse-check-"), "a key no run asked for is not tested against a running location")
		})
	}
}

func TestTurningSSECOffKeepsTheKeyAcrossLaterRuns(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseKey: string(suppliedSSEKey), sseWrite: sseWriteOKMarker}
	off := installSSEConfig(sseEndpoint)
	off.SSECOff = true

	for _, cfg := range []*BackupStorageConfig{off, sseConfig(false, nil), installSSEConfig(sseEndpoint)} {
		on, notice, err := prepareSSECNotice(h, cfg)

		require.NoError(t, err)
		assert.False(t, on)
		assert.Equal(t, string(suppliedSSEKey), h.sseKey)
		assert.Contains(t, notice, "no backup location uses")
		// Velero now writes plain backups to the bucket.
		h.liveBSL = "backups|" + sseEndpoint
	}
	assert.False(t, h.ran("kipper-sse-check-"))
}

func TestALeftoverKeyOnAStoreWithoutSSECStaysUnused(t *testing.T) {
	h := &fakeRepoHost{sseKey: string(suppliedSSEKey), bucket: "empty", sseWrite: sseUnsupportedMarker + " the store served the test object without the key"}

	on, notice, err := prepareSSECNotice(h, installSSEConfig(sseEndpoint))

	require.NoError(t, err)
	assert.False(t, on)
	assert.Equal(t, string(suppliedSSEKey), h.sseKey)
	assert.Contains(t, notice, "SSE-C stays off")
}

func TestALeftoverKeyOverPlainBackupsStaysUnused(t *testing.T) {
	h := &fakeRepoHost{sseKey: string(suppliedSSEKey), bucket: "populated", sseProbe: ssePlainMarker, sseWrite: sseWriteOKMarker}

	on, notice, err := prepareSSECNotice(h, installSSEConfig(sseEndpoint))

	require.NoError(t, err)
	assert.False(t, on)
	assert.Equal(t, string(suppliedSSEKey), h.sseKey)
	assert.Contains(t, notice, "already holds backup data")
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

// installSSEConfig is external storage as kip install passes it: with
// credentials and without an SSE-C flag.
func installSSEConfig(endpoint string) *BackupStorageConfig {
	return &BackupStorageConfig{Bucket: "backups", Region: "hel1", Endpoint: endpoint, AccessKeyID: "ak", SecretAccessKey: "sk"}
}

func prepareSSECNotice(h *fakeRepoHost, cfg *BackupStorageConfig) (bool, string, error) {
	return prepareBackupSSEC(h, cfg, sseTarget, &bucketCheck{client: h, target: sseTarget})
}

func TestAnInstallTurnsSSECOnWhereTheStoreSupportsIt(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseWrite: sseWriteOKMarker}

	on, notice, err := prepareSSECNotice(h, installSSEConfig(sseEndpoint))

	require.NoError(t, err)
	assert.True(t, on)
	assert.NotEmpty(t, h.sseKey)
	assert.Contains(t, notice, "kip backup sse-key export", "the key only lives in the cluster until it is exported")
}

func TestAnInstallOntoPlainBackupsLeavesSSECOffWithAWarning(t *testing.T) {
	h := &fakeRepoHost{bucket: "populated", sseProbe: ssePlainMarker, sseWrite: sseWriteOKMarker}

	on, notice, err := prepareSSECNotice(h, installSSEConfig(sseEndpoint))

	require.NoError(t, err)
	assert.False(t, on)
	assert.Empty(t, h.sseKey)
	assert.Contains(t, notice, "already holds backup data")
}

func TestAStoreWithoutSSECGetsAWarningInsteadOfAnError(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseWrite: sseUnsupportedMarker + " the store served the test object without the key"}

	on, notice, err := prepareSSECNotice(h, installSSEConfig(sseEndpoint))

	require.NoError(t, err)
	assert.False(t, on)
	assert.Empty(t, h.sseKey, "a key the store cannot use must not be left for the next run")
	assert.Contains(t, notice, "--backup-storage-sse-c", "the warning names the flag that makes SSE-C a requirement")
}

// TestARunningPlainLocationKeepsSSECOff avoids automatic activation at a live
// target because Velero could write plain objects during the checks.
func TestARunningPlainLocationKeepsSSECOff(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", liveBSL: "backups|" + sseEndpoint, sseWrite: sseWriteOKMarker}

	on, notice, err := prepareSSECNotice(h, installSSEConfig(sseEndpoint))

	require.NoError(t, err)
	assert.False(t, on)
	assert.Empty(t, h.sseKey)
	assert.False(t, h.ran("name: "+sseCheckJob))
	assert.Contains(t, notice, "already writes backups")
}

// TestMovingToAnEmptyBucketTurnsSSECOn covers an encrypted start at a new
// location while Velero keeps writing to the old one until the switch.
func TestMovingToAnEmptyBucketTurnsSSECOn(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", liveBSL: "velero|" + inClusterMinIOURL, sseWrite: sseWriteOKMarker}

	on, notice, err := prepareSSECNotice(h, installSSEConfig(sseEndpoint))

	require.NoError(t, err)
	assert.True(t, on)
	assert.NotEmpty(t, h.sseKey)
	assert.Contains(t, notice, "kip backup sse-key export")
}

// TestAWriteTestThatCouldNotRunStopsTheInstall prevents transient failures
// from permanently leaving a bucket without SSE-C.
func TestAWriteTestThatCouldNotRunStopsTheInstall(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseWrite: "mc: <ERROR> Unable to stat. 503 Service Unavailable."}

	on, _, err := prepareSSECNotice(h, installSSEConfig(sseEndpoint))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not run")
	assert.Contains(t, err.Error(), "Retry, or pass --backup-storage-sse-c=false if the store refuses SSE-C")
	assert.False(t, on)
	assert.Empty(t, h.sseKey, "the key this run created goes again")
}

func TestAFailedKeyGenerationStopsTheInstall(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseWrite: sseWriteOKMarker, failSSEGenerate: true}

	on, _, err := prepareSSECNotice(h, installSSEConfig(sseEndpoint))

	require.Error(t, err)
	assert.False(t, on)
}

// TestTurningSSECOffKeepsAnUnusedKey preserves keys that may protect
// backups elsewhere.
func TestTurningSSECOffKeepsAnUnusedKey(t *testing.T) {
	for _, tc := range []struct {
		name   string
		bucket string
		probe  string
	}{
		{"empty bucket", "empty", ""},
		{"plain backups", "populated", ssePlainMarker},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &fakeRepoHost{bucket: tc.bucket, sseKey: string(suppliedSSEKey), sseProbe: tc.probe, sseWrite: sseWriteOKMarker}
			cfg := installSSEConfig(sseEndpoint)
			cfg.SSECOff = true

			on, notice, err := prepareSSECNotice(h, cfg)

			require.NoError(t, err)
			assert.False(t, on)
			assert.Equal(t, string(suppliedSSEKey), h.sseKey)
			assert.Contains(t, notice, "no backup location uses")
			assert.Contains(t, notice, "--backup-storage-sse-c=false")
			assert.NotContains(t, notice, "stays on")
		})
	}
}

func TestTurningSSECOffOverSSECBackupsIsRefused(t *testing.T) {
	h := &fakeRepoHost{bucket: "populated", sseKey: string(suppliedSSEKey), sseProbe: sseEncryptedMarker}
	cfg := installSSEConfig(sseEndpoint)
	cfg.SSECOff = true

	_, _, err := prepareSSECNotice(h, cfg)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "written with SSE-C")
	assert.Contains(t, err.Error(), "velero/"+sseKeySecret, "the cluster may already hold the key those backups need")
	assert.Equal(t, string(suppliedSSEKey), h.sseKey)
}

func TestInClusterMinIOAfterAnInterruptedExternalInstall(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseKey: string(suppliedSSEKey)}

	on, notice, err := prepareBackupSSEC(h, nil, minioTarget, &bucketCheck{client: h, target: minioTarget})

	require.NoError(t, err)
	assert.False(t, on)
	assert.Equal(t, string(suppliedSSEKey), h.sseKey, "the key may belong to backups in the external bucket")
	assert.Contains(t, notice, "no backup location uses")
	assert.Contains(t, notice, "MinIO")
}

func TestTurningSSECOffWarns(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseWrite: sseWriteOKMarker}
	cfg := installSSEConfig(sseEndpoint)
	cfg.SSECOff = true

	on, notice, err := prepareSSECNotice(h, cfg)

	require.NoError(t, err)
	assert.False(t, on)
	assert.Empty(t, h.sseKey)
	assert.False(t, h.ran("name: "+sseCheckJob), "SSE-C was turned off, so the store is not tested for it")
	assert.Contains(t, notice, "--backup-storage-sse-c=false")
}

func TestTurningSSECOffKeepsAnActiveKey(t *testing.T) {
	h := &fakeRepoHost{sseKey: string(suppliedSSEKey), liveBSL: "backups|" + sseEndpoint + "|" + sseKeySecret + "/" + sseKeyField}
	cfg := installSSEConfig(sseEndpoint)
	cfg.SSECOff = true

	on, notice, err := prepareSSECNotice(h, cfg)

	require.NoError(t, err)
	assert.True(t, on, "turning SSE-C off would leave the existing backups unreadable to Velero")
	assert.Contains(t, notice, "stays on")
}

func TestAPlainHTTPStoreLeavesSSECOffWithAWarning(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseWrite: sseWriteOKMarker}

	on, notice, err := prepareSSECNotice(h, installSSEConfig("http://10.0.0.5:9000"))

	require.NoError(t, err)
	assert.False(t, on)
	assert.Empty(t, h.sseKey)
	assert.Contains(t, notice, "HTTPS")
}

func TestNativeAWSS3NeedsNoSSEC(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty", sseWrite: sseWriteOKMarker}

	on, notice, err := prepareSSECNotice(h, installSSEConfig(""))

	require.NoError(t, err)
	assert.False(t, on)
	assert.Empty(t, notice, "AWS S3 encrypts the backups with SSE-S3")
	assert.False(t, h.ran("name: "+sseCheckJob))
}

func TestInClusterMinIOBackupsWarn(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty"}

	on, notice, err := prepareBackupSSEC(h, nil, minioTarget, &bucketCheck{client: h, target: minioTarget})

	require.NoError(t, err)
	assert.False(t, on)
	assert.Contains(t, notice, "MinIO")
}

func TestAnUpgradeWithoutCredentialsStaysQuiet(t *testing.T) {
	h := &fakeRepoHost{liveBSL: "backups|" + sseEndpoint, bucket: "empty", sseWrite: sseWriteOKMarker}

	on, notice, err := prepareSSECNotice(h, sseConfig(false, nil))

	require.NoError(t, err)
	assert.False(t, on)
	assert.Empty(t, notice, "the install already warned, and an upgrade cannot test the store without credentials")
	assert.Empty(t, h.sseKey)
}
