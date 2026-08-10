package installer

import (
	"fmt"
	"io"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/hopca"
	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

// commandRunner is the part of an SSH connection this package needs to reach a
// cluster's trust material.
//
// It exists as a seam because the functions below decide what every install and
// every upgrade writes to the anchor the API server verifies logins against,
// and that decision was previously testable only by inspecting the strings it
// would have sent. A test could assert the predicate and still pass with the
// decision inverted. *ssh.Client is the only implementation that ships.
type commandRunner interface {
	Run(command string) (string, error)
	RunStdin(command string, stdin io.Reader) (string, error)
}

// EnsureHopMaterial provisions the certificate the cluster serves on the
// gateway hop, the authority that signs it, and the copy of that authority the
// API server verifies against.
//
// It runs before operator authentication because that step's probe, and the API
// server moments later, both fetch OIDC discovery from a gateway-fronted Dex
// host. That host is served the hop certificate, which no public authority
// signed, so without an anchor in place first neither can verify it and the
// install stops with a certificate error that names cert-manager, which was
// never involved.
//
// Console-api's reconciler owns this material from its first run onward:
// rotation, reissue and the TLSStore are its job. This function exists because
// that reconciler does not exist yet at install time, and waiting for it would
// put a bootstrap dependency on a pod that has not been deployed.
func EnsureHopMaterial(client commandRunner) error {
	if _, err := client.Run(ensureNamespaceCmd(hopNamespace)); err != nil {
		return fmt.Errorf("ensuring %s namespace: %w", hopNamespace, err)
	}

	caCertPEM, caKeyPEM, err := ensureHopCA(client)
	if err != nil {
		return err
	}
	if err := ensureHopLeaf(client, caCertPEM, caKeyPEM); err != nil {
		return err
	}
	if _, err := client.Run(applyTLSStoreCmd()); err != nil {
		return fmt.Errorf("pointing Traefik's default certificate at the hop certificate: %w", err)
	}
	// The API server reads this file, and so does the operator-auth probe. It is
	// written last so a failure above never leaves an anchor for material that
	// does not exist.
	if err := ensureAnchorCovers(client, caCertPEM); err != nil {
		return err
	}
	return nil
}

// ensureAnchorCovers adds the active signer to the trust anchor if it is not
// already there, and changes nothing otherwise.
//
// The anchor is only ever added to. What it holds are an operator's trust
// decisions — widened by hand partway through replacing an authority, narrowed
// by hand at the end — and an install has no business reversing either.
// Rebuilding it from the Secret reversed both: it dropped an incoming authority
// the operator had just widened trust to, and it re-added an outgoing one they
// had just narrowed away from, silently rewinding a replacement that the
// documentation says is safe to leave part-finished.
//
// What an install actually has to guarantee is narrower: that the API server
// can verify what this cluster serves, which means the anchor names whatever is
// signing. Adding is always safe. Removing is the operation that locks people
// out, and nothing here does it.
func ensureAnchorCovers(client commandRunner, activePEM string) error {
	existing, err := readHopCA(client)
	if err != nil {
		return err
	}
	if anchorContains(existing, activePEM) {
		return nil
	}
	return writeAnchor(client, activePEM, existing)
}

// anchorContains reports whether a trust anchor already names an authority,
// comparing with whitespace removed so a certificate written into a bundle
// matches the same certificate read out of a Secret.
func anchorContains(anchor, certPEM string) bool {
	return certPEM != "" && strings.Contains(normalisePEM(anchor), normalisePEM(certPEM))
}

// writeAnchor puts the authorities the API server should trust where it reads
// them, the active signer first, as one bundle so this file and console-api's
// rendering of the same material hash identically.
//
// alsoTrust is everything else that must stay trusted: whatever the anchor
// already held. Narrowing trust behind the back of whatever put it there is the
// one thing this must never do, so callers pass what they read rather than what
// they think it should be.
func writeAnchor(client commandRunner, active, alsoTrust string) error {
	bundle := hopca.Bundle([]byte(active), []byte(alsoTrust))
	if _, err := client.RunStdin(writeFileCmd(hopCAPath, "0644"), strings.NewReader(string(bundle))); err != nil {
		return fmt.Errorf("writing the cluster certificate authority for the API server: %w", err)
	}
	return nil
}

const (
	hopNamespace  = "kipper-system"
	hopCASecret   = "kipper-hop-ca"   //nolint:gosec // G101: a Secret object name, not a credential
	hopCertSecret = "kipper-hop-cert" //nolint:gosec // G101: a Secret object name, not a credential

	// retainedCAKey holds an authority that is trusted but no longer signs, put
	// there by hand partway through replacing an authority. The name is defined
	// once, in the module both this and console-api already depend on, because
	// the two render the same anchor and a disagreement would park every cutover.
	retainedCAKey = hopca.RetainedCAKey
)

// ensureHopCA returns the cluster's authority, minting one only when the cluster
// genuinely has none.
//
// The distinction between "absent" and "could not tell" is the whole safety
// property here. The API server is handed this exact anchor and nothing in the
// cluster can give it another, so replacing an established CA breaks operator
// authentication until someone intervenes by hand. A read that fails for any
// reason other than the Secret not existing therefore stops the install rather
// than being read as a fresh cluster, and the create is a create — never an
// apply — so even a misjudged absence loses the race instead of overwriting.
func ensureHopCA(client commandRunner) (caCertPEM, caKeyPEM string, err error) {
	existingCert, err := readSecretKey(client, hopCASecret, "tls.crt")
	if err != nil {
		return "", "", err
	}
	existingKey, err := readSecretKey(client, hopCASecret, "tls.key")
	if err != nil {
		return "", "", err
	}
	if existingCert != "" && existingKey != "" {
		return existingCert, existingKey, nil
	}
	if existingCert != "" || existingKey != "" {
		return "", "", fmt.Errorf("the cluster certificate authority in %s/%s is missing its certificate or its key; "+
			"repair or delete it before installing, since replacing it would break operator authentication",
			hopNamespace, hopCASecret)
	}

	material, err := hopca.New(hostnames.GatewayDomain)
	if err != nil {
		return "", "", fmt.Errorf("minting the cluster certificate authority: %w", err)
	}
	if err := createTLSSecret(client, hopCASecret, string(material.CACertPEM), string(material.CAKeyPEM)); err != nil {
		// Losing a create race means someone else got there first, which is the
		// outcome we want: read theirs rather than insisting on ours.
		if !isAlreadyExists(err) {
			return "", "", fmt.Errorf("storing the cluster certificate authority: %w", err)
		}
		return readHopCAPair(client)
	}
	return string(material.CACertPEM), string(material.CAKeyPEM), nil
}

// readHopCAPair reads back an authority that already existed, insisting on both
// halves: half an authority cannot sign and cannot anchor.
func readHopCAPair(client commandRunner) (caCertPEM, caKeyPEM string, err error) {
	caCertPEM, err = readSecretKey(client, hopCASecret, "tls.crt")
	if err != nil {
		return "", "", err
	}
	caKeyPEM, err = readSecretKey(client, hopCASecret, "tls.key")
	if err != nil {
		return "", "", err
	}
	if caCertPEM == "" || caKeyPEM == "" {
		return "", "", fmt.Errorf("the cluster certificate authority in %s/%s exists but is unreadable", hopNamespace, hopCASecret)
	}
	return caCertPEM, caKeyPEM, nil
}

func isAlreadyExists(err error) bool {
	return err != nil && strings.Contains(err.Error(), "AlreadyExists")
}

// ensureHopLeaf makes the served certificate one the CA signed. A cluster that
// already has a leaf keeps its key and has that key re-signed, so its SPKI —
// and with it the gateway's pin and its registration — is untouched. Doing this
// here rather than leaving it to console-api matters on an upgrade or a cutover:
// the reconciler would not converge until its next heartbeat, up to a day away,
// and the probe runs in the next few seconds.
func ensureHopLeaf(client commandRunner, caCertPEM, caKeyPEM string) error {
	keyPEM, err := readSecretKey(client, hopCertSecret, "tls.key")
	if err != nil {
		return err
	}
	existingCert, err := readSecretKey(client, hopCertSecret, "tls.crt")
	if err != nil {
		return err
	}

	if keyPEM != "" && hopca.SignedBy([]byte(existingCert), []byte(caCertPEM)) {
		return nil
	}

	var certPEM string
	if keyPEM != "" {
		signed, err := hopca.SignLeaf([]byte(caCertPEM), []byte(caKeyPEM), []byte(keyPEM), hostnames.GatewayDomain)
		if err != nil {
			return fmt.Errorf("re-signing the hop certificate under the cluster authority: %w", err)
		}
		certPEM = string(signed)
	} else {
		leafCert, leafKey, err := hopca.NewLeaf([]byte(caCertPEM), []byte(caKeyPEM), hostnames.GatewayDomain)
		if err != nil {
			return fmt.Errorf("minting the hop certificate: %w", err)
		}
		certPEM, keyPEM = string(leafCert), string(leafKey)
	}

	if err := applyTLSSecret(client, hopCertSecret, certPEM, keyPEM); err != nil {
		return fmt.Errorf("storing the hop certificate: %w", err)
	}
	return nil
}

// readSecretKey returns one key of a Secret, empty when the Secret does not
// exist. A read that fails for any other reason is returned as an error and
// never mistaken for absence — see ensureHopCA for why that distinction is the
// safety property of this file.
func readSecretKey(client commandRunner, name, key string) (string, error) {
	out, err := client.Run(readSecretKeyCmd(name, key))
	if err != nil {
		return "", fmt.Errorf("reading %s/%s: %w", hopNamespace, name, err)
	}
	return strings.TrimSpace(out), nil
}

// readSecretKeyCmd prints one base64-decoded key of a Secret. --ignore-not-found
// makes an absent Secret an empty success, so a non-zero exit means the read
// genuinely failed rather than the object being new.
func readSecretKeyCmd(name, key string) string {
	return fmt.Sprintf("set -o pipefail; kubectl -n %s get secret %s --ignore-not-found -o jsonpath='{.data.%s}' | base64 -d",
		hopNamespace, name, strings.ReplaceAll(key, ".", `\.`))
}

// createTLSSecret creates a kubernetes.io/tls Secret and fails if one is already
// there, which is what keeps an established authority from being overwritten.
func createTLSSecret(client commandRunner, name, certPEM, keyPEM string) error {
	return writeTLSSecret(client, name, certPEM, keyPEM, false)
}

// applyTLSSecret upserts a kubernetes.io/tls Secret. The material is piped
// through stdin rather than the command line, so no private key reaches the
// process table or an error message.
func applyTLSSecret(client commandRunner, name, certPEM, keyPEM string) error {
	return writeTLSSecret(client, name, certPEM, keyPEM, true)
}

func writeTLSSecret(client commandRunner, name, certPEM, keyPEM string, upsert bool) error {
	_, err := client.RunStdin(tlsSecretCmd(name, upsert), strings.NewReader(certPEM+"\n"+keyPEM+"\n"))
	return err
}

// tlsSecretCmd builds the command that turns a PEM pair on stdin into a
// kubernetes.io/tls Secret. The material never appears in the command string, so
// no private key reaches the process table or an error message, and the temp
// copy is removed however the command exits.
//
// upsert distinguishes the two callers. The hop certificate is replaced on
// adoption and reissue, so it applies. The authority is created and only
// created: an apply would overwrite an anchor the API server already holds.
func tlsSecretCmd(name string, upsert bool) string {
	write := fmt.Sprintf(`kubectl -n %s create secret tls %s --cert="$tmp/tls.crt" --key="$tmp/tls.key"`, hopNamespace, name)
	if upsert {
		write += " --dry-run=client -o yaml | kubectl apply -f -"
	}
	return `set -o pipefail; tmp=$(mktemp -d) && trap 'rm -rf "$tmp"' EXIT && ` +
		`awk '/^-----BEGIN CERTIFICATE-----$/,/^-----END CERTIFICATE-----$/{print > "'"$tmp"'/tls.crt"} ` +
		`/^-----BEGIN PRIVATE KEY-----$/,/^-----END PRIVATE KEY-----$/{print > "'"$tmp"'/tls.key"}' && ` + write
}

// writeFileCmd writes stdin to path atomically with the given mode.
func writeFileCmd(path, mode string) string {
	return fmt.Sprintf("cat > %s.kipper-tmp && chmod %s %s.kipper-tmp && mv %s.kipper-tmp %s",
		path, mode, path, path, path)
}

// applyTLSStoreCmd points Traefik's default certificate at the hop certificate,
// which is what makes the cluster serve it for a gateway-fronted host: those
// Ingresses carry no secretName, so without a default store Traefik answers with
// its own generated certificate, which nothing can verify or pin.
func applyTLSStoreCmd() string {
	manifest := fmt.Sprintf(`apiVersion: traefik.io/v1alpha1
kind: TLSStore
metadata:
  name: default
  namespace: %s
  labels:
    app.kubernetes.io/managed-by: kipper
spec:
  defaultCertificate:
    secretName: %s
`, hopNamespace, hopCertSecret)
	return fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)
}
