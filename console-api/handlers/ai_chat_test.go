package handlers

import (
	"strings"
	"testing"

	"github.com/getkipper/kipper/console-api/ai"
)

func TestBuildAIChatSystemPrompt_Empty(t *testing.T) {
	prompt := buildAIChatSystemPrompt(chatRequest{
		Runtime:  "node",
		Code:     "console.log('hi')",
		Messages: []ai.Message{{Role: "user", Content: "hi"}},
	})

	if !strings.Contains(prompt, "no service bindings, env vars, secrets, or dependencies are configured yet") {
		t.Errorf("expected empty-context warning, got prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "```javascript\nconsole.log('hi')\n```") {
		t.Errorf("expected code block with javascript fence, got:\n%s", prompt)
	}
}

func TestBuildAIChatSystemPrompt_FullContext(t *testing.T) {
	prompt := buildAIChatSystemPrompt(chatRequest{
		Runtime: "node",
		Code:    "// stub",
		Bindings: []chatBinding{
			{Service: "eventdb", Type: "postgres", Env: []string{"DB_HOST", "DB_PORT", "DB_USERNAME", "DB_PASSWORD", "DB_NAME"}},
		},
		EnvKeys:    []string{"REGISTRAR_HOST", "REGISTRAR_USERNAME"},
		SecretKeys: []string{"REGISTRAR_API_KEY"},
		Dependencies: map[string]string{
			"pg":    "8.11.5",
			"axios": "1.6.7",
		},
	})

	mustContain := []string{
		"eventdb (postgres) → DB_HOST, DB_PORT, DB_USERNAME, DB_PASSWORD, DB_NAME",
		"REGISTRAR_HOST, REGISTRAR_USERNAME",
		"REGISTRAR_API_KEY",
		"axios@1.6.7",
		"pg@8.11.5",
		"Only import packages that appear in the Dependencies list",
	}
	for _, want := range mustContain {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected prompt to contain %q, got:\n%s", want, prompt)
		}
	}

	mustNotContain := []string{
		"No external npm/pip packages are available", // the old prompt's lie
	}
	for _, banned := range mustNotContain {
		if strings.Contains(prompt, banned) {
			t.Errorf("prompt still contains stale claim %q", banned)
		}
	}
}

func TestBuildAIChatSystemPrompt_Python(t *testing.T) {
	prompt := buildAIChatSystemPrompt(chatRequest{
		Runtime: "python",
		Code:    "print('hi')",
	})
	if !strings.Contains(prompt, "```python\nprint('hi')\n```") {
		t.Errorf("expected python fence, got:\n%s", prompt)
	}
}

func TestBuildAIChatSystemPrompt_PassesError(t *testing.T) {
	prompt := buildAIChatSystemPrompt(chatRequest{
		Runtime: "node",
		Code:    "// stub",
		Error:   "ECONNREFUSED at db:5432",
	})
	if !strings.Contains(prompt, "ECONNREFUSED at db:5432") {
		t.Errorf("expected error context to be included, got:\n%s", prompt)
	}
}

func TestBuildAIChatSystemPrompt_DBMode_EmptySchema(t *testing.T) {
	prompt := buildAIChatSystemPrompt(chatRequest{
		Mode:     "db",
		DBSchema: &chatDBSchema{Dialect: "postgres"},
	})
	if !strings.Contains(prompt, "Kipper AI database assistant") {
		t.Errorf("expected DB assistant intro, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "postgres database") {
		t.Errorf("expected dialect mention, got:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Database schema: empty or unknown") {
		t.Errorf("expected empty-schema warning, got:\n%s", prompt)
	}
}

func TestBuildAIChatSystemPrompt_DBMode_FullSchema(t *testing.T) {
	prompt := buildAIChatSystemPrompt(chatRequest{
		Mode: "db",
		SQL:  "SELECT * FROM domains WHERE owner_id = 7",
		DBSchema: &chatDBSchema{
			Dialect: "postgres",
			Tables: []chatDBTable{
				{
					Schema: "public", Name: "domains",
					Columns: []chatDBColumn{
						{Name: "id", Type: "bigint", Nullable: false, PK: true},
						{Name: "name", Type: "text", Nullable: false},
						{Name: "owner_id", Type: "bigint", Nullable: true},
					},
					Indexes: []chatDBIndex{
						{Name: "domains_pkey", Columns: []string{"id"}, Unique: true},
						{Name: "domains_owner_idx", Columns: []string{"owner_id"}},
					},
					ForeignKeys: []chatDBForeignKey{
						{Column: "owner_id", RefSchema: "public", RefTable: "users", RefColumn: "id"},
					},
				},
			},
		},
	})

	mustContain := []string{
		"Table public.domains:",
		"id bigint NOT NULL PRIMARY KEY",
		"name text NOT NULL",
		"owner_id bigint NULL",
		"domains_pkey UNIQUE on (id)",
		"domains_owner_idx on (owner_id)",
		"owner_id -> public.users.id",
		"```sql\nSELECT * FROM domains WHERE owner_id = 7\n```",
		"Postgres specifics",
	}
	for _, want := range mustContain {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected prompt to contain %q, got:\n%s", want, prompt)
		}
	}
}

func TestBuildAIChatSystemPrompt_DBMode_MySQLDialect(t *testing.T) {
	prompt := buildAIChatSystemPrompt(chatRequest{
		Mode:     "db",
		DBSchema: &chatDBSchema{Dialect: "mysql"},
	})
	if !strings.Contains(prompt, "MySQL specifics") {
		t.Errorf("expected MySQL specifics, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "Postgres specifics") {
		t.Errorf("should not include Postgres specifics for MySQL prompt")
	}
}
