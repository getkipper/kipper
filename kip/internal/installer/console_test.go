package installer

import "testing"

func TestAdminEmailPattern(t *testing.T) {
	valid := []string{
		"admin@kipper.local",
		"ops.team@example.com",
		"a+tag@sub.example.co.uk",
	}
	for _, e := range valid {
		if !adminEmailPattern.MatchString(e) {
			t.Errorf("expected %q to be accepted", e)
		}
	}

	// Anything that could break out of the seed shell command or the JSON
	// it builds must be rejected.
	invalid := []string{
		"",
		"not-an-email",
		"a@b",                  // no TLD
		`x'; rm -rf / #@x.com`, // shell breakout via single quote
		`a"@b.com`,             // JSON-breaking double quote
		"a b@example.com",      // space
		"a@example.com; touch pwned",
		"a@example.com`whoami`",
	}
	for _, e := range invalid {
		if adminEmailPattern.MatchString(e) {
			t.Errorf("expected %q to be rejected", e)
		}
	}
}

func TestSeedAdminUserRejectsInvalidEmail(t *testing.T) {
	// Invalid input must error before any SSH command is built, so a nil
	// client is safe here.
	if err := seedAdminUser(nil, "x'; rm -rf / #"); err == nil {
		t.Fatal("expected seedAdminUser to reject an invalid admin email")
	}
}
