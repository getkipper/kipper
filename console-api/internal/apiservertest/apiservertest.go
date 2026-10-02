package apiservertest

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// Start runs a real API server with Kipper's CRDs installed and stops it when
// the test ends. Without KUBEBUILDER_ASSETS the test is skipped, except in CI,
// where it fails because the job must install the envtest binaries.
func Start(t *testing.T) *rest.Config {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("KUBEBUILDER_ASSETS is not set; the CI job must install the envtest binaries")
		}
		t.Skip(`KUBEBUILDER_ASSETS is not set; to run the API server tests: ` +
			`export KUBEBUILDER_ASSETS="$(setup-envtest use 1.35.0 -p path)" && go test ./...`)
	}
	_, self, _, _ := runtime.Caller(0)
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(filepath.Dir(self), "..", "..", "..", "deploy", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting API server: %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stopping API server: %v", err)
		}
	})
	return cfg
}
