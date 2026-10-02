package cmd

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Version validation should report the supported release even without a configured cluster.
func TestServiceAddRefusesAMinIOReleaseKipperDoesNotBuild(t *testing.T) {
	cmd := &cobra.Command{}
	for _, f := range []string{"name", "project", "environment", "storage", "memory", "cpu", "version"} {
		cmd.Flags().String(f, "", "")
	}
	_ = cmd.Flags().Set("name", "storage")
	_ = cmd.Flags().Set("version", "RELEASE.2024-06-13T22-53-53Z")

	err := runServiceAdd(cmd, []string{"minio"})
	if err == nil || !strings.Contains(err.Error(), "RELEASE.2025-09-07T16-13-09Z") {
		t.Fatalf("want a refusal naming the release Kipper builds, got %v", err)
	}
}
