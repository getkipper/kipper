package installer

import (
	"encoding/base64"
	"fmt"
	"io"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/hopca"
	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

// fakeCluster stands in for a node's Secrets and files so the trust-material
// path can be driven end to end.
//
// It refuses any command it does not recognise rather than returning empty.
// A permissive fake would let the code under test drift into sending something
// else entirely and still pass, which is the failure this whole file exists to
// close.
type fakeCluster struct {
	t        *testing.T
	files    map[string]string
	secrets  map[string]map[string]string
	commands []string
	// Keep the copy separate because both namespaces use the same Secret name.
	traefikCopy       map[string]string
	copyVersion       int
	traefikAnnotation string
	restarts          int
	rolloutWaits      int
	// Mimic kubectl errors containing the object, including plain and base64 key material.
	failWritesEchoingInput bool
}

func newFakeCluster(t *testing.T) *fakeCluster {
	t.Helper()
	return &fakeCluster{
		t:       t,
		files:   map[string]string{},
		secrets: map[string]map[string]string{},
	}
}

var (
	readSecretKeyPattern   = regexp.MustCompile(`get secret (\S+) --ignore-not-found -o jsonpath='\{\.data\.(\S+)\}'`)
	readFilePattern        = regexp.MustCompile(`^if \[ ! -e (\S+) \] && \[ ! -L \S+ \]; then exit 0; fi; cat \S+$`)
	writeFilePattern       = regexp.MustCompile(`^cat > (\S+)\.kipper-tmp && chmod \S+ \S+\.kipper-tmp && mv \S+\.kipper-tmp (\S+)$`)
	createSecretPattern    = regexp.MustCompile(`create secret tls (\S+) --cert=`)
	traefikCopyPattern     = regexp.MustCompile(`-n traefik create secret generic kipper-hop-cert `)
	readCopyVersionPattern = regexp.MustCompile(`^kubectl -n traefik get secret kipper-hop-cert -o jsonpath='\{\.metadata\.resourceVersion\}'$`)
	readRestartPattern     = regexp.MustCompile(`^kubectl -n traefik get deployment traefik -o jsonpath='\{\.spec\.template\.metadata\.annotations\.kipper\\\.run/hop-copy-version\}'$`)
	restartPattern         = regexp.MustCompile(`^kubectl -n traefik patch deployment traefik --type merge -p '\{"spec":\{"template":\{"metadata":\{"annotations":\{"kipper\.run/hop-copy-version":"([0-9]+)"\}\}\}\}\}'$`)
	rolloutPattern         = regexp.MustCompile(`^kubectl -n traefik rollout status deployment/traefik --timeout=\d+s$`)
)

func (f *fakeCluster) Run(command string) (string, error) {
	f.commands = append(f.commands, command)
	switch {
	case strings.Contains(command, "create namespace"), strings.Contains(command, "kind: TLSStore"):
		return "", nil
	case readFilePattern.MatchString(command):
		m := readFilePattern.FindStringSubmatch(command)
		return f.files[m[1]], nil
	case readSecretKeyPattern.MatchString(command):
		m := readSecretKeyPattern.FindStringSubmatch(command)
		return f.secrets[m[1]][strings.ReplaceAll(m[2], `\.`, ".")], nil
	case readCopyVersionPattern.MatchString(command):
		return strconv.Itoa(f.copyVersion), nil
	case readRestartPattern.MatchString(command):
		return f.traefikAnnotation, nil
	case restartPattern.MatchString(command):
		f.traefikAnnotation = restartPattern.FindStringSubmatch(command)[1]
		f.restarts++
		return "", nil
	case rolloutPattern.MatchString(command):
		f.rolloutWaits++
		return "", nil
	}
	f.t.Fatalf("the fake cluster was sent a command it does not model: %q", command)
	return "", nil
}

func (f *fakeCluster) RunStdin(command string, stdin io.Reader) (string, error) {
	f.commands = append(f.commands, command)
	body, err := io.ReadAll(stdin)
	if err != nil {
		f.t.Fatalf("reading stdin: %v", err)
	}
	if f.failWritesEchoingInput && (traefikCopyPattern.MatchString(command) || createSecretPattern.MatchString(command)) {
		return "", fmt.Errorf("error when applying patch: %s %s", base64.StdEncoding.EncodeToString(body), body)
	}
	switch {
	case traefikCopyPattern.MatchString(command):
		cert, key := splitCertAndKey(string(body))
		written := map[string]string{"tls.crt": cert, "tls.key": key, "hop.yaml": hopYAMLIn(command)}
		if !maps.Equal(written, f.traefikCopy) {
			f.traefikCopy = written
			f.copyVersion++
		}
		return "", nil
	case writeFilePattern.MatchString(command):
		m := writeFilePattern.FindStringSubmatch(command)
		f.files[m[1]] = string(body)
		return "", nil
	case createSecretPattern.MatchString(command):
		name := createSecretPattern.FindStringSubmatch(command)[1]
		upsert := strings.Contains(command, "kubectl apply -f -")
		if _, exists := f.secrets[name]; exists && !upsert {
			return "", fmt.Errorf(`Error from server (AlreadyExists): secrets "%s" already exists`, name)
		}
		cert, key := splitCertAndKey(string(body))
		if f.secrets[name] == nil {
			f.secrets[name] = map[string]string{}
		}
		f.secrets[name]["tls.crt"], f.secrets[name]["tls.key"] = cert, key
		return "", nil
	}
	f.t.Fatalf("the fake cluster was sent a command it does not model: %q", command)
	return "", nil
}

// hopYAMLIn returns the file-provider configuration the copy command writes, which travels in the command
// itself since it holds nothing secret.
func hopYAMLIn(command string) string {
	marker := strings.Index(command, "<<'HOPEOF'")
	end := strings.LastIndex(command, "\nHOPEOF")
	if marker < 0 || end < 0 {
		return ""
	}
	start := marker + strings.Index(command[marker:], "\n") + 1
	return command[start:end] + "\n"
}

// splitCertAndKey mirrors the awk in tlsSecretCmd: certificate blocks to one
// file, private key blocks to the other.
func splitCertAndKey(body string) (certPEM, keyPEM string) {
	var cert, key []string
	inCert, inKey := false, false
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.HasPrefix(line, "-----BEGIN CERTIFICATE"):
			inCert = true
		case strings.HasPrefix(line, "-----BEGIN PRIVATE KEY"):
			inKey = true
		}
		switch {
		case inCert:
			cert = append(cert, line)
		case inKey:
			key = append(key, line)
		}
		switch {
		case strings.HasPrefix(line, "-----END CERTIFICATE"):
			inCert = false
		case strings.HasPrefix(line, "-----END PRIVATE KEY"):
			inKey = false
		}
	}
	return strings.Join(cert, "\n") + "\n", strings.Join(key, "\n") + "\n"
}

// An install or upgrade re-run must never take an authority out of the trust
// anchor. What the anchor holds are decisions an operator made — widened by
// hand partway through replacing an authority, narrowed by hand at the end —
// and removing either locks every operator out of the login path.
//
// This drives the real function against a simulated cluster and asserts on the
// anchor afterwards. Asserting on the predicate instead passed with the
// decision inverted, which is how a destructive rebuild survived a green suite.
func TestInstallReRunNeverRemovesAnAuthorityFromTheAnchor(t *testing.T) {
	caA, caKeyA, leafA, leafKeyA := authorityFixture(t)
	caB, caKeyB, _, _ := authorityFixture(t)
	both := string(hopca.Bundle([]byte(caA), []byte(caB)))
	leafUnderB := mustSignUnder(t, caB, caKeyB, leafKeyA)

	tests := []struct {
		name string
		// the cluster as the operator left it
		anchor, active, activeKey, pending, retained, leaf string
		// what must still be trusted afterwards
		mustStillTrust []string
	}{
		{
			name:   "steady",
			anchor: caA, active: caA, activeKey: caKeyA, leaf: leafA,
			mustStillTrust: []string{caA},
		},
		{
			name:   "staged: an incoming authority exists, trust not yet widened",
			anchor: caA, active: caA, activeKey: caKeyA, pending: caB, leaf: leafA,
			mustStillTrust: []string{caA},
		},
		{
			name:   "expanded: trust was widened by hand and must survive",
			anchor: both, active: caA, activeKey: caKeyA, pending: caB, leaf: leafA,
			mustStillTrust: []string{caA, caB},
		},
		{
			name:   "promoted: both authorities trusted, the new one signs",
			anchor: both, active: caB, activeKey: caKeyB, retained: caA, leaf: leafUnderB,
			mustStillTrust: []string{caA, caB},
		},
		{
			name:   "narrowed: trust was narrowed by hand and must not come back",
			anchor: caB, active: caB, activeKey: caKeyB, retained: caA, leaf: leafUnderB,
			mustStillTrust: []string{caB},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := newFakeCluster(t)
			cluster.files[hopCAPath] = tt.anchor
			cluster.secrets[hopCASecret] = map[string]string{"tls.crt": tt.active, "tls.key": tt.activeKey}
			if tt.pending != "" {
				cluster.secrets[hopCASecret][pendingCACertKey] = tt.pending
			}
			if tt.retained != "" {
				cluster.secrets[hopCASecret][retainedCAKey] = tt.retained
			}
			cluster.secrets[hopCertSecret] = map[string]string{"tls.crt": tt.leaf, "tls.key": leafKeyA}

			if err := EnsureHopMaterial(cluster); err != nil {
				t.Fatalf("an install re-run failed: %v", err)
			}

			anchor := cluster.files[hopCAPath]
			for _, authority := range tt.mustStillTrust {
				if !anchorContains(anchor, authority) {
					t.Errorf("an install re-run removed an authority the cluster still trusts (%s)", shortFingerprint(authority))
				}
			}
			// Whatever else it did, the anchor must name the signer, or the API
			// server cannot verify what this cluster serves.
			if !anchorContains(anchor, tt.active) {
				t.Error("the anchor no longer names the active signer")
			}
			// The narrowed case is the one a rebuild-from-Secret got wrong in the
			// other direction: the retained authority must not reappear.
			if tt.name == "narrowed" && anchorContains(anchor, caA) {
				t.Error("an install re-run re-trusted an authority the operator had narrowed away from")
			}
		})
	}
}

// The gateway pins the hop certificate's public key, so anything that moves it
// takes the cluster off the gateway. An install re-run mid-replacement re-signs
// the certificate under whichever authority is active, and it must do that with
// the key it found rather than a fresh one.
func TestInstallReRunKeepsThePinnedKey(t *testing.T) {
	caA, caKeyA, leafA, leafKeyA := authorityFixture(t)
	caB, caKeyB, _, _ := authorityFixture(t)

	cluster := newFakeCluster(t)
	cluster.files[hopCAPath] = string(hopca.Bundle([]byte(caB), []byte(caA)))
	cluster.secrets[hopCASecret] = map[string]string{"tls.crt": caB, "tls.key": caKeyB, retainedCAKey: caA}
	// The authority has moved but the certificate has not followed yet: the gap
	// between steps 2.2 and 2.3 of the documented procedure.
	cluster.secrets[hopCertSecret] = map[string]string{"tls.crt": leafA, "tls.key": leafKeyA}

	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatalf("an install re-run failed: %v", err)
	}

	served := cluster.secrets[hopCertSecret]["tls.crt"]
	if !hopca.SignedBy([]byte(served), []byte(caB)) {
		t.Error("the certificate was not re-signed under the active authority, so the procedure did not converge forward")
	}
	if got := cluster.secrets[hopCertSecret]["tls.key"]; strings.TrimSpace(got) != strings.TrimSpace(leafKeyA) {
		t.Error("the private key changed, which moves the fingerprint the gateway pins and takes this cluster off the gateway")
	}
	_ = caKeyA
}

// A cluster that has never had an authority gets one, and the anchor it is
// handed names it. This is the fresh-install path.
func TestAFreshClusterIsGivenAnAuthorityAndAnAnchor(t *testing.T) {
	cluster := newFakeCluster(t)

	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatalf("a fresh install failed: %v", err)
	}

	ca := cluster.secrets[hopCASecret]["tls.crt"]
	if ca == "" {
		t.Fatal("no authority was created")
	}
	if !anchorContains(cluster.files[hopCAPath], ca) {
		t.Error("the API server was not given an anchor naming the authority this cluster serves under")
	}
	served := cluster.secrets[hopCertSecret]["tls.crt"]
	if !hopca.SignedBy([]byte(served), []byte(ca)) {
		t.Error("the served certificate is not signed by the cluster's own authority")
	}
	if _, err := hopca.ParseCert([]byte(served)); err != nil {
		t.Errorf("the served certificate is not usable: %v", err)
	}
	if !strings.Contains(served, "CERTIFICATE") || !strings.Contains(cluster.secrets[hopCertSecret]["tls.key"], "PRIVATE KEY") {
		t.Error("the certificate and key were not stored in their own fields")
	}
	_ = hostnames.GatewayDomain
}

func TestHopMaterialWritesTraefiksCopyOfTheLeaf(t *testing.T) {
	cluster := newFakeCluster(t)

	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatal(err)
	}

	leaf := cluster.secrets[hopCertSecret]
	if cluster.traefikCopy["tls.crt"] != leaf["tls.crt"] || cluster.traefikCopy["tls.key"] != leaf["tls.key"] {
		t.Error("Traefik's copy must hold the live leaf pair")
	}
	if cluster.traefikCopy["hop.yaml"] != hopca.TraefikFileConfig {
		t.Errorf("hop.yaml = %q", cluster.traefikCopy["hop.yaml"])
	}
}

func TestHopMaterialKeepsTheCopysKeyOffTheCommandLine(t *testing.T) {
	cluster := newFakeCluster(t)
	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatal(err)
	}
	keyBody := strings.Split(strings.TrimSpace(cluster.secrets[hopCertSecret]["tls.key"]), "\n")[1]
	for _, c := range cluster.commands {
		if strings.Contains(c, keyBody) {
			t.Errorf("a command carries the private key: %q", c)
		}
	}
}

func TestHopMaterialLeavesTheTLSStoreForAfterTraefik(t *testing.T) {
	cluster := newFakeCluster(t)
	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatal(err)
	}
	for _, c := range cluster.commands {
		if strings.Contains(c, "kind: TLSStore") {
			t.Error("the TLSStore needs Traefik's CRDs, so it is applied by ServeHopCertificate after Traefik")
		}
	}

	before := len(cluster.commands)
	if err := ServeHopCertificate(cluster); err != nil {
		t.Fatal(err)
	}
	applied := false
	for _, c := range cluster.commands[before:] {
		applied = applied || strings.Contains(c, "kind: TLSStore")
	}
	if !applied {
		t.Error("ServeHopCertificate did not apply the TLSStore")
	}
}

func TestRestartTraefikOntoCopyRestartsTraefikOntoTheCopy(t *testing.T) {
	cluster := newFakeCluster(t)
	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatal(err)
	}

	if err := RestartTraefikOntoCopy(cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.traefikAnnotation != strconv.Itoa(cluster.copyVersion) || cluster.restarts != 1 {
		t.Errorf("annotation = %q after %d restarts, want %d after one", cluster.traefikAnnotation, cluster.restarts, cluster.copyVersion)
	}
	if cluster.rolloutWaits != 1 {
		t.Errorf("waited for the rollout %d times, want once", cluster.rolloutWaits)
	}
}

func TestRestartTraefikOntoCopyLeavesTraefikRunningWhenItServesTheCopy(t *testing.T) {
	cluster := newFakeCluster(t)
	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatal(err)
	}
	if err := RestartTraefikOntoCopy(cluster); err != nil {
		t.Fatal(err)
	}

	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatal(err)
	}
	if err := RestartTraefikOntoCopy(cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.restarts != 1 {
		t.Errorf("Traefik restarted %d times, want once: a rerun must leave it running", cluster.restarts)
	}
}

// A rerun after a rollout that timed out finds the annotation already set, and must still wait for the rollout
// rather than report success while Traefik serves the old copy.
func TestRestartTraefikOntoCopyWaitsForTheRolloutOnARerun(t *testing.T) {
	cluster := newFakeCluster(t)
	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatal(err)
	}
	if err := RestartTraefikOntoCopy(cluster); err != nil {
		t.Fatal(err)
	}

	if err := RestartTraefikOntoCopy(cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.rolloutWaits != 2 {
		t.Errorf("waited for the rollout %d times over two runs, want each run to wait", cluster.rolloutWaits)
	}
}

func TestRestartTraefikOntoCopyRestartsTraefikAfterTheCopyIsRewritten(t *testing.T) {
	cluster := newFakeCluster(t)
	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatal(err)
	}
	if err := RestartTraefikOntoCopy(cluster); err != nil {
		t.Fatal(err)
	}
	cluster.traefikCopy["tls.crt"] = "edited out of band"
	cluster.copyVersion++

	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatal(err)
	}
	if err := RestartTraefikOntoCopy(cluster); err != nil {
		t.Fatal(err)
	}
	if cluster.restarts != 2 || cluster.traefikAnnotation != strconv.Itoa(cluster.copyVersion) {
		t.Errorf("annotation = %q after %d restarts, want %d after two", cluster.traefikAnnotation, cluster.restarts, cluster.copyVersion)
	}
}

// The certificate and key are read in separate commands, so a rotation landing between them hands back a
// certificate for one key and the other key. Traefik cannot load such a pair, so it must never reach the copy.
func TestHopMaterialNeverWritesAMismatchedPairIntoTraefiksCopy(t *testing.T) {
	ca, caKey, leaf, leafKey := authorityFixture(t)
	rotated, _, err := hopca.NewLeaf([]byte(ca), []byte(caKey), hostnames.GatewayDomain)
	if err != nil {
		t.Fatal(err)
	}
	cluster := newFakeCluster(t)
	cluster.files[hopCAPath] = ca
	cluster.secrets[hopCASecret] = map[string]string{"tls.crt": ca, "tls.key": caKey}
	cluster.secrets[hopCertSecret] = map[string]string{"tls.crt": string(rotated), "tls.key": leafKey}
	cluster.traefikCopy = map[string]string{"tls.crt": leaf, "tls.key": leafKey, "hop.yaml": hopca.TraefikFileConfig}

	if err := EnsureHopMaterial(cluster); err == nil {
		t.Error("a certificate that does not belong to the key must stop the run")
	}
	if cluster.traefikCopy["tls.crt"] != leaf {
		t.Error("Traefik's copy was overwritten with a mismatched pair")
	}
}

func TestHopMaterialKeepsKeyMaterialOutOfWriteErrors(t *testing.T) {
	// A first run creates the material, so the next run's writes are updates that can fail.
	cluster := newFakeCluster(t)
	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatal(err)
	}
	key := cluster.secrets[hopCertSecret]["tls.key"]
	keyBody := strings.Split(strings.TrimSpace(key), "\n")[1]

	cluster.failWritesEchoingInput = true
	err := EnsureHopMaterial(cluster)
	if err == nil {
		t.Fatal("expected the failed copy write to surface")
	}
	for _, leak := range []string{keyBody, base64.StdEncoding.EncodeToString([]byte(key))[:40], "PRIVATE KEY"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the error carries key material: %v", err)
		}
	}
}

func TestSecretWriteKeepsTheAlreadyExistsSignal(t *testing.T) {
	cluster := newFakeCluster(t)
	if err := EnsureHopMaterial(cluster); err != nil {
		t.Fatal(err)
	}
	err := createTLSSecret(cluster, hopCASecret, "-----BEGIN CERTIFICATE-----\nx\n-----END CERTIFICATE-----", "-----BEGIN PRIVATE KEY-----\ny\n-----END PRIVATE KEY-----")
	if !isAlreadyExists(err) {
		t.Errorf("an existing authority must still read as AlreadyExists, got %v", err)
	}
	if strings.Contains(err.Error(), "PRIVATE KEY") {
		t.Errorf("the error carries key material: %v", err)
	}
}
