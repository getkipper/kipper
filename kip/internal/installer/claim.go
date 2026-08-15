package installer

import (
	"fmt"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/domain"
)

// NormaliseDomain returns the canonical spelling of a domain the operator
// typed: lowercase, no surrounding space, no trailing dot.
//
// DNS names are case-insensitive and may be written as a fully-qualified name
// ending in a dot, so LAB.KIPPER.RUN and lab.kipper.run. name the same host.
// Everything downstream compares them as strings — the gateway suffix test, the
// label rule, the CRD's lowercase pattern — so a name that reaches them
// unnormalised is read as a different host than the one meant.
func NormaliseDomain(domain string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(domain)), ".")
}

// KipperRunLabelFor returns the *.kipper.run label an install claims for a
// server, and whether it claims one at all.
//
// An empty domain takes the label derived from the server's address, which is
// the default every free-tier install has always used. A *.kipper.run domain
// takes the operator's chosen label. Any other domain is a custom domain whose
// DNS the operator controls, so nothing is claimed from the gateway and no label
// rule applies.
//
// The chosen label is checked against the same rule the gateway enforces, and
// against the guard protecting address-shaped names, so a name that could never
// be registered fails before this machine has connected to anything.
func KipperRunLabelFor(domain, host string) (string, bool, error) {
	domain = NormaliseDomain(domain)
	if domain == "" {
		return hostnames.LabelForIP(host), true, nil
	}
	// The apex is checked ahead of the suffix test, which excludes it: IsKipperRun
	// asks whether a host sits under the gateway domain, and the apex sits on it.
	// Falling through would install a cluster claiming to serve every other
	// tenant's hostname.
	if domain == hostnames.GatewayDomain {
		return "", false, fmt.Errorf("%s is the gateway's own domain; choose a name under it (--domain lab.%s) or a domain you control",
			hostnames.GatewayDomain, hostnames.GatewayDomain)
	}
	if !hostnames.IsKipperRun(domain) {
		return "", false, nil
	}

	label := strings.TrimSuffix(domain, "."+hostnames.GatewayDomain)
	if err := hostnames.ValidateClusterLabel(label); err != nil {
		return "", false, fmt.Errorf("%s cannot be registered: %w", domain, err)
	}
	// The gateway refuses an address-shaped label from any other address. Saying
	// so here names the server that owns it, which the gateway's answer cannot.
	if hostnames.IPShapedLabel(label) && label != hostnames.LabelForIP(host) {
		return "", false, fmt.Errorf("%s is the name reserved for the server at %s, so it cannot be registered for %s. Choose a different name",
			domain, strings.ReplaceAll(label, "-", "."), host)
	}
	return label, true, nil
}

// gatewayRegistrationFor returns the *.kipper.run domain a fresh install records
// in the ClusterIdentity's gateway block, or empty when the cluster registers
// nothing.
//
// It reads the serving domain rather than the --domain flag. Asking whether the
// flag was empty is what left a chosen label with no gateway block: console-api
// got no KIPPER_RUN_DOMAIN, never heartbeated, and the cluster served a name the
// gateway would not route. An adopted identity keeps whatever its CR already
// records, which after a custom-domain move differs from the serving domain.
func gatewayRegistrationFor(existing *ExistingIdentity, domainName string) string {
	if existing != nil {
		return existing.KipperRunDomain
	}
	if hostnames.IsKipperRun(domainName) {
		return domainName
	}
	return ""
}

// registrar claims and releases names with the gateway. Narrow so a test can
// drive the answers that matter — a fresh creation, a renewal, and a name
// already held by someone else — none of which can be produced on demand from
// the real gateway.
type registrar interface {
	Register(subdomain, ip, token string) (*domain.Registration, error)
	Deregister(token string) error
}

// tokenStore is the durable record of a gateway token, whose only other copy
// lives on a cluster that does not exist yet.
type tokenStore interface {
	tokenFor(host string) string
	save(host, clusterName, token string) error
}

// claim is a name this install may serve on, and the token proving it.
type claim struct {
	Domain string
	Token  string
	// Created distinguishes a name this run brought into existence from one it
	// renewed. Only the former may be handed back if the install fails: a
	// renewal means the registration predates this attempt, and a half-built
	// cluster from an earlier one may still be serving on it.
	Created bool
}

// claimGatewayName registers a *.kipper.run name for a host and guarantees its
// token is recorded before returning.
//
// The gateway discloses a registration's token exactly once, at creation. Three
// things follow, and each of them was a defect before it was a rule:
//
//   - A token already held must be presented. Arriving anonymously against a
//     name this host previously claimed gets an answer with no token, which the
//     refusal below then turns into a dead end — a single failed attempt locking
//     an operator out of their own host's name until the gateway frees it.
//   - A registration without a token must fail the install, unless the gateway
//     issued a challenge — which it does only for a token it recognised, and is
//     therefore the only evidence a renewal was authorised rather than turned
//     away. Such a cluster can never prove possession, so the gateway will not
//     route it and the console URL printed at the end would never answer.
//   - The token must be durable before this returns. Until it is, the only copy
//     is in memory; and a claim that cannot be recorded is handed straight back,
//     because failing with it unrecorded strands the name exactly as the wipe
//     without a release used to.
func claimGatewayName(gw registrar, store tokenStore, subdomain, host string) (claim, error) {
	known := store.tokenFor(host)

	reg, err := gw.Register(subdomain, host, known)
	if err != nil {
		return claim{}, fmt.Errorf("registering subdomain: %w", err)
	}

	created := reg.Token != ""
	token := reg.Token
	if token == "" && reg.Challenge != "" {
		// A renewal the gateway authorised. It answers a renewal and a request
		// it turned away identically — 201, no token — and issues a challenge
		// only to a caller whose token it recognised. So the challenge is the
		// acceptance signal; a token this side merely held proves nothing, and
		// treating it as proof lets a stale credential build a cluster that can
		// never renew, prove or release its own name.
		token = known
	}
	if token == "" {
		return claim{}, fmt.Errorf("the subdomain %s is already registered, and the gateway did not accept a token for it from this machine "+
			"(there may be none, or the one stored here may no longer be the one it holds), so it would never route to this cluster. "+
			"Install with --domain <your-own-domain> to use a domain you control instead", reg.Domain)
	}

	if token == known && known != "" {
		// A renewal of a token that came out of this store: it is already
		// durable, and failing the install on a redundant write would refuse a
		// retry holding everything it needs.
		return claim{Domain: reg.Domain, Token: token, Created: created}, nil
	}
	if saveErr := store.save(host, reg.Domain, token); saveErr != nil {
		// Nothing durable holds this token, and returning now would leave the
		// name claimed by a registration nobody can prove ownership of — the
		// stranding this whole path exists to prevent. Give it back while it is
		// still in hand. Only a registration this run created may be released:
		// renewing someone's existing name confers no right to end it.
		if created {
			if relErr := gw.Deregister(token); relErr != nil {
				return claim{}, fmt.Errorf("could not record the gateway credential for %s (%w), and could not release the name either (%v), "+
					"it stays registered until the gateway's 30-day inactivity sweep frees it", reg.Domain, saveErr, relErr)
			}
			return claim{}, fmt.Errorf("could not record the gateway credential for %s: %w (the name was released, so re-running is safe)", reg.Domain, saveErr)
		}
		return claim{}, fmt.Errorf("could not record the gateway credential for %s: %w", reg.Domain, saveErr)
	}
	return claim{Domain: reg.Domain, Token: token, Created: created}, nil
}

// ClearHostWipedMarker retires the record that this host was wiped and is
// waiting only to have its gateway name released.
//
// An install is the thing that makes it false: from here on the host carries a
// cluster again, so `kip cluster uninstall` must go back to wiping it rather
// than skipping straight to the gateway. Left set through a half-built install
// it is worse than stale — the uninstall would hand back the name, delete the
// local entry, and leave a live k3s on a server nothing records any more.
//
// Called once the install commits to touching the host and never before. That
// ordering is about accuracy rather than safety now: an uninstall that cannot
// reach a host offers to release its name anyway, so clearing the marker early
// costs a prompt rather than the name.
func ClearHostWipedMarker(host string) error {
	return config.Update(func(cfg *config.Config) error {
		// Every entry naming this host, not just the first. A config can hold
		// more than one — an earlier install whose checkpoint could not read the
		// config appends a second under a different name — and clearing only the
		// first leaves the marker set on a host that is about to go live again.
		cleared := false
		for i := range cfg.Clusters {
			if cfg.Clusters[i].Host == host && cfg.Clusters[i].HostWiped {
				cfg.Clusters[i].HostWiped = false
				cleared = true
			}
		}
		if !cleared {
			return config.ErrNoChange
		}
		return nil
	})
}

// clearTokenIfHeld removes a gateway credential from the entry for host, and
// only while that entry still holds exactly it.
//
// The token being cleared names a registration this install just handed back, so
// leaving it on disk would let a later command present a dead credential and read
// the gateway's "not registered" as "already released", hiding a real
// registration made since. Clearing unconditionally is the other half of that
// mistake: by the time a failing install gets here another command may have put
// a live credential in the same entry.
func clearTokenIfHeld(host, token string) error {
	return config.Update(func(cfg *config.Config) error {
		entry := cfg.GetClusterByHost(host)
		if entry == nil || entry.GatewayToken != token {
			return config.ErrNoChange
		}
		entry.GatewayToken = ""
		return nil
	})
}

// localTokenStore is the tokenStore backed by ~/.kip/config.yaml, which is where
// a gateway token has to survive an install that fails before the cluster exists
// to hold it.
type localTokenStore struct{}

func (localTokenStore) tokenFor(host string) string {
	cfg, err := config.Load()
	if err != nil {
		return ""
	}
	if entry := cfg.GetClusterByHost(host); entry != nil {
		return entry.GatewayToken
	}
	return ""
}

func (localTokenStore) save(host, clusterName, token string) error {
	return config.Update(func(cfg *config.Config) error {
		if entry := cfg.GetClusterByHost(host); entry != nil {
			if entry.GatewayToken == token {
				return config.ErrNoChange
			}
			entry.GatewayToken = token
			return nil
		}
		if token == "" {
			// Clearing a token for a host with no entry. Creating one to hold
			// nothing would leave a stub naming a cluster that does not exist.
			return config.ErrNoChange
		}
		// No entry yet: this is a first install, and the token must be durable
		// before the steps that can fail begin. Named for the domain rather than
		// the host, because the checkpoint later reuses this entry's name —
		// naming it by IP would leave the finished cluster called after its
		// address.
		cfg.Clusters = append(cfg.Clusters, config.Cluster{
			Name: clusterName, Host: host, Provider: "baremetal",
			Domain: clusterName, GatewayToken: token,
		})
		return nil
	})
}
