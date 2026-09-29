package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/go-sql-driver/mysql"
)

// markDoneTemplate binds row values as query parameters in KIPPER_MARK_DONE.
type markDoneTemplate struct {
	driver string
	pieces []markDonePiece
}

// markDonePiece is one of: raw SQL text, a placeholder standing as a value,
// or a single-quoted literal with placeholders inside it.
type markDonePiece struct {
	text    string
	column  string
	quoted  bool
	literal []literalPart
}

// literalPart is raw literal content, escapes intact, or a placeholder.
type literalPart struct {
	text   string
	column string
}

// parseMarkDone prepares placeholders in value positions and ordinary single-quoted
// strings for binding. It rejects unsupported placeholder contexts, native parameter
// markers, and text after a statement terminator other than whitespace or comments.
// PostgreSQL ordinary strings are assumed to use standard_conforming_strings=on.
func parseMarkDone(driver, template string) (*markDoneTemplate, error) {
	mysqlDialect := driver == "mysql"
	t := &markDoneTemplate{driver: driver}
	var text strings.Builder
	flush := func() {
		if text.Len() > 0 {
			t.pieces = append(t.pieces, markDonePiece{text: text.String()})
			text.Reset()
		}
	}
	ended := false
	lastLiteralEnd, lastLiteralBinds := -1, false

	for i := 0; i < len(template); {
		c := template[i]
		if ended && !isSpace(c) && !startsComment(template[i:], mysqlDialect) {
			return nil, errors.New("a mark-done template must be one statement")
		}
		switch {
		case strings.HasPrefix(template[i:], "{{"):
			column, next, ok := placeholderAt(template, i)
			if !ok {
				text.WriteByte(c)
				i++
				continue
			}
			flush()
			t.pieces = append(t.pieces, markDonePiece{column: column})
			i = next

		case c == '\'':
			backslashEscapes := mysqlDialect || stringPrefix(template, i) == "e"
			end, parts, err := scanLiteral(template, i, backslashEscapes)
			if err != nil {
				return nil, err
			}
			raw := template[i:end]
			binds := hasColumn(parts)
			if (binds || lastLiteralBinds) && continuesLiteral(template, lastLiteralEnd, i, mysqlDialect) {
				return nil, errors.New("a placeholder inside adjacent string literals cannot be bound; join them into one literal")
			}
			lastLiteralEnd, lastLiteralBinds = end, binds
			switch {
			case !binds:
				text.WriteString(raw)
			case hasStringPrefix(template, i, mysqlDialect):
				return nil, fmt.Errorf("a placeholder inside the prefixed string %s cannot be bound", raw)
			case len(parts) == 1:
				flush()
				t.pieces = append(t.pieces, markDonePiece{column: parts[0].column, quoted: true})
			default:
				flush()
				t.pieces = append(t.pieces, markDonePiece{literal: parts})
			}
			i = end

		case c == '"' || c == '`':
			end, err := scanQuoted(template, i, c, mysqlDialect)
			if err != nil {
				return nil, err
			}
			raw := template[i:end]
			if strings.Contains(raw, "{{") {
				if c == '"' && mysqlDialect {
					return nil, fmt.Errorf("a placeholder inside the double-quoted string %s cannot be bound; use single quotes", raw)
				}
				return nil, fmt.Errorf("a placeholder inside the quoted identifier %s cannot be bound", raw)
			}
			text.WriteString(raw)
			i = end

		case strings.HasPrefix(template[i:], "--") || (mysqlDialect && c == '#'):
			end := strings.IndexByte(template[i:], '\n')
			if end < 0 {
				end = len(template)
			} else {
				end += i
			}
			if err := commentHasNoPlaceholder(template[i:end]); err != nil {
				return nil, err
			}
			text.WriteString(template[i:end])
			i = end

		case strings.HasPrefix(template[i:], "/*"):
			end := blockCommentEnd(template, i, !mysqlDialect)
			if end < 0 {
				return nil, errors.New("unterminated comment in the mark-done template")
			}
			if err := commentHasNoPlaceholder(template[i:end]); err != nil {
				return nil, err
			}
			text.WriteString(template[i:end])
			i = end

		case c == '$' && !mysqlDialect:
			if i+1 < len(template) && isDigit(template[i+1]) && (i == 0 || !isIdentByte(template[i-1])) {
				return nil, errors.New("write parameters as {{column}}; native $N markers are not supported")
			}
			if tag, ok := dollarTagAt(template, i); ok {
				end := strings.Index(template[i+len(tag):], tag)
				if end < 0 {
					return nil, errors.New("unterminated dollar-quoted string in the mark-done template")
				}
				end += i + 2*len(tag)
				raw := template[i:end]
				if strings.Contains(raw, "{{") {
					return nil, fmt.Errorf("a placeholder inside the dollar-quoted string %s cannot be bound", raw)
				}
				text.WriteString(raw)
				i = end
				continue
			}
			text.WriteByte(c)
			i++

		case c == '?' && mysqlDialect:
			return nil, errors.New("write parameters as {{column}}; native ? markers are not supported")

		case c == ';':
			text.WriteByte(c)
			ended = true
			i++

		default:
			text.WriteByte(c)
			i++
		}
	}
	flush()
	return t, nil
}

// bind renders the template for one row, preserving placeholders for missing columns.
func (t *markDoneTemplate) bind(event map[string]interface{}) (string, []interface{}) {
	var query strings.Builder
	var args []interface{}
	// Preserve token boundaries when replacing quotes with parameters or CONCAT.
	separate := func() {
		if q := query.String(); q != "" && (isIdentByte(q[len(q)-1]) || q[len(q)-1] == '$') {
			query.WriteByte(' ')
		}
	}
	param := func(value interface{}) string {
		args = append(args, value)
		if t.driver == "mysql" {
			return "?"
		}
		return fmt.Sprintf("$%d", len(args))
	}

	afterMarker := false
	for _, p := range t.pieces {
		if afterMarker && p.text != "" && isIdentByte(p.text[0]) {
			query.WriteByte(' ')
		}
		afterMarker = false
		switch {
		case p.literal != nil:
			separate()
			query.WriteString(t.concat(p.literal, event, param))
		case p.column != "":
			switch value, ok := event[p.column]; {
			case ok:
				separate()
				query.WriteString(param(value))
				afterMarker = true
			case p.quoted:
				query.WriteString("'{{" + p.column + "}}'")
			default:
				query.WriteString("{{" + p.column + "}}")
			}
		default:
			query.WriteString(p.text)
		}
	}
	return query.String(), args
}

// concat renders a literal with placeholders as a concatenation of its fixed
// parts and bound values: ('a' || $1) on postgres, CONCAT('a', ?) on mysql.
func (t *markDoneTemplate) concat(parts []literalPart, event map[string]interface{}, param func(interface{}) string) string {
	var terms []string
	var pending strings.Builder
	flush := func() {
		if pending.Len() > 0 {
			terms = append(terms, "'"+pending.String()+"'")
			pending.Reset()
		}
	}
	for _, part := range parts {
		if part.column == "" {
			pending.WriteString(part.text)
			continue
		}
		value, ok := event[part.column]
		if !ok {
			pending.WriteString("{{" + part.column + "}}")
			continue
		}
		flush()
		terms = append(terms, param(value))
	}
	flush()

	switch {
	case len(terms) == 1:
		return terms[0]
	case t.driver == "mysql":
		return "CONCAT(" + strings.Join(terms, ", ") + ")"
	default:
		return "(" + strings.Join(terms, " || ") + ")"
	}
}

// scanLiteral reads the single-quoted literal starting at start and returns
// the index just past it, with its content split into text and placeholders.
func scanLiteral(s string, start int, backslashEscapes bool) (int, []literalPart, error) {
	var parts []literalPart
	var text strings.Builder
	for i := start + 1; i < len(s); {
		switch {
		case s[i] == '\'' && i+1 < len(s) && s[i+1] == '\'':
			text.WriteString("''")
			i += 2
		case s[i] == '\'':
			if text.Len() > 0 {
				parts = append(parts, literalPart{text: text.String()})
			}
			return i + 1, parts, nil
		case backslashEscapes && s[i] == '\\' && i+1 < len(s):
			text.WriteString(s[i : i+2])
			i += 2
		case strings.HasPrefix(s[i:], "{{"):
			column, next, ok := placeholderAt(s, i)
			if !ok {
				text.WriteByte(s[i])
				i++
				continue
			}
			if text.Len() > 0 {
				parts = append(parts, literalPart{text: text.String()})
				text.Reset()
			}
			parts = append(parts, literalPart{column: column})
			i = next
		default:
			text.WriteByte(s[i])
			i++
		}
	}
	return 0, nil, errors.New("unterminated string literal in the mark-done template")
}

// scanQuoted returns the index just past the quoted identifier or string
// opened by quote at start.
func scanQuoted(s string, start int, quote byte, mysqlDialect bool) (int, error) {
	for i := start + 1; i < len(s); i++ {
		switch {
		case s[i] == quote && i+1 < len(s) && s[i+1] == quote:
			i++
		case s[i] == quote:
			return i + 1, nil
		case mysqlDialect && quote == '"' && s[i] == '\\':
			i++
		}
	}
	return 0, fmt.Errorf("unterminated %c-quoted text in the mark-done template", quote)
}

// placeholderAt expects i to point to the opening {{.
func placeholderAt(s string, i int) (column string, next int, ok bool) {
	end := strings.Index(s[i+2:], "}}")
	if end < 0 {
		return "", 0, false
	}
	column = s[i+2 : i+2+end]
	if column == "" || strings.ContainsAny(column, "{}'\"`") {
		return "", 0, false
	}
	return column, i + 2 + end + 2, true
}

// dollarTagAt reports the $tag$ opening a postgres dollar-quoted string at i.
func dollarTagAt(s string, i int) (string, bool) {
	if i > 0 && isIdentByte(s[i-1]) {
		return "", false
	}
	for j := i + 1; j < len(s); j++ {
		if s[j] == '$' {
			return s[i : j+1], true
		}
		if !isIdentByte(s[j]) || isDigit(s[i+1]) {
			return "", false
		}
	}
	return "", false
}

// hasStringPrefix recognizes E, B, X, N and U& on PostgreSQL, and N, B, X
// and _charset introducers on MySQL, whose literal semantics require special handling.
func hasStringPrefix(s string, i int, mysqlDialect bool) bool {
	if !mysqlDialect && i >= 2 && s[i-1] == '&' && (s[i-2]|0x20) == 'u' && (i == 2 || !isIdentByte(s[i-3])) {
		return true
	}
	word := stringPrefix(s, i)
	switch {
	case word == "":
		return false
	case mysqlDialect && strings.HasPrefix(word, "_"):
		return true
	case word == "b" || word == "x" || word == "n":
		return true
	default:
		return word == "e" && !mysqlDialect
	}
}

// stringPrefix returns the identifier characters directly before the quote at
// i, lower-cased.
func stringPrefix(s string, i int) string {
	start := i
	for start > 0 && isIdentByte(s[start-1]) {
		start--
	}
	return strings.ToLower(s[start:i])
}

// blockCommentEnd returns the index after the closing */, or -1 if unterminated.
// nested enables PostgreSQL-style nested block comments.
func blockCommentEnd(s string, i int, nested bool) int {
	depth := 0
	for j := i; j+1 < len(s); j++ {
		switch {
		case s[j] == '/' && s[j+1] == '*' && (nested || depth == 0):
			depth++
			j++
		case s[j] == '*' && s[j+1] == '/':
			depth--
			j++
			if depth == 0 {
				return j + 1
			}
		}
	}
	return -1
}

// continuesLiteral recognizes separators between adjacent literals: whitespace
// and line comments with at least one line break for PostgreSQL; whitespace and
// comments for MySQL, excluding executable /*! comments.
func continuesLiteral(s string, prevEnd, start int, mysqlDialect bool) bool {
	if prevEnd < 0 {
		return false
	}
	gap := s[prevEnd:start]
	lineEnds := "\n\r"
	if mysqlDialect {
		lineEnds = "\n"
	}
	for i := 0; i < len(gap); {
		switch {
		case strings.IndexByte(" \t\n\r\f\v", gap[i]) >= 0:
			i++
		case startsLineComment(gap[i:], mysqlDialect):
			end := strings.IndexAny(gap[i:], lineEnds)
			if end < 0 {
				return false
			}
			i += end
		case mysqlDialect && strings.HasPrefix(gap[i:], "/*") && !strings.HasPrefix(gap[i:], "/*!"):
			end := blockCommentEnd(gap, i, false)
			if end < 0 {
				return false
			}
			i = end
		default:
			return false
		}
	}
	return mysqlDialect || strings.ContainsAny(gap, "\n\r")
}

// startsLineComment expects nonempty s. For MySQL, -- requires following
// whitespace, a control character, or the end of s.
func startsLineComment(s string, mysqlDialect bool) bool {
	if !mysqlDialect {
		return strings.HasPrefix(s, "--")
	}
	return s[0] == '#' || (strings.HasPrefix(s, "--") && (len(s) == 2 || s[2] <= ' ' || s[2] == 0x7f))
}

func startsComment(s string, mysqlDialect bool) bool {
	return strings.HasPrefix(s, "--") || strings.HasPrefix(s, "/*") || (mysqlDialect && strings.HasPrefix(s, "#"))
}

func commentHasNoPlaceholder(comment string) error {
	if strings.Contains(comment, "{{") {
		return fmt.Errorf("a placeholder inside the comment %q cannot be bound", comment)
	}
	return nil
}

func hasColumn(parts []literalPart) bool {
	for _, p := range parts {
		if p.column != "" {
			return true
		}
	}
	return false
}

func isSpace(c byte) bool     { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
func isDigit(c byte) bool     { return c >= '0' && c <= '9' }
func isIdentByte(c byte) bool { return c == '_' || isDigit(c) || (c|0x20 >= 'a' && c|0x20 <= 'z') }

// mysqlDSN disables client-side interpolation so bound values travel separately
// from SQL, even when the source URL requests interpolation.
func mysqlDSN(sourceURL string) (string, error) {
	cfg, err := mysql.ParseDSN(convertMySQLDSN(sourceURL))
	if err != nil {
		return "", err
	}
	cfg.InterpolateParams = false
	return cfg.FormatDSN(), nil
}
