package handlers

import (
	"strings"
	"testing"
)

func TestQuoteIdent(t *testing.T) {
	cases := []struct {
		driver, in, want string
	}{
		{"pgx", "users", `"users"`},
		{"pgx", `weird"name`, `"weird""name"`},
		{"mysql", "users", "`users`"},
		{"mysql", "weird`name", "`weird``name`"},
	}
	for _, c := range cases {
		if got := quoteIdent(c.driver, c.in); got != c.want {
			t.Errorf("quoteIdent(%q, %q) = %q, want %q", c.driver, c.in, got, c.want)
		}
	}
}

func TestPlaceholders(t *testing.T) {
	got := placeholders("pgx", 3, 1)
	want := []string{"$1", "$2", "$3"}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("pgx placeholder[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	got = placeholders("mysql", 3, 1)
	for _, p := range got {
		if p != "?" {
			t.Errorf("mysql placeholder = %q, want ?", p)
		}
	}
	got = placeholders("pgx", 2, 5)
	if got[0] != "$5" || got[1] != "$6" {
		t.Errorf("startAt offset broken: %v", got)
	}
}

func TestBuildSelectRows(t *testing.T) {
	q := buildSelectRows("pgx", "public", "users", "id", "ASC", 50, 100, "")
	want := `SELECT * FROM "public"."users" ORDER BY "id" ASC LIMIT 50 OFFSET 100`
	if q != want {
		t.Errorf("got %q\nwant %q", q, want)
	}
	q = buildSelectRows("mysql", "appdb", "orders", "created_at", "DESC", 10, 0, "status")
	if !strings.Contains(q, "FROM `appdb`.`orders`") {
		t.Errorf("missing qualified ident: %q", q)
	}
	if !strings.Contains(q, "WHERE `status` = ?") {
		t.Errorf("missing filter clause: %q", q)
	}
	if !strings.Contains(q, "ORDER BY `created_at` DESC") {
		t.Errorf("missing order clause: %q", q)
	}
	if !strings.Contains(q, "LIMIT 10 OFFSET 0") {
		t.Errorf("missing pagination: %q", q)
	}
}

func TestBuildInsertReturning(t *testing.T) {
	q := buildInsertReturning("pgx", "public", "users", []string{"name", "email"}, 4)
	want := `INSERT INTO "public"."users" ("name", "email") VALUES ($1, $2) RETURNING *`
	if q != want {
		t.Errorf("got %q\nwant %q", q, want)
	}
}

func TestBuildInsert_MySQL(t *testing.T) {
	q := buildInsert("mysql", "appdb", "users", []string{"name", "email"})
	want := "INSERT INTO `appdb`.`users` (`name`, `email`) VALUES (?, ?)"
	if q != want {
		t.Errorf("got %q\nwant %q", q, want)
	}
}

func TestBuildUpdate(t *testing.T) {
	changes := map[string]interface{}{"name": "alice", "email": "a@b.c"}
	pk := map[string]interface{}{"id": 1}
	q, args := buildUpdate("pgx", "public", "users", []string{"email", "name"}, []string{"id"}, changes, pk)
	want := `UPDATE "public"."users" SET "email" = $1, "name" = $2 WHERE "id" = $3`
	if q != want {
		t.Errorf("got %q\nwant %q", q, want)
	}
	if len(args) != 3 {
		t.Errorf("expected 3 args, got %d", len(args))
	}
	if args[0] != "a@b.c" || args[1] != "alice" || args[2] != 1 {
		t.Errorf("args order wrong: %v", args)
	}
}

func TestBuildDelete(t *testing.T) {
	pk := map[string]interface{}{"id": 7}
	q, args := buildDelete("mysql", "appdb", "users", []string{"id"}, pk)
	want := "DELETE FROM `appdb`.`users` WHERE `id` = ?"
	if q != want {
		t.Errorf("got %q\nwant %q", q, want)
	}
	if len(args) != 1 || args[0] != 7 {
		t.Errorf("args wrong: %v", args)
	}
}

func TestSortedKeys_DeterministicOrder(t *testing.T) {
	in := map[string]interface{}{"z": 1, "a": 2, "m": 3}
	got := sortedKeys(in)
	want := []string{"a", "m", "z"}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("sortedKeys[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseIntDefault(t *testing.T) {
	cases := []struct {
		in   string
		def  int
		want int
	}{
		{"", 50, 50},
		{"abc", 50, 50},
		{"10", 50, 10},
		{"-1", 50, -1}, // caller is responsible for clamping
	}
	for _, c := range cases {
		if got := parseIntDefault(c.in, c.def); got != c.want {
			t.Errorf("parseIntDefault(%q, %d) = %d, want %d", c.in, c.def, got, c.want)
		}
	}
}
