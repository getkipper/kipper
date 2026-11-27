package handlers

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// mysqlColumn splits a SHOW CREATE TABLE column definition into its type and
// remaining attributes (character set, nullability, default, AUTO_INCREMENT,
// ON UPDATE, and comment). MODIFY COLUMN replaces the whole definition, so
// edits preserve the other attributes except incompatible character sets,
// collations, and NULL defaults.
type mysqlColumn struct {
	typ       string
	rest      string
	generated bool
}

// mysqlColumns uses lowercase keys because MySQL column names are case-insensitive.
// A nil entry marks a definition invalidated by adding the column, changing its
// default, or adding a primary key earlier in the batch. Reload the definition
// after applying the batch before changing its type or nullability.
type mysqlColumns map[string]*mysqlColumn

// add records a column added in this batch, whose definition is unknown.
func (m mysqlColumns) add(name string) { m[strings.ToLower(name)] = nil }

func (m mysqlColumns) drop(name string) { delete(m, strings.ToLower(name)) }

// rename preserves the definition, including an unknown definition, under the
// new name. It leaves the map unchanged if the old name is absent.
func (m mysqlColumns) rename(oldName, newName string) {
	if c, ok := m[strings.ToLower(oldName)]; ok {
		delete(m, strings.ToLower(oldName))
		m[strings.ToLower(newName)] = c
	}
}

// forget marks an existing column's definition unknown for the rest of the batch.
func (m mysqlColumns) forget(name string) {
	if _, ok := m[strings.ToLower(name)]; ok {
		m[strings.ToLower(name)] = nil
	}
}

// prepareMySQLSession enables identifier quoting in SHOW CREATE TABLE output.
// It preserves sql_mode so designer DDL uses the same SQL semantics as the SQL
// editor. On success, it returns the identifier quote character, whether
// NO_BACKSLASH_ESCAPES is enabled, and a function that restores the original
// quoting setting. If restoration fails, that function discards the connection.
func prepareMySQLSession(ctx context.Context, conn *sql.Conn) (quote string, noBackslashEscapes bool, restore func(), err error) {
	var mode string
	var quoteShowCreate int
	if err := conn.QueryRowContext(ctx, "SELECT @@SESSION.sql_mode, @@SESSION.sql_quote_show_create").Scan(&mode, &quoteShowCreate); err != nil {
		return "", false, nil, err
	}
	modes := strings.Split(mode, ",")
	quote = "`"
	if slices.Contains(modes, "ANSI_QUOTES") {
		quote = `"`
	}
	noBackslashEscapes = slices.Contains(modes, "NO_BACKSLASH_ESCAPES")
	if quoteShowCreate == 1 {
		return quote, noBackslashEscapes, func() {}, nil
	}
	if _, err := conn.ExecContext(ctx, "SET SESSION sql_quote_show_create = 1"); err != nil {
		return "", false, nil, err
	}
	restore = func() {
		// Restore session settings even if the request context is already canceled.
		restoreCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := conn.ExecContext(restoreCtx, "SET SESSION sql_quote_show_create = ?", quoteShowCreate); err != nil {
			// A connection left with the wrong settings must not go back to the pool.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}
	return quote, noBackslashEscapes, restore, nil
}

// standardStringLiterals converts SHOW CREATE TABLE's single-quoted literals
// to preserve their bytes under NO_BACKSLASH_ESCAPES. It preserves backtick-quoted
// identifiers and, when ansiQuotes is true, double-quoted identifiers. Expressions
// in SHOW CREATE TABLE use backticks even under ANSI_QUOTES.
func standardStringLiterals(def string, ansiQuotes bool) string {
	var b strings.Builder
	for i := 0; i < len(def); i++ {
		c := def[i]
		if c == '"' && !ansiQuotes {
			b.WriteByte(c)
			continue
		}
		switch c {
		case '`', '"':
			// A doubled quote belongs to the identifier rather than ending it.
			j := i + 1
			for j < len(def) {
				if def[j] == c {
					if j+1 < len(def) && def[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			b.WriteString(def[i:min(j+1, len(def))])
			i = j
		case '\'':
			b.WriteByte(c)
			for i++; i < len(def) && def[i] != '\''; i++ {
				if def[i] != '\\' || i+1 == len(def) {
					b.WriteByte(def[i])
					continue
				}
				i++
				switch def[i] {
				case '0':
					b.WriteByte(0)
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 'Z':
					b.WriteByte(0x1a)
				case '\'':
					b.WriteString("''")
				default:
					b.WriteByte(def[i])
				}
			}
			if i < len(def) {
				b.WriteByte('\'')
			}
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// readMySQLColumns reads each column's escaped, reusable definition from
// SHOW CREATE TABLE on a session prepared by prepareMySQLSession. It uses
// information_schema to match column names, locate type boundaries, and
// identify generated columns.
func readMySQLColumns(ctx context.Context, conn *sql.Conn, quote, schema, table string) (mysqlColumns, error) {

	rows, err := conn.QueryContext(ctx, `
SELECT column_name, column_type, extra
FROM information_schema.columns
WHERE table_schema = ? AND table_name = ?
ORDER BY ordinal_position`, schema, table)
	if err != nil {
		return nil, err
	}
	type catalogColumn struct{ name, typ, extra string }
	var catalog []catalogColumn
	for rows.Next() {
		var c catalogColumn
		if err := rows.Scan(&c.name, &c.typ, &c.extra); err != nil {
			_ = rows.Close()
			return nil, err
		}
		catalog = append(catalog, c)
	}
	_ = rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var name, create string
	if err := conn.QueryRowContext(ctx, "SHOW CREATE TABLE "+qualified("mysql", schema, table)).Scan(&name, &create); err != nil { //nolint:gosec // schema and table are quoted identifiers.
		return nil, err
	}
	lines := strings.Split(create, "\n")

	out := mysqlColumns{}
	for _, c := range catalog {
		prefix := quote + strings.ReplaceAll(c.name, quote, quote+quote) + quote + " " + c.typ
		found := false
		for _, line := range lines {
			def := strings.TrimSuffix(strings.TrimSpace(line), ",")
			if def == prefix || strings.HasPrefix(def, prefix+" ") {
				out[strings.ToLower(c.name)] = &mysqlColumn{
					typ:       c.typ,
					rest:      def[len(prefix):],
					generated: strings.Contains(c.extra, "VIRTUAL GENERATED") || strings.Contains(c.extra, "STORED GENERATED"),
				}
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("cannot find the definition of column %q in SHOW CREATE TABLE", c.name)
		}
	}
	return out, nil
}

// currentMySQLColumns returns the column definitions buildAlterTable needs on
// MySQL, or nil and a no-op cleanup function for other drivers. On MySQL, it
// leaves conn prepared by prepareMySQLSession. After a successful call, the
// caller must run any resulting DDL on conn before calling the returned cleanup
// function. On error, cleanup has already been attempted if settings changed.
func currentMySQLColumns(ctx context.Context, conn *sql.Conn, driver, schema, table string) (mysqlColumns, func(), error) {
	if driver != "mysql" {
		return nil, func() {}, nil
	}
	quote, noBackslashEscapes, restore, err := prepareMySQLSession(ctx, conn)
	if err != nil {
		return nil, nil, fmt.Errorf("prepare the session to read %s.%s: %w", schema, table, err)
	}
	cols, err := readMySQLColumns(ctx, conn, quote, schema, table)
	if err != nil {
		restore()
		return nil, nil, fmt.Errorf("read the current definition of %s.%s: %w", schema, table, err)
	}
	if noBackslashEscapes {
		// ENUM and SET member lists in the type are escaped the same way as the attributes.
		for _, c := range cols {
			c.typ = standardStringLiterals(c.typ, quote == `"`)
			c.rest = standardStringLiterals(c.rest, quote == `"`)
		}
	}
	return cols, restore, nil
}

// lookup returns a column definition suitable for MODIFY COLUMN. It rejects
// missing columns, columns whose definition is unknown in this batch, and
// generated columns.
func (m mysqlColumns) lookup(name string) (*mysqlColumn, error) {
	c, ok := m[strings.ToLower(name)]
	switch {
	case !ok:
		return nil, fmt.Errorf("column %q does not exist", name)
	case c == nil:
		return nil, fmt.Errorf("column %q is added or changed earlier in this batch. Apply the batch before changing it again", name)
	case c.generated:
		return nil, fmt.Errorf("column %q is generated. Change it in the SQL editor", name)
	}
	return c, nil
}

var mysqlCharsetClause = regexp.MustCompile(`^( CHARACTER SET \S+)?( COLLATE \S+)?`)

// mysqlCharacterType matches the column types that take a character set. The
// national types are left out because they fix their own character set and
// reject a CHARACTER SET clause.
var mysqlCharacterType = regexp.MustCompile(`^(var)?char\b|^character\b|^(tiny|medium|long)?text\b|^enum\b|^set\b`)

// withType returns the definition with a new type. The character set and
// collation are preserved when the new type supports them and removed otherwise,
// because MySQL rejects them on a numeric or date type.
func (c *mysqlColumn) withType(typ string) string {
	rest := c.rest
	if !mysqlCharacterType.MatchString(strings.ToLower(strings.TrimSpace(typ))) {
		rest = rest[len(mysqlCharsetClause.FindString(rest)):]
	}
	return typ + rest
}

// withNullability returns the attributes after the type with NULL or NOT NULL.
// It reads nullability immediately after the character set and collation in
// SHOW CREATE TABLE output, preserving later clauses such as comments containing
// NOT NULL. Making a column NOT NULL also removes DEFAULT NULL, which may
// follow a spatial column's SRID, because that default requires a nullable
// column.
func (c *mysqlColumn) withNullability(nullable bool) string {
	charset := mysqlCharsetClause.FindString(c.rest)
	rest := c.rest[len(charset):]
	rest = trimKeyword(rest, " NOT NULL")
	rest = trimKeyword(rest, " NULL")
	if nullable {
		return charset + " NULL" + rest
	}
	srid := mysqlSRIDClause.FindString(rest)
	return charset + " NOT NULL" + srid + trimKeyword(rest[len(srid):], " DEFAULT NULL")
}

// mysqlSRIDClause matches the SRID that SHOW CREATE TABLE prints between the
// nullability and the default of a spatial column.
var mysqlSRIDClause = regexp.MustCompile(`^ /\*!\d+ SRID \d+ \*/`)

// trimKeyword removes the prefix kw only when followed by a space or the end of s.
func trimKeyword(s, kw string) string {
	if strings.HasPrefix(s, kw) && (len(s) == len(kw) || s[len(kw)] == ' ') {
		return s[len(kw):]
	}
	return s
}
