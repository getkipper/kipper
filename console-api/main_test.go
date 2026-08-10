package main

import (
	"testing"
)

func TestServiceUIAuthCheckURL(t *testing.T) {
	t.Setenv("SERVICE_UI_AUTH_CHECK_URL", "")

	// No console domain → no gating URL (feature off).
	t.Setenv("CONSOLE_DOMAIN", "")
	if got := serviceUIAuthCheckURL(); got != "" {
		t.Errorf("without CONSOLE_DOMAIN = %q, want empty", got)
	}

	// With a console domain, the forwardAuth address points at the
	// in-cluster console-api Service, not the public console host — so
	// the auth sub-request has no second Traefik hop to overwrite
	// X-Forwarded-Host (which share-link validation depends on).
	t.Setenv("CONSOLE_DOMAIN", "console.storefront.com")
	got := serviceUIAuthCheckURL()
	want := "http://console-api.kipper-system.svc.cluster.local:8080/auth/check"
	if got != want {
		t.Errorf("auth check URL = %q, want %q", got, want)
	}

	// Explicit override wins.
	t.Setenv("SERVICE_UI_AUTH_CHECK_URL", "http://custom:9000/auth/check")
	if got := serviceUIAuthCheckURL(); got != "http://custom:9000/auth/check" {
		t.Errorf("override ignored: got %q", got)
	}
}
