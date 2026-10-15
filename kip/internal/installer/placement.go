package installer

import (
	"fmt"
	"strings"
)

// serverNodeLabel identifies k3s server nodes, matching Traefik's selector.
const serverNodeLabel = "node-role.kubernetes.io/control-plane"

// serviceLBNodeLabel enables ServiceLB only on labeled nodes once any node
// has this label set to true.
const serviceLBNodeLabel = "svccontroller.k3s.cattle.io/enablelb"

// platformNamespaces scopes placement to namespaces reserved for platform components.
var platformNamespaces = []string{"cert-manager", "dex", "keda", "kipper-system", "monitoring", "velero"}

var k3sAddonDeployments = []string{"coredns", "metrics-server"}

// serverNodeSelectorPatch is a strategic merge patch that adds the server
// selector to a pod template while preserving other nodeSelector keys.
var serverNodeSelectorPatch = fmt.Sprintf(`{"spec":{"template":{"spec":{"nodeSelector":{%q:"true"}}}}}`, serverNodeLabel)

const placementRolloutTimeout = "300s"

// PinPlatformToServer applies server placement to existing platform workloads
// and labels server nodes for ServiceLB. It patches Deployments and StatefulSets
// without owner references in platformNamespaces, the selected k3s addons, and
// the Prometheus resource. It waits for rollouts even when selectors are unchanged,
// so retries also check workloads whose previous rollout is incomplete.
func PinPlatformToServer(client CommandRunner) error {
	if _, err := client.Run(fmt.Sprintf("kubectl label node -l %s=true %s=true --overwrite", serverNodeLabel, serviceLBNodeLabel)); err != nil {
		return fmt.Errorf("labeling the server for the service load balancer: %w", err)
	}
	var pinned []workloadRef
	for _, name := range k3sAddonDeployments {
		present, err := pinWorkload(client, "kube-system", "deployment/"+name, true)
		if err != nil {
			return err
		}
		if present {
			pinned = append(pinned, workloadRef{"kube-system", "deployment/" + name})
		}
	}
	for _, ns := range platformNamespaces {
		out, err := client.Run(fmt.Sprintf(`kubectl -n %s get deployments,statefulsets -o jsonpath='{range .items[*]}{.kind} {.metadata.name} {.metadata.ownerReferences[0].kind}{"\n"}{end}'`, ns))
		if err != nil {
			return fmt.Errorf("listing workloads in %s: %w", ns, err)
		}
		for _, w := range unownedWorkloads(out) {
			if _, err := pinWorkload(client, ns, w, false); err != nil {
				return err
			}
			pinned = append(pinned, workloadRef{ns, w})
		}
	}
	prometheus, err := pinPrometheusToServer(client)
	if err != nil {
		return err
	}
	if prometheus {
		pinned = append(pinned, prometheusStatefulSet)
	}
	for _, m := range pinned {
		if _, err := client.Run(fmt.Sprintf("kubectl -n %s rollout status %s --timeout=%s", m.namespace, m.ref, placementRolloutTimeout)); err != nil {
			return fmt.Errorf("%s in %s failed to complete its rollout on the server: %w", m.ref, m.namespace, err)
		}
	}
	return nil
}

// PinK3sAddonsToServer restores server placement for CoreDNS and metrics-server
// after k3s reapplies their manifests. Addons that are still absent are skipped.
func PinK3sAddonsToServer(client CommandRunner) error {
	for _, name := range k3sAddonDeployments {
		if _, err := pinWorkload(client, "kube-system", "deployment/"+name, true); err != nil {
			return err
		}
	}
	return nil
}

type workloadRef struct {
	namespace string
	ref       string
}

// pinWorkload adds the server selector to a workload's pod template.
// With mayBeAbsent, a missing workload returns false without an error.
func pinWorkload(client CommandRunner, ns, ref string, mayBeAbsent bool) (bool, error) {
	out, err := client.Run(fmt.Sprintf("kubectl -n %s patch %s --type strategic -p '%s'", ns, ref, serverNodeSelectorPatch))
	if err != nil {
		if mayBeAbsent && identityAbsentFromKubectl(out+" "+err.Error()) {
			return false, nil
		}
		return false, fmt.Errorf("pinning %s in %s to the server: %w", ref, ns, err)
	}
	return true, nil
}

// prometheusStatefulSet is the StatefulSet the operator generates from the
// Prometheus resource.
var prometheusStatefulSet = workloadRef{"monitoring", "statefulset/prometheus-kube-prometheus-stack-prometheus"}

// pinPrometheusToServer sets placement on the Prometheus resource so the
// operator preserves it when reconciling the StatefulSet.
// It waits for the StatefulSet selector before the caller checks the rollout.
// A missing Prometheus resource or resource type returns false without an error.
func pinPrometheusToServer(client CommandRunner) (bool, error) {
	cmd := fmt.Sprintf(`kubectl -n monitoring patch prometheuses.monitoring.coreos.com kube-prometheus-stack-prometheus --type merge -p '{"spec":{"nodeSelector":{%q:"true"}}}'`, serverNodeLabel)
	out, err := client.Run(cmd)
	if err != nil {
		if identityAbsentFromKubectl(out + " " + err.Error()) {
			return false, nil
		}
		return false, fmt.Errorf("pinning Prometheus to the server: %w", err)
	}
	wait := fmt.Sprintf(`for i in $(seq 1 60); do [ "$(kubectl -n %s get %s -o jsonpath='{.spec.template.spec.nodeSelector.%s}' 2>/dev/null)" = true ] && exit 0; sleep 5; done; exit 1`,
		prometheusStatefulSet.namespace, prometheusStatefulSet.ref, strings.ReplaceAll(serverNodeLabel, ".", `\.`))
	if _, err := client.Run(wait); err != nil {
		return false, fmt.Errorf("timed out waiting for the Prometheus operator to apply the server selector to its StatefulSet: %w", err)
	}
	return true, nil
}

// unownedWorkloads parses "Kind name owner" lines into kubectl references,
// keeping only workloads with an empty owner field.
func unownedWorkloads(listing string) []string {
	var refs []string
	for _, line := range strings.Split(listing, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		refs = append(refs, strings.ToLower(fields[0])+"/"+fields[1])
	}
	return refs
}
