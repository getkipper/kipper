package installer

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/getkipper/kipper/kip/internal/config"
)

// BackupStorageConfig configures Velero's BackupStorageLocation to point
// at off-cluster S3-compatible object storage instead of the default
// in-cluster MinIO. A non-nil config makes the installer skip the MinIO
// deployment entirely and configure Velero with the user's bucket.
//
// All fields are required except Endpoint, which is empty for native
// AWS S3 (Velero derives the endpoint from the region) and required for
// every other S3-compatible provider (Cloudflare R2, MinIO, Backblaze
// B2, Wasabi, DigitalOcean Spaces, etc.).
type BackupStorageConfig struct {
	// Bucket is the S3 bucket name. Must already exist; kip does not
	// create buckets.
	Bucket string
	// Region is the AWS region or provider-equivalent ("auto" for R2,
	// an AWS region for native S3, the configured region string for
	// self-hosted MinIO — passed through to Velero verbatim).
	Region string
	// Endpoint is the provider URL (e.g.
	// "https://<accountid>.r2.cloudflarestorage.com"). Empty means
	// native AWS S3.
	Endpoint string
	// AccessKeyID and SecretAccessKey are the credentials Velero uses
	// to read and write the bucket. Read from the user's local AWS
	// credentials file at install time, then stored only as a
	// Kubernetes Secret in the velero namespace.
	AccessKeyID     string
	SecretAccessKey string
}

// AWSCredentials holds a single profile's access key and secret read
// from an AWS-style credentials file.
type AWSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// LoadAWSCredentials reads the named profile from an AWS-style INI
// credentials file (typically `~/.aws/credentials`) and returns the
// access key + secret key it contains. The file format follows the
// AWS CLI convention:
//
//	[default]
//	aws_access_key_id = AKIA...
//	aws_secret_access_key = ...
//
//	[acme]
//	aws_access_key_id = AKIA...
//	aws_secret_access_key = ...
//
// Section headers wrapped in `[profile NAME]` (the form `~/.aws/config`
// uses) are also accepted, so the same file can be shared between the
// two AWS CLI files without surprises.
func LoadAWSCredentials(path, profile string) (*AWSCredentials, error) {
	if profile == "" {
		profile = "default"
	}
	f, err := os.Open(path) //nolint:gosec // path comes from a user flag at install time
	if err != nil {
		return nil, fmt.Errorf("opening credentials file %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	var current string
	creds := &AWSCredentials{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			name := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			// Accept "[profile name]" as well so a single file can be
			// used both as ~/.aws/credentials and ~/.aws/config.
			name = strings.TrimPrefix(name, "profile ")
			current = strings.TrimSpace(name)
			continue
		}
		if current != profile {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		switch key {
		case "aws_access_key_id":
			creds.AccessKeyID = value
		case "aws_secret_access_key":
			creds.SecretAccessKey = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading credentials file %s: %w", path, err)
	}

	if creds.AccessKeyID == "" || creds.SecretAccessKey == "" {
		return nil, fmt.Errorf("profile %q in %s is missing aws_access_key_id or aws_secret_access_key", profile, path)
	}
	return creds, nil
}

// VeleroCredentialsBlock renders the credentials in the format Velero's
// AWS plugin expects: a `[default]` INI section regardless of which
// profile we read from. Velero ignores the section name in practice but
// the plugin documentation specifies `[default]`, so we keep to that.
func (c *AWSCredentials) VeleroCredentialsBlock() string {
	return fmt.Sprintf("[default]\naws_access_key_id = %s\naws_secret_access_key = %s\n",
		c.AccessKeyID, c.SecretAccessKey)
}

// backupStorageRef converts an installer.BackupStorageConfig into the
// trimmed BackupStorageRef the kip config persists. Credentials are
// deliberately omitted — they live on the cluster as a Kubernetes
// Secret and must not be written to ~/.kip/config.yaml. Returns nil
// for in-cluster mode so the config stays compact.
func backupStorageRef(cfg *BackupStorageConfig) *config.BackupStorageRef {
	if cfg == nil {
		return &config.BackupStorageRef{Mode: "in-cluster"}
	}
	return &config.BackupStorageRef{
		Mode:     "external",
		Bucket:   cfg.Bucket,
		Region:   cfg.Region,
		Endpoint: cfg.Endpoint,
	}
}
