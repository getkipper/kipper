package domain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestSubdomainFor(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		domain string
		want   string
	}{
		{
			name:   "kipper.run subdomain uses double dash",
			prefix: "console",
			domain: "203-0-113-12.kipper.run",
			want:   "console--203-0-113-12.kipper.run",
		},
		{
			name:   "custom domain uses dot",
			prefix: "console",
			domain: "appcann.com",
			want:   "console.appcann.com",
		},
		{
			name:   "function prefix with kipper.run",
			prefix: "fn-hello",
			domain: "203-0-113-12.kipper.run",
			want:   "fn-hello--203-0-113-12.kipper.run",
		},
		{
			name:   "function prefix with custom domain",
			prefix: "fn-hello",
			domain: "appcann.com",
			want:   "fn-hello.appcann.com",
		},
		{
			name:   "app with environment on kipper.run",
			prefix: "web-staging",
			domain: "203-0-113-12.kipper.run",
			want:   "web-staging--203-0-113-12.kipper.run",
		},
		{
			name:   "app with environment on custom domain",
			prefix: "web-staging",
			domain: "appcann.com",
			want:   "web-staging.appcann.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, SubdomainFor(tt.prefix, tt.domain))
		})
	}
}

func TestComponentHostsDefaultToConvention(t *testing.T) {
	const clusterDomain = "acme-prod.kipper.run"
	assert.Equal(t, "console--acme-prod.kipper.run", ConsoleHost("", clusterDomain))
	assert.Equal(t, "console-api--acme-prod.kipper.run", ConsoleAPIHost("", clusterDomain))
	assert.Equal(t, "dex--acme-prod.kipper.run", DexHost("", clusterDomain))
}

func TestComponentHostsRespectOverrides(t *testing.T) {
	const clusterDomain = "acme-prod.kipper.run"
	assert.Equal(t, "console.example.com", ConsoleHost("console.example.com", clusterDomain))
	assert.Equal(t, "api.example.com", ConsoleAPIHost("api.example.com", clusterDomain))
	assert.Equal(t, "auth.example.com", DexHost("auth.example.com", clusterDomain))
}
