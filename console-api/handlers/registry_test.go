package handlers

import (
	"testing"

	"github.com/getkipper/kipper/controller/pkg/registrycred"
)

func TestNormalizeRegistryServer(t *testing.T) {
	dockerHub := "https://index.docker.io/v1/"
	cases := map[string]string{
		"docker.io":                     dockerHub,
		"index.docker.io":               dockerHub,
		"registry-1.docker.io":          dockerHub,
		"registry.hub.docker.com":       dockerHub,
		"https://index.docker.io/v1/":   dockerHub,
		"https://docker.io":             dockerHub,
		"  docker.io  ":                 dockerHub,
		"ghcr.io":                       "ghcr.io",
		"registry.example.com":          "registry.example.com",
		"https://registry.example.com/": "https://registry.example.com/",
	}
	for in, want := range cases {
		if got := registrycred.NormalizeServer(in); got != want {
			t.Errorf("NormalizeServer(%q) = %q, want %q", in, got, want)
		}
	}
}
