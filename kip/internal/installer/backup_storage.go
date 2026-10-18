package installer

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/getkipper/kipper/kip/internal/config"
)

// BackupStorageConfig selects external S3-compatible storage and skips the
// in-cluster MinIO deployment. The bucket must already exist. Region is passed
// to Velero unchanged, for example "auto" for R2, an AWS region for native S3,
// or the configured region for self-hosted MinIO. An empty Endpoint selects
// native AWS S3, which derives its endpoint from Region. Other providers need
// an endpoint, such as https://<accountid>.r2.cloudflarestorage.com, including
// Cloudflare R2, MinIO, Backblaze B2, Wasabi and DigitalOcean Spaces.
//
// Credentials come from the local AWS credentials file and are stored in a
// Kubernetes Secret in velero. Upgrades can omit them to reuse that Secret.
// SSEC requests a server-generated key for a bucket without backups. SSECKey
// supplies 32 raw bytes instead, including for recovery. An existing key is
// retained, and an active SSE-C location stays enabled without either setting.
type BackupStorageConfig struct {
	Bucket          string
	Region          string
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	SSEC            bool
	SSECKey         []byte
}

// AWSCredentials holds a single profile's access key and secret read
// from an AWS-style credentials file.
type AWSCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// LoadAWSCredentials reads access and secret keys from an INI profile, using
// default when profile is empty. Both [NAME] and [profile NAME] headers are
// accepted. Missing key fields return an error.
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

// BackupStorageFromRef restores external storage settings from persisted config.
// The installer reuses the cluster's credential Secret. A nil reference or
// a mode other than external returns nil.
func BackupStorageFromRef(ref *config.BackupStorageRef) *BackupStorageConfig {
	if ref == nil || ref.Mode != "external" {
		return nil
	}
	return &BackupStorageConfig{Bucket: ref.Bucket, Region: ref.Region, Endpoint: ref.Endpoint}
}

// recordBackupStorage saves ref as the backup location of the local cluster
// entry called name. A missing entry is left alone.
func recordBackupStorage(name string, ref *config.BackupStorageRef) error {
	return config.Update(func(cfg *config.Config) error {
		if c := cfg.GetCluster(name); c != nil {
			c.BackupStorage = ref
		}
		return nil
	})
}

// backupStorageRef persists the storage mode and location while credentials
// remain in the cluster's Secret.
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
