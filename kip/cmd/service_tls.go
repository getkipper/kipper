package cmd

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

// serviceTLSOnCreate returns the tls value a new service starts with:
// "required" for Postgres unless flag says otherwise, or empty for other types.
// It rejects unsupported values and any explicit TLS flag for other service types.
func serviceTLSOnCreate(serviceType, flag string) (string, error) {
	if serviceType != "postgres" {
		if flag != "" {
			return "", fmt.Errorf("--tls is supported for postgres services only")
		}
		return "", nil
	}
	if flag == "" {
		return "required", nil
	}
	return flag, validTLSMode(flag)
}

// validTLSMode rejects a --tls value the service CRD does not accept.
func validTLSMode(mode string) error {
	switch mode {
	case "required", "on", "off":
		return nil
	}
	return fmt.Errorf("--tls must be required, on or off, not %q", mode)
}

// describeServiceTLS describes the service's TLS mode, plaintext policy and
// any pending issuance or rollout.
func describeServiceTLS(s service.Snapshot) string {
	mode := s.TLS
	switch {
	case mode != "on" && mode != "required":
		return "off"
	case s.TLSServing && mode == "required":
		return "required (connections without TLS are refused)"
	case s.TLSServing:
		return "on (connections without TLS are still accepted)"
	case s.TLSWaiting != "":
		return mode + ", not served yet: " + s.TLSWaiting
	}
	return mode + ", not served yet: the controller has not reported it"
}

// plaintextClientsQuery selects network sessions without TLS, ordered by
// client address and application name.
const plaintextClientsQuery = `SELECT a.client_addr, coalesce(a.application_name, ''), a.usename FROM pg_stat_ssl s JOIN pg_stat_activity a ON a.pid = s.pid WHERE a.client_addr IS NOT NULL AND NOT s.ssl ORDER BY 1, 2`

// plaintextClients returns current network sessions without TLS, one per line
// with the client address, application name and user separated by spaces.
// Query failures return an error so the TLS gate can distinguish them from
// an empty result.
func plaintextClients(ctx context.Context, exec podExecFunc) ([]string, error) {
	var stdout, stderr bytes.Buffer
	cmd := []string{"sh", "-c", `psql -U "${POSTGRES_USER:-postgres}" -d postgres -XAtq -v ON_ERROR_STOP=1 -F ' ' -c "$1"`, "sh", plaintextClientsQuery}
	if err := exec(ctx, cmd, nil, &stdout, &stderr); err != nil {
		return nil, fmt.Errorf("listing connections without TLS: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var clients []string
	for _, line := range strings.Split(stdout.String(), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			clients = append(clients, line)
		}
	}
	return clients, nil
}

// requireTLSGate allows services already configured as required. Otherwise,
// it requires confirmed TLS service and a successful query of current clients.
// Plaintext clients block the change unless force is set. Disconnected clients
// cannot be checked.
func requireTLSGate(ctx context.Context, snapshot service.Snapshot, name string, exec func() (podExecFunc, error), force bool) error {
	if snapshot.TLS == "required" {
		return nil
	}
	if snapshot.TLS != "on" || !snapshot.TLSServing {
		return fmt.Errorf("%s does not serve TLS yet. Run kip service update %s --tls on, let its apps reconnect with sslmode=require or verify-full, then require TLS", name, name)
	}
	run, err := exec()
	if err != nil {
		return err
	}
	clients, err := plaintextClients(ctx, run)
	if err != nil {
		return err
	}
	if len(clients) > 0 && !force {
		return fmt.Errorf("these clients of %s are connected without TLS and would be refused:\n    %s\n  Set sslmode=require or verify-full in those apps and restart them, or pass --force to switch anyway", name, strings.Join(clients, "\n    "))
	}
	return nil
}

// setServiceTLS sets spec.tls on a Postgres service's CR, which the service
// controller turns into the matching pod. It reports whether the value
// changed. An absent value counts as off.
func setServiceTLS(ctx context.Context, dyn dynamic.Interface, namespace, name, tls string) (bool, error) {
	svc, err := dyn.Resource(manifest.ServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading service %s: %w", name, err)
	}
	if t, _, _ := unstructured.NestedString(svc.Object, "spec", "type"); t != "postgres" {
		return false, fmt.Errorf("--tls is supported for postgres services only. %s is %s", name, t)
	}
	current, _, _ := unstructured.NestedString(svc.Object, "spec", "tls")
	if current == "" {
		current = "off"
	}
	if current == tls {
		return false, nil
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"tls":%q}}`, tls))
	if _, err := dyn.Resource(manifest.ServiceGVR).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return false, fmt.Errorf("setting TLS on %s: %w", name, err)
	}
	return true, nil
}
