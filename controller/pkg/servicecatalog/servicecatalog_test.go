package servicecatalog

import (
	"strings"
	"testing"
)

func TestMinIOImage(t *testing.T) {
	for _, tc := range []struct {
		name, version, want string
	}{
		{"the release Kipper builds comes from Kipper's registry", "RELEASE.2025-09-07T16-13-09Z", "ghcr.io/getkipper/minio:RELEASE.2025-09-07T16-13-09Z"},
		{"any other release keeps the reference it was created with", "RELEASE.2024-06-13T22-53-53Z", "minio/minio:RELEASE.2024-06-13T22-53-53Z"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MinIOImage(tc.version); got != tc.want {
				t.Errorf("MinIOImage(%q) = %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}

func TestCheckNewVersion(t *testing.T) {
	for _, tc := range []struct {
		name, serviceType, version string
		refused                    bool
	}{
		{"minio without a version", "minio", "", false},
		{"minio on the release Kipper builds", "minio", MinIORelease, false},
		{"minio on any other release", "minio", "RELEASE.2024-06-13T22-53-53Z", true},
		{"another type on any version", "postgres", "15", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckNewVersion(tc.serviceType, tc.version)
			if (err != nil) != tc.refused {
				t.Errorf("CheckNewVersion(%q, %q) = %v, refused want %v", tc.serviceType, tc.version, err, tc.refused)
			}
		})
	}
}

// The CLI capacity check relies on defaults matching the reconciler.
// The console-api tests compare the reconciler presets with this table.
func TestDefaultResourcesMatchTheReconcilerPresets(t *testing.T) {
	for _, tt := range []struct{ svcType, cpu, memory string }{
		{"postgres", "500m", "1Gi"},
		{"mysql", "500m", "1Gi"},
		{"mongodb", "500m", "1Gi"},
		{"redis", "100m", "128Mi"},
		{"rabbitmq", "100m", "512Mi"},
		{"minio", "100m", "128Mi"},
		{"mailhog", "100m", "128Mi"},
		{"opensearch", "100m", "2Gi"},
		{"something-new", "100m", "128Mi"},
	} {
		cpu, memory := DefaultResources(tt.svcType)
		if cpu != tt.cpu || memory != tt.memory {
			t.Errorf("DefaultResources(%q) = %s/%s, want %s/%s", tt.svcType, cpu, memory, tt.cpu, tt.memory)
		}
	}
}

func TestInitContainerRequestsAreTheReconcilersFloor(t *testing.T) {
	cpu, memory := InitContainerRequests()
	if cpu != "10m" || memory != "16Mi" {
		t.Errorf("InitContainerRequests() = %s/%s, want 10m/16Mi", cpu, memory)
	}
}

func TestCheckVersionChange(t *testing.T) {
	for _, tc := range []struct {
		name, serviceType, running, next string
		majorUpgrade                     bool
		refusal                          string
	}{
		{"same major Postgres", "postgres", "16-alpine", "16.6-alpine", false, ""},
		{"Postgres major up", "postgres", "16-alpine", "17-alpine", false, "kip service export"},
		{"Postgres major up even when confirmed", "postgres", "16-alpine", "17-alpine", true, "kip service export"},
		{"Postgres major down", "postgres", "16", "15", true, "kip service export"},
		{"MySQL series down", "mysql", "8-oracle", "7-oracle", true, "cannot go back"},
		{"MySQL down names export", "mysql", "8-oracle", "7-oracle", true, "kip service export"},
		{"Redis down names a backup, since kip cannot export Redis", "redis", "8-alpine", "7-alpine", true, "backup taken before"},
		{"Redis major up without confirmation", "redis", "7-alpine", "8-alpine", false, "--major-upgrade"},
		{"Redis major up confirmed", "redis", "7-alpine", "8-alpine", true, ""},
		{"MongoDB major up names the step", "mongodb", "7", "8", false, "8.0"},
		{"RabbitMQ major up names the stop", "rabbitmq", "3-management-alpine", "4-management-alpine", false, "4.2"},
		{"a tag without a version needs confirmation", "redis", "7-alpine", "latest", false, "--major-upgrade"},
		{"a tag without a version confirmed", "redis", "7-alpine", "latest", true, ""},
		{"MinIO is left to CheckNewVersion", "minio", "RELEASE.2024-06-13T22-53-53Z", MinIORelease, false, ""},
		{"MailHog keeps no data", "mailhog", "v1.0.1", "v2.0.0", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckVersionChange(tc.serviceType, tc.running, tc.next, tc.majorUpgrade)
			switch {
			case tc.refusal == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.refusal != "" && err == nil:
				t.Fatalf("allowed, want a refusal naming %q", tc.refusal)
			case tc.refusal != "" && !strings.Contains(err.Error(), tc.refusal):
				t.Fatalf("refusal %q does not name %q", err, tc.refusal)
			}
		})
	}
}
