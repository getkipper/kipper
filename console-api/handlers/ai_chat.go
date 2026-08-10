package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/getkipper/kipper/console-api/ai"
)

// AIChat handles streaming chat with the configured AI provider.
type AIChat struct {
	Settings *AISettings
}

// chatBinding mirrors the form's view of a service binding for the AI.
// service + type + the env var names the binding injects.
type chatBinding struct {
	Service string   `json:"service"`
	Type    string   `json:"type"`
	Env     []string `json:"env"`
}

// chatDBColumn / chatDBIndex / chatDBTable / chatDBSchema describe the
// live database schema in just enough detail for the AI to write
// correct DDL or SQL. Names + types only — no row data.
type chatDBColumn struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	PK       bool   `json:"pk,omitempty"`
}

type chatDBIndex struct {
	Name    string   `json:"name"`
	Columns []string `json:"columns"`
	Unique  bool     `json:"unique,omitempty"`
}

type chatDBForeignKey struct {
	Column    string `json:"column"`
	RefSchema string `json:"ref_schema"`
	RefTable  string `json:"ref_table"`
	RefColumn string `json:"ref_column"`
}

type chatDBTable struct {
	Schema      string             `json:"schema"`
	Name        string             `json:"name"`
	Columns     []chatDBColumn     `json:"columns"`
	Indexes     []chatDBIndex      `json:"indexes,omitempty"`
	ForeignKeys []chatDBForeignKey `json:"foreign_keys,omitempty"`
}

type chatDBSchema struct {
	Dialect string        `json:"dialect"` // "postgres" | "mysql"
	Tables  []chatDBTable `json:"tables"`
}

type chatRequest struct {
	Messages []ai.Message `json:"messages"`
	Code     string       `json:"code"`
	Runtime  string       `json:"runtime"`
	Error    string       `json:"error,omitempty"`

	// Mode selects the system prompt. "" or "code" → function code
	// assistant (existing). "db" → SQL/DBA assistant that uses the
	// schema context below.
	Mode string `json:"mode,omitempty"`

	// Function-environment context. The AI uses these to write code that
	// reads the right env var names and imports the right packages.
	// Secret VALUES are never sent — names only.
	Bindings     []chatBinding     `json:"bindings,omitempty"`
	EnvKeys      []string          `json:"env_keys,omitempty"`
	SecretKeys   []string          `json:"secret_keys,omitempty"`
	Dependencies map[string]string `json:"dependencies,omitempty"`

	// Database context for mode=db. Names + types only.
	DBSchema *chatDBSchema `json:"db_schema,omitempty"`
	// SQL is the current contents of the SQL editor. Surfaced to the
	// model so "explain this query" and "make this faster" prompts
	// have something to work with.
	SQL string `json:"sql,omitempty"`
}

// Chat streams an AI response via Server-Sent Events.
// POST /api/v1/ai/chat
func (a *AIChat) Chat(w http.ResponseWriter, r *http.Request) {
	// 8 MB ceiling on the request body. Frontend caps attachments at
	// 1 MB; the rest is room for accumulated chat history and the
	// per-message metadata (code, schema, etc).
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)

	var req chatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.Messages) == 0 {
		respondError(w, http.StatusBadRequest, "messages required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	cfg, err := a.Settings.GetRaw(ctx)
	if err != nil || cfg.Provider == "" {
		respondError(w, http.StatusBadRequest, "AI not configured — go to Settings to add a provider")
		return
	}

	provider, err := ai.NewProvider(ai.Config{
		Provider:  cfg.Provider,
		APIKey:    cfg.APIKey,
		Model:     cfg.Model,
		OllamaURL: cfg.OllamaURL,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create AI provider: %v", err))
		return
	}

	system := buildAIChatSystemPrompt(req)

	stream, err := provider.Chat(ctx, system, req.Messages)
	if err != nil {
		respondError(w, http.StatusBadGateway, fmt.Sprintf("AI provider error: %v", err))
		return
	}

	// Stream response via SSE
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		respondError(w, http.StatusInternalServerError, "streaming not supported")
		return
	}

	for chunk := range stream {
		if chunk.Err != nil {
			_, _ = fmt.Fprintf(w, "data: {\"error\": %q}\n\n", chunk.Err.Error())
			flusher.Flush()
			return
		}

		if chunk.Done {
			_, _ = fmt.Fprintf(w, "data: {\"done\": true}\n\n")
			flusher.Flush()
			return
		}

		data, _ := json.Marshal(map[string]string{"content": chunk.Content})
		_, _ = fmt.Fprintf(w, "data: %s\n\n", data)
		flusher.Flush()
	}
}

// buildAIChatSystemPrompt assembles the system prompt sent to the model.
// It interpolates runtime details, the current code, any error context,
// and — most importantly for Phase E — the function's environment so
// the model writes code that uses the actual env var names and only
// imports packages the user has declared as dependencies.
func buildAIChatSystemPrompt(req chatRequest) string {
	if req.Mode == "db" {
		return buildAIChatDBSystemPrompt(req)
	}

	lang := "javascript"
	if req.Runtime == "python" {
		lang = "python"
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are the Kipper AI code assistant, a helpful coding partner built into the Kipper serverless function editor.\n\n")
	fmt.Fprintf(&b, "The user is editing a %s function on the Kipper platform. Your job is to help them write, fix, or improve their code.\n\n", req.Runtime)

	b.WriteString("Rules:\n")
	b.WriteString("- Be concise and direct. Lead with the code, explain after.\n")
	b.WriteString("- When suggesting code changes, return the complete updated function, not just a diff.\n")
	b.WriteString("- Wrap code in markdown code blocks with the correct language tag.\n")
	b.WriteString("- If the user shares an error, diagnose the root cause and provide a fix.\n")
	b.WriteString("- Read configuration from environment variables only. Never hardcode hosts, ports, credentials, or API keys.\n")
	b.WriteString("- Only import packages that appear in the Dependencies list below. If you need a package that isn't listed, tell the user to add it before importing.\n")
	b.WriteString("- Keep functions compact and under 500 lines. Avoid placeholder comments like \"// add more here\".\n")
	b.WriteString("- Never apologise. Never use filler phrases.\n\n")

	writeContextBlock(&b, req)

	fmt.Fprintf(&b, "Current code:\n```%s\n%s\n```", lang, req.Code)

	if req.Error != "" {
		fmt.Fprintf(&b, "\n\nThe function is currently failing with this error:\n```\n%s\n```", req.Error)
	}
	return b.String()
}

// buildAIChatDBSystemPrompt produces the SQL/DBA-flavoured system
// prompt used when the user is in the database console. The AI gets
// dialect, table+column shapes, indexes, and FKs so it can write DDL
// and queries that match the real schema instead of guessing names.
func buildAIChatDBSystemPrompt(req chatRequest) string {
	dialect := "postgres"
	if req.DBSchema != nil && req.DBSchema.Dialect != "" {
		dialect = req.DBSchema.Dialect
	}

	var b strings.Builder
	fmt.Fprintf(&b, "You are the Kipper AI database assistant, a helpful DBA built into the Kipper database console.\n\n")
	fmt.Fprintf(&b, "The user is working with a %s database. Your job is to help them write SQL, design schema, optimise queries, and explain results.\n\n", dialect)

	b.WriteString("Rules:\n")
	b.WriteString("- Be concise and direct. Lead with the SQL, explain after.\n")
	b.WriteString("- Wrap SQL in fenced code blocks tagged `sql` so the user can apply it with one click.\n")
	b.WriteString("- Use the dialect-correct syntax for the target. ")
	if dialect == "postgres" {
		b.WriteString("Postgres specifics: jsonb, tsvector, arrays, RETURNING, CREATE INDEX CONCURRENTLY, partial indexes (WHERE), gen_random_uuid().\n")
	} else {
		b.WriteString("MySQL specifics: backticked identifiers, AUTO_INCREMENT, JSON type, no RETURNING, no partial indexes.\n")
	}
	b.WriteString("- Always quote identifiers when they contain mixed case or special characters.\n")
	b.WriteString("- Prefer transactions for multi-statement DDL. Recommend `CREATE INDEX CONCURRENTLY` on Postgres for hot tables.\n")
	b.WriteString("- For destructive changes (DROP TABLE, DROP COLUMN, TRUNCATE), warn the user and suggest a backup first.\n")
	b.WriteString("- When the user asks to add columns, indexes, or constraints, return the exact ALTER TABLE statement, then a one-line explanation.\n")
	b.WriteString("- For \"explain this query\" prompts, describe what it does in plain English, list the tables / indexes it uses, and call out anything obviously slow (full scans, missing indexes, cross joins).\n")
	b.WriteString("- Never invent table or column names. Use only what's listed in the schema context below.\n")
	b.WriteString("- Never apologise. Never use filler phrases.\n\n")

	writeDBContextBlock(&b, req)

	if strings.TrimSpace(req.SQL) != "" {
		fmt.Fprintf(&b, "Current SQL editor contents:\n```sql\n%s\n```\n", req.SQL)
	}

	if req.Error != "" {
		fmt.Fprintf(&b, "\nA recent query failed with:\n```\n%s\n```\n", req.Error)
	}

	return b.String()
}

func writeDBContextBlock(b *strings.Builder, req chatRequest) {
	s := req.DBSchema
	if s == nil || len(s.Tables) == 0 {
		b.WriteString("Database schema: empty or unknown. Ask the user to create tables before writing queries that depend on them.\n\n")
		return
	}
	b.WriteString("Database schema (use these exact names — do not invent any):\n\n")
	for _, t := range s.Tables {
		fmt.Fprintf(b, "Table %s.%s:\n", t.Schema, t.Name)
		for _, c := range t.Columns {
			marker := ""
			if c.PK {
				marker = " PRIMARY KEY"
			}
			nullable := "NOT NULL"
			if c.Nullable {
				nullable = "NULL"
			}
			fmt.Fprintf(b, "  - %s %s %s%s\n", c.Name, c.Type, nullable, marker)
		}
		if len(t.Indexes) > 0 {
			b.WriteString("  Indexes:\n")
			for _, idx := range t.Indexes {
				u := ""
				if idx.Unique {
					u = " UNIQUE"
				}
				fmt.Fprintf(b, "    - %s%s on (%s)\n", idx.Name, u, strings.Join(idx.Columns, ", "))
			}
		}
		if len(t.ForeignKeys) > 0 {
			b.WriteString("  Foreign keys:\n")
			for _, fk := range t.ForeignKeys {
				fmt.Fprintf(b, "    - %s -> %s.%s.%s\n", fk.Column, fk.RefSchema, fk.RefTable, fk.RefColumn)
			}
		}
		b.WriteString("\n")
	}
}

// writeContextBlock renders the function's environment context. The
// "Kipper knows" panel in the FunctionForm shows the same data to the
// user, so the user can verify the AI sees what they expect.
func writeContextBlock(b *strings.Builder, req chatRequest) {
	hasContext := len(req.Bindings) > 0 || len(req.EnvKeys) > 0 || len(req.SecretKeys) > 0 || len(req.Dependencies) > 0
	if !hasContext {
		b.WriteString("Function environment: no service bindings, env vars, secrets, or dependencies are configured yet. Tell the user what to add before writing code that depends on them.\n\n")
		return
	}

	b.WriteString("Function environment (use these exact names — do not invent variables):\n\n")

	if len(req.Bindings) > 0 {
		b.WriteString("Service bindings:\n")
		for _, bind := range req.Bindings {
			fmt.Fprintf(b, "- %s (%s) → %s\n", bind.Service, bind.Type, strings.Join(bind.Env, ", "))
		}
		b.WriteString("\n")
	}

	if len(req.EnvKeys) > 0 {
		sorted := append([]string{}, req.EnvKeys...)
		sort.Strings(sorted)
		fmt.Fprintf(b, "Environment variables: %s\n\n", strings.Join(sorted, ", "))
	}

	if len(req.SecretKeys) > 0 {
		sorted := append([]string{}, req.SecretKeys...)
		sort.Strings(sorted)
		fmt.Fprintf(b, "Secrets (also exposed as env vars; values are write-only and not visible to you): %s\n\n", strings.Join(sorted, ", "))
	}

	if len(req.Dependencies) > 0 {
		keys := make([]string, 0, len(req.Dependencies))
		for k := range req.Dependencies {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteString("Installed dependencies:\n")
		for _, k := range keys {
			fmt.Fprintf(b, "- %s@%s\n", k, req.Dependencies[k])
		}
		b.WriteString("\n")
	}
}
