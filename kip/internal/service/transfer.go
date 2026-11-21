package service

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/mongotool"
)

// Import and export run the engine's own dump/restore tools inside the
// service pod, streaming the dump file over the exec connection. Credentials
// stay out of the child processes' argv: postgres authenticates over the
// pod-local socket, mysql reads MYSQL_PWD from the environment, and the
// mongodb tools read the password from a private, short-lived config file,
// their documented mechanism for sensitive options.

// TransferSupported reports whether a service type has import/export tooling
// in its image.
func TransferSupported(svcType string) bool {
	switch svcType {
	case "postgres", "mysql", "mongodb":
		return true
	}
	return false
}

// databaseNamePattern keeps interpolated database names shell- and
// engine-safe. Anything fancier than this is a sign the dump should be
// loaded by hand.
var databaseNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func validDatabaseName(name string) error {
	if !databaseNamePattern.MatchString(name) {
		return fmt.Errorf("invalid database name %q (allowed: letters, digits, _ and -)", name)
	}
	return nil
}

// pipeline joins producer | consumer so the script fails when either side
// fails. Plain sh reports only the last command's status, and pipefail is
// not available on every image's shell, so the producer's status travels
// through a private temp file. Without this, a corrupt gzip stream or a
// failed dump would exit 0 and bless a partial import or an empty backup.
func pipeline(producer, consumer string) string {
	return fmt.Sprintf(
		`s="$(mktemp)"; { %s; echo $? >"$s"; } | { %s; }; rc=$?; prc="$(cat "$s")"; rm -f "$s"; [ "$prc" = 0 ] && [ "$rc" = 0 ]`,
		producer, consumer)
}

// afterPreparing runs load only once prepare has succeeded, and otherwise
// exits with prepare's status. load may be a list of commands, such as a
// pipeline, so a plain && would guard only its first one.
func afterPreparing(prepare, load string) string {
	return prepare + " || exit $?; " + load
}

// mysqlClient prefixes a mysql/mysqldump invocation with the password in the
// child's environment (argv stays clean).
const mysqlClient = `MYSQL_PWD="$MYSQL_ROOT_PASSWORD" `

// DumpFormat describes what the first bytes of an import file revealed.
type DumpFormat int

const (
	DumpPlain DumpFormat = iota
	DumpGzip
	DumpPGCustom
)

// SniffDumpFormat recognizes gzip streams (SQL scripts or mongodump archives)
// and Postgres custom archives (pg_dump -F c) by their leading bytes. It returns
// DumpPlain for an unrecognized or incomplete header, including empty input.
// For Postgres and MySQL, DumpPlain is treated as a plain SQL script.
func SniffDumpFormat(head []byte) DumpFormat {
	if len(head) >= 2 && head[0] == 0x1f && head[1] == 0x8b {
		return DumpGzip
	}
	if len(head) >= 5 && string(head[:5]) == "PGDMP" {
		return DumpPGCustom
	}
	return DumpPlain
}

// ImportOptions configures BuildImportCommand.
type ImportOptions struct {
	Database       string
	SourceDatabase string
	Drop           bool
	Format         DumpFormat
}

// BuildImportCommand returns a command to run inside the service pod with the
// dump streamed to stdin. Format selects how the dump is read.
//
// Postgres and MySQL require Database. MongoDB restores the archive's original
// database names when both names are empty. To rename a MongoDB database, both
// SourceDatabase and Database are required for mongorestore --nsFrom and --nsTo.
// SourceDatabase is supported only for MongoDB. Database names must contain
// only ASCII letters, digits, underscores and hyphens.
//
// Drop drops MongoDB collections before restoring them, cleans objects before
// restoring a Postgres custom archive, or recreates the MySQL target database.
// Plain and gzipped Postgres SQL scripts must manage their own DROP statements.
// The returned command creates a missing Postgres or MySQL database before
// loading data and grants the MySQL service user access when MYSQL_USER is set.
// Postgres custom archives restore without their original owners or grants.
// Unsupported service types, invalid names and incompatible options or formats
// return an error and a nil command.
func BuildImportCommand(svcType string, opts ImportOptions) ([]string, error) {
	switch svcType {
	case "mongodb":
		return buildMongoImport(opts)
	case "postgres":
		return buildPostgresImport(opts)
	case "mysql":
		return buildMySQLImport(opts)
	}
	return nil, fmt.Errorf("service type %q does not support import (supported: mongodb, postgres, mysql)", svcType)
}

func buildMongoImport(opts ImportOptions) ([]string, error) {
	if opts.Format == DumpPGCustom {
		return nil, fmt.Errorf("the file is a postgres custom-format dump, not a mongodump archive")
	}
	invocation := "mongorestore --archive"
	if opts.Format == DumpGzip {
		invocation += " --gzip"
	}
	if opts.Drop {
		invocation += " --drop"
	}
	if opts.Database != "" || opts.SourceDatabase != "" {
		if opts.Database == "" || opts.SourceDatabase == "" {
			return nil, fmt.Errorf("mongodb rename needs both --database (target) and --source-database (name inside the dump)")
		}
		if err := validDatabaseName(opts.Database); err != nil {
			return nil, err
		}
		if err := validDatabaseName(opts.SourceDatabase); err != nil {
			return nil, err
		}
		invocation += fmt.Sprintf(" --nsFrom=%q --nsTo=%q", opts.SourceDatabase+".*", opts.Database+".*")
	}
	return []string{"sh", "-c", mongotool.Script(invocation)}, nil
}

func buildPostgresImport(opts ImportOptions) ([]string, error) {
	if opts.Database == "" {
		return nil, fmt.Errorf("postgres import needs --database")
	}
	if err := validDatabaseName(opts.Database); err != nil {
		return nil, err
	}
	if opts.SourceDatabase != "" {
		return nil, fmt.Errorf("--source-database is only used for mongodb archives")
	}
	// A fresh service may lack the requested database, and both restore tools
	// require it to exist before loading data.
	// The quotes are SQL identifier quotes, which %q would not produce, and
	// validDatabaseName has already ruled out a quote in the name.
	create := fmt.Sprintf( //nolint:gocritic // see above
		`psql -U "$POSTGRES_USER" -d postgres -At -c "SELECT 1 FROM pg_database WHERE datname = '%s'" | grep -q 1 || psql -U "$POSTGRES_USER" -d postgres -c 'CREATE DATABASE "%s"'`,
		opts.Database, opts.Database)
	if opts.Format == DumpPGCustom {
		// The archive's owners and grants name roles from wherever it was
		// dumped, which a Kipper service does not have.
		script := fmt.Sprintf(`pg_restore -U "$POSTGRES_USER" -d %q --no-owner --no-privileges`, opts.Database)
		if opts.Drop {
			script += " --clean --if-exists"
		}
		return []string{"sh", "-c", afterPreparing(create, script)}, nil
	}
	if opts.Drop {
		return nil, fmt.Errorf("--drop needs a custom-format dump (pg_dump -F c). A plain SQL script manages its own DROP statements")
	}
	load := fmt.Sprintf(`psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d %q`, opts.Database)
	if opts.Format == DumpGzip {
		load = pipeline("gunzip -c", load)
	}
	return []string{"sh", "-c", afterPreparing(create, load)}, nil
}

func buildMySQLImport(opts ImportOptions) ([]string, error) {
	if opts.Database == "" {
		return nil, fmt.Errorf("mysql import needs --database")
	}
	if err := validDatabaseName(opts.Database); err != nil {
		return nil, err
	}
	if opts.SourceDatabase != "" {
		return nil, fmt.Errorf("--source-database is only used for mongodb archives")
	}
	if opts.Format == DumpPGCustom {
		return nil, fmt.Errorf("the file is a postgres custom-format dump, not a mysql dump")
	}
	load := fmt.Sprintf(mysqlClient+`mysql -u root %q`, opts.Database)
	if opts.Format == DumpGzip {
		load = pipeline("gunzip -c", load)
	}
	// The database may not exist yet, as on a cluster a project is moving to,
	// and one root creates is closed to the service user until it is granted.
	// MYSQL_USER is unset for a service that authenticates as root.
	db := backtick(opts.Database)
	create := "CREATE DATABASE IF NOT EXISTS " + db + ";"
	if opts.Drop {
		create = "DROP DATABASE IF EXISTS " + db + "; CREATE DATABASE " + db + ";"
	}
	// MySQL allows grants before a database exists. Grant access first so
	// mysql -e stops on a grant failure before dropping any existing database.
	statements := `${MYSQL_USER:+GRANT ALL PRIVILEGES ON ` + db + `.* TO '$MYSQL_USER'@'%'; }` + create
	// Escape backticks to prevent command substitution inside shell double quotes.
	// The username becomes a SQL string, so reject unsupported characters
	// before changing any data.
	prepare := mysqlUserGuard + mysqlClient + `mysql -u root -e "` + strings.ReplaceAll(statements, "`", "\\`") + `"`
	return []string{"sh", "-c", afterPreparing(prepare, load)}, nil
}

// mysqlUserGuard restricts MYSQL_USER to ASCII letters, digits, underscores,
// periods and hyphens so it can be used in a SQL string without escaping.
const mysqlUserGuard = `case "$MYSQL_USER" in *[!A-Za-z0-9_.-]*) echo "the service username $MYSQL_USER requires SQL escaping, which kip does not support" >&2; exit 1;; esac; `

// backtick quotes a validated identifier for a mysql -e statement.
func backtick(name string) string {
	return "`" + name + "`"
}

// BuildExportCommand returns the command whose stdout is the dump stream.
// mongodb produces a gzipped archive, postgres a custom-format dump
// (pg_restore input), and mysql a gzipped SQL script that includes routines,
// triggers, and scheduled events.
// An empty database exports all MongoDB databases. Postgres and MySQL require
// a database. Names must contain only ASCII letters, digits, underscores and
// hyphens. Invalid names, missing required databases and unsupported service
// types return an error and a nil command.
func BuildExportCommand(svcType, database string) ([]string, error) {
	if database != "" {
		if err := validDatabaseName(database); err != nil {
			return nil, err
		}
	}
	switch svcType {
	case "mongodb":
		invocation := "mongodump --archive --gzip"
		if database != "" {
			invocation += fmt.Sprintf(" --db=%q", database)
		}
		return []string{"sh", "-c", mongotool.Script(invocation)}, nil
	case "postgres":
		if database == "" {
			return nil, fmt.Errorf("postgres export needs --database")
		}
		return []string{"sh", "-c", fmt.Sprintf(`pg_dump -U "$POSTGRES_USER" -F c -d %q`, database)}, nil
	case "mysql":
		if database == "" {
			return nil, fmt.Errorf("mysql export needs --database")
		}
		dump := fmt.Sprintf(mysqlClient+`mysqldump --single-transaction --routines --triggers --events %q`, database)
		return []string{"sh", "-c", pipeline(dump, "gzip")}, nil
	}
	return nil, fmt.Errorf("service type %q does not support export (supported: mongodb, postgres, mysql)", svcType)
}

// ExportExtension returns the file extension, including the leading dot, for
// BuildExportCommand's output, or an empty string for an unsupported service type.
func ExportExtension(svcType string) string {
	switch svcType {
	case "mongodb":
		return ".archive.gz"
	case "postgres":
		return ".dump"
	case "mysql":
		return ".sql.gz"
	}
	return ""
}
