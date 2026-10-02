package healthcheck

import (
	"strings"
	"testing"
)

func i32(v int32) *int32 { return &v }

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   Check
		want Check
	}{
		{name: "http keeps everything", in: Check{Type: "http", Path: "/ready", Port: i32(8081), StartupTimeoutSeconds: i32(600), TimeoutSeconds: i32(3)},
			want: Check{Type: "http", Path: "/ready", Port: i32(8081), StartupTimeoutSeconds: i32(600), TimeoutSeconds: i32(3)}},
		{name: "a switch to tcp drops the path", in: Check{Type: "tcp", Path: "/ready", Port: i32(8081)},
			want: Check{Type: "tcp", Port: i32(8081)}},
		{name: "none keeps only the startup timeout", in: Check{Type: "none", Path: "/ready", Port: i32(8081), StartupTimeoutSeconds: i32(600), TimeoutSeconds: i32(3)},
			want: Check{Type: "none", StartupTimeoutSeconds: i32(600)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in
			got.Normalize()
			if got.Type != tt.want.Type || got.Path != tt.want.Path ||
				!eq(got.Port, tt.want.Port) || !eq(got.StartupTimeoutSeconds, tt.want.StartupTimeoutSeconds) || !eq(got.TimeoutSeconds, tt.want.TimeoutSeconds) {
				t.Errorf("Normalize = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func eq(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		check   Check
		wantErr string
	}{
		{name: "http", check: Check{Type: "http", Path: "/actuator/health/readiness"}},
		{name: "tcp on another port", check: Check{Type: "tcp", Port: i32(9090)}},
		{name: "none with a startup timeout", check: Check{Type: "none", StartupTimeoutSeconds: i32(900)}},
		{name: "unknown type", check: Check{Type: "grpc"}, wantErr: "http, tcp or none"},
		{name: "http without a path", check: Check{Type: "http"}, wantErr: "needs a path"},
		{name: "relative path", check: Check{Type: "http", Path: "ready"}, wantErr: "start with /"},
		{name: "path with a space", check: Check{Type: "http", Path: "/re ady"}, wantErr: "no spaces"},
		{name: "path too long", check: Check{Type: "http", Path: "/" + strings.Repeat("a", 1024)}, wantErr: "1024"},
		{name: "port zero", check: Check{Type: "tcp", Port: i32(0)}, wantErr: "between 1 and 65535"},
		{name: "the instance proxy port", check: Check{Type: "tcp", Port: i32(18080)}, wantErr: "instance proxy"},
		{name: "startup too short", check: Check{Type: "tcp", StartupTimeoutSeconds: i32(5)}, wantErr: "between 10 and 3600"},
		{name: "timeout too long", check: Check{Type: "tcp", TimeoutSeconds: i32(61)}, wantErr: "between 1 and 60"},
		{name: "a path on a tcp check", check: Check{Type: "tcp", Path: "/ready"}, wantErr: "only an HTTP check supports a path"},
		{name: "none with a port", check: Check{Type: "none", Port: i32(8081)}, wantErr: "none accepts no port or timeout"},
		{name: "none with a timeout", check: Check{Type: "none", TimeoutSeconds: i32(3)}, wantErr: "none accepts no port or timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.check.Validate(8080)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("rejected: %v", err)
			case tt.wantErr != "" && err == nil:
				t.Fatalf("accepted, want an error mentioning %q", tt.wantErr)
			case tt.wantErr != "" && !strings.Contains(err.Error(), tt.wantErr):
				t.Fatalf("error %q does not mention %q", err, tt.wantErr)
			}
		})
	}
}
