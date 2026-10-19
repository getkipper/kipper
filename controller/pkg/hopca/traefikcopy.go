package hopca

import "fmt"

// Traefik reads its own copy of the hop certificate from a file, so it serves the hop key from its first
// handshake instead of after its Kubernetes provider has loaded the default TLSStore. A Secret volume can only
// reference a Secret in the pod's namespace, hence a copy in Traefik's namespace. The installer and console-api
// both write it, so its shape is defined once, here.
const (
	// TraefikNamespace is where Traefik runs and where the copy lives.
	TraefikNamespace = "traefik"
	// TraefikCopyName is the copy's Secret name.
	TraefikCopyName = "kipper-hop-cert" //nolint:gosec // G101: a Secret object name, not a credential
	// TraefikMountPath is where Traefik mounts the copy.
	TraefikMountPath = "/hop"
	// TraefikConfigKey is the copy's key holding the file-provider configuration.
	TraefikConfigKey = "hop.yaml"
	// TraefikDeploymentName is Traefik's Deployment.
	TraefikDeploymentName = "traefik"
	// TraefikCopyAnnotation records the copy's resourceVersion on the pod template
	// when a restart is requested. File-provider reloads are unreliable, so writers
	// set it after updating the copy. A new version triggers a rollout even when
	// a repair restores the same certificate. The annotation tracks the restart
	// request, not whether every pod has loaded the version.
	TraefikCopyAnnotation = "kipper.run/hop-copy-version"
)

// TraefikFileConfig is the file-provider configuration in the copy. It declares the certificate without a TLS
// store because Traefik drops the default store when two providers define one.
// The Kubernetes TLSStore supplies the default store.
// The certificate is matched by SNI against its *.kipper.run name.
const TraefikFileConfig = "tls:\n" +
	"  certificates:\n" +
	"    - certFile: " + TraefikMountPath + "/tls.crt\n" +
	"      keyFile: " + TraefikMountPath + "/tls.key\n"

// TraefikFileArgs enables the file provider and watches the copy's configuration.
// Filename mode reads one file. Directory mode also reads kubelet's timestamped
// copy, and the duplicate-certificate warning can expose the private key.
var TraefikFileArgs = []string{
	"--providers.file.filename=" + TraefikMountPath + "/" + TraefikConfigKey,
	"--providers.file.watch=true",
}

// TraefikCopyData returns Secret data containing the supplied PEM pair and the
// file-provider configuration. The caller must supply the live serving pair.
// Staged rotation candidates must remain unserved until the gateway accepts
// them. The returned map shares the supplied byte slices.
func TraefikCopyData(certPEM, keyPEM []byte) map[string][]byte {
	return map[string][]byte{
		"tls.crt":        certPEM,
		"tls.key":        keyPEM,
		TraefikConfigKey: []byte(TraefikFileConfig),
	}
}

// TraefikRestartPatch returns a JSON merge patch that sets TraefikCopyAnnotation
// on Traefik's pod template to the copy's resourceVersion, triggering a rollout
// when the annotation changes.
func TraefikRestartPatch(copyVersion string) []byte {
	return []byte(fmt.Sprintf(`{"spec":{"template":{"metadata":{"annotations":{%q:%q}}}}}`, TraefikCopyAnnotation, copyVersion))
}
