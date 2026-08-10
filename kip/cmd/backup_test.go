package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/getkipper/kipper/kip/internal/config"
)

func TestBuildBackupCR(t *testing.T) {
	tests := []struct {
		name          string
		project       string
		environment   string
		includeSystem bool
		wantIncluded  []interface{}
		wantExcluded  bool
	}{
		{
			name:          "cluster-wide backup applies the default exclusion list",
			project:       "",
			includeSystem: false,
			wantExcluded:  true,
		},
		{
			name:          "include-system flag drops the exclusion list",
			project:       "",
			includeSystem: true,
			wantExcluded:  false,
		},
		{
			name:          "project scope uses includedNamespaces, no exclusion list",
			project:       "acme",
			environment:   "test",
			includeSystem: false,
			wantIncluded:  []interface{}{"acme-test"},
			wantExcluded:  false,
		},
		{
			name:          "project scope honors org prefix",
			project:       "acme",
			environment:   "",
			includeSystem: false,
			wantIncluded:  []interface{}{"acme"},
			wantExcluded:  false,
		},
	}

	cluster := &config.Cluster{Name: "test", Provider: "baremetal"}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildBackupCR("test-backup", tt.project, tt.environment, "168h", tt.includeSystem, cluster)

			assert.Equal(t, "velero.io/v1", got.Object["apiVersion"])
			assert.Equal(t, "Backup", got.Object["kind"])

			spec := got.Object["spec"].(map[string]interface{})
			assert.Equal(t, "168h", spec["ttl"])
			assert.Equal(t, "default", spec["storageLocation"])
			assert.Equal(t, true, spec["defaultVolumesToFsBackup"])

			if tt.wantIncluded != nil {
				assert.Equal(t, tt.wantIncluded, spec["includedNamespaces"])
				_, hasExcluded := spec["excludedNamespaces"]
				assert.False(t, hasExcluded, "project-scoped backup must not set excludedNamespaces")
			}

			if tt.wantExcluded {
				excluded, ok := spec["excludedNamespaces"].([]interface{})
				assert.True(t, ok, "cluster-wide backup must include excludedNamespaces")
				assert.Contains(t, excluded, "velero", "velero exclusion is load-bearing")
				assert.Contains(t, excluded, "kube-system")
				assert.Contains(t, excluded, "monitoring")
			} else if tt.project == "" {
				// include-system case: no excludedNamespaces, no includedNamespaces
				_, hasExcluded := spec["excludedNamespaces"]
				assert.False(t, hasExcluded, "include-system must not set excludedNamespaces")
				_, hasIncluded := spec["includedNamespaces"]
				assert.False(t, hasIncluded, "include-system without project must not set includedNamespaces")
			}
		})
	}
}

func TestBuildBackupCR_OrgPrefix(t *testing.T) {
	// Regression guard for the ResolveNamespace path: when the cluster
	// has an org, project-scoped backups must land in the prefixed
	// namespace, not the unprefixed one.
	cluster := &config.Cluster{Org: "acme"}
	got := buildBackupCR("b", "deck", "", "168h", false, cluster)
	spec := got.Object["spec"].(map[string]interface{})
	assert.Equal(t, []interface{}{"acme-deck"}, spec["includedNamespaces"])
}
