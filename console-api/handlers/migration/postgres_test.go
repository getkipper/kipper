package migration

import "testing"

// Database names come out of the source cluster's pg_database and end up
// inside SQL sent to the target, so anything that could carry quoting or
// injection is refused rather than escaped.
func TestValidDBName(t *testing.T) {
	valid := []string{"app", "acme_prod", "shop-db", "Analytics", "_staging", "db$1"}
	for _, name := range valid {
		if !validDBName.MatchString(name) {
			t.Errorf("expected %q to be accepted", name)
		}
	}

	invalid := []string{
		"",
		"1starts-with-digit",
		"has space",
		"quote'break",
		`double"quote`,
		"semi;colon",
		"back`tick",
		"new\nline",
		"dollar$(sub)",
		"paren(",
	}
	for _, name := range invalid {
		if validDBName.MatchString(name) {
			t.Errorf("expected %q to be rejected", name)
		}
	}
}
