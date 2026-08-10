package migration

import "testing"

func TestRewriteHostRefs(t *testing.T) {
	rewrites := map[string]string{
		"hrportal-frontend-test.storefront.com": "hrportal-frontend-test.appcann.com",
		"docuseal-test.storefront.com":          "docuseal-test.appcann.com",
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			"scheme and path",
			"https://hrportal-frontend-test.storefront.com/auth/callback",
			"https://hrportal-frontend-test.appcann.com/auth/callback",
		},
		{
			"host and port",
			"hrportal-frontend-test.storefront.com:8080",
			"hrportal-frontend-test.appcann.com:8080",
		},
		{
			"uppercase host is DNS-equivalent and rewritten",
			"https://HRPORTAL-FRONTEND-TEST.STOREFRONT.COM/x",
			"https://hrportal-frontend-test.appcann.com/x",
		},
		{
			"trailing root dot is the same FQDN, rewritten",
			"https://hrportal-frontend-test.storefront.com./x",
			"https://hrportal-frontend-test.appcann.com./x",
		},
		{
			"trailing root dot at end of value",
			"hrportal-frontend-test.storefront.com.",
			"hrportal-frontend-test.appcann.com.",
		},
		{
			"longer host after a dot is not a match",
			"hrportal-frontend-test.storefront.com.evil.example/x",
			"hrportal-frontend-test.storefront.com.evil.example/x",
		},
		{
			"comma-joined CORS list, two hosts",
			"https://hrportal-frontend-test.storefront.com,https://docuseal-test.storefront.com",
			"https://hrportal-frontend-test.appcann.com,https://docuseal-test.appcann.com",
		},
		{
			"bare comma-adjacent hosts",
			"hrportal-frontend-test.storefront.com,docuseal-test.storefront.com",
			"hrportal-frontend-test.appcann.com,docuseal-test.appcann.com",
		},
		{
			"leading label is not a boundary — left alone",
			"nothrportal-frontend-test.storefront.com",
			"nothrportal-frontend-test.storefront.com",
		},
		{
			"longer TLD is not a match — left alone",
			"https://hrportal-frontend-test.storefront.community/x",
			"https://hrportal-frontend-test.storefront.community/x",
		},
		{
			"postgres DSN with userinfo is rewritten",
			"postgres://kipper:secret@hrportal-frontend-test.storefront.com:5432/app",
			"postgres://kipper:secret@hrportal-frontend-test.appcann.com:5432/app",
		},
		{
			"authenticated HTTP URL with userinfo is rewritten",
			"https://user@docuseal-test.storefront.com/path",
			"https://user@docuseal-test.appcann.com/path",
		},
		{
			"out-of-scope host is left alone",
			"https://left-behind.storefront.com/health",
			"https://left-behind.storefront.com/health",
		},
		{
			"unrelated substring untouched",
			"storefront.community is unrelated",
			"storefront.community is unrelated",
		},
		{
			"no reference passes through",
			"just some text",
			"just some text",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rewriteHostRefs(tt.in, rewrites); got != tt.want {
				t.Errorf("rewriteHostRefs(%q)\n got  %q\n want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestRewriteHostRefs_EmptyTable(t *testing.T) {
	in := "https://hrportal-frontend-test.storefront.com/x"
	if got := rewriteHostRefs(in, map[string]string{}); got != in {
		t.Errorf("empty table changed value: %q", got)
	}
}
