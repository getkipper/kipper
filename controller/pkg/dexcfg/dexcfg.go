// Package dexcfg is the one editor of Kipper's Dex config.yaml. It round-trips
// through a yaml.Node so every field Kipper does not manage — SSO connectors
// written by the console at runtime, storage, oauth2 settings, and anything a
// future Dex version adds — survives verbatim. Line-level string surgery on this
// file (three separate sites before this package) is exactly what let host
// reconfiguration corrupt login config, so all reads and writes go through here.
package dexcfg

import (
	"fmt"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// Client IDs Kipper always defines. The console client's redirect URIs are
// host-dependent and move on a domain change; the CLI client is a public client
// whose only redirect is a fixed localhost loopback and must never be touched.
const (
	consoleClientID = "kipper-console"
	cliClientID     = "kipper-cli"
	adminUsername   = "admin"
)

// Config is a parsed Dex config that edits in place and re-marshals unchanged
// except for the fields explicitly set.
type Config struct {
	doc  yaml.Node  // document node
	root *yaml.Node // the top-level mapping
}

// ConnectorInfo identifies a configured SSO connector, for surfacing which
// external OAuth callbacks an operator must update after a host change.
type ConnectorInfo struct {
	ID   string
	Type string
	Name string
}

// Load parses raw Dex config.yaml. It fails closed on malformed structure a
// cutover must never write into: a non-mapping root, duplicate top-level keys,
// or a managed collection (staticClients/staticPasswords/connectors) that is
// present but not a sequence.
func Load(raw string) (*Config, error) {
	c := &Config{}
	if err := yaml.Unmarshal([]byte(raw), &c.doc); err != nil {
		return nil, fmt.Errorf("parsing dex config: %w", err)
	}
	if len(c.doc.Content) == 0 || c.doc.Content[0].Kind != yaml.MappingNode {
		return nil, fmt.Errorf("dex config is not a yaml mapping")
	}
	c.root = c.doc.Content[0]

	seen := map[string]bool{}
	for i := 0; i+1 < len(c.root.Content); i += 2 {
		key := c.root.Content[i].Value
		if seen[key] {
			return nil, fmt.Errorf("dex config has duplicate top-level key %q", key)
		}
		seen[key] = true
	}
	for _, key := range []string{"staticClients", "staticPasswords", "connectors"} {
		if v := mapValue(c.root, key); v != nil && v.Kind != yaml.SequenceNode && v.Tag != "!!null" {
			return nil, fmt.Errorf("dex config %q is not a sequence", key)
		}
	}
	return c, nil
}

// Marshal re-serialises the config. Untouched fields are byte-preserving up to
// yaml.v3's re-emit; touched fields carry their new values.
func (c *Config) Marshal() (string, error) {
	out, err := yaml.Marshal(&c.doc)
	if err != nil {
		return "", fmt.Errorf("serialising dex config: %w", err)
	}
	return string(out), nil
}

// Issuer returns the top-level OIDC issuer URL.
func (c *Config) Issuer() string {
	if v := mapValue(c.root, "issuer"); v != nil {
		return v.Value
	}
	return ""
}

// SetIssuer sets the top-level OIDC issuer URL. This is the value every JWT's
// `iss` claim carries and console-api validates against.
func (c *Config) SetIssuer(url string) {
	setScalar(c.root, "issuer", url)
}

// SetFrontend ensures the Kipper frontend branding block: the "Kipper" display
// name, the console-hosted logo URL, and the light theme. It creates the
// frontend mapping when absent and preserves any other keys already present, so
// branding is always applied after a reconcile — including on a live config that
// never had a frontend block.
func (c *Config) SetFrontend(logoURL string) {
	fe := ensureMapping(c.root, "frontend")
	setScalar(fe, "issuer", "Kipper")
	setScalar(fe, "logoURL", logoURL)
	setScalar(fe, "theme", "light")
}

// ensureMapping returns the mapping node stored under key, creating (or coercing)
// it to an empty mapping when absent or of another kind.
func ensureMapping(m *yaml.Node, key string) *yaml.Node {
	if v := mapValue(m, key); v != nil {
		if v.Kind != yaml.MappingNode {
			v.Kind = yaml.MappingNode
			v.Tag = "!!map"
			v.Value = ""
			v.Content = nil
		}
		return v
	}
	node := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		node,
	)
	return node
}

// ConsoleRedirectURIs returns the redirect URIs of the kipper-console client.
func (c *Config) ConsoleRedirectURIs() []string {
	client, err := staticClientByID(c.root, consoleClientID)
	if err != nil || client == nil {
		return nil
	}
	uris := mapValue(client, "redirectURIs")
	if uris == nil || uris.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]string, 0, len(uris.Content))
	for _, n := range uris.Content {
		out = append(out, n.Value)
	}
	return out
}

// SetConsoleRedirectURIs replaces the redirect URIs of ONLY the kipper-console
// client. The public kipper-cli client (localhost loopback) is never touched. It
// fails closed if the console client is missing or duplicated, so a cutover can
// never apply a config where the redirect was not actually updated.
func (c *Config) SetConsoleRedirectURIs(uris ...string) error {
	client, err := staticClientByID(c.root, consoleClientID)
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("dex config has no %q static client", consoleClientID)
	}
	setStringSeq(client, "redirectURIs", uris)
	return nil
}

// Connectors lists the configured SSO connectors.
func (c *Config) Connectors() []ConnectorInfo {
	seq := mapValue(c.root, "connectors")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}
	out := make([]ConnectorInfo, 0, len(seq.Content))
	for _, conn := range seq.Content {
		if conn.Kind != yaml.MappingNode {
			continue
		}
		out = append(out, ConnectorInfo{
			ID:   scalarValue(mapValue(conn, "id")),
			Type: scalarValue(mapValue(conn, "type")),
			Name: scalarValue(mapValue(conn, "name")),
		})
	}
	return out
}

// RehostConnectors rewrites each connector's config.redirectURI to Dex's own
// callback on the new issuer host (Dex terminates the provider callback at
// <issuer>/callback). The provider-side allow-list is external and cannot be
// changed from here — the caller surfaces that to the operator.
func (c *Config) RehostConnectors(issuer string) {
	seq := mapValue(c.root, "connectors")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return
	}
	for _, conn := range seq.Content {
		if conn.Kind != yaml.MappingNode {
			continue
		}
		cfg := mapValue(conn, "config")
		if cfg == nil || cfg.Kind != yaml.MappingNode {
			continue
		}
		if mapValue(cfg, "redirectURI") != nil {
			setScalar(cfg, "redirectURI", strings.TrimRight(issuer, "/")+"/callback")
		}
	}
}

// AdminEmail returns the Kipper admin static-password email. ok is false when
// there is no admin entry; err is set only when the entry is ambiguous.
func (c *Config) AdminEmail() (email string, ok bool, err error) {
	entry, err := adminPassword(c.root)
	if err != nil {
		return "", false, err
	}
	if entry == nil {
		return "", false, nil
	}
	if v := mapValue(entry, "email"); v != nil {
		return v.Value, true, nil
	}
	return "", false, nil
}

// SetAdminEmail rewrites the admin static-password email while preserving its
// bcrypt hash and username — the hash is the one field that cannot be
// recomputed, so it is never regenerated. It fails closed if no unambiguous
// admin entry exists.
func (c *Config) SetAdminEmail(email string) error {
	entry, err := adminPassword(c.root)
	if err != nil {
		return err
	}
	if entry == nil {
		return fmt.Errorf("dex config has no admin static password")
	}
	setScalar(entry, "email", email)
	return nil
}

// --- yaml.Node navigation helpers ---

func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalarValue(n *yaml.Node) string {
	if n == nil {
		return ""
	}
	return n.Value
}

// setScalar sets key to a scalar value, creating the key if absent.
func setScalar(m *yaml.Node, key, val string) {
	if v := mapValue(m, key); v != nil {
		v.Kind = yaml.ScalarNode
		v.Tag = "!!str"
		v.Value = val
		v.Content = nil
		return
	}
	m.Content = append(m.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key},
		&yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: val},
	)
}

// setStringSeq sets key to a sequence of string scalars, replacing any existing
// value.
func setStringSeq(m *yaml.Node, key string, vals []string) {
	seq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
	for _, v := range vals {
		seq.Content = append(seq.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: v})
	}
	if existing := mapValue(m, key); existing != nil {
		*existing = *seq
		return
	}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}, seq)
}

// staticClientByID returns the single client with the given id. It errors on a
// duplicate id (ambiguous — updating one would leave a stale one). A missing
// client is (nil, nil); the caller decides whether that is fatal.
func staticClientByID(root *yaml.Node, id string) (*yaml.Node, error) {
	seq := mapValue(root, "staticClients")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil, nil
	}
	var found *yaml.Node
	for _, client := range seq.Content {
		if client.Kind == yaml.MappingNode && scalarValue(mapValue(client, "id")) == id {
			if found != nil {
				return nil, fmt.Errorf("dex config has duplicate static client %q", id)
			}
			found = client
		}
	}
	return found, nil
}

// adminPassword returns the admin static-password entry: the one with username
// "admin", or the sole entry when there is exactly one. It errors when the admin
// is ambiguous (multiple "admin" usernames, or several entries with none named
// "admin"), so SetAdminEmail never guesses which login it is renaming.
func adminPassword(root *yaml.Node) (*yaml.Node, error) {
	seq := mapValue(root, "staticPasswords")
	if seq == nil || seq.Kind != yaml.SequenceNode || len(seq.Content) == 0 {
		return nil, nil
	}
	var byName *yaml.Node
	for _, e := range seq.Content {
		if e.Kind == yaml.MappingNode && scalarValue(mapValue(e, "username")) == adminUsername {
			if byName != nil {
				return nil, fmt.Errorf("dex config has multiple %q static passwords", adminUsername)
			}
			byName = e
		}
	}
	if byName != nil {
		return byName, nil
	}
	if len(seq.Content) == 1 && seq.Content[0].Kind == yaml.MappingNode {
		return seq.Content[0], nil
	}
	return nil, fmt.Errorf("dex config admin static password is ambiguous: %d entries, none named %q", len(seq.Content), adminUsername)
}
