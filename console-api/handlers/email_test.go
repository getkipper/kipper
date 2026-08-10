package handlers

import (
	"strings"
	"testing"
)

func TestInviteEmailHTML(t *testing.T) {
	html := inviteEmailHTML(
		"https://console.example.com/invite/abc123",
		"deployer",
		"48h",
		"console.example.com",
	)

	if !strings.Contains(html, "https://console.example.com/invite/abc123") {
		t.Error("expected invite URL in email")
	}
	if !strings.Contains(html, "deployer") {
		t.Error("expected role in email")
	}
	if !strings.Contains(html, "48h") {
		t.Error("expected expiry in email")
	}
	if !strings.Contains(html, "console.example.com") {
		t.Error("expected cluster domain in email")
	}
	if !strings.Contains(html, "Accept Invite") {
		t.Error("expected CTA button text in email")
	}
}
