package installer

import (
	"strings"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/hopca"
)

// A read that fails must never look like a cluster that has no authority. The
// API server is handed this exact anchor and nothing in the cluster can give it
// another, so minting a second one over an established CA breaks operator
// authentication until someone repairs it by hand.
func TestSecretReadDistinguishesAbsenceFromFailure(t *testing.T) {
	cmd := readSecretKeyCmd(hopCASecret, "tls.crt")

	if !strings.Contains(cmd, "--ignore-not-found") {
		t.Error("an absent Secret must be an empty success, not an error")
	}
	if strings.Contains(cmd, "|| true") || strings.Contains(cmd, "2>/dev/null") {
		t.Error("a genuine read failure must surface; swallowing it reads as a fresh cluster and mints a second CA")
	}
	if !strings.Contains(cmd, "set -o pipefail") {
		t.Error("without pipefail a failing kubectl is masked by the exit status of base64")
	}
	if !strings.Contains(cmd, `{.data.tls\.crt}`) {
		t.Errorf("the jsonpath key must be escaped, got: %s", cmd)
	}
}

// The authority is created, never applied. Even a misjudged absence then loses
// the race to whoever got there first instead of overwriting their anchor.
func TestAuthorityIsCreatedAndTheLeafIsUpserted(t *testing.T) {
	created := tlsSecretCmd(hopCASecret, false)
	applied := tlsSecretCmd(hopCertSecret, true)

	if strings.Contains(created, "kubectl apply") {
		t.Error("the authority must be created, not applied: an apply overwrites an established anchor")
	}
	if !strings.Contains(created, "create secret tls "+hopCASecret) {
		t.Errorf("the authority must be created as a TLS secret, got: %s", created)
	}
	// The leaf is meant to be replaced — that is how adoption and reissue land.
	if !strings.Contains(applied, "kubectl apply") {
		t.Errorf("the hop certificate must upsert so adoption can replace it, got: %s", applied)
	}
}

// Key material goes through stdin, never the command line, so it cannot reach
// the process table or an error message, and the temp copy is removed however
// the command exits.
func TestSecretWriteKeepsKeyMaterialOffTheCommandLine(t *testing.T) {
	cmd := tlsSecretCmd(hopCertSecret, true)

	// The builder takes only a name, so material cannot reach the command by
	// construction. What it must also do is read from the files awk wrote rather
	// than from anything interpolated.
	if !strings.Contains(cmd, `--cert="$tmp/tls.crt"`) || !strings.Contains(cmd, `--key="$tmp/tls.key"`) {
		t.Errorf("kubectl must read the material from files fed by stdin, got: %s", cmd)
	}
	if !strings.Contains(cmd, "mktemp -d") || !strings.Contains(cmd, "trap 'rm -rf") {
		t.Error("the temp copy must be removed however the command exits")
	}
	if !strings.Contains(cmd, "set -o pipefail") {
		t.Error("a failure in the pipeline must fail the command")
	}
}

// The default store is what makes Traefik serve the hop certificate at all: a
// gateway-fronted Ingress carries no secretName, so without it Traefik answers
// with its own generated certificate, which nothing can verify or pin.
func TestTLSStorePointsAtTheHopCertificate(t *testing.T) {
	manifest := applyTLSStoreCmd()
	for _, want := range []string{
		"kind: TLSStore",
		"name: default",
		"namespace: " + hopNamespace,
		"secretName: " + hopCertSecret,
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("the default TLSStore must contain %q", want)
		}
	}
}

// The anchor lives under /etc/rancher/k3s so uninstall's removal of that tree
// takes it too, rather than leaving one behind for the next install to inherit.
func TestAnchorLivesWhereUninstallSweepsIt(t *testing.T) {
	if !strings.HasPrefix(hopCAPath, "/etc/rancher/k3s/") {
		t.Errorf("the anchor at %s would survive an uninstall", hopCAPath)
	}
	write := writeFileCmd(hopCAPath, "0644")
	if !strings.Contains(write, "mv ") {
		t.Error("the anchor must be written atomically, or a crash leaves a partial certificate the API server cannot parse")
	}
}

// The anchor is only ever added to. Its contents are trust decisions someone
// made — widened by hand partway through replacing an authority, narrowed by
// hand at the end — and an install re-run must not reverse either. Rebuilding
// it from the Secret reversed both directions: it dropped an incoming authority
// at the expanded phase and re-added an outgoing one at the narrowed phase,
// rewinding a replacement the documentation says is safe to leave part-done.
func TestAnInstallReRunNeverNarrowsTheAnchor(t *testing.T) {
	caA, _, _, _ := authorityFixture(t)
	caB, _, _, _ := authorityFixture(t)
	both := string(hopca.Bundle([]byte(caA), []byte(caB)))

	tests := []struct {
		name     string
		anchor   string
		active   string
		rewrites bool
	}{
		{
			name:   "expanded: an incoming authority the operator widened trust to",
			anchor: both, active: caA, rewrites: false,
		},
		{
			name:   "narrowed: an outgoing authority the operator narrowed away from",
			anchor: caB, active: caB, rewrites: false,
		},
		{
			name:   "steady: nothing to do",
			anchor: caA, active: caA, rewrites: false,
		},
		{
			name:   "the anchor does not name the signer at all",
			anchor: caA, active: caB, rewrites: true,
		},
		{
			name:   "a cluster with no anchor yet",
			anchor: "", active: caA, rewrites: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := anchorContains(tt.anchor, tt.active); got == tt.rewrites {
				t.Fatalf("anchorContains = %v, so the anchor would %sbe rewritten", got, map[bool]string{true: "not ", false: ""}[got])
			}
			if !tt.rewrites {
				return
			}
			// When it does rewrite, everything already trusted must survive.
			rewritten := string(hopca.Bundle([]byte(tt.active), []byte(tt.anchor)))
			if !anchorContains(rewritten, tt.active) {
				t.Error("the rewritten anchor does not name the active signer")
			}
			if tt.anchor != "" && !anchorContains(rewritten, tt.anchor) {
				t.Error("the rewritten anchor dropped an authority that was already trusted")
			}
		})
	}
}

// The retained-authority key name is one contract across two modules that
// cannot import each other: the installer builds the host anchor from it and
// console-api renders the same bundle from the Secret to gate a cutover. It now
// has one definition in the module both depend on, so drift is a compile error
// rather than something a test has to catch. This pins the value, since changing
// it silently orphans the key on every cluster that already has one.
func TestRetainedAuthorityKeyIsTheSharedOne(t *testing.T) {
	if retainedCAKey != hopca.RetainedCAKey {
		t.Errorf("the installer's retained-anchor key is %q, not the shared %q", retainedCAKey, hopca.RetainedCAKey)
	}
	if hopca.RetainedCAKey != "previous.crt" {
		t.Errorf("the shared retained-anchor key is now %q; every cluster holding the old name orphans it", hopca.RetainedCAKey)
	}
}
