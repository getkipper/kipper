package installer

import (
	"testing"
)

// The stamp exists so kip status can name the nodes still running an older
// configuration, or none at all, without opening an SSH connection to each one.
// Writing it and never reading it would make the whole mechanism decorative.

func TestStorageRestartCoverage(t *testing.T) {
	tests := []struct {
		name        string
		version     string
		stampedID   string
		liveID      string
		wantCovered bool
		wantWhy     string
	}{
		{
			name:        "written by this version, on this machine",
			version:     StorageRestartConfigVersion,
			stampedID:   "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d",
			liveID:      "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d",
			wantCovered: true,
		},
		{
			name:      "no stamp at all: added by an older kip",
			version:   "",
			stampedID: "",
			liveID:    "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d",
			wantWhy:   "never configured",
		},
		{
			name:      "an older config version",
			version:   "0",
			stampedID: "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d",
			liveID:    "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d",
			wantWhy:   "configured with an older version",
		},
		{
			name:      "reimaged under the same node name",
			version:   StorageRestartConfigVersion,
			stampedID: "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d",
			liveID:    "0000ffff0000ffff0000ffff0000ffff",
			wantWhy:   "reimaged since it was configured",
		},
		{
			name:      "the node reports no machine id, so nothing can be confirmed",
			version:   StorageRestartConfigVersion,
			stampedID: "9a3c1f2e4b5d6a7b8c9d0e1f2a3b4c5d",
			liveID:    "",
			wantWhy:   "cannot be confirmed",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			covered, why := StorageRestartCoverage(tc.version, tc.stampedID, tc.liveID)
			if covered != tc.wantCovered {
				t.Errorf("covered = %v, want %v (%s)", covered, tc.wantCovered, why)
			}
			if tc.wantWhy != "" && why != tc.wantWhy {
				t.Errorf("why = %q, want %q", why, tc.wantWhy)
			}
			if tc.wantCovered && why != "" {
				t.Errorf("a covered node needs no explanation, got %q", why)
			}
		})
	}
}

// The annotation names are the contract between the writer and this reader.
func TestCoverageAnnotationNames(t *testing.T) {
	if StorageRestartsVersionAnnotation != "kipper.run/storage-restarts-version" {
		t.Errorf("version annotation is %q", StorageRestartsVersionAnnotation)
	}
	if StorageRestartsMachineAnnotation != "kipper.run/storage-restarts-machine" {
		t.Errorf("machine annotation is %q", StorageRestartsMachineAnnotation)
	}
}
