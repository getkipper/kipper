package domain

import "testing"

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"App.Hrportal.EU":  "app.hrportal.eu",
		"app.hrportal.eu.": "app.hrportal.eu",
		"APP.HRPORTAL.EU.": "app.hrportal.eu",
		"app.hrportal.eu":  "app.hrportal.eu",
	}
	for in, want := range cases {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsGatewayHost(t *testing.T) {
	cases := map[string]bool{
		"foo--acme.kipper.run":  true,
		"FOO--ACME.KIPPER.RUN":  true,
		"foo--acme.kipper.run.": true,
		"kipper.run":            false,
		"fakekipper.run":        false,
		"app.hrportal.eu":       false,
	}
	for host, want := range cases {
		if got := IsGatewayHost(host); got != want {
			t.Errorf("IsGatewayHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestAppRoutePrefix(t *testing.T) {
	if got := AppRoutePrefix("hrportal-backend", "test"); got != "hrportal-backend-test" {
		t.Errorf("with env = %q, want hrportal-backend-test", got)
	}
	if got := AppRoutePrefix("hrportal-backend", ""); got != "hrportal-backend" {
		t.Errorf("no env = %q, want hrportal-backend", got)
	}
}

func TestClassifyHost(t *testing.T) {
	tests := []struct {
		name    string
		host    string
		derived string
		want    DomainClass
	}{
		{"platform subdomain exact", "hrportal-backend-test.storefront.com", "hrportal-backend-test.storefront.com", DomainClassPlatform},
		{"platform subdomain case/dot", "Hrportal-Backend-Test.storefront.com.", "hrportal-backend-test.storefront.com", DomainClassPlatform},
		{"custom domain", "app.hrportal.eu", "hrportal-frontend-prod.storefront.com", DomainClassCustom},
		// The self-host trap: base domain IS the user's own domain, so a real
		// custom domain is a suffix of the base. Exact match keeps it custom.
		{"self-host custom under own base", "app.hrportal.eu", "frontend-prod.hrportal.eu", DomainClassCustom},
		{"gateway wins over platform", "foo--acme.kipper.run", "foo--acme.kipper.run", DomainClassGateway},
		{"apex-ish non-match is custom", "hrportal.eu", "hrportal-website-prod.storefront.com", DomainClassCustom},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyHost(tt.host, tt.derived); got != tt.want {
				t.Errorf("ClassifyHost(%q, %q) = %q, want %q", tt.host, tt.derived, got, tt.want)
			}
		})
	}
}

func TestTargetEquivalent(t *testing.T) {
	if got := TargetEquivalent("hrportal-backend-test", "appcann.com"); got != "hrportal-backend-test.appcann.com" {
		t.Errorf("custom base = %q, want hrportal-backend-test.appcann.com", got)
	}
	if got := TargetEquivalent("foo", "acme.kipper.run"); got != "foo--acme.kipper.run" {
		// A free-tier cluster domain is a label under kipper.run, so the prefix
		// joins with -- for the single-label wildcard cert.
		t.Errorf("kipper.run base = %q, want foo--acme.kipper.run", got)
	}
	if got := TargetEquivalent("", "appcann.com"); got != "" {
		t.Errorf("empty prefix = %q, want empty", got)
	}
	if got := TargetEquivalent("app", ""); got != "" {
		t.Errorf("empty base = %q, want empty", got)
	}
}
