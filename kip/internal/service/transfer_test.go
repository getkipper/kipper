package service

import (
	"os/exec"
	"strings"
	"testing"
)

func TestSniffDumpFormat(t *testing.T) {
	tests := []struct {
		name string
		head []byte
		want DumpFormat
	}{
		{"gzip", []byte{0x1f, 0x8b, 0x08, 0x00}, DumpGzip},
		{"pg custom", []byte("PGDMP\x01"), DumpPGCustom},
		{"plain sql", []byte("-- MySQL dump"), DumpPlain},
		{"empty", nil, DumpPlain},
		{"one byte", []byte{0x1f}, DumpPlain},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SniffDumpFormat(tt.head); got != tt.want {
				t.Errorf("SniffDumpFormat(%v) = %v, want %v", tt.head, got, tt.want)
			}
		})
	}
}

func TestTransferSupported(t *testing.T) {
	for _, svc := range []string{"postgres", "mysql", "mongodb"} {
		if !TransferSupported(svc) {
			t.Errorf("expected %s to support transfer", svc)
		}
	}
	for _, svc := range []string{"redis", "rabbitmq", "minio", "mailhog", "opensearch", ""} {
		if TransferSupported(svc) {
			t.Errorf("expected %s to not support transfer", svc)
		}
	}
}

func TestBuildImportCommand(t *testing.T) {
	tests := []struct {
		name    string
		svcType string
		opts    ImportOptions
		want    []string // substrings the script must contain
		wantNot []string // substrings the script must not contain
		wantErr string
	}{
		{
			name:    "mongodb gzip archive",
			svcType: "mongodb",
			opts:    ImportOptions{Format: DumpGzip},
			want:    []string{"mongorestore --archive", "--gzip", `--config="$c"`, "chmod 600", "--authenticationDatabase admin"},
			wantNot: []string{"--drop", "--nsFrom", `-p "$MONGO_INITDB_ROOT_PASSWORD"`},
		},
		{
			name:    "mongodb plain archive with drop",
			svcType: "mongodb",
			opts:    ImportOptions{Drop: true},
			want:    []string{"mongorestore --archive", "--drop"},
			wantNot: []string{"--gzip"},
		},
		{
			name:    "mongodb rename",
			svcType: "mongodb",
			opts:    ImportOptions{Database: "supplemento", SourceDatabase: "prod", Format: DumpGzip},
			want:    []string{`--nsFrom="prod.*"`, `--nsTo="supplemento.*"`},
		},
		{
			name:    "mongodb rename needs both names",
			svcType: "mongodb",
			opts:    ImportOptions{Database: "supplemento"},
			wantErr: "--source-database",
		},
		{
			name:    "postgres custom format with drop",
			svcType: "postgres",
			opts:    ImportOptions{Database: "app", Drop: true, Format: DumpPGCustom},
			want:    []string{"pg_restore", `-d "app"`, "--no-owner", "--clean --if-exists"},
		},
		{
			name:    "postgres plain sql",
			svcType: "postgres",
			opts:    ImportOptions{Database: "app"},
			want:    []string{"psql", "ON_ERROR_STOP=1", `-d "app"`},
			wantNot: []string{"gunzip"},
		},
		{
			name:    "postgres gzipped sql propagates gunzip failures",
			svcType: "postgres",
			opts:    ImportOptions{Database: "app", Format: DumpGzip},
			want:    []string{"gunzip -c", "psql", `echo $? >"$s"`, `[ "$prc" = 0 ]`},
		},
		{
			name:    "postgres drop needs custom format",
			svcType: "postgres",
			opts:    ImportOptions{Database: "app", Drop: true},
			wantErr: "custom-format",
		},
		{
			name:    "postgres needs database",
			svcType: "postgres",
			opts:    ImportOptions{},
			wantErr: "--database",
		},
		{
			name:    "mysql plain sql",
			svcType: "mysql",
			opts:    ImportOptions{Database: "app"},
			want:    []string{`MYSQL_PWD="$MYSQL_ROOT_PASSWORD" mysql -u root`, `"app"`},
			wantNot: []string{"gunzip", "DROP DATABASE", `-p"$MYSQL_ROOT_PASSWORD"`},
		},
		{
			name:    "mysql gzipped with drop",
			svcType: "mysql",
			opts:    ImportOptions{Database: "app", Drop: true, Format: DumpGzip},
			want:    []string{"DROP DATABASE IF EXISTS `app`", "CREATE DATABASE `app`", "gunzip -c", "MYSQL_PWD=", `echo $? >"$s"`},
			wantNot: []string{`-p"$MYSQL_ROOT_PASSWORD"`},
		},
		{
			name:    "mysql rejects pg dump",
			svcType: "mysql",
			opts:    ImportOptions{Database: "app", Format: DumpPGCustom},
			wantErr: "postgres custom-format",
		},
		{
			name:    "injection in database name",
			svcType: "postgres",
			opts:    ImportOptions{Database: `app"; rm -rf /`},
			wantErr: "invalid database name",
		},
		{
			name:    "injection in source database",
			svcType: "mongodb",
			opts:    ImportOptions{Database: "app", SourceDatabase: "x$(reboot)"},
			wantErr: "invalid database name",
		},
		{
			name:    "unsupported type",
			svcType: "redis",
			opts:    ImportOptions{},
			wantErr: "does not support import",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := BuildImportCommand(tt.svcType, tt.opts)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildImportCommand: %v", err)
			}
			if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
				t.Fatalf("expected sh -c wrapper, got %v", cmd)
			}
			for _, sub := range tt.want {
				if !strings.Contains(cmd[2], sub) {
					t.Errorf("script missing %q:\n%s", sub, cmd[2])
				}
			}
			for _, sub := range tt.wantNot {
				if strings.Contains(cmd[2], sub) {
					t.Errorf("script must not contain %q:\n%s", sub, cmd[2])
				}
			}
		})
	}
}

func TestBuildExportCommand(t *testing.T) {
	tests := []struct {
		name     string
		svcType  string
		database string
		want     []string
		wantErr  string
	}{
		{
			name:     "mongodb single database",
			svcType:  "mongodb",
			database: "supplemento",
			want:     []string{"mongodump --archive --gzip", `--db="supplemento"`, `--config="$c"`},
		},
		{
			name:    "mongodb all databases",
			svcType: "mongodb",
			want:    []string{"mongodump --archive --gzip", "chmod 600"},
		},
		{
			name:     "postgres",
			svcType:  "postgres",
			database: "app",
			want:     []string{"pg_dump", "-F c", `-d "app"`},
		},
		{
			name:    "postgres needs database",
			svcType: "postgres",
			wantErr: "--database",
		},
		{
			name:     "mysql",
			svcType:  "mysql",
			database: "app",
			want:     []string{"MYSQL_PWD=", "mysqldump", "--single-transaction", "gzip", `echo $? >"$s"`},
		},
		{
			name:     "injection rejected",
			svcType:  "mysql",
			database: "app; reboot",
			wantErr:  "invalid database name",
		},
		{
			name:    "unsupported type",
			svcType: "mailhog",
			wantErr: "does not support export",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd, err := BuildExportCommand(tt.svcType, tt.database)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildExportCommand: %v", err)
			}
			for _, sub := range tt.want {
				if !strings.Contains(cmd[2], sub) {
					t.Errorf("script missing %q:\n%s", sub, cmd[2])
				}
			}
		})
	}
}

func TestExportExtension(t *testing.T) {
	if got := ExportExtension("mongodb"); got != ".archive.gz" {
		t.Errorf("mongodb extension = %q", got)
	}
	if got := ExportExtension("postgres"); got != ".dump" {
		t.Errorf("postgres extension = %q", got)
	}
	if got := ExportExtension("mysql"); got != ".sql.gz" {
		t.Errorf("mysql extension = %q", got)
	}
	if got := ExportExtension("redis"); got != "" {
		t.Errorf("redis extension = %q", got)
	}
}

// TestPipelineStatusPropagation executes the pipeline construction in a real
// shell: a failing producer must fail the whole script even though the
// consumer exits cleanly, and vice versa. Plain `a | b` in sh reports only
// b's status, which is exactly the masked-failure bug this guards against.
func TestPipelineStatusPropagation(t *testing.T) {
	run := func(script string) error {
		return exec.Command("sh", "-c", script).Run() //nolint:gosec // test-local scripts built from constants
	}

	if err := run(pipeline("true", "cat >/dev/null")); err != nil {
		t.Errorf("healthy pipeline failed: %v", err)
	}
	if err := run(pipeline("false", "cat >/dev/null")); err == nil {
		t.Error("a failing producer must fail the pipeline")
	}
	if err := run(pipeline("true", "false")); err == nil {
		t.Error("a failing consumer must fail the pipeline")
	}
	// The control that motivates the construction: plain sh masks the
	// producer's failure.
	if err := run("false | cat >/dev/null"); err != nil {
		t.Error("expected plain sh to mask the producer failure (control)")
	}
}
