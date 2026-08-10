package envtemplate

import (
	"reflect"
	"strings"
	"testing"
)

func from(values map[string]string) Lookup {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

// The case the feature exists for: DocuSeal wants one connection string and
// Kipper injects five discrete variables, so the only route available was
// pasting the password into spec.env, where `kip export` and the console Env
// tab both show it.
func TestResolve_ComposesAConnectionString(t *testing.T) {
	got, missing := Resolve(
		"postgresql://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}:${DB_PORT}/${DB_NAME}",
		from(map[string]string{
			"DB_USERNAME": "kipper", "DB_PASSWORD": "s3cret",
			"DB_HOST": "db.shop-test.svc", "DB_PORT": "5432", "DB_NAME": "docuseal",
		}))
	want := "postgresql://kipper:s3cret@db.shop-test.svc:5432/docuseal"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if missing != nil {
		t.Errorf("nothing should be unresolved, got %v", missing)
	}
}

// A typo has to reach the process as written. Substituting empty would produce
// a connection to no host, and an error naming neither the variable nor the
// mistake.
func TestResolve_UnknownNameSurvivesVerbatim(t *testing.T) {
	got, missing := Resolve("postgresql://${DB_HSOT}/db", from(map[string]string{"DB_HOST": "db"}))
	if want := "postgresql://${DB_HSOT}/db"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if !reflect.DeepEqual(missing, []string{"DB_HSOT"}) {
		t.Errorf("the unresolved name must be reported, got %v", missing)
	}
}

// One pass. A cycle terminates because a substituted value is never itself
// resolved, and the same rule is why a transitive reference stays literal.
func TestResolve_SinglePass(t *testing.T) {
	cycle := from(map[string]string{"A": "${B}", "B": "${A}"})
	if got, _ := Resolve("${A}", cycle); got != "${B}" {
		t.Errorf("a cycle must terminate with a literal, got %q", got)
	}

	transitive := from(map[string]string{"HOST_TEMP": "${DB_HOST}", "DB_HOST": "db.internal"})
	if got, _ := Resolve("url://${HOST_TEMP}/db", transitive); got != "url://${DB_HOST}/db" {
		t.Errorf("a transitive reference must not resolve, got %q", got)
	}
}

// Without an escape there is no way to say "I meant this literally", and a
// value that is harmless today becomes a substitution the day a key of that
// name appears in scope.
func TestResolve_Escape(t *testing.T) {
	lookup := from(map[string]string{"LEVEL": "debug", "MSG": "hello"})
	for _, tc := range []struct{ in, want string }{
		{"$${LEVEL}", "${LEVEL}"},
		{"$${LEVEL} ${MSG}", "${LEVEL} hello"},
		{"${LEVEL} $${MSG}", "debug ${MSG}"},
		{"$${UNKNOWN}", "${UNKNOWN}"},
		{"$${NAME:urlencode}", "${NAME:urlencode}"},
	} {
		if got, missing := Resolve(tc.in, lookup); got != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.in, got, tc.want)
		} else if missing != nil {
			t.Errorf("Resolve(%q) reported %v; an escaped placeholder is not a reference", tc.in, missing)
		}
	}
}

// The property that makes this safe to run over every existing value: a string
// with no well-formed placeholder comes back byte for byte. Anything else would
// silently rewrite values nobody meant as templates.
func TestResolve_LeavesNonTemplatesAlone(t *testing.T) {
	lookup := from(map[string]string{"NAME": "x", "PATH": "y"})
	for _, s := range []string{
		"",
		"plain value",
		"$",
		"$$",
		"$$$",
		"$5.00 and $$10",
		"${}",
		"${1BAD}",
		"${NO_CLOSE",
		"${NAME:unknown}",
		"${NAME:}",
		"${WITH-DASH}",
		"${WITH SPACE}",
		"$NAME",
		"${{NAME}}",
		"jdbc:postgresql://host/db?ssl=true",
		"awk '{print $1}'",
		"regex ^\\$\\{.*\\}$",
		"price: 100$",
	} {
		got, missing := Resolve(s, lookup)
		if got != s {
			t.Errorf("Resolve(%q) = %q; a value with no placeholder must be unchanged", s, got)
		}
		if missing != nil {
			t.Errorf("Resolve(%q) reported %v; nothing here is a reference", s, missing)
		}
	}
}

// A password reaches a URL through this modifier or it corrupts the URL.
// Operators type these through `kip app secret set`, so @ and : are ordinary.
func TestResolve_URLEncode(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"p@ss", "p%40ss"},
		{"a:b", "a%3Ab"},
		{"a/b", "a%2Fb"},
		{"100%", "100%25"},
		{"with space", "with%20space"},
		{"tab\there", "tab%09here"},
		{"grüß", "gr%C3%BC%C3%9F"},
		{"safe-._~AZaz09", "safe-._~AZaz09"},
		{"", ""},
	} {
		got, _ := Resolve("${P:urlencode}", from(map[string]string{"P": tc.raw}))
		if got != tc.want {
			t.Errorf("urlencode(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}

	// Space must not become '+': a password field would read that back as a
	// literal plus and authentication fails with nothing to see.
	if got, _ := Resolve("${P:urlencode}", from(map[string]string{"P": "a b"})); got == "a+b" {
		t.Error("space must percent-encode, not become '+'")
	}
}

// The modifier is opt-in. Encoding every value would corrupt the parts of a URL
// that are supposed to contain delimiters.
func TestResolve_NoEncodingWithoutTheModifier(t *testing.T) {
	got, _ := Resolve("${HOST}/${PATH}", from(map[string]string{"HOST": "db.internal:5432", "PATH": "a/b"}))
	if want := "db.internal:5432/a/b"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A composed URL where every credential component is encoded is the shape wave
// 4's starter snippets ship, so it has to survive a hostile password intact.
func TestResolve_EncodedCredentialsInAWholeURL(t *testing.T) {
	got, missing := Resolve(
		"postgresql://${DB_USERNAME:urlencode}:${DB_PASSWORD:urlencode}@${DB_HOST}:${DB_PORT}/${DB_NAME}",
		from(map[string]string{
			"DB_USERNAME": "kipper", "DB_PASSWORD": "p@ss:w/rd 1%",
			"DB_HOST": "db.internal", "DB_PORT": "5432", "DB_NAME": "app",
		}))
	want := "postgresql://kipper:p%40ss%3Aw%2Frd%201%25@db.internal:5432/app"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if missing != nil {
		t.Errorf("unexpected unresolved names %v", missing)
	}
}

func TestResolve_ReportsEveryUnresolvedNameOnceSorted(t *testing.T) {
	_, missing := Resolve("${Z} ${A} ${Z} ${M}", from(map[string]string{}))
	if want := []string{"A", "M", "Z"}; !reflect.DeepEqual(missing, want) {
		t.Errorf("got %v, want %v", missing, want)
	}
}

// An empty value is a value. Treating "set but empty" as missing would leave a
// placeholder in the pod for a variable that genuinely resolves to nothing.
func TestResolve_EmptyValueResolves(t *testing.T) {
	got, missing := Resolve("[${EMPTY}]", from(map[string]string{"EMPTY": ""}))
	if got != "[]" {
		t.Errorf("got %q, want %q", got, "[]")
	}
	if missing != nil {
		t.Errorf("an empty value is resolved, not missing; got %v", missing)
	}
}

func TestResolveAll_UnionsUnresolvedNames(t *testing.T) {
	out, missing := ResolveAll(map[string]string{
		"A": "${KNOWN}",
		"B": "${GONE}",
		"C": "${GONE} ${ALSO_GONE}",
	}, from(map[string]string{"KNOWN": "yes"}))

	if out["A"] != "yes" || out["B"] != "${GONE}" {
		t.Errorf("unexpected resolution: %v", out)
	}
	if want := []string{"ALSO_GONE", "GONE"}; !reflect.DeepEqual(missing, want) {
		t.Errorf("got %v, want %v", missing, want)
	}
}

func TestNames_ListsReferencesInOrderWithoutEscaped(t *testing.T) {
	got := Names("${B}://${A}:${B}@$${SKIPPED}/${C:urlencode}")
	if want := []string{"B", "A", "C"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if n := Names("no placeholders here"); n != nil {
		t.Errorf("got %v, want nil", n)
	}
}

// A run of dollars resolves left to right, and only a $$ sitting immediately
// before a placeholder escapes it. In "$$${NAME}" a third $ separates the pair
// from the brace, so the pair is two literal dollars and the placeholder that
// follows resolves normally. Pinned because it is the corner where the
// scanner's lookahead could drift without any other test noticing.
func TestResolve_OnlyADollarPairTouchingAPlaceholderEscapes(t *testing.T) {
	lookup := from(map[string]string{"NAME": "x"})
	for _, tc := range []struct{ in, want string }{
		{"${NAME}", "x"},
		{"$${NAME}", "${NAME}"},
		{"$$${NAME}", "$$x"},
		{"$$$${NAME}", "$$${NAME}"},
	} {
		if got, _ := Resolve(tc.in, lookup); got != tc.want {
			t.Errorf("Resolve(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// An unresolved name carrying a modifier still passes through exactly as
// written, modifier included — encoding nothing is right, but so is leaving the
// text intact so the typo is visible.
func TestResolve_UnknownNameKeepsItsModifier(t *testing.T) {
	got, missing := Resolve("u://${GONE:urlencode}@h", from(map[string]string{}))
	if want := "u://${GONE:urlencode}@h"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if len(missing) != 1 || missing[0] != "GONE" {
		t.Errorf("got %v, want [GONE]", missing)
	}
}

// The credential warnings ask what a value still carries once the templating is
// taken out. Answering that with a regex of its own is what this replaces: the
// grammar had three spellings, and a spelling that drifts is silent in both
// directions.
func TestStripPlaceholders(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{
			name:  "a templated URL keeps no credential",
			value: "postgresql://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}:${DB_PORT}/${DB_NAME}",
			want:  "postgresql://:@:/",
		},
		{
			name:  "a literal password survives",
			value: "postgresql://kipper:s3cret@host:5432/app",
			want:  "postgresql://kipper:s3cret@host:5432/app",
		},
		{
			// The regex this replaces matched any ${...} with a valid name, so
			// it stripped the escape's placeholder too and reported a value
			// holding a literal ${PASSWORD} as though it held nothing.
			name:  "an escaped placeholder is literal text and stays",
			value: "postgresql://u:$${PASSWORD}@host/db",
			want:  "postgresql://u:${PASSWORD}@host/db",
		},
		{
			// A URL's own delimiters are not placeholders, and erasing them
			// would hide the password sitting between them.
			name:  "text that is not a reference is left alone",
			value: "postgresql://user${:pass}@host/db",
			want:  "postgresql://user${:pass}@host/db",
		},
		{
			name:  "the urlencode modifier is part of the reference",
			value: "amqp://user:${RABBIT_PASSWORD:urlencode}@host/vhost",
			want:  "amqp://user:@host/vhost",
		},
		{
			name:  "an unknown modifier is not a reference",
			value: "postgresql://u:${PASSWORD:base64}@host/db",
			want:  "postgresql://u:${PASSWORD:base64}@host/db",
		},
		{"a lone dollar", "cost is $5", "cost is $5"},
		{"nothing to strip", "plain-value", "plain-value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := StripPlaceholders(tc.value); got != tc.want {
				t.Fatalf("StripPlaceholders(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// Whatever Resolve treats as a reference, StripPlaceholders must remove, and
// whatever it leaves literal must stay. Pinning them against each other is the
// point of having one definition: a change to the grammar that reaches only one
// of them fails here rather than in a warning nobody double-checks.
func TestStripPlaceholdersAgreesWithResolve(t *testing.T) {
	lookup := func(string) (string, bool) { return "", false }
	for _, value := range []string{
		"postgresql://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}/${DB_NAME}",
		"postgresql://user${:pass}@host/db",
		"$${ESCAPED}",
		"${NAME:urlencode}",
		"${NAME:base64}",
		"${}",
		"${1INVALID}",
		"plain",
	} {
		stripped := StripPlaceholders(value)
		// An unresolvable reference is left verbatim by Resolve, so anything
		// Resolve preserved and StripPlaceholders removed was a reference, and
		// the two agreeing on the remainder is what is being pinned.
		resolved, _ := Resolve(value, lookup)
		if len(Names(value)) == 0 && stripped != resolved {
			t.Fatalf("value %q references nothing, so stripping (%q) must not change it (%q)",
				value, stripped, resolved)
		}
		if len(stripped) > len(value) {
			t.Fatalf("stripping %q grew it to %q", value, stripped)
		}
	}
}

func TestShellStyleRefs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  []string
	}{
		{"kubernetes form is reported", "$(DB_HOST)", []string{"DB_HOST"}},
		{"inside a URL", "postgres://u:p@$(DB_HOST):5432/app", []string{"DB_HOST"}},
		{"several, deduplicated, in order", "$(B)-$(A)-$(B)", []string{"B", "A"}},
		{"kipper's own form is not shell style", "${DB_HOST}", nil},
		{"a command substitution is not a name", "$(date +%s)", nil},
		{"a digit-leading name is not a name", "$(1BAD)", nil},
		{"an empty reference is not a name", "$()", nil},
		{"an unclosed reference is not a name", "$(DB_HOST", nil},
		{"plain text has none", "hello world", nil},
		{"a lone dollar has none", "cost: $5", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ShellStyleRefs(tc.value); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ShellStyleRefs(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

// TestShellStyleRefs_LeavesTheValueAlone is the property that matters more than
// the reporting: Kipper never rewrites $(NAME), so a value carrying one comes
// back from the resolver byte for byte.
func TestShellStyleRefs_LeavesTheValueAlone(t *testing.T) {
	for _, value := range []string{
		"$(DB_HOST)",
		"redis://:$(REDIS_PASSWORD)@cache:6379",
		"CMD=$(date +%s)",
		"$$(ESCAPED)",
	} {
		got, unresolved := Resolve(value, func(string) (string, bool) {
			t.Fatalf("resolver must not look up a shell-style name in %q", value)
			return "", false
		})
		if got != value {
			t.Errorf("Resolve(%q) = %q, want it unchanged", value, got)
		}
		if len(unresolved) != 0 {
			t.Errorf("Resolve(%q) reported unresolved %v, want none", value, unresolved)
		}
	}
}

func masked(values map[string]Value) MaskedLookup {
	return func(name string) (Value, bool) {
		v, ok := values[name]
		return v, ok
	}
}

// TestResolveMasked_NeverEmitsASecret is the D13 constraint. Each case is one
// way a preview built by searching the resolved text for the credential would
// hand it over anyway.
func TestResolveMasked_NeverEmitsASecret(t *testing.T) {
	const secret = "hunter2:p@ss/word"
	table := map[string]Value{
		"DB_PASSWORD": {Text: secret, Secret: true},
		"DB_USERNAME": {Text: "kipper", Secret: false},
		"DB_HOST":     {Text: "db.blog-test.svc.cluster.local", Secret: false},
	}

	for _, tc := range []struct {
		name  string
		value string
	}{
		{"on its own", "${DB_PASSWORD}"},
		{"inside a URL", "postgres://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}/app"},
		{"url-encoded, where the encoding would change its shape", "${DB_PASSWORD:urlencode}"},
		{"twice in one value", "${DB_PASSWORD}-${DB_PASSWORD}"},
		{"once raw and once encoded", "${DB_PASSWORD}|${DB_PASSWORD:urlencode}"},
		{"embedded, where the value no longer resembles the credential", "BASE64=${DB_PASSWORD}"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := ResolveMasked(tc.value, masked(table))
			if strings.Contains(got, secret) {
				t.Errorf("ResolveMasked(%q) = %q, which contains the credential", tc.value, got)
			}
			if strings.Contains(got, urlEncodeComponent(secret)) {
				t.Errorf("ResolveMasked(%q) = %q, which contains the encoded credential", tc.value, got)
			}
			if !strings.Contains(got, Mask) {
				t.Errorf("ResolveMasked(%q) = %q, which has no mask in it", tc.value, got)
			}
		})
	}
}

// TestResolveMasked_MaskWidthSaysNothingAboutTheValue pins the second half of
// D13: the length of a credential is a fact about the credential.
func TestResolveMasked_MaskWidthSaysNothingAboutTheValue(t *testing.T) {
	var widths []int
	for _, secret := range []string{"", "x", "hunter2", strings.Repeat("a", 200)} {
		got, _ := ResolveMasked("${P}", masked(map[string]Value{
			"P": {Text: secret, Secret: true},
		}))
		widths = append(widths, len(got))
	}
	for _, w := range widths {
		if w != widths[0] {
			t.Errorf("mask widths %v differ, so the preview leaks the credential's length", widths)
			break
		}
	}
}

// TestResolveMasked_NonSecretsResolveNormally keeps the masking from swallowing
// the values the preview exists to show.
func TestResolveMasked_NonSecretsResolveNormally(t *testing.T) {
	got, unresolved := ResolveMasked(
		"postgres://${DB_USERNAME}:${DB_PASSWORD}@${DB_HOST}/${DB_HSOT}",
		masked(map[string]Value{
			"DB_USERNAME": {Text: "kipper"},
			"DB_PASSWORD": {Text: "hunter2", Secret: true},
			"DB_HOST":     {Text: "db.svc"},
		}))

	want := "postgres://kipper:" + Mask + "@db.svc/${DB_HSOT}"
	if got != want {
		t.Errorf("ResolveMasked = %q, want %q", got, want)
	}
	if len(unresolved) != 1 || unresolved[0] != "DB_HSOT" {
		t.Errorf("unresolved = %v, want [DB_HSOT]", unresolved)
	}
}
