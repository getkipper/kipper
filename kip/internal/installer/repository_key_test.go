package installer

import (
	"encoding/base64"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/getkipper/kipper/kip/internal/config"
)

type fakeRepoHost struct {
	secret  string
	liveBSL string // The default backup location as "bucket|s3Url[|sseKeySecret]", or empty if absent.
	bucket  string // The simulated listing result: "empty", "populated" or "fail".

	sseKey   string // The live SSE-C key Secret's raw bytes, or empty if absent.
	sseWrite string // The SSE-C write test's log, sseWriteOKMarker when it passes.
	sseCheck string // The SSE-C key check's log.
	sseProbe string // The log of the check for SSE-C backups on an install without SSE-C.

	cmds       []string
	stdins     []string
	lastSSEJob string
}

// discoveryWarning is what kubectl prints on stderr while an aggregated API,
// such as KEDA's metrics server, restarts. Run merges it into the output.
const discoveryWarning = "E1018 12:00:00.000000 memcache.go:265] couldn't get current server API group list\n"

func (h *fakeRepoHost) Run(cmd string) (string, error) {
	out := h.run(cmd)
	if !strings.Contains(cmd, "2>/dev/null") && !strings.Contains(cmd, "logs job/") {
		out = discoveryWarning + out
	}
	return out, nil
}

func (h *fakeRepoHost) run(cmd string) string {
	h.cmds = append(h.cmds, cmd)
	switch {
	case strings.Contains(cmd, "get secret "+sseKeySecret) && strings.Contains(cmd, "jsonpath"):
		return base64.StdEncoding.EncodeToString([]byte(h.sseKey))
	case strings.Contains(cmd, "get secret "+sseKeySecret) && strings.Contains(cmd, "-o name"):
		if h.sseKey != "" {
			return "secret/" + sseKeySecret
		}
		return ""
	case strings.Contains(cmd, "delete secret "+sseKeySecret):
		h.sseKey = ""
	case strings.Contains(cmd, "get job "+sseCheckJob):
		return "1/"
	case strings.Contains(cmd, "logs job/"+sseCheckJob):
		switch {
		case strings.Contains(h.lastSSEJob, sseKeyMatchMarker):
			return h.sseCheck + "\n"
		case strings.Contains(h.lastSSEJob, sseEncryptedMarker):
			return h.sseProbe + "\n"
		}
		return h.sseWrite + "\n"
	case strings.Contains(cmd, "kubectl apply -f -") && strings.Contains(cmd, "name: "+sseCheckJob):
		h.lastSSEJob = cmd
	case strings.Contains(cmd, "/dev/urandom") && strings.Contains(cmd, sseKeySecret):
		h.sseKey = strings.Repeat("g", 32)
	case strings.Contains(cmd, "get secret "+repositoryKeySecret) && strings.Contains(cmd, "jsonpath"):
		return base64.StdEncoding.EncodeToString([]byte(h.secret))
	case strings.Contains(cmd, "get secret "+repositoryKeySecret) && strings.Contains(cmd, "-o name"):
		if h.secret != "" {
			return "secret/" + repositoryKeySecret
		}
		return ""
	case strings.Contains(cmd, "get crd backupstoragelocations.velero.io"):
		if h.liveBSL != "" {
			return "customresourcedefinition.apiextensions.k8s.io/backupstoragelocations.velero.io"
		}
		return ""
	case strings.Contains(cmd, "get backupstoragelocation default"):
		return h.liveBSL
	case strings.Contains(cmd, "get job "+bucketCheckJob):
		if h.bucket == "fail" {
			return "/1"
		}
		return "1/"
	case strings.Contains(cmd, "logs job/"+bucketCheckJob):
		switch h.bucket {
		case "populated":
			return bucketPopulatedMarker + "\n"
		case "empty":
			return bucketEmptyMarker + "\n"
		}
		return "mc: <ERROR> Access Denied.\n"
	case strings.Contains(cmd, "/dev/urandom"):
		h.secret = "generated"
	}
	return ""
}

func (h *fakeRepoHost) RunStdin(cmd string, in io.Reader) (string, error) {
	b, _ := io.ReadAll(in)
	h.stdins = append(h.stdins, string(b))
	h.cmds = append(h.cmds, cmd)
	if strings.Contains(cmd, "create secret generic "+repositoryKeySecret) {
		h.secret = string(b)
	}
	if strings.Contains(cmd, "create secret generic "+sseKeySecret) {
		h.sseKey = string(b)
	}
	return "", nil
}

func (h *fakeRepoHost) ran(part string) bool {
	for _, c := range h.cmds {
		if strings.Contains(c, part) {
			return true
		}
	}
	return false
}

var minioTarget = backupTarget{bucket: "velero", s3URL: inClusterMinIOURL, mcHost: "http://kipper:pw@minio.velero.svc:9000"} //nolint:gosec // G101: a synthetic credentials fixture

func TestRepositoryKeyIsGeneratedOnTheServerForAnEmptyBucket(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty"}

	require.NoError(t, prepareBackupRepository(h, minioTarget, &bucketCheck{client: h, target: minioTarget}, nil))

	assert.Equal(t, "generated", h.secret)
	for _, c := range h.cmds {
		if strings.Contains(c, "/dev/urandom") {
			assert.Contains(t, c, "kubectl create -f -", "create refuses to replace a Secret that appeared meanwhile")
			assert.Contains(t, c, "velero.io/exclude-from-backup=true")
		}
	}
}

func TestExistingRepositoryKeyIsKeptAndOnlyLabelled(t *testing.T) {
	h := &fakeRepoHost{secret: "old-key", liveBSL: "velero|" + inClusterMinIOURL, bucket: "populated"}

	require.NoError(t, prepareBackupRepository(h, minioTarget, &bucketCheck{client: h, target: minioTarget}, nil))

	assert.Equal(t, "old-key", h.secret)
	assert.True(t, h.ran("label secret "+repositoryKeySecret+" velero.io/exclude-from-backup=true"))
	assert.False(t, h.ran("/dev/urandom"))
	assert.False(t, h.ran(bucketCheckJob), "the location did not change, so the bucket is not listed")
}

func TestPopulatedBucketWithoutAKeyIsRefused(t *testing.T) {
	h := &fakeRepoHost{bucket: "populated"}

	err := prepareBackupRepository(h, minioTarget, &bucketCheck{client: h, target: minioTarget}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--backup-repository-key-file")
	assert.Empty(t, h.secret, "no key is invented in front of existing backups")
}

func TestPopulatedBucketTakesTheSuppliedKeyOverStdin(t *testing.T) {
	h := &fakeRepoHost{bucket: "populated"}

	require.NoError(t, prepareBackupRepository(h, minioTarget, &bucketCheck{client: h, target: minioTarget}, []byte("supplied-key")))

	assert.Equal(t, "supplied-key", h.secret)
	for _, c := range h.cmds {
		assert.NotContains(t, c, "supplied-key", "a key never appears in a command line")
	}
}

func TestBucketCheckFailureIsRefused(t *testing.T) {
	h := &fakeRepoHost{bucket: "fail"}

	err := prepareBackupRepository(h, minioTarget, &bucketCheck{client: h, target: minioTarget}, nil)

	require.Error(t, err)
	assert.Empty(t, h.secret)
}

func TestMissingKeyWithoutStorageCredentialsIsRefused(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty"}
	noCreds := backupTarget{bucket: "kipper-backups", s3URL: "https://s3.example.com"}

	err := prepareBackupRepository(h, noCreds, &bucketCheck{client: h, target: noCreds}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "--backup-repository-key-file")
	assert.Empty(t, h.secret)
}

func TestSuppliedKeyMustMatchTheExistingOne(t *testing.T) {
	same := &fakeRepoHost{secret: "the-key", liveBSL: "velero|" + inClusterMinIOURL}
	require.NoError(t, prepareBackupRepository(same, minioTarget, &bucketCheck{client: same, target: minioTarget}, []byte("the-key")))

	different := &fakeRepoHost{secret: "the-key", liveBSL: "velero|" + inClusterMinIOURL}
	err := prepareBackupRepository(different, minioTarget, &bucketCheck{client: different, target: minioTarget}, []byte("another-key"))
	require.Error(t, err)
	assert.Equal(t, "the-key", different.secret)
}

func TestMovingToAPopulatedBucketIsRefused(t *testing.T) {
	h := &fakeRepoHost{secret: "the-key", liveBSL: "old-bucket|https://s3.example.com", bucket: "populated"}
	target := backupTarget{bucket: "new-bucket", s3URL: "https://s3.example.com", mcHost: "https://AKIAEXAMPLE:secret@s3.example.com"} //nolint:gosec // G101: a synthetic credentials fixture

	err := prepareBackupRepository(h, target, &bucketCheck{client: h, target: target}, nil)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "new-bucket")
}

func TestMovingToAnEmptyBucketKeepsTheKey(t *testing.T) {
	h := &fakeRepoHost{secret: "the-key", liveBSL: "old-bucket|https://s3.example.com", bucket: "empty"}
	target := backupTarget{bucket: "new-bucket", s3URL: "https://s3.example.com", mcHost: "https://AKIAEXAMPLE:secret@s3.example.com"} //nolint:gosec // G101: a synthetic credentials fixture

	require.NoError(t, prepareBackupRepository(h, target, &bucketCheck{client: h, target: target}, nil))
	assert.Equal(t, "the-key", h.secret)
}

func TestBucketCheckKeepsStorageCredentialsOutOfCommands(t *testing.T) {
	h := &fakeRepoHost{bucket: "empty"}
	target := backupTarget{bucket: "kipper-backups", s3URL: "https://s3.example.com", mcHost: "https://AKIAEXAMPLE:ab/c+d@s3.example.com"}

	require.NoError(t, prepareBackupRepository(h, target, &bucketCheck{client: h, target: target}, nil))

	assert.Contains(t, h.stdins, "https://AKIAEXAMPLE:ab/c+d@s3.example.com")
	for _, c := range h.cmds {
		assert.NotContains(t, c, "ab/c+d")
	}
	assert.True(t, h.ran("secret/"+bucketCheckSecret+" --ignore-not-found"), "the short-lived Secret is removed")
}

func TestNewBackupTarget(t *testing.T) {
	inCluster, err := newBackupTarget(nil, "pw")
	require.NoError(t, err)
	assert.Equal(t, backupTarget{bucket: "velero", s3URL: inClusterMinIOURL, mcHost: "http://kipper:pw@minio.velero.svc:9000"}, inCluster) //nolint:gosec // G101: a synthetic credentials fixture

	// mc splits MC_HOST with a regex and does not URL-decode, so the
	// credentials go in verbatim.
	external, err := newBackupTarget(&BackupStorageConfig{Bucket: "kipper-backups", Region: "auto", Endpoint: "https://s3.example.com", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "ab/c+d"}, "")
	require.NoError(t, err)
	assert.Equal(t, "https://AKIAEXAMPLE:ab/c+d@s3.example.com", external.mcHost)
	assert.Equal(t, "https://s3.example.com", external.s3URL)

	region := "us-east-2"
	aws, err := newBackupTarget(&BackupStorageConfig{Bucket: "kipper-backups", Region: region, AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "s"}, "")
	require.NoError(t, err)
	assert.Equal(t, "https://AKIAEXAMPLE:s@s3."+region+".amazonaws.com", aws.mcHost, "native AWS uses the regional endpoint")
	assert.Equal(t, "", aws.s3URL)

	upgrade, err := newBackupTarget(&BackupStorageConfig{Bucket: "kipper-backups", Region: "auto", Endpoint: "https://s3.example.com"}, "")
	require.NoError(t, err)
	assert.Empty(t, upgrade.mcHost, "an upgrade has no storage credentials to list with")

	for _, bad := range []*BackupStorageConfig{
		{Bucket: "kipper;reboot", Region: "auto", Endpoint: "https://s3.example.com"},
	} {
		_, err := newBackupTarget(bad, "")
		assert.Error(t, err, "%+v", bad)
	}
}

func TestCredentialsMcCannotParseLeaveTheBucketUnlistable(t *testing.T) {
	for _, secret := range []string{"a:b", "a@b"} {
		target, err := newBackupTarget(&BackupStorageConfig{Bucket: "kipper-backups", Region: "auto", Endpoint: "https://s3.example.com", AccessKeyID: "AKIA", SecretAccessKey: secret}, "")
		require.NoError(t, err)
		assert.Empty(t, target.mcHost)
		assert.NotEmpty(t, target.noList)
	}
}

func writeKeyFile(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "repository.key")
	require.NoError(t, os.WriteFile(path, []byte(content), mode))
	require.NoError(t, os.Chmod(path, mode))
	return path
}

func TestReadRepositoryKeyFile(t *testing.T) {
	key, err := ReadRepositoryKeyFile(writeKeyFile(t, "s3cret-key\n", 0o600))
	require.NoError(t, err)
	assert.Equal(t, []byte("s3cret-key"), key, "one trailing newline from an editor or echo is dropped")

	key, err = ReadRepositoryKeyFile(writeKeyFile(t, "s3cret-key\r\n", 0o600))
	require.NoError(t, err)
	assert.Equal(t, []byte("s3cret-key"), key)

	_, err = ReadRepositoryKeyFile(writeKeyFile(t, "\n", 0o600))
	assert.Error(t, err, "an empty key is refused")

	_, err = ReadRepositoryKeyFile(writeKeyFile(t, strings.Repeat("k", maxRepositoryKeyBytes+1), 0o600))
	assert.Error(t, err, "an oversized file is refused")

	_, err = ReadRepositoryKeyFile(t.TempDir())
	assert.Error(t, err, "a directory is refused")

	if runtime.GOOS != "windows" {
		_, err = ReadRepositoryKeyFile(writeKeyFile(t, "s3cret-key", 0o640))
		require.Error(t, err, "a key others can read is refused, as ssh refuses such a private key")
		assert.Contains(t, err.Error(), "chmod 600")
	}
}

func TestWriteRepositoryKeyFileRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "repository.key")
	require.NoError(t, WriteRepositoryKeyFile(path, []byte("s3cret-key")))

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}
	key, err := ReadRepositoryKeyFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("s3cret-key"), key)

	assert.Error(t, WriteRepositoryKeyFile(path, []byte("other")), "an existing file is never overwritten")
	key, _ = ReadRepositoryKeyFile(path)
	assert.Equal(t, []byte("s3cret-key"), key)
}

func TestWriteRepositoryKeyFileDoesNotFollowASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	link := filepath.Join(dir, "repository.key")
	require.NoError(t, os.Symlink(target, link))

	assert.Error(t, WriteRepositoryKeyFile(link, []byte("s3cret-key")))
	_, err := os.Stat(target)
	assert.True(t, os.IsNotExist(err), "nothing is written through the link")
}

func TestIsDefaultRepositoryKey(t *testing.T) {
	assert.True(t, IsDefaultRepositoryKey([]byte("static-passw0rd")))
	assert.False(t, IsDefaultRepositoryKey([]byte("Zm9vYmFy")))
}

func TestRefusedMoveLeavesTheLiveCredentialsAlone(t *testing.T) {
	h := &fakeVeleroHost{
		osRelease: "ID=ubuntu\nID_LIKE=debian\nVERSION_ID=\"24.04\"\n",
		liveBSL:   "old-bucket|https://s3.example.com",
		bucket:    bucketPopulatedMarker,
	}
	cfg := &BackupStorageConfig{Bucket: "new-bucket", Region: "auto", Endpoint: "https://s3.example.com", AccessKeyID: "AKIAEXAMPLE", SecretAccessKey: "s"}

	require.Error(t, installVelero(h, cfg, "", nil))

	for _, cmd := range h.cmds {
		assert.NotContains(t, cmd, "create secret generic cloud-credentials", "the live Secret keeps the old bucket's credentials")
	}
}

func TestKeysThatCannotRoundTripAreRefused(t *testing.T) {
	_, err := ReadRepositoryKeyFile(writeKeyFile(t, "secret\n\n", 0o600))
	assert.Error(t, err, "a second line break would not survive export and import")

	_, err = ReadRepositoryKeyFile(writeKeyFile(t, "sec\x00ret", 0o600))
	assert.Error(t, err)

	assert.Error(t, CheckRepositoryKey([]byte("secret\n")))
	assert.Error(t, CheckRepositoryKey([]byte(strings.Repeat("k", maxRepositoryKeyBytes+1))))
	assert.NoError(t, CheckRepositoryKey([]byte("static-passw0rd")))
	assert.NoError(t, CheckRepositoryKey([]byte("Zm9vYmFyYmF6cXV4Zm9vYmFyYmF6cXV4Zm9vYmFyYmE=")))
}

func TestKubectlWarningsDoNotLookLikeAMove(t *testing.T) {
	h := &fakeRepoHost{secret: "old-key", liveBSL: "velero|" + inClusterMinIOURL}

	require.NoError(t, prepareBackupRepository(h, minioTarget, &bucketCheck{client: h, target: minioTarget}, nil))
	assert.Equal(t, "old-key", h.secret)
	assert.False(t, h.ran(bucketCheckJob))
}

func TestCredentialsMcCannotParseAreOnlyRefusedWhenAListingIsNeeded(t *testing.T) {
	cfg := &BackupStorageConfig{Bucket: "kipper-backups", Region: "auto", Endpoint: "https://s3.example.com", AccessKeyID: "minio", SecretAccessKey: "p@ss:word"}
	target, err := newBackupTarget(cfg, "")
	require.NoError(t, err)

	existing := &fakeRepoHost{secret: "the-key", liveBSL: "kipper-backups|https://s3.example.com"}
	require.NoError(t, prepareBackupRepository(existing, target, &bucketCheck{client: existing, target: target}, nil), "an unchanged location needs no listing")

	fresh := &fakeRepoHost{bucket: "empty"}
	err = prepareBackupRepository(fresh, target, &bucketCheck{client: fresh, target: target}, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "':' or '@'")
}

func TestATrailingSlashOnTheEndpointIsNotAMove(t *testing.T) {
	cfg := &BackupStorageConfig{Bucket: "kipper-backups", Region: "auto", Endpoint: "https://s3.example.com/"}
	target, err := newBackupTarget(cfg, "")
	require.NoError(t, err)

	h := &fakeRepoHost{secret: "the-key", liveBSL: "kipper-backups|https://s3.example.com"}
	require.NoError(t, prepareBackupRepository(h, target, &bucketCheck{client: h, target: target}, nil))
}

func TestKeysWithSurroundingSpacesAreRefused(t *testing.T) {
	// Velero trims the key it reads, so these would not be the key it uses.
	assert.Error(t, CheckRepositoryKey([]byte(" key")))
	assert.Error(t, CheckRepositoryKey([]byte("key ")))
	assert.NoError(t, CheckRepositoryKey([]byte("two words")))
	assert.True(t, IsDefaultRepositoryKey([]byte("static-passw0rd\n")))
}

func TestResticRepositoriesCountAsBackups(t *testing.T) {
	assert.Contains(t, bucketCheckManifest("kipper-backups"), "(backups|kopia|restic)/$")
}

func TestRecordBackupStorageUpdatesOnlyThatEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USERPROFILE", os.Getenv("HOME"))
	require.NoError(t, config.Update(func(cfg *config.Config) error {
		cfg.AddCluster(config.Cluster{Name: "lab", BackupStorage: &config.BackupStorageRef{Mode: "in-cluster"}})
		cfg.AddCluster(config.Cluster{Name: "other", BackupStorage: &config.BackupStorageRef{Mode: "in-cluster"}})
		return nil
	}))

	require.NoError(t, recordBackupStorage("lab", &config.BackupStorageRef{Mode: "external", Bucket: "kipper-backups", Region: "eu-west-1"}))

	cfg, err := config.Load()
	require.NoError(t, err)
	assert.Equal(t, "kipper-backups", cfg.GetCluster("lab").BackupStorage.Bucket)
	assert.Equal(t, "in-cluster", cfg.GetCluster("other").BackupStorage.Mode)
}

func TestUppercaseBucketNamesStillUpgrade(t *testing.T) {
	_, err := newBackupTarget(&BackupStorageConfig{Bucket: "Kipper_Backups", Region: "auto", Endpoint: "https://s3.example.com"}, "")
	assert.NoError(t, err, "older S3 and some providers allow these names, and an upgrade validates the saved one")
}

func TestKeysWithSurroundingUnicodeSpaceAreRefused(t *testing.T) {
	assert.Error(t, CheckRepositoryKey([]byte(" secret-key")))
	assert.Error(t, CheckRepositoryKey([]byte("secret-key ")))
}

func TestSuppliedKeyMatchesALiveKeyWithSurroundingWhitespace(t *testing.T) {
	// Velero trims the live key, and so does export, so the exported key matches.
	h := &fakeRepoHost{secret: "\n the-key\r\n", liveBSL: "velero|" + inClusterMinIOURL} //nolint:gosec // G101: a synthetic key fixture
	require.NoError(t, prepareBackupRepository(h, minioTarget, &bucketCheck{client: h, target: minioTarget}, []byte("the-key")))

	other := &fakeRepoHost{secret: "\nthe-key-2\n", liveBSL: "velero|" + inClusterMinIOURL} //nolint:gosec // G101: a synthetic key fixture
	assert.Error(t, prepareBackupRepository(other, minioTarget, &bucketCheck{client: other, target: minioTarget}, []byte("the-key")))
}

func TestKeysThatAreNotUTF8AreRefused(t *testing.T) {
	assert.Error(t, CheckRepositoryKey([]byte{'a', 0xff, 'b'}), "JSON would change these bytes on their way to the server")
}

func TestAnEndpointWithAPathOnlyBlocksListing(t *testing.T) {
	target, err := newBackupTarget(&BackupStorageConfig{Bucket: "kipper-backups", Region: "auto", Endpoint: "https://s3.example.com/path", AccessKeyID: "AKIA", SecretAccessKey: "s"}, "")
	require.NoError(t, err, "an upgrade must not fail on a saved endpoint Velero already uses")
	assert.NotEmpty(t, target.noList)
}

func TestSensitiveReadsKeepTheValueOutOfErrors(t *testing.T) {
	const encoded = "c2VjcmV0LXZhbHVl" // base64 of "secret-value"
	failing := func(contains string) *routedRunner {
		return &routedRunner{routes: []route{{contains: contains, out: encoded, err: errors.New("connection lost: " + encoded)}}}
	}

	err := requireSameRepositoryKey(failing("get secret "+repositoryKeySecret), []byte("other"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), encoded)
	assert.NotContains(t, err.Error(), "secret-value")

	_, err = ensureMinIOPassword(failing("get secret " + minioCredentialsSecret))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), encoded)

	_, err = readSecretData(failing("get secret"), "hop-ca")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), encoded)

	_, err = readSecretKey(failing("get secret"), "hop-ca", "tls.key")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), encoded)

	_, err = ReadGatewayCredentials(failing("get secret"))
	require.Error(t, err)
	assert.NotContains(t, err.Error(), encoded)

	_, err = readSecretValue(failing("get secret"), "kipper-system", "example", "token")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), encoded)
}
