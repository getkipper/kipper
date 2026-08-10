package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCheckWebSocketOrigin(t *testing.T) {
	tests := []struct {
		name          string
		origin        string
		consoleDomain string
		want          bool
	}{
		{"no origin (non-browser client) allowed", "", "console.example.com", true},
		{"matching console origin allowed", "https://console.example.com", "console.example.com", true},
		{"cross-site origin rejected", "https://evil.example.com", "console.example.com", false},
		{"http origin against https console rejected", "http://console.example.com", "console.example.com", false},
		{"no console domain configured fails closed", "https://console.example.com", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("CONSOLE_DOMAIN", tt.consoleDomain)

			r := httptest.NewRequest(http.MethodGet, "/ws", nil)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			if got := CheckWebSocketOrigin(r); got != tt.want {
				t.Errorf("CheckWebSocketOrigin(origin=%q) = %v, want %v", tt.origin, got, tt.want)
			}
		})
	}
}
