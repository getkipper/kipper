package ai

import "testing"

func TestMinIOListerImageComesFromKippersRegistry(t *testing.T) {
	if want := "ghcr.io/getkipper/mc:RELEASE.2025-08-13T08-35-41Z"; minioListerImage != want {
		t.Errorf("got %s, want %s", minioListerImage, want)
	}
}
