package installer

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
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
