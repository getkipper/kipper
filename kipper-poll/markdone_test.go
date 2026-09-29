package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestMarkDoneBindsRowValuesAsParameters(t *testing.T) {
	tests := []struct {
		name      string
		driver    string
		template  string
		event     map[string]interface{}
		wantQuery string
		wantArgs  []interface{}
	}{
		{
			name:      "documented form on postgres",
			driver:    "postgres",
			template:  "UPDATE orders SET status = 'done' WHERE id = {{id}}",
			event:     map[string]interface{}{"id": int64(42)},
			wantQuery: "UPDATE orders SET status = 'done' WHERE id = $1",
			wantArgs:  []interface{}{int64(42)},
		},
		{
			name:      "documented form on mysql",
			driver:    "mysql",
			template:  "UPDATE users SET synced = 1 WHERE id = {{id}}",
			event:     map[string]interface{}{"id": "7"},
			wantQuery: "UPDATE users SET synced = 1 WHERE id = ?",
			wantArgs:  []interface{}{"7"},
		},
		{
			name:      "a whole quoted placeholder becomes one parameter",
			driver:    "postgres",
			template:  "UPDATE t SET done = true WHERE name = '{{name}}'",
			event:     map[string]interface{}{"name": "o'brien"},
			wantQuery: "UPDATE t SET done = true WHERE name = $1",
			wantArgs:  []interface{}{"o'brien"},
		},
		{
			name:      "row data never reaches the SQL text",
			driver:    "postgres",
			template:  "UPDATE t SET done = true WHERE id = {{id}}",
			event:     map[string]interface{}{"id": "1; DROP TABLE t; --"},
			wantQuery: "UPDATE t SET done = true WHERE id = $1",
			wantArgs:  []interface{}{"1; DROP TABLE t; --"},
		},
		{
			name:      "each occurrence gets its own parameter",
			driver:    "postgres",
			template:  "UPDATE t SET a = {{id}} WHERE id = {{id}} AND k = '{{k}}'",
			event:     map[string]interface{}{"id": 3, "k": "x"},
			wantQuery: "UPDATE t SET a = $1 WHERE id = $2 AND k = $3",
			wantArgs:  []interface{}{3, 3, "x"},
		},
		{
			name:      "a placeholder inside a literal becomes a concatenation on postgres",
			driver:    "postgres",
			template:  "UPDATE t SET done = true WHERE name LIKE '%{{name}}%'",
			event:     map[string]interface{}{"name": "x"},
			wantQuery: "UPDATE t SET done = true WHERE name LIKE ('%' || $1 || '%')",
			wantArgs:  []interface{}{"x"},
		},
		{
			name:      "a placeholder inside a literal becomes CONCAT on mysql",
			driver:    "mysql",
			template:  "UPDATE t SET done = 1 WHERE ref = 'order-{{id}}'",
			event:     map[string]interface{}{"id": 9},
			wantQuery: "UPDATE t SET done = 1 WHERE ref = CONCAT('order-', ?)",
			wantArgs:  []interface{}{9},
		},
		{
			name:      "escaped quotes inside a literal survive the split",
			driver:    "postgres",
			template:  "UPDATE t SET done = true WHERE note = 'it''s {{id}}'",
			event:     map[string]interface{}{"id": 1},
			wantQuery: "UPDATE t SET done = true WHERE note = ('it''s ' || $1)",
			wantArgs:  []interface{}{1},
		},
		{
			name:      "a mysql backslash escape is not mistaken for the end of the literal",
			driver:    "mysql",
			template:  `UPDATE t SET done = 1 WHERE note = 'a\'b {{id}}'`,
			event:     map[string]interface{}{"id": 1},
			wantQuery: `UPDATE t SET done = 1 WHERE note = CONCAT('a\'b ', ?)`,
			wantArgs:  []interface{}{1},
		},
		{
			name:      "a placeholder for a column the row lacks stays as text",
			driver:    "postgres",
			template:  "UPDATE t SET done = true WHERE id = {{missing}}",
			event:     map[string]interface{}{"id": 1},
			wantQuery: "UPDATE t SET done = true WHERE id = {{missing}}",
			wantArgs:  nil,
		},
		{
			name:      "a whole quoted placeholder for a missing column keeps its quotes",
			driver:    "postgres",
			template:  "UPDATE t SET note = '{{missing}}' WHERE id = {{id}}",
			event:     map[string]interface{}{"id": 1},
			wantQuery: "UPDATE t SET note = '{{missing}}' WHERE id = $1",
			wantArgs:  []interface{}{1},
		},
		{
			name:      "a literal right after a keyword is not a prefixed string",
			driver:    "postgres",
			template:  "UPDATE t SET done = true WHERE name LIKE'%{{name}}%'",
			event:     map[string]interface{}{"name": "x"},
			wantQuery: "UPDATE t SET done = true WHERE name LIKE ('%' || $1 || '%')",
			wantArgs:  []interface{}{"x"},
		},
		{
			name:      "a CONCAT right after a keyword keeps a space on mysql",
			driver:    "mysql",
			template:  "UPDATE t SET done = 1 WHERE name LIKE'%{{name}}%'",
			event:     map[string]interface{}{"name": "x"},
			wantQuery: "UPDATE t SET done = 1 WHERE name LIKE CONCAT('%', ?, '%')",
			wantArgs:  []interface{}{"x"},
		},
		{
			name:      "a parameter right after a keyword keeps a space on postgres",
			driver:    "postgres",
			template:  "UPDATE t SET done = true WHERE name LIKE'{{name}}'",
			event:     map[string]interface{}{"name": "x"},
			wantQuery: "UPDATE t SET done = true WHERE name LIKE $1",
			wantArgs:  []interface{}{"x"},
		},
		{
			name:      "a comment after the closing semicolon is allowed",
			driver:    "postgres",
			template:  "UPDATE t SET done = true WHERE id = {{id}}; -- mark delivered",
			event:     map[string]interface{}{"id": 1},
			wantQuery: "UPDATE t SET done = true WHERE id = $1; -- mark delivered",
			wantArgs:  []interface{}{1},
		},
		{
			name:      "a dollar sign inside an identifier is not a native marker",
			driver:    "postgres",
			template:  "UPDATE order$1 SET done = true WHERE id = {{id}}",
			event:     map[string]interface{}{"id": 1},
			wantQuery: "UPDATE order$1 SET done = true WHERE id = $1",
			wantArgs:  []interface{}{1},
		},
		{
			name:      "a parameter keeps its distance from a keyword that follows",
			driver:    "postgres",
			template:  "UPDATE t SET done = true WHERE id='{{id}}'AND status='p'",
			event:     map[string]interface{}{"id": 42},
			wantQuery: "UPDATE t SET done = true WHERE id=$1 AND status='p'",
			wantArgs:  []interface{}{42},
		},
		{
			name:      "a nested postgres comment is one comment",
			driver:    "postgres",
			template:  "UPDATE t SET done = true /* outer /* inner */ it's still a comment */ WHERE id = {{id}}",
			event:     map[string]interface{}{"id": 1},
			wantQuery: "UPDATE t SET done = true /* outer /* inner */ it's still a comment */ WHERE id = $1",
			wantArgs:  []interface{}{1},
		},
		{
			name:      "a line comment before an operator does not join literals",
			driver:    "postgres",
			template:  "UPDATE t SET done = true WHERE ref = 'order-' -- c\n|| '{{id}}'",
			event:     map[string]interface{}{"id": 42},
			wantQuery: "UPDATE t SET done = true WHERE ref = 'order-' -- c\n|| $1",
			wantArgs:  []interface{}{42},
		},
		{
			name:      "mysql double minus without a space is arithmetic",
			driver:    "mysql",
			template:  "UPDATE t SET n = '{{id}}'--1 +\n'2'",
			event:     map[string]interface{}{"id": 42},
			wantQuery: "UPDATE t SET n = ?--1 +\n'2'",
			wantArgs:  []interface{}{42},
		},
		{
			name:      "mysql executable comment is not a separator",
			driver:    "mysql",
			template:  "UPDATE t SET n = '{{id}}' /*! + */ '2'",
			event:     map[string]interface{}{"id": 42},
			wantQuery: "UPDATE t SET n = ? /*! + */ '2'",
			wantArgs:  []interface{}{42},
		},
		{
			name:      "adjacent literals without placeholders are left alone",
			driver:    "postgres",
			template:  "UPDATE t SET note = 'a'\n'b' WHERE id = {{id}}",
			event:     map[string]interface{}{"id": 1},
			wantQuery: "UPDATE t SET note = 'a'\n'b' WHERE id = $1",
			wantArgs:  []interface{}{1},
		},
		{
			name:      "a trailing semicolon is allowed",
			driver:    "postgres",
			template:  "UPDATE t SET done = true WHERE id = {{id}};",
			event:     map[string]interface{}{"id": 1},
			wantQuery: "UPDATE t SET done = true WHERE id = $1;",
			wantArgs:  []interface{}{1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := parseMarkDone(tt.driver, tt.template)
			if err != nil {
				t.Fatalf("parseMarkDone: %v", err)
			}
			query, args := tmpl.bind(tt.event)
			if query != tt.wantQuery {
				t.Errorf("query = %q, want %q", query, tt.wantQuery)
			}
			if !reflect.DeepEqual(args, tt.wantArgs) {
				t.Errorf("args = %#v, want %#v", args, tt.wantArgs)
			}
		})
	}
}

func TestMarkDoneRejectsPlaceholdersThatCannotBeBound(t *testing.T) {
	tests := []struct {
		name     string
		driver   string
		template string
		want     string
	}{
		{"postgres identifier", "postgres", `UPDATE "{{table}}" SET done = true`, "identifier"},
		{"mysql identifier", "mysql", "UPDATE `{{table}}` SET done = 1", "identifier"},
		{"line comment", "postgres", "UPDATE t SET done = true -- {{id}}", "comment"},
		{"block comment", "postgres", "UPDATE t SET done = true /* {{id}} */ WHERE id = 1", "comment"},
		{"mysql hash comment", "mysql", "UPDATE t SET done = 1 # {{id}}", "comment"},
		{"dollar quote", "postgres", "UPDATE t SET note = $$a {{id}}$$", "dollar-quoted"},
		{"escape string", "postgres", "UPDATE t SET note = E'a {{id}}'", "prefixed string"},
		{"unicode string", "postgres", "UPDATE t SET note = U&'a {{id}}'", "prefixed string"},
		{"mysql charset introducer", "mysql", "UPDATE t SET note = _utf8mb4'a {{id}}'", "prefixed string"},
		{"statement after a trailing comment", "postgres", "UPDATE t SET done = true; -- x\nDELETE FROM t", "one statement"},
		{"mysql double-quoted string", "mysql", `UPDATE t SET done = 1 WHERE n = "{{id}}"`, "double-quoted"},
		{"second statement", "postgres", "UPDATE t SET done = true WHERE id = {{id}}; DELETE FROM t", "one statement"},
		{"native postgres parameter", "postgres", "UPDATE t SET done = true WHERE id = $1", "{{column}}"},
		{"native mysql parameter", "mysql", "UPDATE t SET done = 1 WHERE id = ?", "{{column}}"},
		{"unterminated literal", "postgres", "UPDATE t SET done = true WHERE n = 'a{{id}}", "unterminated"},
		{"placeholder in the rest of a nested comment", "postgres", "UPDATE t SET done = true /* a /* b */ {{id}} */", "comment"},
		{"continued literal on postgres", "postgres", "UPDATE t SET done = true WHERE ref = 'order-'\n'{{id}}'", "adjacent string literals"},
		{"placeholder in the first of continued literals", "postgres", "UPDATE t SET done = true WHERE ref = '{{id}}'\n'-x'", "adjacent string literals"},
		{"literals continued over a carriage return", "postgres", "UPDATE t SET done = true WHERE ref = 'order-'\r'{{id}}'", "adjacent string literals"},
		{"adjacent literals on mysql", "mysql", "UPDATE t SET done = 1 WHERE ref = 'order-' '{{id}}'", "adjacent string literals"},
		{"placeholder in the first of literals continued over a carriage return", "postgres", "UPDATE t SET done = true WHERE ref = '{{id}}'\r'-x'", "adjacent string literals"},
		{"literals continued past a line comment", "postgres", "UPDATE t SET done = true WHERE ref = 'order-'\r-- continuation\n'{{id}}'", "adjacent string literals"},
		{"placeholder in the first of literals continued past a line comment", "postgres", "UPDATE t SET done = true WHERE ref = '{{id}}' -- continuation\n'-x'", "adjacent string literals"},
		{"mysql literals joined across a block comment", "mysql", "UPDATE t SET done = 1 WHERE ref = 'order-' /* c */ '{{id}}'", "adjacent string literals"},
		{"mysql literals joined across a hash comment", "mysql", "UPDATE t SET done = 1 WHERE ref = '{{id}}' # c\n'-x'", "adjacent string literals"},
		{"literals continued over a form feed", "postgres", "UPDATE t SET note = 'a'\f\n'{{id}}'", "adjacent string literals"},
		{"mysql line comment running past a carriage return", "mysql", "UPDATE t SET note = 'a' -- c\rstill comment\n'{{id}}'", "adjacent string literals"},
		{"mysql hash comment running past a carriage return", "mysql", "UPDATE t SET note = '{{id}}' # c\rstill comment\n'b'", "adjacent string literals"},
		{"mysql line comment opened by a delete byte", "mysql", "UPDATE t SET note = 'a' --\x7fc\n'{{id}}'", "adjacent string literals"},
		{"placeholder before a mysql line comment opened by a delete byte", "mysql", "UPDATE t SET note = '{{id}}' --\x7fc\n'b'", "adjacent string literals"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseMarkDone(tt.driver, tt.template)
			if err == nil {
				t.Fatalf("parseMarkDone(%q) must fail", tt.template)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q should mention %q", err, tt.want)
			}
		})
	}
}

func TestMarkDoneAllowsPlaceholderFreeLiterals(t *testing.T) {
	for _, tmpl := range []string{
		"UPDATE t SET note = 'what? $1 -- ; /* x */' WHERE id = {{id}}",
		`UPDATE "Orders" SET done = true WHERE id = {{id}}`,
		"UPDATE t SET note = $$a;b$$ WHERE id = {{id}}",
		"UPDATE t SET done = true WHERE id = {{id}} -- mark it",
	} {
		if _, err := parseMarkDone("postgres", tmpl); err != nil {
			t.Errorf("parseMarkDone(%q): %v", tmpl, err)
		}
	}
}

func TestMySQLSourceDisablesClientInterpolation(t *testing.T) {
	dsn, err := mysqlDSN("mysql://u:p@db:3306/app?interpolateParams=true")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(dsn, "interpolateParams=true") {
		t.Errorf("client-side interpolation must be off, got %q", dsn)
	}
	if !strings.Contains(dsn, "@tcp(db:3306)/app") {
		t.Errorf("the address and database must survive, got %q", dsn)
	}
}

func TestMarkDoneAcceptsPlaceholderFreeLiteralsOnMySQL(t *testing.T) {
	for _, tmpl := range []string{
		`UPDATE t SET note = 'what? -- ; /* x */ # y' WHERE id = {{id}}`,
		"UPDATE `Orders` SET done = 1 WHERE id = {{id}}",
		`UPDATE t SET note = "a;b" WHERE id = {{id}}`,
		"UPDATE t SET done = 1 WHERE id = {{id}} # mark it",
		`UPDATE t SET note = 'it\'s' WHERE id = {{id}}`,
	} {
		if _, err := parseMarkDone("mysql", tmpl); err != nil {
			t.Errorf("parseMarkDone(%q): %v", tmpl, err)
		}
	}
}

func TestMarkDoneAcceptsAnEscapeStringWithoutPlaceholders(t *testing.T) {
	tmpl, err := parseMarkDone("postgres", `UPDATE t SET note = E'a\'b' WHERE id = {{id}}`)
	if err != nil {
		t.Fatalf("parseMarkDone: %v", err)
	}
	query, _ := tmpl.bind(map[string]interface{}{"id": 1})
	if want := `UPDATE t SET note = E'a\'b' WHERE id = $1`; query != want {
		t.Errorf("query = %q, want %q", query, want)
	}
}

// SQL metacharacters in the marker expose accidental interpolation into query text.
func TestMarkDoneNeverPutsARowValueInTheQuery(t *testing.T) {
	const marker = "ZZ'MARKER\"--;`$1?"
	templates := map[string][]string{
		"postgres": {
			"UPDATE t SET done = true WHERE id = {{c}}",
			"UPDATE t SET done = true WHERE n = '{{c}}'",
			"UPDATE t SET done = true WHERE n LIKE '%{{c}}%' AND m = 'x{{c}}y{{c}}'",
			"UPDATE t SET done = true WHERE n = 'it''s {{c}}' AND n LIKE'{{c}}'",
			"UPDATE \"T\" SET note = $$a$$, e = E'q' WHERE id = {{c}}; -- done",
			"UPDATE t SET j = j ? 'k', n = '{{' WHERE id = {{c}}",
		},
		"mysql": {
			"UPDATE t SET done = 1 WHERE id = {{c}}",
			"UPDATE t SET done = 1 WHERE n = CONCAT('{{c}}', 'x') AND m = 'a\\'{{c}}'",
			"UPDATE `t` SET done = 1 WHERE n LIKE '%{{c}}%' # done",
		},
	}
	for driver, list := range templates {
		for _, tmpl := range list {
			parsed, err := parseMarkDone(driver, tmpl)
			if err != nil {
				t.Errorf("%s: parseMarkDone(%q): %v", driver, tmpl, err)
				continue
			}
			query, args := parsed.bind(map[string]interface{}{"c": marker})
			if strings.Contains(query, "MARKER") {
				t.Errorf("%s: a row value reached the query text: %q", driver, query)
			}
			if len(args) == 0 {
				t.Errorf("%s: %q bound no argument", driver, tmpl)
			}
		}
	}
}
