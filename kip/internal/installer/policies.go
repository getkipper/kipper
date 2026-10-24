package installer

import (
	"bytes"
	"embed"
	"fmt"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/utils/ptr"

	"github.com/getkipper/kipper/controller/pkg/platform"
)

// admissionPolicies holds Kipper's ValidatingAdmissionPolicies and their
// bindings, copied from deploy/policies.
//
//go:embed policies/*.yaml
var admissionPolicies embed.FS

// offeredCopiesLine is the copies offer the shipped
// kipper-storage-copies-offer policy carries.
const offeredCopiesLine = "    - name: offered\n      expression: \"1\"\n"

// serverNodesLine is the empty server node list the shipped
// kipper-longhorn-server-replicas policy carries.
const serverNodesLine = "    - name: serverNodes\n      expression: \"[]\"\n"

// AdmissionPolicyManifests returns embedded policy YAML in filename order,
// with the copies offer set to offered and the server node list set to
// serverNodes. It returns an error if offered is outside 1 to 2, a server node
// name is not a DNS-1123 subdomain, a file cannot be read or a placeholder is
// missing. No server nodes render an empty list, which denies every replica
// delete by console-api.
func AdmissionPolicyManifests(offered int32, serverNodes []string) ([][]byte, error) {
	if offered < 1 || offered > platform.MaxCopies {
		return nil, fmt.Errorf("the copies offer must be 1 or 2, not %d", offered)
	}
	servers, err := celStringList(serverNodes)
	if err != nil {
		return nil, err
	}
	entries, err := admissionPolicies.ReadDir("policies")
	if err != nil {
		return nil, fmt.Errorf("reading embedded admission policies: %w", err)
	}
	manifests := make([][]byte, 0, len(entries))
	for _, e := range entries {
		data, err := admissionPolicies.ReadFile("policies/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading admission policy %s: %w", e.Name(), err)
		}
		if e.Name() == "kipper-storage-copies-offer.yaml" {
			if !bytes.Contains(data, []byte(offeredCopiesLine)) {
				return nil, fmt.Errorf("admission policy %s carries no copies offer to render", e.Name())
			}
			data = bytes.Replace(data, []byte(offeredCopiesLine), []byte(fmt.Sprintf("    - name: offered\n      expression: \"%d\"\n", offered)), 1)
		}
		if e.Name() == "kipper-longhorn-server-replicas.yaml" {
			if !bytes.Contains(data, []byte(serverNodesLine)) {
				return nil, fmt.Errorf("admission policy %s carries no server node list to render", e.Name())
			}
			data = bytes.Replace(data, []byte(serverNodesLine), []byte(fmt.Sprintf("    - name: serverNodes\n      expression: %q\n", servers)), 1)
		}
		manifests = append(manifests, data)
	}
	return manifests, nil
}

// celStringList renders node names as a CEL list literal. Each name must be a
// DNS-1123 subdomain, so no name can end the string it is quoted in.
func celStringList(names []string) (string, error) {
	quoted := make([]string, 0, len(names))
	for _, n := range names {
		if errs := validation.IsDNS1123Subdomain(n); len(errs) > 0 {
			return "", fmt.Errorf("server node name %q cannot be rendered into an admission policy: %s", n, strings.Join(errs, "; "))
		}
		quoted = append(quoted, "'"+n+"'")
	}
	return "[" + strings.Join(quoted, ", ") + "]", nil
}

// InstallAdmissionPolicies applies embedded policies in filename order using kubectl,
// preserving the copies offer recorded in PlatformConfig and listing the
// nodes labeled ServerNodeLabel as servers. The offer defaults to one when the
// object, field or resource kind is absent.
// Call it after InstallCRDs because the policies use Kipper resources as parameters.
// Offer and node reads and manifest rendering must succeed before any policy is
// applied. An apply error stops installation, leaving earlier policies applied.
func InstallAdmissionPolicies(client CommandRunner) error {
	offered, err := recordedCopiesOffer(client)
	if err != nil {
		return err
	}
	servers, err := ServerNodeNames(client)
	if err != nil {
		return err
	}
	manifests, err := AdmissionPolicyManifests(offered, servers)
	if err != nil {
		return err
	}
	for _, m := range manifests {
		if _, err := client.Run(fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", m)); err != nil {
			return fmt.Errorf("applying admission policies: %w", err)
		}
	}
	return nil
}

// offerPrefix marks the value line in recordedCopiesOffer's output, which
// also carries kubectl's warnings from stderr.
const offerPrefix = "offer="

// recordedCopiesOffer reads PlatformConfig.spec.maxCopies with kubectl. A
// missing PlatformConfig, an unset field or a PlatformConfig kind the API
// server does not serve yet offers one copy. Other read failures and unparsable
// values return an error to avoid replacing a recorded offer with a default.
func recordedCopiesOffer(client CommandRunner) (int32, error) {
	out, err := client.Run(`kubectl get platformconfig platform --ignore-not-found -o jsonpath='{"` + offerPrefix + `"}{.spec.maxCopies}'`)
	if err != nil {
		if strings.Contains(out, "doesn't have a resource type") {
			return 1, nil
		}
		return 0, fmt.Errorf("reading the recorded copies offer: %w: %s", err, strings.TrimSpace(out))
	}
	value := ""
	for _, line := range strings.Split(out, "\n") {
		if v, found := strings.CutPrefix(strings.TrimSpace(line), offerPrefix); found {
			value = v
		}
	}
	if value == "" {
		return 1, nil
	}
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("reading the recorded copies offer: unexpected value %q", value)
	}
	return platform.OfferedCopies(ptr.To(int32(n))), nil
}

// ServerNodeNames lists the nodes labeled ServerNodeLabel, in kubectl's order.
// A cluster with none returns an empty list.
func ServerNodeNames(client CommandRunner) ([]string, error) {
	out, err := client.Run("kubectl get nodes -l " + ServerNodeLabel + " --request-timeout=30s -o name")
	if err != nil {
		return nil, fmt.Errorf("listing the server nodes: %w: %s", err, strings.TrimSpace(out))
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if name, ok := strings.CutPrefix(strings.TrimSpace(line), "node/"); ok && name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}
