package installer

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// ingressPeerQuery prints the ingress-controller settings as namespace|labelKey|labelValue.
const ingressPeerQuery = `kubectl -n kipper-system get configmap ingress-controller ` +
	`-o jsonpath='{.data.namespace}{"|"}{.data.labelKey}{"|"}{.data.labelValue}' 2>/dev/null`

// applyGrafanaNetworkPolicy restricts access to the ingress controller and
// Prometheus before the chart enables trusted identity headers.
func applyGrafanaNetworkPolicy(client *ssh.Client) error {
	// A missing ConfigMap or a failed read leaves the defaults for the Traefik Kipper installs.
	out, _ := client.Run(ingressPeerQuery)
	manifest := platform.GrafanaNetworkPolicy(parseIngressPeer(out))
	cmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", manifest)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("applying grafana network policy: %w", err)
	}
	return nil
}

// parseIngressPeer expects one line of three fields. Invalid output uses all
// defaults; an invalid namespace or label pair uses its respective default.
func parseIngressPeer(out string) platform.IngressPeer {
	out = strings.TrimRight(out, "\r\n")
	if strings.Contains(out, "\n") {
		return platform.IngressPeer{}
	}
	fields := strings.Split(out, "|")
	if len(fields) != 3 {
		return platform.IngressPeer{}
	}
	var peer platform.IngressPeer
	if ns := fields[0]; ns != "" && len(validation.IsDNS1123Label(ns)) == 0 {
		peer.Namespace = ns
	}
	key, value := fields[1], fields[2]
	if key != "" && value != "" && len(validation.IsQualifiedName(key)) == 0 && len(validation.IsValidLabelValue(value)) == 0 {
		peer.LabelKey, peer.LabelValue = key, value
	}
	return peer
}
