// Package envtemplate resolves ${NAME} references in environment values.
// Rendering and credential warnings share this grammar so they agree on which
// parts are references and which are literal text.
package envtemplate

import (
	"sort"
	"strings"
)

// Lookup answers what a name resolves to, reporting false for a name with no
// value. Values come back raw and are never themselves resolved.
type Lookup func(name string) (string, bool)

// Resolve substitutes ${NAME} once, left to right, preserving unknown and
// malformed placeholders. Substituted values stay literal rather than expanding
// recursively. $${NAME} escapes a placeholder; $$${NAME} retains the dollar
// pair and resolves the following placeholder.
// It also returns sorted, deduplicated names with no lookup value.
func Resolve(value string, lookup Lookup) (string, []string) {
	return resolve(value, func(name string) (Value, bool) {
		text, ok := lookup(name)
		return Value{Text: text}, ok
	})
}

// Value is what a name resolves to, and whether the reader may be shown it.
type Value struct {
	Text string
	// Secret says the value came from a source the reading role may not see:
	// the workload's own Secrets, or a service binding's credentials. A
	// preview substitutes Mask for it.
	Secret bool
}

// MaskedLookup answers what a name resolves to with that provenance attached.
type MaskedLookup func(name string) (Value, bool)

// Mask stands in for a secret-derived value in a preview.
//
// Fixed width, and deliberately not one bullet per character: the length of a
// credential is a fact about the credential, and a preview that leaks it tells
// a reader how much of one to guess at.
const Mask = "••••••••"

// ResolveMasked resolves value the way Resolve does, replacing every
// secret-derived substitution with Mask.
//
// Masking happens at the lookup, from the provenance the caller attaches,
// rather than by searching the resolved text for something that looks like a
// credential. Searching cannot work: BASE64_KEY=${SECRET_KEY} produces a value
// that no longer resembles what it was built from, so there is nothing to find.
//
// A masked value skips the modifier. Percent-encoding the mask would turn it
// into %E2%80%A2… — a string whose shape says "a value was here and this is how
// long it was", which is the leak the fixed width exists to prevent.
func ResolveMasked(value string, lookup MaskedLookup) (string, []string) {
	return resolve(value, lookup)
}

// resolve is the substitution both entry points share.
func resolve(value string, lookup MaskedLookup) (string, []string) {
	var out strings.Builder
	out.Grow(len(value))
	unresolved := map[string]bool{}

	walk(value,
		func(text string) { out.WriteString(text) },
		func(ref reference, raw string) {
			v, found := lookup(ref.name)
			if !found {
				unresolved[ref.name] = true
				out.WriteString(raw)
				return
			}
			if v.Secret {
				out.WriteString(Mask)
				return
			}
			if ref.urlencode {
				out.WriteString(urlEncodeComponent(v.Text))
				return
			}
			out.WriteString(v.Text)
		})

	if len(unresolved) == 0 {
		return out.String(), nil
	}
	names := make([]string, 0, len(unresolved))
	for name := range unresolved {
		names = append(names, name)
	}
	sort.Strings(names)
	return out.String(), names
}

// walk drives the grammar over value exactly once and is the only place that
// decides what a placeholder is.
//
// literal receives every stretch of text to be emitted as written, including
// the text an escape made literal. ref receives each live reference along with
// the raw source it was parsed from, which is what a caller emits when the name
// does not resolve.
//
// Four functions used to carry their own copy of this loop. They agreed, which
// is the only reason nothing had gone wrong yet; the wave 0 defect was two
// copies of the grammar drifting, and copies inside one file drift the same way.
func walk(value string, literal func(string), ref func(reference, string)) {
	for i := 0; i < len(value); {
		if value[i] != '$' {
			literal(value[i : i+1])
			i++
			continue
		}

		// $${NAME} is the escape, and only directly before a well-formed
		// placeholder. Leaving every other $$ alone keeps this a no-op on
		// values that were never templates.
		if i+1 < len(value) && value[i+1] == '$' {
			if r, ok := parsePlaceholder(value[i+1:]); ok {
				literal(value[i+1 : i+1+r.width])
				i += 1 + r.width
				continue
			}
			literal("$$")
			i += 2
			continue
		}

		r, ok := parsePlaceholder(value[i:])
		if !ok {
			literal("$")
			i++
			continue
		}
		ref(r, value[i:i+r.width])
		i += r.width
	}
}

// ResolveAll resolves every value in a map, returning the resolved map and the
// union of unresolved names across it.
func ResolveAll(values map[string]string, lookup Lookup) (map[string]string, []string) {
	out := make(map[string]string, len(values))
	seen := map[string]bool{}
	for k, v := range values {
		resolved, missing := Resolve(v, lookup)
		out[k] = resolved
		for _, name := range missing {
			seen[name] = true
		}
	}
	if len(seen) == 0 {
		return out, nil
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return out, names
}

// StripPlaceholders returns the literal text around references, using the
// same grammar as rendering. Credential warnings inspect that text for embedded
// passwords while accepting credentials supplied through references.
func StripPlaceholders(value string) string {
	var out strings.Builder
	walk(value,
		func(text string) { out.WriteString(text) },
		func(reference, string) {})
	return out.String()
}

// Names returns the names a value references, whether or not they resolve, in
// order of appearance and deduplicated. Escaped placeholders are not
// references. Used to show which variables a template depends on.
func Names(value string) []string {
	var names []string
	seen := map[string]bool{}
	walk(value,
		func(string) {},
		func(r reference, _ string) {
			if !seen[r.name] {
				seen[r.name] = true
				names = append(names, r.name)
			}
		})
	return names
}

// ShellStyleRefs returns deduplicated $(NAME) references in appearance order
// for diagnostics. Kipper resolves ${NAME}; its rendered envFrom values carry
// $(NAME) literally to the process.
func ShellStyleRefs(value string) []string {
	var names []string
	seen := map[string]bool{}
	for i := 0; i+3 < len(value); {
		if value[i] != '$' || value[i+1] != '(' {
			i++
			continue
		}
		close := strings.IndexByte(value[i:], ')')
		if close < 0 {
			break
		}
		name := value[i+2 : i+close]
		if !validName(name) {
			i++
			continue
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
		i += close + 1
	}
	return names
}

// reference is one parsed ${NAME} or ${NAME:urlencode}.
type reference struct {
	name      string
	urlencode bool
	width     int // bytes consumed, including the leading $
}

// parsePlaceholder reads a placeholder at the start of s, which must begin with
// '$'. A name is [A-Za-z_][A-Za-z0-9_]*, matching what a shell and Kubernetes
// both accept, and the only modifier is :urlencode. Anything else is not a
// placeholder and the caller emits it verbatim — an unknown modifier is left
// literal rather than guessed at, so a typo shows up instead of silently
// dropping the encoding a credential needed.
func parsePlaceholder(s string) (reference, bool) {
	if len(s) < 4 || s[0] != '$' || s[1] != '{' {
		return reference{}, false
	}
	close := strings.IndexByte(s, '}')
	if close < 0 {
		return reference{}, false
	}
	body := s[2:close]

	// A colon introduces a modifier, so "${NAME:}" is a modifier that is not one
	// rather than a plain reference. Left literal like any other malformed
	// placeholder, so the mistake is visible instead of quietly dropping an
	// encoding a credential needed.
	name, modifier, hasModifier := body, "", false
	if colon := strings.IndexByte(body, ':'); colon >= 0 {
		name, modifier, hasModifier = body[:colon], body[colon+1:], true
	}
	if !validName(name) {
		return reference{}, false
	}
	if !hasModifier {
		return reference{name: name, width: close + 1}, true
	}
	if modifier == "urlencode" {
		return reference{name: name, urlencode: true, width: close + 1}, true
	}
	return reference{}, false
}

func validName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}

// urlEncodeComponent percent-encodes everything outside RFC 3986's unreserved
// set, so the result is safe in any single URL component.
//
// net/url is no use here. QueryEscape writes a space as '+', which a password
// field reads back as a literal plus, and PathEscape leaves ':' and '@' alone,
// which is exactly what breaks
// postgresql://user:p@ss@host/db by ending the userinfo early. Passwords
// routinely contain both: they are typed by operators through
// `kip app secret set`, and the migration path force-aligns a target role's
// password to whatever arrived, so nothing guarantees they are hex.
func urlEncodeComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var out strings.Builder
	out.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			c == '-' || c == '.' || c == '_' || c == '~' {
			out.WriteByte(c)
			continue
		}
		out.WriteByte('%')
		out.WriteByte(hex[c>>4])
		out.WriteByte(hex[c&0x0f])
	}
	return out.String()
}
