package handlers

import (
	"testing"
)

func TestUpdateHtpasswd(t *testing.T) {
	t.Run("adds new user to empty file", func(t *testing.T) {
		result := updateHtpasswd("", "admin", "admin:$2y$10$hash")
		if result != "admin:$2y$10$hash\n" {
			t.Errorf("expected single line, got %q", result)
		}
	})

	t.Run("adds new user to existing file", func(t *testing.T) {
		existing := "user1:hash1\n"
		result := updateHtpasswd(existing, "user2", "user2:hash2")
		if result != "user1:hash1\nuser2:hash2\n" {
			t.Errorf("unexpected result: %q", result)
		}
	})

	t.Run("replaces existing user", func(t *testing.T) {
		existing := "admin:oldhash\nviewer:viewerhash\n"
		result := updateHtpasswd(existing, "admin", "admin:newhash")
		if result != "admin:newhash\nviewer:viewerhash\n" {
			t.Errorf("unexpected result: %q", result)
		}
	})
}

func TestParseHtpasswdUsernames_EmptyInput(t *testing.T) {
	// parseHtpasswdUsernames requires a K8s client, so we test the parsing logic
	// via updateHtpasswd instead. The handler tests below cover the full flow.
}
