package installer

import "fmt"

// ServiceCAName names the ClusterIssuer that signs the certificates Kipper
// services serve TLS with, its CA Certificate and that certificate's Secret.
const ServiceCAName = "kipper-service-ca"

// serviceCAManifest is a self-signed root in cert-manager's namespace, where a
// ClusterIssuer reads its CA Secret, and the ClusterIssuer that signs service
// certificates with it. The root keeps its key on renewal, so existing client
// copies can still verify signatures while those CA certificates remain valid.
const serviceCAManifest = `apiVersion: cert-manager.io/v1
kind: Issuer
metadata:
  name: kipper-service-ca-selfsigned
  namespace: cert-manager
  labels:
    app.kubernetes.io/managed-by: kipper
spec:
  selfSigned: {}
---
apiVersion: cert-manager.io/v1
kind: Certificate
metadata:
  name: ` + ServiceCAName + `
  namespace: cert-manager
  labels:
    app.kubernetes.io/managed-by: kipper
spec:
  isCA: true
  commonName: Kipper service CA
  secretName: ` + ServiceCAName + `
  secretTemplate:
    labels:
      velero.io/exclude-from-backup: "true"
  duration: 87600h
  privateKey:
    algorithm: ECDSA
    size: 256
    rotationPolicy: Never
  issuerRef:
    name: kipper-service-ca-selfsigned
    kind: Issuer
---
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: ` + ServiceCAName + `
  labels:
    app.kubernetes.io/managed-by: kipper
spec:
  ca:
    secretName: ` + ServiceCAName + `
`

// InstallServiceCA applies the service CA and its ClusterIssuer. It needs a
// running cert-manager and preserves an existing root on repeated calls.
// It returns command errors and completes without waiting for CA readiness.
func InstallServiceCA(client CommandRunner) error {
	if _, err := client.Run(fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%s\nKIPEOF", serviceCAManifest)); err != nil {
		return fmt.Errorf("applying the service CA: %w", err)
	}
	return nil
}
