package dexcfg

import (
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// A fixture matching Kipper's real dex-config, plus a connector and an unknown
// future field, to prove the editor touches only what it must.
const fixture = `issuer: https://dex-old.kipper.run/dex
enablePasswordDB: true
oauth2:
  skipApprovalScreen: true
frontend:
  issuer: Kipper
  logoURL: https://console-old.kipper.run/logo.svg
  theme: light
someFutureDexKnob: keep-me
connectors:
- id: github
  type: github
  name: GitHub
  config:
    clientID: abc
    redirectURI: https://dex-old.kipper.run/dex/callback
staticClients:
- id: kipper-console
  name: Kipper Console
  redirectURIs:
  - https://console-old.kipper.run/callback
  secret: super-secret-value
- id: kipper-cli
  name: Kipper CLI
  public: true
  redirectURIs:
  - http://localhost:18741/callback
staticPasswords:
- email: admin@old.kipper.run
  hash: $2y$10$BCRYPTHASHVALUE
  username: admin
storage:
  type: kubernetes
  config:
    inCluster: true
`

func load(t *testing.T) *Config {
	t.Helper()
	c, err := Load(fixture)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return c
}

func asMap(t *testing.T, c *Config) map[string]any {
	t.Helper()
	out, err := c.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	return m
}

func TestSetIssuer(t *testing.T) {
	c := load(t)
	c.SetIssuer("https://dex-new.kipper.run/dex")
	if got := c.Issuer(); got != "https://dex-new.kipper.run/dex" {
		t.Fatalf("issuer = %q", got)
	}
}

func TestSetConsoleRedirectURIsLeavesCLIClientUntouched(t *testing.T) {
	c := load(t)
	if err := c.SetConsoleRedirectURIs("https://console-new.kipper.run/callback"); err != nil {
		t.Fatal(err)
	}
	if got := c.ConsoleRedirectURIs(); len(got) != 1 || got[0] != "https://console-new.kipper.run/callback" {
		t.Fatalf("console redirectURIs = %v", got)
	}

	// The public CLI client's localhost callback is host-independent and must
	// never change — that would break `kip auth login` for everyone.
	if uris := cliRedirectURIs(t, c); len(uris) != 1 || uris[0] != "http://localhost:18741/callback" {
		t.Fatalf("kipper-cli redirectURIs changed: %v", uris)
	}
}

// Client order must not matter: the public CLI client is untouched even when it
// comes first in the list.
func TestSetConsoleRedirectURIs_ClientOrderIndependent(t *testing.T) {
	reordered := `issuer: https://dex-old.kipper.run/dex
staticClients:
- id: kipper-cli
  public: true
  redirectURIs:
  - http://localhost:18741/callback
- id: kipper-console
  redirectURIs:
  - https://console-old.kipper.run/callback
`
	c, err := Load(reordered)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetConsoleRedirectURIs("https://console-new.kipper.run/callback"); err != nil {
		t.Fatal(err)
	}
	if uris := cliRedirectURIs(t, c); len(uris) != 1 || uris[0] != "http://localhost:18741/callback" {
		t.Fatalf("kipper-cli redirectURIs changed: %v", uris)
	}
	if got := c.ConsoleRedirectURIs(); got[0] != "https://console-new.kipper.run/callback" {
		t.Fatalf("console redirect not updated: %v", got)
	}
}

func TestSetConsoleRedirectURIs_FailsClosed(t *testing.T) {
	t.Run("missing console client", func(t *testing.T) {
		c, err := Load("issuer: x\nstaticClients:\n- id: kipper-cli\n  redirectURIs: [http://localhost:18741/callback]\n")
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SetConsoleRedirectURIs("https://new/callback"); err == nil {
			t.Fatal("expected an error when kipper-console client is missing")
		}
	})
	t.Run("duplicate console client", func(t *testing.T) {
		c, err := Load("issuer: x\nstaticClients:\n- id: kipper-console\n  redirectURIs: [a]\n- id: kipper-console\n  redirectURIs: [b]\n")
		if err != nil {
			t.Fatal(err)
		}
		if err := c.SetConsoleRedirectURIs("https://new/callback"); err == nil {
			t.Fatal("expected an error on duplicate kipper-console client")
		}
	})
}

func TestSetAdminEmailPreservesHash(t *testing.T) {
	c := load(t)
	if err := c.SetAdminEmail("admin@new.kipper.run"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.AdminEmail()
	if err != nil || !ok || got != "admin@new.kipper.run" {
		t.Fatalf("admin email = %q ok=%v err=%v", got, ok, err)
	}
	m := asMap(t, c)
	pw := m["staticPasswords"].([]any)[0].(map[string]any)
	if pw["hash"] != "$2y$10$BCRYPTHASHVALUE" {
		t.Fatalf("bcrypt hash was not preserved: %v", pw["hash"])
	}
	if pw["username"] != "admin" {
		t.Fatalf("username changed: %v", pw["username"])
	}
}

// The admin is picked by username even with other users present, and its hash
// is preserved; other users are untouched.
func TestSetAdminEmail_PicksAdminAmongUsers(t *testing.T) {
	c, err := Load(`issuer: x
staticPasswords:
- email: bob@old
  hash: HASH_BOB
  username: bob
- email: admin@old
  hash: HASH_ADMIN
  username: admin
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetAdminEmail("admin@new"); err != nil {
		t.Fatal(err)
	}
	m := asMap(t, c)
	pws := m["staticPasswords"].([]any)
	for _, p := range pws {
		pm := p.(map[string]any)
		switch pm["username"] {
		case "admin":
			if pm["email"] != "admin@new" || pm["hash"] != "HASH_ADMIN" {
				t.Fatalf("admin entry wrong: %v", pm)
			}
		case "bob":
			if pm["email"] != "bob@old" || pm["hash"] != "HASH_BOB" {
				t.Fatalf("bob entry was disturbed: %v", pm)
			}
		}
	}
}

func TestSetAdminEmail_FailsClosed(t *testing.T) {
	cases := map[string]string{
		"no admin, multiple users": "issuer: x\nstaticPasswords:\n- {email: a, username: alice}\n- {email: b, username: bob}\n",
		"duplicate admin":          "issuer: x\nstaticPasswords:\n- {email: a, username: admin}\n- {email: b, username: admin}\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := Load(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.SetAdminEmail("admin@new"); err == nil {
				t.Fatalf("expected fail-closed error for %q", name)
			}
		})
	}
}

// A blank email is not a login: Dex matches a static password on its email, so
// reporting one as present hands the caller an address nobody can sign in with.
func TestAdminEmail_TreatsABlankAddressAsAbsent(t *testing.T) {
	for name, raw := range map[string]string{
		"empty string": "issuer: x\nstaticPasswords:\n- {email: \"\", hash: H, username: admin}\n",
		"null":         "issuer: x\nstaticPasswords:\n- {email: , hash: H, username: admin}\n",
		"whitespace":   "issuer: x\nstaticPasswords:\n- {email: \"   \", hash: H, username: admin}\n",
		"no key":       "issuer: x\nstaticPasswords:\n- {hash: H, username: admin}\n",
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(raw)
			if err != nil {
				t.Fatal(err)
			}
			email, ok, err := c.AdminEmail()
			if err != nil {
				t.Fatal(err)
			}
			if ok {
				t.Fatalf("a %s email was reported as usable: %q", name, email)
			}
		})
	}
}

// HasAdmin separates "no admin entry at all" from "an admin entry with nothing
// to sign in as", which AdminEmail alone reports identically.
func TestHasAdmin(t *testing.T) {
	for name, tc := range map[string]struct {
		raw  string
		want bool
	}{
		"entry with an email": {"issuer: x\nstaticPasswords:\n- {email: a@b, hash: H, username: admin}\n", true},
		"entry without one":   {"issuer: x\nstaticPasswords:\n- {hash: H, username: admin}\n", true},
		"no static passwords": {"issuer: x\n", false},
		"empty list":          {"issuer: x\nstaticPasswords: []\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Load(tc.raw)
			if err != nil {
				t.Fatal(err)
			}
			if got := c.HasAdmin(); got != tc.want {
				t.Fatalf("HasAdmin() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSetAdminHashPreservesEmailAndUsername(t *testing.T) {
	c := load(t)
	if err := c.SetAdminHash("$2y$10$NEWHASHVALUE"); err != nil {
		t.Fatal(err)
	}
	m := asMap(t, c)
	pw := m["staticPasswords"].([]any)[0].(map[string]any)
	if pw["hash"] != "$2y$10$NEWHASHVALUE" {
		t.Fatalf("hash = %v", pw["hash"])
	}
	if pw["email"] != "admin@old.kipper.run" {
		t.Fatalf("email changed: %v", pw["email"])
	}
	if pw["username"] != "admin" {
		t.Fatalf("username changed: %v", pw["username"])
	}
	if m["someFutureDexKnob"] != "keep-me" {
		t.Fatalf("unmanaged field lost: %v", m["someFutureDexKnob"])
	}
}

// The reason the command cannot keep replacing the first `hash:` line: with a
// per-operator account listed first, that line belongs to somebody else.
func TestSetAdminHash_PicksAdminAmongUsers(t *testing.T) {
	c, err := Load(`issuer: x
staticPasswords:
- email: bob@old
  hash: HASH_BOB
  username: bob
- email: admin@old
  hash: HASH_ADMIN
  username: admin
`)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetAdminHash("HASH_NEW"); err != nil {
		t.Fatal(err)
	}
	m := asMap(t, c)
	for _, p := range m["staticPasswords"].([]any) {
		pm := p.(map[string]any)
		switch pm["username"] {
		case "admin":
			if pm["hash"] != "HASH_NEW" || pm["email"] != "admin@old" {
				t.Fatalf("admin entry wrong: %v", pm)
			}
		case "bob":
			if pm["hash"] != "HASH_BOB" || pm["email"] != "bob@old" {
				t.Fatalf("bob entry was disturbed: %v", pm)
			}
		}
	}
}

func TestSetAdminHash_FailsClosed(t *testing.T) {
	cases := map[string]string{
		"no admin, multiple users": "issuer: x\nstaticPasswords:\n- {email: a, username: alice}\n- {email: b, username: bob}\n",
		"duplicate admin":          "issuer: x\nstaticPasswords:\n- {email: a, username: admin}\n- {email: b, username: admin}\n",
		"no static passwords":      "issuer: x\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := Load(raw)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.SetAdminHash("HASH_NEW"); err == nil {
				t.Fatalf("expected fail-closed error for %q", name)
			}
		})
	}
}

func TestLoad_FailsClosedOnMalformed(t *testing.T) {
	cases := map[string]string{
		"non-mapping root":        "- just\n- a\n- list\n",
		"duplicate top-level key": "issuer: a\nissuer: b\n",
		"staticClients not a seq": "issuer: x\nstaticClients:\n  id: not-a-list\n",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(raw); err == nil {
				t.Fatalf("expected Load to reject %q", name)
			}
		})
	}
}

func TestUnknownAndUnmanagedFieldsSurviveRoundTrip(t *testing.T) {
	c := load(t)
	c.SetIssuer("https://dex-new.kipper.run/dex")
	if err := c.SetConsoleRedirectURIs("https://console-new.kipper.run/callback"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetAdminEmail("admin@new.kipper.run"); err != nil {
		t.Fatal(err)
	}
	c.SetFrontend("https://console-new.kipper.run/logo.svg")
	c.RehostConnectors("https://dex-new.kipper.run/dex")

	m := asMap(t, c)
	if m["someFutureDexKnob"] != "keep-me" {
		t.Fatalf("unknown field lost: %v", m["someFutureDexKnob"])
	}
	if m["enablePasswordDB"] != true {
		t.Fatalf("enablePasswordDB lost")
	}
	if m["storage"].(map[string]any)["type"] != "kubernetes" {
		t.Fatalf("storage config lost")
	}
	// The console client secret is untouched (render never regenerates it).
	clients := m["staticClients"].([]any)
	for _, cl := range clients {
		cm := cl.(map[string]any)
		if cm["id"] == consoleClientID && cm["secret"] != "super-secret-value" {
			t.Fatalf("console client secret changed: %v", cm["secret"])
		}
	}
}

func TestConnectorsAndRehost(t *testing.T) {
	c := load(t)
	conns := c.Connectors()
	if len(conns) != 1 || conns[0].ID != "github" || conns[0].Type != "github" {
		t.Fatalf("connectors = %+v", conns)
	}

	c.RehostConnectors("https://dex-new.kipper.run/dex/") // trailing slash tolerated
	m := asMap(t, c)
	cfg := m["connectors"].([]any)[0].(map[string]any)["config"].(map[string]any)
	if cfg["redirectURI"] != "https://dex-new.kipper.run/dex/callback" {
		t.Fatalf("connector redirectURI not rehosted cleanly: %v", cfg["redirectURI"])
	}
	if cfg["clientID"] != "abc" {
		t.Fatalf("connector clientID changed: %v", cfg["clientID"])
	}
}

// A connector without a redirectURI (or empty connectors) must be a safe no-op.
func TestRehostConnectors_Safe(t *testing.T) {
	c, err := Load("issuer: https://x/dex\nconnectors:\n- id: oidc\n  type: oidc\n  config:\n    clientID: z\n")
	if err != nil {
		t.Fatal(err)
	}
	c.RehostConnectors("https://y/dex")
	m := asMap(t, c)
	cfg := m["connectors"].([]any)[0].(map[string]any)["config"].(map[string]any)
	if _, has := cfg["redirectURI"]; has {
		t.Fatalf("must not invent a redirectURI: %v", cfg)
	}
}

func cliRedirectURIs(t *testing.T, c *Config) []any {
	t.Helper()
	m := asMap(t, c)
	for _, cl := range m["staticClients"].([]any) {
		cm := cl.(map[string]any)
		if cm["id"] == cliClientID {
			if u, ok := cm["redirectURIs"].([]any); ok {
				return u
			}
		}
	}
	return nil
}

func TestMarshalRoundTripsWithoutManglingIssuer(t *testing.T) {
	c, err := Load("issuer: https://x/dex\nconnectors: []\n")
	if err != nil {
		t.Fatal(err)
	}
	out, _ := c.Marshal()
	if !strings.Contains(out, "issuer: https://x/dex") {
		t.Fatalf("issuer mangled: %q", out)
	}
}

func TestSetFrontendCreatesBlockWhenAbsent(t *testing.T) {
	// A live config with no frontend block must still gain full Kipper branding.
	const noFrontend = "issuer: https://dex.example.com/dex\n" +
		"enablePasswordDB: true\n" +
		"staticClients:\n- id: kipper-console\n  redirectURIs: [https://console.example.com/callback]\n  secret: keep\n"
	c, err := Load(noFrontend)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	c.SetFrontend("https://console.example.com/logo-stacked-light.svg")

	out, err := c.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	reloaded, err := Load(out)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	fe := mapValue(reloaded.root, "frontend")
	if fe == nil {
		t.Fatal("frontend block was not created")
	}
	if got := scalarValue(mapValue(fe, "issuer")); got != "Kipper" {
		t.Fatalf("frontend.issuer = %q, want Kipper", got)
	}
	if got := scalarValue(mapValue(fe, "logoURL")); got != "https://console.example.com/logo-stacked-light.svg" {
		t.Fatalf("frontend.logoURL wrong: %q", got)
	}
	if got := scalarValue(mapValue(fe, "theme")); got != "light" {
		t.Fatalf("frontend.theme = %q, want light", got)
	}
	// The pre-existing client secret survives.
	if !strings.Contains(out, "secret: keep") {
		t.Fatal("unrelated config lost")
	}
}
