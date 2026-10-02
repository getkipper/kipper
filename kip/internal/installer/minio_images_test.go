package installer

import "testing"

func TestMinIOImagesComeFromKippersRegistry(t *testing.T) {
	for got, want := range map[string]string{
		minioServerImage: "ghcr.io/getkipper/minio:RELEASE.2025-09-07T16-13-09Z",
		minioClientImage: "ghcr.io/getkipper/mc:RELEASE.2025-08-13T08-35-41Z",
	} {
		if got != want {
			t.Errorf("got %s, want %s", got, want)
		}
	}
}
