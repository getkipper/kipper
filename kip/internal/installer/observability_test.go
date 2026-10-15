package installer

import (
	"strings"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

func TestInstallNodeExporterTLSAppliesTheCertificateIntoMonitoring(t *testing.T) {
	runner := &recordingRunner{}
	if err := InstallNodeExporterTLS(runner); err != nil {
		t.Fatalf("InstallNodeExporterTLS: %v", err)
	}
	if len(runner.commands) != 3 {
		t.Fatalf("want a namespace command, one apply and a wait, got %q", runner.commands)
	}
	if runner.commands[0] != ensureNamespaceCmd(platform.MonitoringNamespace) {
		t.Errorf("the monitoring namespace must exist first, got %q", runner.commands[0])
	}
	if !strings.Contains(runner.commands[1], "kubectl apply") ||
		!strings.Contains(runner.commands[1], platform.NodeExporterTLSManifests()) {
		t.Errorf("the TLS manifests were not applied, got %q", runner.commands[1])
	}
	want := "kubectl wait --for=condition=Ready certificate/" + platform.NodeExporterCertName + " -n " + platform.MonitoringNamespace
	if !strings.HasPrefix(runner.commands[2], want) {
		t.Errorf("the step must wait for the certificate to be issued, got %q", runner.commands[2])
	}
}
