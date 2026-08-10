package installer

import (
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
			want:  []string{"rpcbind exposed on 0.0.0.0:111"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.audit.Findings())
		})
	}
}
