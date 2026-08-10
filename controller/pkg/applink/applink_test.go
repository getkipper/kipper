package applink

import "testing"

func TestEnvKey(t *testing.T) {
	tests := []struct{ app, want string }{
		{"domain-service", "DOMAIN_SERVICE_URL"},
		{"api", "API_URL"},
		{"multi-part-name", "MULTI_PART_NAME_URL"},
		{"", "_URL"},
	}
	for _, tt := range tests {
		if got := EnvKey(tt.app); got != tt.want {
			t.Errorf("EnvKey(%q) = %q, want %q", tt.app, got, tt.want)
		}
	}
}
