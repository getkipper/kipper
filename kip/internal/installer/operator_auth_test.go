package installer

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"
)

type authnConfigDoc struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	JWT        []struct {
		Issuer struct {
			URL                 string   `json:"url"`
			DiscoveryURL        string   `json:"discoveryURL"`
			Audiences           []string `json:"audiences"`
			AudienceMatchPolicy string   `json:"audienceMatchPolicy"`
		} `json:"issuer"`
		ClaimMappings struct {
			Username struct {
				Claim  string  `json:"claim"`
				Prefix *string `json:"prefix"`
			} `json:"username"`
			Groups struct {
				Claim  string  `json:"claim"`
				Prefix *string `json:"prefix"`
			} `json:"groups"`
			UID struct {
				Claim string `json:"claim"`
			} `json:"uid"`
		} `json:"claimMappings"`
		ClaimValidationRules []struct {
			Expression string `json:"expression"`
			Message    string `json:"message"`
		} `json:"claimValidationRules"`
	} `json:"jwt"`
}

func TestAuthnConfigStubIsValidAndEmpty(t *testing.T) {
	var doc authnConfigDoc
	require.NoError(t, yaml.Unmarshal([]byte(authnConfigStub), &doc))
	assert.Equal(t, "apiserver.config.k8s.io/v1", doc.APIVersion)
	assert.Equal(t, "AuthenticationConfiguration", doc.Kind)
	// Zero authenticators is the boot state upstream explicitly supports;
	// anything more here would point the API server at an issuer that does
	// not exist yet.
	assert.Empty(t, doc.JWT)
}

func TestRenderAuthnConfig(t *testing.T) {
	var doc authnConfigDoc
	require.NoError(t, yaml.Unmarshal([]byte(renderAuthnConfig("", "dex.cluster.example.com")), &doc))

	require.Len(t, doc.JWT, 1)
	jwt := doc.JWT[0]
	assert.Equal(t, "https://dex.cluster.example.com/dex", jwt.Issuer.URL)
	assert.Empty(t, jwt.Issuer.DiscoveryURL,
		"the loopback hosts pin serves discovery and JWKS through the issuer URL; a discoveryURL would not cover the JWKS fetch anyway (jwks_uri is used exactly as the discovery document returns it)")
	assert.Equal(t, []string{"kipper-cli"}, jwt.Issuer.Audiences,
		"only tokens minted for the CLI client may authenticate to the API server")

	// The prefixes are what confine OIDC identities to the namespace RBAC
	// binds; they must match the staged subjects exactly.
	require.NotNil(t, jwt.ClaimMappings.Username.Prefix)
	assert.Equal(t, oidcUsernamePrefix, *jwt.ClaimMappings.Username.Prefix)
	assert.Equal(t, "email", jwt.ClaimMappings.Username.Claim)
	require.NotNil(t, jwt.ClaimMappings.Groups.Prefix)
	assert.Equal(t, oidcGroupsPrefix, *jwt.ClaimMappings.Groups.Prefix)
	assert.Equal(t, "groups", jwt.ClaimMappings.Groups.Claim)
	assert.Equal(t, "sub", jwt.ClaimMappings.UID.Claim,
		"audit entries key on the stable subject, surviving email changes")

	require.Len(t, jwt.ClaimValidationRules, 1)
	assert.Contains(t, jwt.ClaimValidationRules[0].Expression, "email_verified")
}

func TestAuditPolicyAttributesWithoutPayloads(t *testing.T) {
	var policy struct {
		APIVersion string   `json:"apiVersion"`
		OmitStages []string `json:"omitStages"`
		Rules      []struct {
			Level string `json:"level"`
		} `json:"rules"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(auditPolicy), &policy))
	assert.Equal(t, "audit.k8s.io/v1", policy.APIVersion)

	require.NotEmpty(t, policy.Rules)
	for _, rule := range policy.Rules {
		// Metadata gives who/what/when; Request and RequestResponse would
		// copy secret payloads into the log.
		assert.Contains(t, []string{"None", "Metadata"}, rule.Level)
	}
	assert.Equal(t, "Metadata", policy.Rules[len(policy.Rules)-1].Level,
		"the catch-all must attribute, not drop")
}

func TestK3sConfigWiresAuthnAndAuditFiles(t *testing.T) {
	// The flags must reference exactly the files the installer writes; a
	// path drifting apart here means an apiserver that refuses to boot.
	assert.Contains(t, k3sConfig, "authentication-config="+authnConfigPath)
	assert.Contains(t, k3sConfig, "audit-policy-file="+auditPolicyPath)
	assert.Contains(t, k3sConfig, "audit-log-path=")
}

func TestAuthnStubOnlyWrittenWhenAbsent(t *testing.T) {
	// A re-run of InstallK3s over a live cluster must never clobber the
	// active authenticator config back to the stub — that would silently
	// turn off OIDC authentication.
	assert.True(t, strings.HasPrefix(authnStubWriteCmd(), "[ -f "+authnConfigPath+" ] ||"))
	assert.Contains(t, authnStubWriteCmd(), authnConfigStub)
}

func TestRenderAuthnConfigRejectsNothingSilently(t *testing.T) {
	// The rendered config must contain no unprefixed identity mapping: a
	// missing prefix line would hand OIDC subjects the raw namespace.
	rendered := renderAuthnConfig("", "dex.example.com")
	assert.NotContains(t, strings.ReplaceAll(rendered, " ", ""), `prefix:""`)
}

func TestRenderAuthnConfigSupportsIssuerTransition(t *testing.T) {
	// A domain cutover runs old and new issuers side by side so live
	// sessions drain instead of dying at the cut; this is the primitive the
	// transition machinery consumes.
	var doc authnConfigDoc
	require.NoError(t, yaml.Unmarshal([]byte(renderAuthnConfig("", "dex.old.example.com", "dex.new.example.com")), &doc))
	require.Len(t, doc.JWT, 2)
	assert.Equal(t, "https://dex.old.example.com/dex", doc.JWT[0].Issuer.URL)
	assert.Equal(t, "https://dex.new.example.com/dex", doc.JWT[1].Issuer.URL)
	for _, jwt := range doc.JWT {
		require.NotNil(t, jwt.ClaimMappings.Username.Prefix)
		assert.Equal(t, oidcUsernamePrefix, *jwt.ClaimMappings.Username.Prefix)
	}
}

func TestAuditPolicyAttributesSystemWrites(t *testing.T) {
	var policy struct {
		Rules []struct {
			Level      string   `json:"level"`
			Verbs      []string `json:"verbs"`
			UserGroups []string `json:"userGroups"`
		} `json:"rules"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(auditPolicy), &policy))

	// The None rule covering nodes and kube-system service accounts must be
	// verb-scoped to reads: a blanket drop would let a stolen kube-system
	// ServiceAccount token act with zero audit trail.
	for _, rule := range policy.Rules {
		if rule.Level != "None" {
			continue
		}
		for _, g := range rule.UserGroups {
			if g == "system:serviceaccounts:kube-system" || g == "system:nodes" {
				assert.ElementsMatch(t, []string{"get", "list", "watch"}, rule.Verbs,
					"drops for %s must cover reads only", g)
			}
		}
	}
}

// The probe is a gate only if it verifies the way the API server will, and the
// two host classes are served differently. A gateway-fronted host gets the
// cluster's own hop certificate, which no public authority signed; verifying it
// against the system trust store is what deadlocked a fresh install, and the
// error blamed cert-manager, which never issues for these hosts.
func TestProbeVerifiesEachHostClassTheWayTheAPIServerWill(t *testing.T) {
	const ca = "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----"

	gateway, err := probeCommand("dex--159-195-217-19.kipper.run", ca)
	if err != nil {
		t.Fatalf("gateway-fronted host: %v", err)
	}
	if !strings.Contains(gateway, `--cacert "$tmp"`) {
		t.Errorf("a gateway-fronted host must be verified against the cluster CA, got: %s", gateway)
	}
	// The anchor comes from stdin, not from the file, so the probe attests to
	// the bytes the caller is about to install rather than to whatever the
	// path holds by the time curl runs.
	if strings.Contains(gateway, hopCAPath) {
		t.Errorf("the probe must verify the caller's anchor, not re-read the mutable path, got: %s", gateway)
	}
	if !probeNeedsAnchor("dex--159-195-217-19.kipper.run") {
		t.Error("a gateway-fronted host must be told to expect the anchor on stdin")
	}
	if probeNeedsAnchor("dex.example.com") {
		t.Error("a custom domain must not be sent an anchor it does not verify against")
	}
	if !strings.Contains(gateway, "trap 'rm -f") {
		t.Errorf("the temporary anchor must be removed however the probe exits, got: %s", gateway)
	}

	custom, err := probeCommand("dex.example.com", ca)
	if err != nil {
		t.Fatalf("custom domain: %v", err)
	}
	if strings.Contains(custom, "--cacert") {
		t.Errorf("a custom domain must keep verifying against the system trust store, got: %s", custom)
	}
	if !strings.Contains(custom, "curl -fsS") {
		t.Errorf("the custom-domain certificate gate must still fire, got: %s", custom)
	}

	// Both keep retrying: a fresh cluster reaches here as the certificate comes up.
	for _, cmd := range []string{gateway, custom} {
		if !strings.Contains(cmd, "seq 1 24") {
			t.Errorf("the probe must retry rather than fire once: %s", cmd)
		}
		if !strings.Contains(cmd, `grep -q '"issuer"'`) {
			t.Errorf("the probe must confirm a discovery document, not just a 200: %s", cmd)
		}
	}
}

// Without an anchor a gateway-fronted host cannot be verified by anything, so
// the step must say that rather than probe with the system trust store and
// report a certificate error that names the wrong cause.
func TestGatewayHostWithoutAnAnchorIsRefusedClearly(t *testing.T) {
	_, err := probeCommand("dex--159-195-217-19.kipper.run", "")
	if err == nil {
		t.Fatal("a gateway-fronted host with no cluster CA must be refused")
	}
	if !strings.Contains(err.Error(), hopCAPath) {
		t.Errorf("the error must name the missing anchor, got %v", err)
	}
	// A custom domain needs no anchor and must not be refused for lacking one.
	if _, err := probeCommand("dex.example.com", ""); err != nil {
		t.Errorf("a custom domain must not require a cluster CA: %v", err)
	}
}

// The error text has to point at the thing that is actually wrong. Blaming
// cert-manager for a host it never issues for is what sent the original
// diagnosis down the wrong path for an afternoon.
func TestProbeFailureNamesTheRightCause(t *testing.T) {
	if hint := probeHint("dex--acme.kipper.run"); strings.Contains(hint, "cert-manager") {
		t.Errorf("a gateway-fronted host must not blame cert-manager: %s", hint)
	}
	if hint := probeHint("dex.example.com"); !strings.Contains(hint, "cert-manager") {
		t.Errorf("a custom domain should still point at cert-manager: %s", hint)
	}
}

// The fenced write is the shell that installs the API server's authentication
// config on every install, not only during a replacement, so its behaviour is
// exercised against real files rather than asserted as a string.
//
// The fence exists because a config rendered from a superseded anchor puts the
// API server on an authority the cluster no longer serves, which is operator
// lockout.
func TestFencedWriteInstallsOnlyWhileTheAnchorIsTheOneVerified(t *testing.T) {
	const anchor = "-----BEGIN CERTIFICATE-----\nQUJDREVGR0hJSktMTU5PUFFSU1RVVldYWVphYmNkZWZnaGlqa2xtbm9wcXJzdHV2\n-----END CERTIFICATE-----"
	const config = "apiVersion: apiserver.config.k8s.io/v1\nkind: AuthenticationConfiguration\njwt:\n  - issuer:\n      url: https://demo.kipper.run/dex\n"

	run := func(t *testing.T, onDisk string) (string, string, error) {
		t.Helper()
		dir := t.TempDir()
		anchorPath := filepath.Join(dir, "kipper-hop-ca.crt")
		configPath := filepath.Join(dir, "authn-config.yaml")
		if err := os.WriteFile(anchorPath, []byte(onDisk+"\n"), 0o600); err != nil {
			t.Fatalf("seeding the anchor: %v", err)
		}
		if err := os.WriteFile(configPath, []byte("previous config\n"), 0o600); err != nil {
			t.Fatalf("seeding the config: %v", err)
		}
		out, err := exec.Command("bash", "-c", fencedWriteScript(anchorPath, configPath, config, anchor)).CombinedOutput()
		written, rerr := os.ReadFile(configPath)
		if rerr != nil {
			t.Fatalf("reading the config back: %v", rerr)
		}
		return string(out), string(written), err
	}

	t.Run("the anchor is the one that was verified", func(t *testing.T) {
		out, written, err := run(t, anchor)
		if err != nil {
			t.Fatalf("the write must succeed, got %v (%s)", err, out)
		}
		if written != config {
			t.Errorf("config = %q, want the rendered config", written)
		}
	})

	t.Run("the anchor moved after it was verified", func(t *testing.T) {
		out, written, err := run(t, "-----BEGIN CERTIFICATE-----\nc29tZXRoaW5nIGVsc2UgZW50aXJlbHkgd2l0aCBhIGRpZmZlcmVudCBhdXRob3Jp\n-----END CERTIFICATE-----")
		if err == nil {
			t.Fatal("a moved anchor must refuse the write")
		}
		if !strings.Contains(out, anchorChangedMarker) {
			t.Errorf("the refusal must be distinguishable from any other failure, got: %s", out)
		}
		if written != "previous config\n" {
			t.Errorf("config = %q, want the previous one untouched", written)
		}
	})

	// The anchor is compared with every whitespace character removed, on both
	// sides. Comparing raw bytes made an anchor that began with a blank line
	// unequal to itself: Go trimmed both ends, the shell trimmed trailing
	// newlines only, and every sync refused for good while blaming a concurrent
	// writer that did not exist.
	t.Run("the anchor differs only in whitespace", func(t *testing.T) {
		dir := t.TempDir()
		anchorPath := filepath.Join(dir, "kipper-hop-ca.crt")
		configPath := filepath.Join(dir, "authn-config.yaml")
		if err := os.WriteFile(anchorPath, []byte("\n\n"+anchor+"   \n\n"), 0o600); err != nil {
			t.Fatalf("seeding the anchor: %v", err)
		}
		out, err := exec.Command("bash", "-c", fencedWriteScript(anchorPath, configPath, config, anchor)).CombinedOutput()
		if err != nil {
			t.Fatalf("whitespace alone must not read as a changed anchor, got %v (%s)", err, out)
		}
		written, rerr := os.ReadFile(configPath)
		if rerr != nil {
			t.Fatalf("reading the config back: %v", rerr)
		}
		if string(written) != config {
			t.Errorf("config = %q, want the rendered config", written)
		}
	})

	// Every install, cutover and sync on a node stages through this path. A
	// fixed name means two of them truncate each other's file and one renames
	// the other's bytes into place.
	t.Run("concurrent writers do not share a staging file", func(t *testing.T) {
		dir := t.TempDir()
		anchorPath := filepath.Join(dir, "kipper-hop-ca.crt")
		configPath := filepath.Join(dir, "authn-config.yaml")
		if err := os.WriteFile(anchorPath, []byte(anchor+"\n"), 0o600); err != nil {
			t.Fatalf("seeding the anchor: %v", err)
		}
		script := fencedWriteScript(anchorPath, configPath, config, anchor)
		if strings.Contains(script, configPath+".kipper-tmp") {
			t.Error("the staging file is a fixed name, so two writers collide on it")
		}
		if !strings.Contains(script, "mktemp") {
			t.Error("the staging file must be unique per run")
		}
		if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("the write must still succeed: %v (%s)", err, out)
		}
		leftovers, _ := filepath.Glob(configPath + ".kipper-*")
		if len(leftovers) != 0 {
			t.Errorf("staging files left behind: %v", leftovers)
		}
	})

	// A cluster serving only a custom domain has no anchor and needs none. The
	// fence must let that install through rather than reading "no file" as "the
	// anchor moved".
	t.Run("a cluster with no anchor at all", func(t *testing.T) {
		dir := t.TempDir()
		anchorPath := filepath.Join(dir, "kipper-hop-ca.crt")
		configPath := filepath.Join(dir, "authn-config.yaml")
		out, err := exec.Command("bash", "-c", fencedWriteScript(anchorPath, configPath, config, "")).CombinedOutput()
		if err != nil {
			t.Fatalf("an anchorless cluster must still install its config, got %v (%s)", err, out)
		}
		written, rerr := os.ReadFile(configPath)
		if rerr != nil {
			t.Fatalf("reading the config back: %v", rerr)
		}
		if string(written) != config {
			t.Errorf("config = %q, want the rendered config", written)
		}
	})

	// A refusal must leave nothing half-written for the next run to trip over.
	t.Run("no staging file is left behind", func(t *testing.T) {
		dir := t.TempDir()
		anchorPath := filepath.Join(dir, "kipper-hop-ca.crt")
		configPath := filepath.Join(dir, "authn-config.yaml")
		if err := os.WriteFile(anchorPath, []byte("something else\n"), 0o600); err != nil {
			t.Fatalf("seeding the anchor: %v", err)
		}
		_ = exec.Command("bash", "-c", fencedWriteScript(anchorPath, configPath, config, anchor)).Run()
		if _, err := os.Stat(configPath + ".kipper-tmp"); !os.IsNotExist(err) {
			t.Error("a refused write left its staging file behind")
		}
	})
}

// The anchor is a file an operator edits by hand during a replacement, and its
// contents reach a script running as root. Nothing in it may ever be
// interpreted: a line that happened to match a heredoc delimiter used to end
// the document and run whatever followed.
func TestNoAnchorContentCanBeInterpretedByTheShell(t *testing.T) {
	dir := t.TempDir()
	anchorPath := filepath.Join(dir, "kipper-hop-ca.crt")
	configPath := filepath.Join(dir, "authn-config.yaml")
	proof := filepath.Join(dir, "executed")

	// An anchor that closes the old heredoc and runs a command.
	hostile := "-----BEGIN CERTIFICATE-----\nQUJD\n-----END CERTIFICATE-----\nKIPCAEOF\ntouch " + proof + "\n"
	if err := os.WriteFile(anchorPath, []byte(hostile), 0o600); err != nil {
		t.Fatalf("seeding the anchor: %v", err)
	}

	// The same content as the probed value, so the comparison matches and the
	// script runs as far as it can.
	script := fencedWriteScript(anchorPath, configPath, "config: body\n", hostile)
	if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("the write should succeed: %v (%s)", err, out)
	}
	if _, err := os.Stat(proof); !os.IsNotExist(err) {
		t.Fatal("anchor content executed as a command")
	}
	written, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("reading the config back: %v", err)
	}
	if string(written) != "config: body\n" {
		t.Errorf("config = %q, want it written verbatim", written)
	}

	// The rendered config is caller-supplied too, and reaches the same script.
	hostileConfig := "config: body\nKIPEOF\ntouch " + proof + "\n"
	script = fencedWriteScript(anchorPath, configPath, hostileConfig, hostile)
	if out, err := exec.Command("bash", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("the write should succeed: %v (%s)", err, out)
	}
	if _, err := os.Stat(proof); !os.IsNotExist(err) {
		t.Fatal("config content executed as a command")
	}
	if written, _ := os.ReadFile(configPath); string(written) != hostileConfig {
		t.Errorf("config = %q, want it written verbatim", written)
	}
}
