package service

import (
	"bytes"
	"compress/gzip"
	"os"
	"os/exec"
	"path/filepath"
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
			opts:    ImportOptions{Database: "acme", SourceDatabase: "prod", Format: DumpGzip},
			want:    []string{`--nsFrom="prod.*"`, `--nsTo="acme.*"`},
		},
		{
			name:    "mongodb rename needs both names",
			svcType: "mongodb",
			opts:    ImportOptions{Database: "acme"},
			wantErr: "--source-database",
		},
		{
			name:    "postgres custom format with drop",
			svcType: "postgres",
			opts:    ImportOptions{Database: "app", Drop: true, Format: DumpPGCustom},
			// The source's roles do not exist on the target, so the archive's
			// grants to them are left out.
			want: []string{"pg_restore", `-d "app"`, "--no-owner", "--no-privileges", "--clean --if-exists"},
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
			want:    []string{"DROP DATABASE IF EXISTS \\`app\\`", "CREATE DATABASE \\`app\\`", "gunzip -c", "MYSQL_PWD=", `echo $? >"$s"`},
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
			database: "acme",
			want:     []string{"mongodump --archive --gzip", `--db="acme"`, `--config="$c"`},
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

// recordingTool logs arguments to $LOG and reads stdin unless given a statement
// to run. It exits with status 3 when its arguments contain a nonempty $FAIL_ON.
// A database existence query returns $EXISTS.
const recordingTool = `#!/bin/sh
echo "$(basename "$0") $*" >> "$LOG"
case " $* " in *" -c "*|*" -e "*) ;; *) cat >/dev/null ;; esac
case "$*" in *"SELECT 1"*) printf '%s' "$EXISTS" ;; esac
if [ -n "$FAIL_ON" ]; then case "$*" in *"$FAIL_ON"*) exit 3 ;; esac; fi
exit 0
`

// runImport runs an import script against stand-ins for the engine tools and
// returns their invocations, one per line, and whether the script succeeded.
func runImport(t *testing.T, svcType string, opts ImportOptions, input []byte, env ...string) (string, bool) {
	t.Helper()
	cmd, err := BuildImportCommand(svcType, opts)
	if err != nil {
		t.Fatalf("BuildImportCommand: %v", err)
	}
	bin := t.TempDir()
	log := filepath.Join(bin, "calls")
	for _, tool := range []string{"mysql", "psql", "pg_restore"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(recordingTool), 0o755); err != nil { //nolint:gosec // test-local executable
			t.Fatal(err)
		}
	}
	run := exec.Command("sh", "-c", cmd[2]) //nolint:gosec // the script under test
	run.Env = append([]string{"PATH=" + bin + ":/usr/bin:/bin", "LOG=" + log, "MYSQL_ROOT_PASSWORD=x", "POSTGRES_USER=legacy"}, env...)
	run.Stdin = bytes.NewReader(input)
	runErr := run.Run()
	calls, _ := os.ReadFile(log)
	return string(calls), runErr == nil
}

func runMySQLImport(t *testing.T, opts ImportOptions, env ...string) string {
	t.Helper()
	calls, ok := runImport(t, "mysql", opts, []byte("CREATE TABLE t (id int);\n"), env...)
	if !ok {
		t.Fatalf("import script failed:\n%s", calls)
	}
	return calls
}

func gzipped(t *testing.T, text string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A refused CREATE or GRANT must stop the import before anything is loaded.
func TestMySQLImportStopsWhenThePreparationFails(t *testing.T) {
	calls, ok := runImport(t, "mysql", ImportOptions{Database: "shop_data", Format: DumpGzip},
		gzipped(t, "CREATE TABLE t (id int);\n"), "MYSQL_USER=legacy", "FAIL_ON=GRANT")

	if ok {
		t.Error("the import reported success after its preparation failed")
	}
	if strings.Contains(calls, "-u root shop_data") {
		t.Errorf("the dump was loaded after the preparation failed:\n%s", calls)
	}
}

// A fresh service may lack the requested database, and both pg_restore and
// psql require it to exist before loading data.
func TestPostgresImportCreatesAMissingDatabase(t *testing.T) {
	for _, format := range []DumpFormat{DumpPGCustom, DumpPlain} {
		calls, ok := runImport(t, "postgres", ImportOptions{Database: "orders", Format: format}, []byte("x"))
		if !ok {
			t.Fatalf("import failed:\n%s", calls)
		}
		if !strings.Contains(calls, `CREATE DATABASE "orders"`) {
			t.Errorf("format %d: the missing database is not created:\n%s", format, calls)
		}
	}

	calls, _ := runImport(t, "postgres", ImportOptions{Database: "orders", Format: DumpPGCustom}, []byte("x"), "EXISTS=1")
	if strings.Contains(calls, "CREATE DATABASE") {
		t.Errorf("an existing database is created again:\n%s", calls)
	}
}

func TestPostgresImportStopsWhenTheDatabaseCannotBeCreated(t *testing.T) {
	calls, ok := runImport(t, "postgres", ImportOptions{Database: "orders", Format: DumpPGCustom}, []byte("x"), "FAIL_ON=CREATE DATABASE")

	if ok || strings.Contains(calls, "pg_restore") {
		t.Errorf("the dump was restored after the database could not be created:\n%s", calls)
	}
}

// A migration's manual steps import each database into a target service that
// has never seen it. mysql refuses a dump into a database that does not exist,
// and a database root created leaves the service user without access to it.
func TestMySQLImportCreatesTheDatabaseAndGrantsTheServiceUser(t *testing.T) {
	calls := runMySQLImport(t, ImportOptions{Database: "shop_data"}, "MYSQL_USER=legacy")

	if !strings.Contains(calls, "CREATE DATABASE IF NOT EXISTS `shop_data`") {
		t.Errorf("the database is not created first:\n%s", calls)
	}
	if !strings.Contains(calls, "GRANT ALL PRIVILEGES ON `shop_data`.* TO 'legacy'@'%'") {
		t.Errorf("the service user is not granted the database:\n%s", calls)
	}
	if !strings.Contains(calls, "-u root shop_data") {
		t.Errorf("the dump is not loaded into the database:\n%s", calls)
	}
}

func TestMySQLImportGrantsNobodyForARootService(t *testing.T) {
	calls := runMySQLImport(t, ImportOptions{Database: "shop_data"})

	if strings.Contains(calls, "GRANT") {
		t.Errorf("a service that authenticates as root has no user to grant:\n%s", calls)
	}
	if !strings.Contains(calls, "CREATE DATABASE IF NOT EXISTS `shop_data`") {
		t.Errorf("the database is not created first:\n%s", calls)
	}
}

func TestMySQLImportWithDropGrantsTheServiceUser(t *testing.T) {
	calls := runMySQLImport(t, ImportOptions{Database: "shop_data", Drop: true}, "MYSQL_USER=legacy")

	if !strings.Contains(calls, "DROP DATABASE IF EXISTS `shop_data`; CREATE DATABASE `shop_data`") {
		t.Errorf("the database is not replaced:\n%s", calls)
	}
	if !strings.Contains(calls, "GRANT ALL PRIVILEGES ON `shop_data`.* TO 'legacy'@'%'") {
		t.Errorf("the service user is not granted the new database:\n%s", calls)
	}
	// mysql -e stops at the first failing statement, so a grant that fails
	// leaves the existing database in place.
	if strings.Index(calls, "GRANT") > strings.Index(calls, "DROP DATABASE") {
		t.Errorf("the grant runs after the database is dropped:\n%s", calls)
	}
}

// The service user's name goes into a SQL string, so a name with a quote or a
// backslash is refused before anything is dropped or loaded.
func TestMySQLImportRefusesAUserNameItCannotQuote(t *testing.T) {
	for _, user := range []string{"o'reilly", `back\slash`, "semi;colon"} {
		calls, ok := runImport(t, "mysql", ImportOptions{Database: "shop_data", Drop: true},
			[]byte("CREATE TABLE t (id int);\n"), "MYSQL_USER="+user)

		if ok {
			t.Errorf("%s: the import reported success", user)
		}
		if calls != "" {
			t.Errorf("%s: mysql ran anyway:\n%s", user, calls)
		}
	}
}

// Include scheduled events, as automatic migration does, to preserve the
// app's periodic jobs in the export.
func TestMySQLExportKeepsEventsRoutinesAndTriggers(t *testing.T) {
	cmd, err := BuildExportCommand("mysql", "shop")
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Join(cmd, " ")
	for _, flag := range []string{"--events", "--routines", "--triggers"} {
		if !strings.Contains(script, flag) {
			t.Errorf("the export leaves out %s: %s", flag, script)
		}
	}
}
