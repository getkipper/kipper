package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/getkipper/kipper/controller/pkg/hostnames"
	"github.com/getkipper/kipper/gateway/internal/registry"
)

func postRegister(t *testing.T, handler http.HandlerFunc, body string) (int, registerResponse) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/register", strings.NewReader(body))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	var resp registerResponse
	_ = json.NewDecoder(w.Body).Decode(&resp)
	return w.Code, resp
}

// An anonymous request naming an existing registration must not echo the
// management token. The label and the address are both readable from public
// DNS, so anyone could otherwise ask for the token and then deregister or
// repoint the cluster. Such a request proves nothing: it refreshes no
// inactivity clock and moves no address.
func TestAnonymousRegisterDoesNotDiscloseToken(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)

	code, first := postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1"}`)
	if code != http.StatusCreated || first.Token == "" {
		t.Fatalf("new registration should return 201 with a token, got %d token=%q", code, first.Token)
	}

	code, second := postRegister(t, handler, `{"subdomain":"myapp","ip":"198.51.100.1"}`)
	if code != http.StatusCreated {
		t.Fatalf("an anonymous request for an existing registration should return 201, got %d", code)
	}
	if second.Token != "" {
		t.Errorf("an anonymous request must not disclose the token, got %q", second.Token)
	}
}

func TestRegisterRejectsNonPublicIP(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)
	for _, ip := range []string{
		"127.0.0.1", "::1", // loopback
		"169.254.169.254", "fe80::1", // link-local incl. cloud metadata
		"10.0.0.1", "192.168.1.5", "172.16.0.1", // private
		"0.0.0.0",       // unspecified
		"224.0.0.1",     // multicast
		"not-an-ip", "", // malformed
	} {
		body := fmt.Sprintf(`{"subdomain":"app","ip":%q}`, ip)
		code, _ := postRegister(t, handler, body)
		if code != http.StatusBadRequest {
			t.Errorf("ip %q: expected 400, got %d", ip, code)
		}
	}
}

func TestRegisterRejectsReservedLabels(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)
	// Iterate the shared reserved set so every reserved label the CRD forbids is
	// also proven rejected at registration, and the two can't drift.
	for sub := range hostnames.ReservedLabels {
		body := fmt.Sprintf(`{"subdomain":%q,"ip":"198.51.100.1"}`, sub)
		code, _ := postRegister(t, handler, body)
		if code != http.StatusConflict {
			t.Errorf("reserved subdomain %q: expected 409, got %d", sub, code)
		}
	}
}

// Derived routes (console--<cluster>, dex--<cluster>, <app>--<cluster>) join
// prefix and cluster with a double dash, so no registered label may contain
// "--". Otherwise an attacker could register the exact derived hostname of a
// victim's console, Dex, or app URL — in either registration order.
func TestRegisterRejectsDoubleDashLabels(t *testing.T) {
	for _, sub := range []string{
		"console--203-0-113-12", "dex--203-0-113-12", "myapp--victim",
		"a--b--c", "xn--punycode",
	} {
		handler := handleRegister(registry.New(), "kipper.run", neverObserve)
		body := fmt.Sprintf(`{"subdomain":%q,"ip":"198.51.100.9"}`, sub)
		code, _ := postRegister(t, handler, body)
		if code != http.StatusBadRequest {
			t.Errorf("double-dash subdomain %q: expected 400, got %d", sub, code)
		}
	}
}

// Single-dash labels that merely look like the old service prefixes are plain
// cluster names under the double-dash scheme and must register normally.
func TestRegisterAllowsSingleDashServiceLookalikes(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)
	for i, sub := range []string{"console-tools", "dex-lab", "myapp-staging"} {
		body := fmt.Sprintf(`{"subdomain":%q,"ip":"198.51.100.%d"}`, sub, 20+i)
		code, resp := postRegister(t, handler, body)
		if code != http.StatusCreated || resp.Token == "" {
			t.Errorf("single-dash subdomain %q: expected 201 with token, got %d token=%q", sub, code, resp.Token)
		}
	}
}

// A legitimate hyphenated base subdomain must register even when a shorter suffix
// component is already taken by another IP. The previous suffix-matching check
// wrongly rejected these.
func TestRegisterAllowsHyphenatedBaseSubdomain(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)

	code, _ := postRegister(t, handler, `{"subdomain":"12","ip":"203.0.113.99"}`)
	if code != http.StatusCreated {
		t.Fatalf("registering short name failed: %d", code)
	}
	code, resp := postRegister(t, handler, `{"subdomain":"203-0-113-12","ip":"203.0.113.12"}`)
	if code != http.StatusCreated || resp.Token == "" {
		t.Errorf("legitimate base subdomain wrongly rejected: got %d token=%q", code, resp.Token)
	}
}

func TestIsPublicIP(t *testing.T) {
	for _, ip := range []string{"198.51.100.1", "203.0.113.5", "8.8.8.8", "2606:4700:4700::1111"} {
		if !isPublicIP(ip) {
			t.Errorf("%s should be treated as public", ip)
		}
	}
	for _, ip := range []string{
		"127.0.0.1", "::1", "10.0.0.1", "192.168.0.1", "172.16.5.5",
		"169.254.169.254", "fe80::1", "0.0.0.0", "224.0.0.1", "", "garbage",
		"100.64.0.1", "100.127.255.255", // CGNAT
		"198.18.0.1",      // benchmarking
		"::ffff:10.0.0.1", // IPv4-mapped private
		"0.1.2.3",         // this-network, can alias local
		"::127.0.0.1",     // IPv4-compatible IPv6 (deprecated)
		"64:ff9b::7f00:1", // NAT64 wrapping loopback
	} {
		if isPublicIP(ip) {
			t.Errorf("%s should not be treated as public", ip)
		}
	}
}

// A label shaped like an address belongs to that address. Without this, anyone
// can hold the default name of a server they do not run: the operator who later
// installs on 203.0.113.77 finds their name taken, and until they do, every link
// under it points wherever the squatter chose. The name is only worth taking now
// that a chosen label is a documented install option.
func TestRegisterRefusesAnIPShapedLabelFromAnotherAddress(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)

	code, _ := postRegister(t, handler, `{"subdomain":"203-0-113-77","ip":"198.51.100.1"}`)
	if code != http.StatusConflict {
		t.Errorf("registering another server's default name: got %d, want 409", code)
	}
}

func TestRegisterAllowsAnIPShapedLabelFromItsOwnAddress(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)

	code, resp := postRegister(t, handler, `{"subdomain":"203-0-113-77","ip":"203.0.113.77"}`)
	if code != http.StatusCreated || resp.Token == "" {
		t.Errorf("a server registering its own default name: got %d token=%q, want 201 with a token", code, resp.Token)
	}
}

// The guard claims only labels that are somebody's default name. An ordinary
// name that merely contains digits and hyphens stays registrable, or the guard
// would quietly deny names it was never meant to cover.
func TestRegisterAllowsLabelsThatOnlyLookNumeric(t *testing.T) {
	handler := handleRegister(registry.New(), "kipper.run", neverObserve)

	for _, body := range []string{
		`{"subdomain":"999-0-0-1","ip":"198.51.100.1"}`,
		`{"subdomain":"203-0-113","ip":"198.51.100.2"}`,
		`{"subdomain":"12","ip":"198.51.100.3"}`,
		`{"subdomain":"acme-2","ip":"198.51.100.4"}`,
	} {
		code, _ := postRegister(t, handler, body)
		if code != http.StatusCreated {
			t.Errorf("%s: got %d, want 201", body, code)
		}
	}
}

// The label rule startup re-applies, expressed through prunableEntry: a name
// nothing ever served under and which the current policy refuses is released.
func TestStartupPruneAppliesTheLabelRule(t *testing.T) {
	refused := map[string]string{
		"a label reserved only by the newer policy": "login",
		"a long-standing reserved label":            "console",
		"a route-shadowing label":                   "console--acme",
		"a malformed label":                         "UPPER",
	}
	for why, label := range refused {
		if !prunableEntry(&registry.Entry{Subdomain: label, IP: "203.0.113.1"}) {
			t.Errorf("prunableEntry(%q) = false, want true (%s)", label, why)
		}
	}

	kept := map[string]struct{ subdomain, ip string }{
		"an ordinary name":              {"acme", "203.0.113.2"},
		"a server's own default name":   {"203-0-113-78", "203.0.113.78"},
		"a merely numeric-looking name": {"999-0-0-1", "198.51.100.4"},
		// A cluster that moved servers keeps the name its links were published
		// under, so its label stops matching its address. The address guard is
		// reported rather than enforced here for exactly that reason.
		"a cluster that moved to a new server": {"203-0-113-10", "198.51.100.5"},
	}
	for why, e := range kept {
		if prunableEntry(&registry.Entry{Subdomain: e.subdomain, IP: e.ip}) {
			t.Errorf("prunableEntry(%q, %q) = true, want false (%s)", e.subdomain, e.ip, why)
		}
	}
}

// Reserving a name must not evict a cluster already serving under it. Expanding
// the reserved list is a rule for new claims; applying it retroactively to a live
// registration deletes its token, takes it off the air on a restart it did not
// ask for, and breaks the promise that a minor upgrade leaves a working cluster
// working. A name nobody ever served is a different matter: nothing is running,
// so the reservation takes effect.
func TestStartupPruneSparesALabelThatHasServed(t *testing.T) {
	served := &registry.Entry{Subdomain: "docs", IP: "203.0.113.1", FirstProvenAt: time.Now().Add(-72 * time.Hour)}
	if prunableEntry(served) {
		t.Error("a reserved-now label that has served must be kept, not deleted")
	}
	if !grandfatheredEntry(served) {
		t.Error("it must be reported so an operator can deal with it deliberately")
	}

	neverServed := &registry.Entry{Subdomain: "docs", IP: "203.0.113.1"}
	if !prunableEntry(neverServed) {
		t.Error("a reserved-now label that never served must be released")
	}

	// An ordinary name is untouched either way.
	fine := &registry.Entry{Subdomain: "acme", IP: "203.0.113.2", FirstProvenAt: time.Now()}
	if prunableEntry(fine) || grandfatheredEntry(fine) {
		t.Error("a registrable label must be neither pruned nor reported")
	}

	// A route-shadowing label is malformed rather than merely reserved, and could
	// never have been claimed through a supported path.
	shadow := &registry.Entry{Subdomain: "console--acme", IP: "203.0.113.3"}
	if !prunableEntry(shadow) {
		t.Error("a route-shadowing label must still be dropped")
	}
}

// The reserved and address guards decide who may CREATE a name. Applied to an
// authenticated renewal they starve a registration the gateway has already
// decided to keep: startup grandfathers a proven cluster on a now-reserved label
// and logs that a restart will not take it away, while every heartbeat gets a 409,
// its proof lease dies within the week and the name lapses anyway. Same for a
// cluster that moved servers keeping its address-derived name.
func TestGuardsDoNotRefuseAnAuthenticatedRenewal(t *testing.T) {
	reg := registry.New()
	handler := handleRegister(reg, "kipper.run", neverObserve)

	// A name that was registrable when it was claimed.
	entry, _, err := reg.Register("203-0-113-77", "203.0.113.77", "")
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}

	// The server moves. Its label now spells an address it no longer points at,
	// which is precisely the case startup reports and keeps.
	body := `{"subdomain":"203-0-113-77","ip":"198.51.100.9","token":"` + entry.Token + `"}`
	if code, _ := postRegister(t, handler, body); code == http.StatusConflict {
		t.Error("an authenticated move of an address-derived name must not be refused")
	}

	// And a label the current policy reserves, held from before it was. Register
	// is the registry API, which carries no reserved list, so this is exactly the
	// state a grandfathered entry loads in.
	held, _, err := reg.Register("docs", "203.0.113.5", "")
	if err != nil {
		t.Fatalf("seeding: %v", err)
	}
	renewal := `{"subdomain":"docs","ip":"203.0.113.5","token":"` + held.Token + `"}`
	if code, _ := postRegister(t, handler, renewal); code == http.StatusConflict {
		t.Error("an authenticated renewal of a now-reserved label must not be refused")
	}

	// A stranger still cannot take either one.
	if code, _ := postRegister(t, handler, `{"subdomain":"203-0-113-77","ip":"198.51.100.20"}`); code != http.StatusConflict {
		t.Errorf("an anonymous claim of another server's name: got %d, want 409", code)
	}
}
