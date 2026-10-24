package installer

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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

func TestLokiStorageRequest(t *testing.T) {
	tests := []struct {
		name     string
		template string
		claim    string
		want     string
		wantErr  bool
	}{
		{name: "fresh install", want: "5Gi"},
		{name: "existing cluster", template: "loki-template[10Gi]", claim: "loki-claim[10Gi]", want: "10Gi"},
		{name: "claim expanded under an unchanged template", template: "loki-template[10Gi]", claim: "loki-claim[20Gi]", want: "10Gi"},
		{name: "only the claim left after a disable", claim: "loki-claim[10Gi]", want: "10Gi"},
		{name: "diagnostics around the payload", template: "Warning: x\nloki-template[10Gi]Connection closed\r\n", claim: "loki-claim[10Gi]", want: "10Gi"},
		{name: "unparseable template size", template: "loki-template[lots]", wantErr: true},
		{name: "unparseable claim size", claim: "loki-claim[lots]", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := lokiStorageRequest(tt.template, tt.claim)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestInstallLokiRendersThePreservedSize(t *testing.T) {
	host := &routedRunner{routes: []route{
		{contains: lokiStatefulSetQuery, out: lokiStatefulSetJSON(`"storageClassName":"local-path",`)},
		{contains: "get statefulset loki", out: "loki-template[10Gi]"},
		{contains: "get pvc storage-loki-0", out: "loki-claim[20Gi]"},
	}}
	require.NoError(t, InstallLokiWithResources(host, platform.ResourcesForProfile(ProfileSmall), StorageLocal))
	assert.Contains(t, strings.Join(host.commands, "\n"), "size: 10Gi\n", "an upgrade keeps the StatefulSet's immutable claim template")

	fresh := &routedRunner{}
	require.NoError(t, InstallLokiWithResources(fresh, platform.ResourcesForProfile(ProfileSmall), StorageLocal))
	assert.Contains(t, strings.Join(fresh.commands, "\n"), "size: 5Gi\n")
}

// A lookup failure must preserve the immutable claim template.
func TestInstallLokiStopsWhenEitherSizeLookupFails(t *testing.T) {
	for _, failing := range []string{"get statefulset loki", "get pvc storage-loki-0"} {
		host := &routedRunner{routes: []route{
			{contains: failing, err: errors.New("connection refused")},
			{contains: "get statefulset loki", out: "loki-template[10Gi]"},
			{contains: "get pvc storage-loki-0", out: "loki-claim[20Gi]"},
		}}
		err := InstallLokiWithResources(host, platform.ResourcesForProfile(ProfileSmall), StorageLocal)
		assert.ErrorContains(t, err, "log volume size", failing)
		for _, c := range host.commands {
			assert.NotContains(t, c, "kind: HelmChart", "nothing is applied when the current size is unknown")
		}
	}
}

func lokiStatefulSetJSON(classField string) string {
	return `Warning: noise
{"kind":"StatefulSet","spec":{"volumeClaimTemplates":[{"metadata":{"name":"storage"},"spec":{` + classField +
		`"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":"10Gi"}}}}]}}`
}

// Claim templates are immutable, so a growth to Longhorn must leave the Loki
// StatefulSet's class exactly as it is, while a new volume follows the mode.
func TestInstallLokiKeepsTheLiveStorageClass(t *testing.T) {
	applied := func(host *routedRunner) string {
		t.Helper()
		require.NoError(t, InstallLokiWithResources(host, platform.ResourcesForProfile(ProfileSmall), StorageLonghorn))
		for _, c := range host.commands {
			if strings.Contains(c, "name: loki\n") {
				return c
			}
		}
		t.Fatal("no Loki HelmChart was applied")
		return ""
	}
	existing := func(classField string) *routedRunner {
		return &routedRunner{routes: []route{
			{contains: lokiStatefulSetQuery, out: lokiStatefulSetJSON(classField)},
			{contains: "get statefulset loki", out: "loki-template[10Gi]"},
			{contains: "get pvc storage-loki-0", out: "loki-claim[10Gi]claim-class[local-path]"},
		}}
	}

	assert.NotContains(t, applied(existing("")), "storageClass:", "a template without a class keeps rendering none")
	assert.Contains(t, applied(existing(`"storageClassName":"local-path",`)), `storageClass: "local-path"`)
	assert.Contains(t, applied(existing(`"storageClassName":"",`)), `storageClass: "-"`)

	claimOnly := &routedRunner{routes: []route{
		{contains: "get pvc storage-loki-0", out: "loki-claim[10Gi]claim-class[local-path]"},
	}}
	assert.Contains(t, applied(claimOnly), `storageClass: "local-path"`, "a surviving claim keeps its class")

	assert.Contains(t, applied(&routedRunner{}), `storageClass: "longhorn-single"`, "a new log volume takes one Longhorn copy")
}

func TestInstallLokiStopsWhenTheTemplateCannotBeRead(t *testing.T) {
	host := &routedRunner{routes: []route{
		{contains: lokiStatefulSetQuery, out: "Connection closed"},
		{contains: "get statefulset loki", out: "loki-template[10Gi]"},
		{contains: "get pvc storage-loki-0", out: "loki-claim[10Gi]"},
	}}
	err := InstallLokiWithResources(host, platform.ResourcesForProfile(ProfileSmall), StorageLonghorn)
	require.Error(t, err)
	for _, c := range host.commands {
		assert.NotContains(t, c, "kind: HelmChart", "nothing is applied when the template's class is unknown")
	}
}
