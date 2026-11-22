package installer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeCredsFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "credentials")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

func TestLoadAWSCredentials_Default(t *testing.T) {
	path := writeCredsFile(t, `[default]
aws_access_key_id = AKIADEFAULT
aws_secret_access_key = secretdefault
`)
	got, err := LoadAWSCredentials(path, "default")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	assert.Equal(t, "AKIADEFAULT", got.AccessKeyID)
	assert.Equal(t, "secretdefault", got.SecretAccessKey)
}

func TestLoadAWSCredentials_NamedProfile(t *testing.T) {
	path := writeCredsFile(t, `[default]
aws_access_key_id = AKIADEFAULT
aws_secret_access_key = secretdefault

[acme]
aws_access_key_id = AKIAEXAMPLE
aws_secret_access_key = secretvalue
`)
	got, err := LoadAWSCredentials(path, "acme")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	assert.Equal(t, "AKIAEXAMPLE", got.AccessKeyID)
	assert.Equal(t, "secretvalue", got.SecretAccessKey)
}

func TestLoadAWSCredentials_ProfilePrefixForm(t *testing.T) {
	// ~/.aws/config uses [profile NAME] but the same parser should work
	// when a user points us at it instead of ~/.aws/credentials.
	path := writeCredsFile(t, `[profile acme]
aws_access_key_id = AKIAEXAMPLE
aws_secret_access_key = secretvalue
`)
	got, err := LoadAWSCredentials(path, "acme")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	assert.Equal(t, "AKIAEXAMPLE", got.AccessKeyID)
	assert.Equal(t, "secretvalue", got.SecretAccessKey)
}

func TestLoadAWSCredentials_EmptyProfileMeansDefault(t *testing.T) {
	path := writeCredsFile(t, `[default]
aws_access_key_id = AKIADEFAULT
aws_secret_access_key = secretdefault
`)
	got, err := LoadAWSCredentials(path, "")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	assert.Equal(t, "AKIADEFAULT", got.AccessKeyID)
}

func TestLoadAWSCredentials_MissingProfile(t *testing.T) {
	path := writeCredsFile(t, `[default]
aws_access_key_id = AKIADEFAULT
aws_secret_access_key = secretdefault
`)
	_, err := LoadAWSCredentials(path, "nope")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), `profile "nope"`)
	}
}

func TestLoadAWSCredentials_MissingFile(t *testing.T) {
	_, err := LoadAWSCredentials("/tmp/does-not-exist-kipper-test", "default")
	assert.Error(t, err)
}

func TestLoadAWSCredentials_IgnoresCommentsAndBlankLines(t *testing.T) {
	path := writeCredsFile(t, `# this is a comment
; this too

[default]
# inside the section
aws_access_key_id = AKIA
aws_secret_access_key = secret

[other]
`)
	got, err := LoadAWSCredentials(path, "default")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	assert.Equal(t, "AKIA", got.AccessKeyID)
}

func TestLoadAWSCredentials_HandlesExtraWhitespace(t *testing.T) {
	path := writeCredsFile(t, `   [acme]
   aws_access_key_id   =   AKIAEXAMPLE
   aws_secret_access_key=secretvalue
`)
	got, err := LoadAWSCredentials(path, "acme")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	assert.Equal(t, "AKIAEXAMPLE", got.AccessKeyID)
	assert.Equal(t, "secretvalue", got.SecretAccessKey)
}

func TestLoadAWSCredentials_IncompleteProfileErrors(t *testing.T) {
	path := writeCredsFile(t, `[default]
aws_access_key_id = AKIA
`)
	_, err := LoadAWSCredentials(path, "default")
	if assert.Error(t, err) {
		assert.Contains(t, err.Error(), "missing")
	}
}

func TestVeleroCredentialsBlock(t *testing.T) {
	c := &AWSCredentials{AccessKeyID: "AKIA", SecretAccessKey: "secret"}
	got := c.VeleroCredentialsBlock()
	want := "[default]\naws_access_key_id = AKIA\naws_secret_access_key = secret\n"
	assert.Equal(t, want, got)
}

func TestPlainHTTPBackupEndpointsMustStayOnAPrivateNetwork(t *testing.T) {
	for _, endpoint := range []string{
		"",
		"https://objects.example.com",
		"HTTPS://objects.example.com:8443",
		"http://127.0.0.1:9000",
		"http://localhost:9000",
		"http://10.0.0.5:9000",
		"http://192.168.1.20:9000",
		"http://[fd00::5]:9000",
		"http://[fe80::1%25eth0]:9000",
		"http://[::ffff:10.0.0.5]:9000",
		"http://minio.velero.svc:9000",
		"http://minio.storage.svc.cluster.local:9000",
		"http://minio:9000",
	} {
		assert.NoError(t, CheckBackupEndpoint(endpoint), endpoint)
	}
	for _, endpoint := range []string{
		"http://objects.example.com",
		"http://203.0.113.7:9000",
		"http://[2001:db8::7]:9000",
		"http://[2001:db8::7%25eth0]:9000",
		"http://3405803783:9000",
		"http://0xcb007107:9000",
		"http://0.0.0.0:9000",
		"objects.example.com",
		"ftp://objects.example.com",
		"https://",
	} {
		assert.Error(t, CheckBackupEndpoint(endpoint), endpoint)
	}
}

// A Service in another namespace is a common plain HTTP target, so the error
// gives an accepted Service name format and a private IP alternative for other hosts.
func TestARefusedEndpointNamesTheAcceptedServiceForm(t *testing.T) {
	err := CheckBackupEndpoint("http://minio.storage:9000")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "minio.storage.svc")
	assert.Contains(t, err.Error(), "private IP address")
}

func TestAnEndpointWithUserInfoIsRefused(t *testing.T) {
	for _, endpoint := range []string{"https://key:secret@s3.example.com", "https://usersentinel@s3.example.com", "http://key:secret@10.0.0.5:9000"} {
		err := CheckBackupEndpoint(endpoint)

		require.Error(t, err, endpoint)
		assert.NotContains(t, err.Error(), "secret", endpoint)
		assert.NotContains(t, err.Error(), "usersentinel", endpoint)
		assert.Contains(t, err.Error(), "credentials file", endpoint)
	}
}

func TestARefusedEndpointNeverEchoesCredentials(t *testing.T) {
	for _, endpoint := range []string{
		"https://key:secret@s3.example.com:bad",
		"https://key:secret@",
		"ftp://key:secret@s3.example.com",
		"http://objects.example.com/?X-Amz-Signature=secret",
	} {
		err := CheckBackupEndpoint(endpoint)

		require.Error(t, err, endpoint)
		assert.NotContains(t, err.Error(), "secret", endpoint)
	}
}

func TestTheStoredPlainEndpointWarningWithholdsCredentials(t *testing.T) {
	warning := PlainEndpointWarning(&BackupStorageConfig{Endpoint: "http://key:secret@objects.example.com"}) //nolint:gosec // G101: a synthetic credentials fixture
	assert.Contains(t, warning, "http://objects.example.com")
	assert.NotContains(t, warning, "secret")

	assert.Empty(t, PlainEndpointWarning(&BackupStorageConfig{Endpoint: "http://key:secret@10.0.0.5:9000"}), "a private host keeps plain HTTP") //nolint:gosec // G101: a synthetic credentials fixture
}

// Records written before URL user information was rejected may still contain it,
// so location messages omit it.
func TestAStorageLocationNamesItsEndpointWithoutCredentials(t *testing.T) {
	l := StorageLocation{Bucket: "backups", Endpoint: "https://key:secret@s3.example.com/?X-Amz-Signature=abc#frag"} //nolint:gosec // G101: a synthetic credentials fixture

	assert.Equal(t, "backups at https://s3.example.com", l.String())
	assert.Equal(t, "backups at AWS S3", StorageLocation{Bucket: "backups"}.String())
	assert.Equal(t, "backups at https://s3.example.com", StorageLocation{Bucket: "backups", Endpoint: "https://s3.example.com/"}.String())
}

// Clusters retain endpoints configured before the plain HTTP check, so install
// and upgrade warn about rejected endpoints until the operator changes them.
func TestAStoredPlainEndpointGetsAWarning(t *testing.T) {
	assert.Contains(t, PlainEndpointWarning(&BackupStorageConfig{Endpoint: "http://objects.example.com"}), "http://objects.example.com")
	assert.Empty(t, PlainEndpointWarning(&BackupStorageConfig{Endpoint: "https://objects.example.com"}))
	assert.Empty(t, PlainEndpointWarning(&BackupStorageConfig{Endpoint: "http://10.0.0.5:9000"}))
	assert.Empty(t, PlainEndpointWarning(nil))
	assert.Contains(t, PlainEndpointWarning(&BackupStorageConfig{Endpoint: "HTTP://objects.example.com"}), "plain HTTP")
	assert.Empty(t, PlainEndpointWarning(&BackupStorageConfig{Endpoint: "ftp://objects.example.com"}), "only a plain HTTP endpoint is described as one")
}
