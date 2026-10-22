package installer

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/getkipper/kipper/controller/pkg/hopca"
	"github.com/getkipper/kipper/controller/pkg/hostnames"
)

// CommandRunner runs installer shell commands on the cluster host.
// SSH and local runners implement it, and tests can substitute a fake.
type CommandRunner interface {
	Run(command string) (string, error)
	RunStdin(command string, stdin io.Reader) (string, error)
}

// EnsureHopMaterial provisions the gateway-hop certificate, its signing
// authority, and the authority's trust anchor for the API server. It also writes
// Traefik's copy of the certificate, which Traefik loads from a file at startup.
// Run it before installing Traefik, then call ServeHopCertificate once Traefik's
// CRDs are available to apply the default TLSStore.
//
// It must also run before operator authentication. That step's probe and the
// API server both fetch OIDC discovery from a gateway-fronted Dex host serving
// the hop certificate. Because no public authority signed it, both need the
// trust anchor first. Otherwise, installation fails with a certificate error
// naming cert-manager, even though cert-manager is not involved.
//
// Console-api reconciles rotation, reissue and the TLSStore once deployed.
// Installation provisions this material first to avoid a bootstrap dependency
// on a reconciler pod that has not yet been deployed.
func EnsureHopMaterial(client CommandRunner) error {
	for _, ns := range []string{hopNamespace, hopca.TraefikNamespace} {
		if _, err := client.Run(ensureNamespaceCmd(ns)); err != nil {
			return fmt.Errorf("ensuring %s namespace: %w", ns, err)
		}
	}

	caCertPEM, caKeyPEM, err := ensureHopCA(client)
	if err != nil {
		return err
	}
	leafCertPEM, leafKeyPEM, err := ensureHopLeaf(client, caCertPEM, caKeyPEM)
	if err != nil {
		return err
	}
	// The key and certificate are read in separate commands, so a rotation
	// between them returns a pair Traefik cannot load.
	if _, err := tls.X509KeyPair([]byte(leafCertPEM), []byte(leafKeyPEM)); err != nil {
		return fmt.Errorf("the hop certificate and key in %s/%s do not belong together: %w. A key rotation while "+
			"kip read them can cause this, so run the command again. If it persists, the Secret itself holds a "+
			"mismatched pair", hopNamespace, hopCertSecret, err)
	}
	if _, err := client.RunStdin(traefikCopyCmd(), strings.NewReader(leafCertPEM+"\n"+leafKeyPEM+"\n")); err != nil {
		return secretWriteError(hopca.TraefikNamespace, hopca.TraefikCopyName, err)
	}
	// The API server reads this file, and so does the operator-auth probe. It is
	// written last so a failure above never leaves an anchor for material that
	// does not exist.
	if err := ensureAnchorCovers(client, caCertPEM); err != nil {
		return err
	}
	return nil
}

// ServeHopCertificate points Traefik's default TLSStore at the hop certificate,
// for clients that send no server name or one without a matching certificate.
// It needs Traefik's CRDs, so it runs after Traefik is installed.
func ServeHopCertificate(client CommandRunner) error {
	if _, err := client.Run(applyTLSStoreCmd()); err != nil {
		return fmt.Errorf("pointing Traefik's default certificate at the hop certificate: %w", err)
	}
	return nil
}

// RestartTraefikOntoCopy requests a restart when the pod template's copy version
// differs from the Secret's resourceVersion because file-provider reloads of
// the mounted copy are unreliable. After successful reads and any required patch,
// it waits up to 300 seconds for the rollout, including on retries after a timeout.
// It returns read, patch and rollout errors to the caller.
//
// Call it after applying the Traefik HelmChart. helm-controller applies chart
// changes asynchronously, so the wait can cover pods from the previous chart.
// Helm preserves the annotation, and later pods read the copy at startup.
func RestartTraefikOntoCopy(client CommandRunner) error {
	copyVersion, err := client.Run(fmt.Sprintf("kubectl -n %s get secret %s -o jsonpath='{.metadata.resourceVersion}'",
		hopca.TraefikNamespace, hopca.TraefikCopyName))
	if err != nil {
		return fmt.Errorf("reading Traefik's copy of the hop certificate: %w", err)
	}
	copyVersion = strings.TrimSpace(copyVersion)
	if copyVersion == "" {
		return errors.New("reading Traefik's copy of the hop certificate: it has no resourceVersion")
	}
	current, err := client.Run(fmt.Sprintf("kubectl -n %s get deployment %s -o jsonpath='{.spec.template.metadata.annotations.%s}'",
		hopca.TraefikNamespace, hopca.TraefikDeploymentName, strings.ReplaceAll(hopca.TraefikCopyAnnotation, ".", `\.`)))
	if err != nil {
		return fmt.Errorf("reading Traefik's Deployment: %w", err)
	}
	if strings.TrimSpace(current) != copyVersion {
		if _, err := client.Run(fmt.Sprintf("kubectl -n %s patch deployment %s --type merge -p '%s'",
			hopca.TraefikNamespace, hopca.TraefikDeploymentName, hopca.TraefikRestartPatch(copyVersion))); err != nil {
			return fmt.Errorf("restarting Traefik onto its copy of the hop certificate: %w", err)
		}
	}
	if _, err := client.Run(fmt.Sprintf("kubectl -n %s rollout status deployment/%s --timeout=300s",
		hopca.TraefikNamespace, hopca.TraefikDeploymentName)); err != nil {
		return fmt.Errorf("waiting for Traefik to serve its copy of the hop certificate: %w", err)
	}
	return nil
}

// ensureAnchorCovers adds the active signer while preserving the existing
// trust bundle. Rebuilding from the Secret alone could undo an operator's
// staged CA replacement.
func ensureAnchorCovers(client CommandRunner, activePEM string) error {
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
func writeAnchor(client CommandRunner, active, alsoTrust string) error {
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
func ensureHopCA(client CommandRunner) (caCertPEM, caKeyPEM string, err error) {
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
func readHopCAPair(client CommandRunner) (caCertPEM, caKeyPEM string, err error) {
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
func ensureHopLeaf(client CommandRunner, caCertPEM, caKeyPEM string) (certPEM, keyPEM string, err error) {
	keyPEM, err = readSecretKey(client, hopCertSecret, "tls.key")
	if err != nil {
		return "", "", err
	}
	existingCert, err := readSecretKey(client, hopCertSecret, "tls.crt")
	if err != nil {
		return "", "", err
	}

	if keyPEM != "" && hopca.SignedBy([]byte(existingCert), []byte(caCertPEM)) {
		return existingCert, keyPEM, nil
	}

	if keyPEM != "" {
		signed, err := hopca.SignLeaf([]byte(caCertPEM), []byte(caKeyPEM), []byte(keyPEM), hostnames.GatewayDomain)
		if err != nil {
			return "", "", fmt.Errorf("re-signing the hop certificate under the cluster authority: %w", err)
		}
		certPEM = string(signed)
	} else {
		leafCert, leafKey, err := hopca.NewLeaf([]byte(caCertPEM), []byte(caKeyPEM), hostnames.GatewayDomain)
		if err != nil {
			return "", "", fmt.Errorf("minting the hop certificate: %w", err)
		}
		certPEM, keyPEM = string(leafCert), string(leafKey)
	}

	if err := applyTLSSecret(client, hopCertSecret, certPEM, keyPEM); err != nil {
		return "", "", fmt.Errorf("storing the hop certificate: %w", err)
	}
	return certPEM, keyPEM, nil
}

// readSecretKey returns one key of a Secret, empty when the Secret does not
// exist. A read that fails for any other reason is returned as an error and
// never mistaken for absence — see ensureHopCA for why that distinction is the
// safety property of this file.
func readSecretKey(client CommandRunner, name, key string) (string, error) {
	out, err := client.Run(readSecretKeyCmd(name, key))
	if err != nil {
		// A failed Run can carry the decoded key in its error.
		return "", fmt.Errorf("reading %s/%s failed", hopNamespace, name)
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
func createTLSSecret(client CommandRunner, name, certPEM, keyPEM string) error {
	return writeTLSSecret(client, name, certPEM, keyPEM, false)
}

// applyTLSSecret upserts a kubernetes.io/tls Secret. The material is piped
// through stdin to keep the private key out of the process table. Write errors
// are sanitized because kubectl can echo the key.
func applyTLSSecret(client CommandRunner, name, certPEM, keyPEM string) error {
	return writeTLSSecret(client, name, certPEM, keyPEM, true)
}

func writeTLSSecret(client CommandRunner, name, certPEM, keyPEM string, upsert bool) error {
	if _, err := client.RunStdin(tlsSecretCmd(name, upsert), strings.NewReader(certPEM+"\n"+keyPEM+"\n")); err != nil {
		return secretWriteError(hopNamespace, name, err)
	}
	return nil
}

// secretWriteError reports a failed Secret write without its output: kubectl
// can echo the attempted object, private key included, when an apply fails. An
// AlreadyExists failure keeps that word so callers can still tell a lost create
// race from a real failure.
func secretWriteError(namespace, name string, err error) error {
	if isAlreadyExists(err) {
		return fmt.Errorf("writing %s/%s failed: AlreadyExists", namespace, name)
	}
	return fmt.Errorf("writing %s/%s failed. Its output is withheld because it can contain the private key", namespace, name)
}

// tlsSecretCmd builds the command that turns a PEM pair on stdin into a
// kubernetes.io/tls Secret. The material never appears in the command string, so
// the key stays out of the process table. The caller sanitizes write errors
// because kubectl can echo the key. The temporary copy is removed on exit.
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

// traefikCopyCmd writes Traefik's copy of the hop certificate from a PEM pair on
// stdin, the same way tlsSecretCmd keeps the key off the command line. The
// file-provider configuration holds nothing secret and travels in the command.
// Leave unchanged data alone because apply can add kubectl's last-applied
// annotation and change the resourceVersion, triggering an unnecessary restart.
func traefikCopyCmd() string {
	configKey := strings.ReplaceAll(hopca.TraefikConfigKey, ".", `\.`)
	return `set -o pipefail; tmp=$(mktemp -d) && trap 'rm -rf "$tmp"' EXIT && ` +
		`b64() { base64 -w0 < "$1" 2>/dev/null || base64 < "$1" | tr -d '\n'; } && ` +
		`awk '/^-----BEGIN CERTIFICATE-----$/,/^-----END CERTIFICATE-----$/{print > "'"$tmp"'/tls.crt"} ` +
		`/^-----BEGIN PRIVATE KEY-----$/,/^-----END PRIVATE KEY-----$/{print > "'"$tmp"'/tls.key"}' && ` +
		`cat > "$tmp/` + hopca.TraefikConfigKey + `" <<'HOPEOF' && ` +
		fmt.Sprintf(`{ [ "$(kubectl -n %s get secret %s --ignore-not-found -o jsonpath='{.data.tls\.crt} {.data.tls\.key} {.data.%s}')" = `+
			`"$(b64 "$tmp/tls.crt") $(b64 "$tmp/tls.key") $(b64 "$tmp/%s")" ] || `,
			hopca.TraefikNamespace, hopca.TraefikCopyName, configKey, hopca.TraefikConfigKey) +
		fmt.Sprintf(`kubectl -n %s create secret generic %s --from-file=tls.crt="$tmp/tls.crt" --from-file=tls.key="$tmp/tls.key" `+
			`--from-file=%s="$tmp/%s" --dry-run=client -o yaml | kubectl apply -f -; }`,
			hopca.TraefikNamespace, hopca.TraefikCopyName, hopca.TraefikConfigKey, hopca.TraefikConfigKey) +
		"\n" + hopca.TraefikFileConfig + "HOPEOF"
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
