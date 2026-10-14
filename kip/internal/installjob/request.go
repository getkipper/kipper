// Package installjob manages server-side installs with persistent state,
// reconnectable logs, and results collected by a client over SSH.
package installjob

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

// RequestVersion is the request format this build reads and writes.
const RequestVersion = 1

// runIDPattern keeps a run id usable as a directory and a systemd unit name.
var runIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Request carries resolved install settings as JSON for the server job.
type Request struct {
	Version    int    `json:"version"`
	RunID      string `json:"run_id"`
	KipVersion string `json:"kip_version,omitempty"`

	Host             string          `json:"host"`
	Domain           string          `json:"domain,omitempty"`
	ConsoleDomain    string          `json:"console_domain,omitempty"`
	ConsoleAPIDomain string          `json:"console_api_domain,omitempty"`
	DexDomain        string          `json:"dex_domain,omitempty"`
	AdminEmail       string          `json:"admin_email,omitempty"`
	Org              string          `json:"org,omitempty"`
	OrgDisplayName   string          `json:"org_display_name,omitempty"`
	Harden           bool            `json:"harden"`
	Firewall         bool            `json:"firewall"`
	AdoptFirewall    bool            `json:"adopt_firewall,omitempty"`
	AdoptFingerprint string          `json:"adopt_fingerprint,omitempty"`
	NoSSHRateLimit   bool            `json:"no_ssh_rate_limit"`
	DNSResolvers     []string        `json:"dns_resolvers,omitempty"`
	TrustedProxies   []string        `json:"trusted_proxies,omitempty"`
	Backup           *Backup         `json:"backup,omitempty"`
	Entry            *config.Cluster `json:"entry,omitempty"`
}

// Backup carries external storage settings and credentials resolved by the client.
type Backup struct {
	Bucket          string `json:"bucket"`
	Region          string `json:"region"`
	Endpoint        string `json:"endpoint,omitempty"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
}

// ParseRequest rejects unknown fields, unsupported versions, invalid run IDs,
// and empty or flag-like hosts.
func ParseRequest(data []byte) (Request, error) {
	var r Request
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return Request{}, fmt.Errorf("reading the install request: %w", err)
	}
	if r.Version != RequestVersion {
		return Request{}, fmt.Errorf("unsupported install request version %d (expected %d); use the same kip version on both machines", r.Version, RequestVersion)
	}
	if !runIDPattern.MatchString(r.RunID) {
		return Request{}, fmt.Errorf("install request run id %q is not a plain name", r.RunID)
	}
	if r.Host == "" {
		return Request{}, fmt.Errorf("install request has no host")
	}
	// Keep the host argument distinct from command-line flags.
	if strings.HasPrefix(r.Host, "-") {
		return Request{}, fmt.Errorf("install request host %q is not a host name or address", r.Host)
	}
	return r, nil
}

// Options turns the request into installer options for an install that runs
// on the server itself.
func (r Request) Options() installer.Options {
	opts := installer.Options{
		KipVersion:       r.KipVersion,
		Host:             r.Host,
		Local:            true,
		Domain:           r.Domain,
		ConsoleDomain:    r.ConsoleDomain,
		ConsoleAPIDomain: r.ConsoleAPIDomain,
		DexDomain:        r.DexDomain,
		AdminEmail:       r.AdminEmail,
		Org:              r.Org,
		OrgDisplayName:   r.OrgDisplayName,
		Harden:           r.Harden,
		Firewall:         r.Firewall,
		AdoptFirewall:    r.AdoptFirewall,
		AdoptFingerprint: r.AdoptFingerprint,
		NoSSHRateLimit:   r.NoSSHRateLimit,
		DNSResolvers:     r.DNSResolvers,
		TrustedProxies:   r.TrustedProxies,
	}
	if b := r.Backup; b != nil {
		opts.BackupStorage = &installer.BackupStorageConfig{
			Bucket: b.Bucket, Region: b.Region, Endpoint: b.Endpoint,
			AccessKeyID: b.AccessKeyID, SecretAccessKey: b.SecretAccessKey,
		}
	}
	return opts
}

// String describes the request without its credentials.
func (r Request) String() string {
	return fmt.Sprintf("install request %s for %s", r.RunID, r.Host)
}
