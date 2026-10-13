package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/config"
)

func TestBackupStorageFromRef(t *testing.T) {
	assert.Nil(t, BackupStorageFromRef(nil))
	assert.Nil(t, BackupStorageFromRef(&config.BackupStorageRef{Mode: "in-cluster"}))
	assert.Equal(t, &BackupStorageConfig{Bucket: "backups", Region: "auto", Endpoint: "https://s3.example.com"},
		BackupStorageFromRef(&config.BackupStorageRef{Mode: "external", Bucket: "backups", Region: "auto", Endpoint: "https://s3.example.com"}))
}

// A reinstall without the flags keeps what the cluster was installed with.
func TestInheritClusterSettings(t *testing.T) {
	existing := &config.Cluster{
		DNSResolvers:   []string{"192.0.2.53"},
		TrustedProxies: []string{"198.51.100.0/24"},
		BackupStorage:  &config.BackupStorageRef{Mode: "external", Bucket: "backups", Region: "eu-west-1"},
	}

	opts := Options{}
	inheritClusterSettings(&opts, existing)
	assert.Equal(t, []string{"192.0.2.53"}, opts.DNSResolvers)
	assert.Equal(t, []string{"198.51.100.0/24"}, opts.TrustedProxies)
	assert.Equal(t, &BackupStorageConfig{Bucket: "backups", Region: "eu-west-1"}, opts.BackupStorage,
		"a reinstall without backup flags keeps the external bucket")

	explicit := Options{BackupStorage: &BackupStorageConfig{Bucket: "new-bucket"}, DNSResolvers: []string{"203.0.113.53"}}
	inheritClusterSettings(&explicit, existing)
	assert.Equal(t, "new-bucket", explicit.BackupStorage.Bucket, "an explicit flag wins")
	assert.Equal(t, []string{"203.0.113.53"}, explicit.DNSResolvers)

	inCluster := Options{}
	inheritClusterSettings(&inCluster, &config.Cluster{BackupStorage: &config.BackupStorageRef{Mode: "in-cluster"}})
	assert.Nil(t, inCluster.BackupStorage)
}

// An install that failed before Dex was sent the password reports none, since
// the one it generated never reached Dex.
func TestPasswordInEffect(t *testing.T) {
	assert.Empty(t, passwordInEffect("generated", false))
	assert.Equal(t, "generated", passwordInEffect("generated", true))
}
