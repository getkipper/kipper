package installer

import (
	"strings"
	"testing"
)

func TestSubdomainFor(t *testing.T) {
	tests := []struct {
		name   string
		prefix string
		domain string
		want   string
	}{
		{
			name:   "kipper.run uses double dash",
			prefix: "console",
			domain: "203-0-113-12.kipper.run",
			want:   "console--203-0-113-12.kipper.run",
		},
		{
			name:   "custom domain uses dot",
			prefix: "console",
			domain: "example.com",
			want:   "console.example.com",
		},
		{
			name:   "console-api with kipper.run",
			prefix: "console-api",
			domain: "203-0-113-12.kipper.run",
			want:   "console-api--203-0-113-12.kipper.run",
		},
		{
			name:   "console-api with custom domain",
			prefix: "console-api",
			domain: "example.com",
			want:   "console-api.example.com",
		},
		{
			name:   "dex with kipper.run",
			prefix: "dex",
			domain: "kipper.example.com",
			want:   "dex.kipper.example.com",
		},
		{
			name:   "function with custom domain",
			prefix: "fn-myapp",
			domain: "example.com",
			want:   "fn-myapp.example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SubdomainFor(tt.prefix, tt.domain)
			if got != tt.want {
				t.Errorf("SubdomainFor(%q, %q) = %q, want %q", tt.prefix, tt.domain, got, tt.want)
			}
		})
	}
}

func TestUIDomainFor(t *testing.T) {
	tests := []struct {
		name   string
		domain string
		want   string
	}{
		{
			name:   "custom domain allows the cluster domain",
			domain: "example.com",
			want:   "example.com",
		},
		{
			name:   "custom subdomain allows itself",
			domain: "kipper.example.com",
			want:   "kipper.example.com",
		},
		{
			name:   "kipper.run subdomain has no safe base",
			domain: "203-0-113-12.kipper.run",
			want:   "",
		},
		{
			name:   "bare kipper.run apex has no safe base",
			domain: "kipper.run",
			want:   "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := UIDomainFor(tt.domain); got != tt.want {
				t.Errorf("UIDomainFor(%q) = %q, want %q", tt.domain, got, tt.want)
			}
		})
	}
}

func TestDexManifestSubdomains(t *testing.T) {
	tests := []struct {
		name            string
		domain          string
		wantIssuer      string
		wantCallback    string
		wantIngressTLS  string
		wantIngressHost string
		wantAdminEmail  string
		wantHashLine    string
		wantLogoURL     string
	}{
		{
			name:            "kipper.run domain uses double dash",
			domain:          "203-0-113-12.kipper.run",
			wantIssuer:      "https://dex--203-0-113-12.kipper.run/dex",
			wantCallback:    "https://console--203-0-113-12.kipper.run/callback",
			wantIngressTLS:  `"dex--203-0-113-12.kipper.run"`,
			wantIngressHost: `host: "dex--203-0-113-12.kipper.run"`,
			wantAdminEmail:  `email: "admin@203-0-113-12.kipper.run"`,
			wantHashLine:    `hash: "fakehash"`,
			wantLogoURL:     "logoURL: https://console--203-0-113-12.kipper.run/logo-stacked-light.svg",
		},
		{
			name:            "custom domain uses dots",
			domain:          "example.com",
			wantIssuer:      "https://dex.example.com/dex",
			wantCallback:    "https://console.example.com/callback",
			wantIngressTLS:  `"dex.example.com"`,
			wantIngressHost: `host: "dex.example.com"`,
			wantAdminEmail:  `email: "admin@example.com"`,
			wantHashLine:    `hash: "fakehash"`,
			wantLogoURL:     "logoURL: https://console.example.com/logo-stacked-light.svg",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dexDomain := SubdomainFor("dex", tt.domain)
			consoleDomain := SubdomainFor("console", tt.domain)
			manifest := renderDexManifest(dexDomain, consoleDomain, tt.domain, "fakehash")

			assertIngressTLSPolicy(t, manifest, tt.domain, "dex-tls")

			// The console client secret is expanded from the environment, not
			// baked into the config, and the Deployment sources it from the Secret.
			if !strings.Contains(manifest, "secretEnv: DEX_CLIENT_SECRET") {
				t.Error("dex config should reference the client secret via secretEnv")
			}
			if strings.Contains(manifest, "secret: fakesecret") {
				t.Error("dex config must not bake an inline client secret")
			}
			if !strings.Contains(manifest, "secretKeyRef:") || !strings.Contains(manifest, "name: dex-oidc-client") {
				t.Error("dex Deployment should source DEX_CLIENT_SECRET from the dex-oidc-client Secret")
			}
			if !strings.Contains(manifest, tt.wantIssuer) {
				t.Errorf("manifest missing issuer %q", tt.wantIssuer)
			}
			if !strings.Contains(manifest, tt.wantCallback) {
				t.Errorf("manifest missing callback %q", tt.wantCallback)
			}
			if !strings.Contains(manifest, tt.wantIngressTLS) {
				t.Errorf("manifest missing TLS host %q", tt.wantIngressTLS)
			}
			if !strings.Contains(manifest, tt.wantIngressHost) {
				t.Errorf("manifest missing Ingress host %q", tt.wantIngressHost)
			}
			if !strings.Contains(manifest, tt.wantAdminEmail) {
				t.Errorf("manifest missing admin email %q", tt.wantAdminEmail)
			}
			if !strings.Contains(manifest, tt.wantHashLine) {
				t.Errorf("manifest missing password hash line %q", tt.wantHashLine)
			}
			if !strings.Contains(manifest, tt.wantLogoURL) {
				t.Errorf("manifest missing logoURL %q", tt.wantLogoURL)
			}
		})
	}
}

func TestConsoleManifestSubdomains(t *testing.T) {
	tests := []struct {
		name               string
		domain             string
		wantDexIssuer      string
		wantRedirectURI    string
		wantConsoleTLS     string
		wantConsoleHost    string
		wantConsoleAPITLS  string
		wantConsoleAPIHost string
		wantClusterDomain  string
		wantUIDomain       string
	}{
		{
			name:               "kipper.run domain uses double dash",
			domain:             "203-0-113-12.kipper.run",
			wantDexIssuer:      "https://dex--203-0-113-12.kipper.run/dex",
			wantRedirectURI:    "https://console--203-0-113-12.kipper.run/callback",
			wantConsoleTLS:     `"console--203-0-113-12.kipper.run"`,
			wantConsoleHost:    `host: "console--203-0-113-12.kipper.run"`,
			wantConsoleAPITLS:  `"console-api--203-0-113-12.kipper.run"`,
			wantConsoleAPIHost: `host: "console-api--203-0-113-12.kipper.run"`,
			wantClusterDomain:  `value: "203-0-113-12.kipper.run"`,
			wantUIDomain:       "- name: UI_DOMAIN\n              value: \"\"",
		},
		{
			name:               "custom domain uses dots",
			domain:             "example.com",
			wantDexIssuer:      "https://dex.example.com/dex",
			wantRedirectURI:    "https://console.example.com/callback",
			wantConsoleTLS:     `"console.example.com"`,
			wantConsoleHost:    `host: "console.example.com"`,
			wantConsoleAPITLS:  `"console-api.example.com"`,
			wantConsoleAPIHost: `host: "console-api.example.com"`,
			wantClusterDomain:  `value: "example.com"`,
			wantUIDomain:       "- name: UI_DOMAIN\n              value: \"example.com\"",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dexDomain := SubdomainFor("dex", tt.domain)
			consoleDomain := SubdomainFor("console", tt.domain)
			consoleAPIDomain := SubdomainFor("console-api", tt.domain)
			manifest := renderConsoleManifest(dexDomain, consoleDomain, consoleAPIDomain,
				tt.domain, tt.domain, "203.0.113.10")

			assertIngressTLSPolicy(t, manifest, tt.domain, "console-tls", "console-api-tls")

			// console-api reads the OAuth client secret from the shared Secret,
			// never from a baked env value.
			if !strings.Contains(manifest, "secretKeyRef:") || !strings.Contains(manifest, "name: dex-oidc-client") {
				t.Error("console-api should source DEX_CLIENT_SECRET from the dex-oidc-client Secret")
			}

			checks := map[string]string{
				"DEX_ISSUER":       tt.wantDexIssuer,
				"redirect URI":     tt.wantRedirectURI,
				"console TLS":      tt.wantConsoleTLS,
				"console host":     tt.wantConsoleHost,
				"console-api TLS":  tt.wantConsoleAPITLS,
				"console-api host": tt.wantConsoleAPIHost,
				"CLUSTER_DOMAIN":   tt.wantClusterDomain,
				"UI_DOMAIN":        tt.wantUIDomain,
			}
			for label, want := range checks {
				if !strings.Contains(manifest, want) {
					t.Errorf("manifest missing %s: %q", label, want)
				}
			}
		})
	}
}

// assertIngressTLSPolicy checks the domain-aware TLS rendering: a kipper.run
// domain serves the gateway-pinned hop certificate (no cert-manager
// annotation, no secretName), a custom domain keeps cert-manager issuance.
func assertIngressTLSPolicy(t *testing.T, manifest, domain string, secrets ...string) {
	t.Helper()
	gateway := strings.HasSuffix(domain, ".kipper.run")
	hasIssuer := strings.Contains(manifest, "cert-manager.io/cluster-issuer")
	if gateway && hasIssuer {
		t.Error("a kipper.run ingress must not request a cert-manager certificate")
	}
	if !gateway && !hasIssuer {
		t.Error("a custom-domain ingress must request a cert-manager certificate")
	}
	for _, secret := range secrets {
		has := strings.Contains(manifest, "secretName: "+secret)
		if gateway && has {
			t.Errorf("a kipper.run ingress must carry no secretName (%s): the host must fall through to the default-store hop certificate", secret)
		}
		if !gateway && !has {
			t.Errorf("a custom-domain ingress must carry secretName %s", secret)
		}
	}
}
