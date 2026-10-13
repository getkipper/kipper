package installer

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHostAuditFindings(t *testing.T) {
	tests := []struct {
		name  string
		audit HostAudit
		want  []string
	}{
		{
			name:  "clean host produces no findings",
			audit: HostAudit{},
			want:  nil,
		},
		{
			name:  "exposed rpcbind is reported",
			audit: HostAudit{RpcbindExposed: true},
			want:  []string{"rpcbind port 111 is listening on all interfaces"},
		},
		{
			name:  "exposed cockpit is reported",
			audit: HostAudit{CockpitExposed: true},
			want:  []string{"Cockpit port 9090 is listening on all interfaces"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.audit.Findings())
		})
	}
}

func TestAuditHostFindsCockpit(t *testing.T) {
	r := &storageHostRunner{replies: map[string]string{
		":9090": "tcp LISTEN 0 4096 *:9090 *:*\n",
	}}

	audit, err := AuditHost(r)

	assert.NoError(t, err)
	assert.True(t, audit.CockpitExposed)
	assert.False(t, audit.RpcbindExposed)
}

func TestHardenHostDisablesCockpit(t *testing.T) {
	r := &storageHostRunner{replies: map[string]string{
		"list-unit-files cockpit.socket": "cockpit.socket\n",
	}}

	assert.NoError(t, HardenHost(r))

	assert.Contains(t, strings.Join(r.commands, "\n"), "systemctl disable --now cockpit.socket")
}

func TestHardenHostLeavesAHostWithoutCockpitAlone(t *testing.T) {
	r := &storageHostRunner{}

	assert.NoError(t, HardenHost(r))

	assert.NotContains(t, strings.Join(r.commands, "\n"), "disable --now cockpit")
}
