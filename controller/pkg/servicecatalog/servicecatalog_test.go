package servicecatalog

import "testing"

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
