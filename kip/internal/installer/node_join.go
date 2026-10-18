package installer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/netip"
	"strings"
	"time"
)

type joinTimeouts struct {
	ready, validation, schedulable, interval time.Duration
}

var defaultJoinTimeouts = joinTimeouts{ready: 5 * time.Minute, validation: 5 * time.Minute, schedulable: 5 * time.Minute, interval: 5 * time.Second}

// pollUntil retries check until it reports success or the timeout expires.
// On timeout it returns the last error, or a timeout error if check returned nil.
func pollUntil(timeout, interval time.Duration, check func() (bool, error)) error {
	deadline := time.Now().Add(timeout)
	for {
		done, err := check()
		if done {
			return nil
		}
		if time.Now().After(deadline) {
			if err == nil {
				err = fmt.Errorf("not done within %s", timeout)
			}
			return err
		}
		time.Sleep(interval)
	}
}

// WaitForWorkerStorage clears the check taint and waits for node and Longhorn
// readiness. The caller must cordon the node first to keep workloads off it.
func WaitForWorkerStorage(master CommandRunner, node string) error {
	return waitForWorkerStorage(master, node, defaultJoinTimeouts)
}

func waitForWorkerStorage(master CommandRunner, node string, t joinTimeouts) error {
	// Clear a previous attempt's taint so storage DaemonSet pods can start.
	if _, err := master.Run(removeTaintCommand(node, checkingTaint)); err != nil {
		return fmt.Errorf("removing the %s taint from %s: %w", checkingTaint, node, err)
	}
	ready := func() (bool, error) {
		out, err := master.Run(fmt.Sprintf(`kubectl get node %s -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}' 2>/dev/null`, node))
		if err != nil {
			return false, fmt.Errorf("reading %s: %w", node, err)
		}
		if strings.TrimSpace(out) != "True" {
			return false, fmt.Errorf("%s is not Ready", node)
		}
		return true, nil
	}
	if err := pollUntil(t.ready, t.interval, ready); err != nil {
		return err
	}
	// Wait for these storage pods before the check taint blocks new scheduling.
	for _, ds := range []struct{ app, selector string }{
		{"longhorn-manager", "app=longhorn-manager"},
		{"longhorn-csi-plugin", "app=longhorn-csi-plugin"},
		{"Longhorn engine image", "longhorn.io/component=engine-image"},
	} {
		app := ds.app
		running := func() (bool, error) {
			out, err := master.Run(fmt.Sprintf(`kubectl -n longhorn-system get pods -l %s --field-selector spec.nodeName=%s -o jsonpath='{range .items[*]}{range .status.containerStatuses[*]}{.ready}{" "}{end}{end}' 2>/dev/null`, ds.selector, node))
			if err != nil {
				return false, fmt.Errorf("reading %s on %s: %w", app, node, err)
			}
			states := strings.Fields(out)
			if len(states) == 0 {
				return false, fmt.Errorf("%s has not started on %s", app, node)
			}
			for _, s := range states {
				if s != "true" {
					return false, fmt.Errorf("%s is not ready on %s", app, node)
				}
			}
			return true, nil
		}
		if err := pollUntil(t.ready, t.interval, running); err != nil {
			return err
		}
	}
	return nil
}

// Pin the validation image contents: the pod runs in kube-system and tolerates all taints.
const validationImage = "docker.io/library/busybox:1.37.0@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"

const validationOK = "KIPPER-NODE-CHECK-OK"

const validationLabel = "kipper.run/node-check"

// validationNodeHash keeps resource names and labels within Kubernetes length limits.
func validationNodeHash(node string) string {
	sum := sha256.Sum256([]byte(node))
	return hex.EncodeToString(sum[:])[:10]
}

// validationName includes the attempt ID to distinguish results from earlier joins.
func validationName(node, attempt string) string {
	return "kipper-node-check-" + validationNodeHash(node) + "-" + attempt
}

// validationManifest pins a test pod to the worker and tolerates its taints.
// It checks Service DNS, queries serverDNS directly for cross-node pod
// connectivity, and reads and writes a single-replica Longhorn volume.
func validationManifest(node, attempt, serverDNS string) string {
	name := validationName(node, attempt)
	script := fmt.Sprintf("set -e; nslookup kubernetes.default.svc.cluster.local; nslookup kubernetes.default.svc.cluster.local %s; "+
		"echo kipper > /data/check; sync; [ \"$(cat /data/check)\" = kipper ]; echo %s", serverDNS, validationOK)
	return fmt.Sprintf(`apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: %[1]s
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: kipper
    %[5]s: "%[6]s"
spec:
  accessModes: [ReadWriteOnce]
  storageClassName: longhorn-single
  resources:
    requests:
      storage: 128Mi
---
apiVersion: v1
kind: Pod
metadata:
  name: %[1]s
  namespace: kube-system
  labels:
    app.kubernetes.io/managed-by: kipper
    %[5]s: "%[6]s"
spec:
  nodeName: %[2]s
  restartPolicy: Never
  tolerations:
    - operator: Exists
  containers:
    - name: check
      image: %[3]s
      command: ["sh", "-c", %[4]q]
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: %[1]s
`, name, node, validationImage, script, validationLabel, validationNodeHash(node))
}

// ValidateWorker checks host registry authentication, containerd's mirror
// configuration and direct access to Zot, pod DNS, connectivity to CoreDNS on
// serverNode, Longhorn volume reads and writes, and kubelet access through
// the API proxy and directly at nodeAddress.
func ValidateWorker(master, worker CommandRunner, j WorkerJoin, node, serverNode, nodeAddress string) error {
	return validateWorker(master, worker, j, node, serverNode, nodeAddress, defaultJoinTimeouts)
}

func validateWorker(master, worker CommandRunner, j WorkerJoin, node, serverNode, nodeAddress string, t joinTimeouts) error {
	if err := verifyZotAuth(worker, j.zotIP, j.pullPassword); err != nil {
		return fmt.Errorf("%s cannot use the cluster registry: %w", node, err)
	}
	if err := probeZotPull(worker, node, j.zotIP); err != nil {
		return err
	}
	dns, err := master.Run(fmt.Sprintf(`kubectl -n kube-system get pods -l k8s-app=kube-dns --field-selector spec.nodeName=%s,status.phase=Running -o jsonpath='{.items[0].status.podIP}' 2>/dev/null`, serverNode))
	if err != nil || strings.TrimSpace(dns) == "" {
		return fmt.Errorf("finding CoreDNS on the server to check pod traffic from %s: %v", node, err)
	}
	attempt, err := generateSecret(3)
	if err != nil {
		return fmt.Errorf("naming the validation pod: %w", err)
	}
	name := validationName(node, attempt)
	// Bound cleanup waits because pods on unreachable nodes can remain terminating.
	cleanup := func(selector string) error {
		_, err := master.Run(fmt.Sprintf("timeout 90 kubectl -n kube-system delete pod %[1]s --ignore-not-found --timeout=60s >/dev/null && timeout 90 kubectl -n kube-system delete pvc %[1]s --ignore-not-found --timeout=60s >/dev/null", selector))
		return err
	}
	_ = cleanup("-l " + validationLabel + "=" + validationNodeHash(node))
	defer func() {
		if err := cleanup(name); err != nil {
			fmt.Printf("  ⚠  could not remove the validation pod and volume %s in kube-system: %v\n", name, err)
		}
	}()
	if _, err := master.RunStdin("kubectl create -f - >/dev/null", strings.NewReader(validationManifest(node, attempt, strings.TrimSpace(dns)))); err != nil {
		return fmt.Errorf("starting the validation pod on %s: %w", node, err)
	}
	phase := ""
	finished := func() (bool, error) {
		out, err := master.Run(fmt.Sprintf("kubectl -n kube-system get pod %s -o jsonpath='{.status.phase}' 2>/dev/null", name))
		if err != nil {
			return false, fmt.Errorf("reading the validation pod: %w", err)
		}
		phase = strings.TrimSpace(out)
		return phase == "Succeeded" || phase == "Failed", nil
	}
	waitErr := pollUntil(t.validation, t.interval, finished)
	logs, _ := master.Run(fmt.Sprintf("kubectl -n kube-system logs %s --tail=20 2>&1", name))
	if waitErr == nil && phase == "Succeeded" && strings.Contains(logs, validationOK) {
		return checkKubelet(master, node, nodeAddress)
	}
	events, _ := master.Run(fmt.Sprintf("kubectl -n kube-system get events --field-selector involvedObject.name=%s -o jsonpath='{range .items[*]}{.reason}: {.message}{\"\\n\"}{end}' 2>/dev/null | tail -n 5", name))
	return fmt.Errorf("the validation pod on %s did not pass (phase %q)%s%s; check that the provider firewall allows 51820/udp and 10250/tcp between the nodes", node, phase, indented("pod output", logs), indented("events", events))
}

const containerdHostsDir = "/var/lib/rancher/k3s/agent/etc/containerd/certs.d"

const containerdLog = "/var/lib/rancher/k3s/agent/containerd/containerd.log"

// probeZotPull checks that Zot's ClusterIP is listed first in the registry's
// mirror configuration, then probes it directly with containerd. Accepting a
// missing-manifest response avoids requiring a stored probe image.
func probeZotPull(worker CommandRunner, node, zotIP string) error {
	endpoint := zotIP + ":5000"
	hosts, err := worker.Run("cat " + containerdHostsDir + "/" + zotRegistryHost + "/hosts.toml")
	if err != nil {
		return fmt.Errorf("reading the registry mirror for %s on %s: %w", zotRegistryHost, node, err)
	}
	mirror := `[host."https://` + endpoint + `/v2"]`
	if i := strings.Index(hosts, `[host."`); i < 0 || !strings.HasPrefix(hosts[i:], mirror) {
		return fmt.Errorf("containerd on %s does not send %s to Zot at %s first; check /etc/rancher/k3s/registries.yaml on %s", node, zotRegistryHost, endpoint, node)
	}
	ref := endpoint + "/kipper-system/node-check:absent"
	out, err := worker.Run("timeout 60 k3s crictl pull " + ref + " 2>&1")
	if err == nil {
		return nil
	}
	// Accept a missing image, but reject resolution failures that can occur
	// before containerd contacts the registry.
	if strings.Contains(out, "failed to resolve reference") && strings.Contains(out, ref+": not found") && !strings.Contains(out, "no resolve hosts") {
		return nil
	}
	reason := fmt.Sprintf("crictl failed without output (%v)", err)
	for _, line := range strings.Split(out, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			reason = line
			break
		}
	}
	return fmt.Errorf("containerd on %s cannot pull from the cluster registry: %s; see %s on %s", node, reason, containerdLog, node)
}

func indented(title, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	return "\n      " + title + ":\n        " + strings.ReplaceAll(text, "\n", "\n        ")
}

// checkKubelet checks proxy health and direct TCP access to port 10250.
// The direct probe covers the path used by metrics scrapers, which the
// API proxy can bypass through the agent tunnel.
func checkKubelet(master CommandRunner, node, address string) error {
	out, err := master.Run(fmt.Sprintf("kubectl get --raw /api/v1/nodes/%s/proxy/healthz 2>&1", node))
	if err != nil || strings.TrimSpace(out) != "ok" {
		return fmt.Errorf("the API server cannot reach the kubelet on %s: %s", node, strings.TrimSpace(out))
	}
	addr, err := netip.ParseAddr(address)
	if err != nil || !addr.Is4() {
		return fmt.Errorf("the address of %s, %q, is not an IPv4 address", node, address)
	}
	if !tcpReachable(master, addr, kubeletPort) {
		return fmt.Errorf("the server cannot connect to the kubelet on %s at %s:%s; allow it in the provider's firewall", node, address, kubeletPort)
	}
	return nil
}

const checkingTaint = "kipper.run/checking"

// removeTaintCommand builds a command that removes taints matching key.
// A successful read must confirm the exact key is absent, allowing safe retries.
func removeTaintCommand(node, key string) string {
	return fmt.Sprintf(`(kubectl taint nodes %[1]s %[2]s- >/dev/null 2>&1; keys=$(kubectl get node %[1]s -o jsonpath='{.spec.taints[*].key}') || exit 1; for k in $keys; do [ "$k" = %[2]s ] && exit 1; done; exit 0)`, node, key)
}

// setLonghornScheduling retries because Longhorn's webhook refuses node
// changes while a newly registered node's disks are still syncing.
func setLonghornScheduling(master CommandRunner, node string, allow bool, t joinTimeouts) error {
	spec := `{"spec":{"allowScheduling":false}}`
	if allow {
		// Longhorn refuses scheduling while an eviction is requested, which an
		// interrupted kip node remove leaves behind.
		spec = `{"spec":{"allowScheduling":true,"evictionRequested":false}}`
	}
	return pollUntil(t.ready, t.interval, func() (bool, error) {
		_, err := master.Run(fmt.Sprintf(`kubectl -n longhorn-system patch nodes.longhorn.io %s --type merge -p '%s' >/dev/null`, node, spec))
		return err == nil, err
	})
}

// OpenWorkerForChecks replaces the cordon with a NoSchedule taint to allow
// Longhorn volume attachment, then waits for instance managers to run.
// It disables local replica scheduling so the check exercises remote storage.
func OpenWorkerForChecks(master CommandRunner, node string) error {
	return openWorkerForChecks(master, node, defaultJoinTimeouts)
}

func openWorkerForChecks(master CommandRunner, node string, t joinTimeouts) error {
	if err := setLonghornScheduling(master, node, false, t); err != nil {
		return fmt.Errorf("disabling Longhorn replica scheduling on %s: %w", node, err)
	}
	if _, err := master.Run(fmt.Sprintf("kubectl taint nodes %s %s=true:NoSchedule --overwrite >/dev/null", node, checkingTaint)); err != nil {
		return fmt.Errorf("keeping workloads off %s during the checks: %w", node, err)
	}
	if _, err := master.Run("kubectl uncordon " + node + " >/dev/null"); err != nil {
		return fmt.Errorf("uncordoning %s for the storage check: %w", node, err)
	}
	running := func() (bool, error) {
		out, err := master.Run(fmt.Sprintf(`kubectl -n longhorn-system get instancemanagers.longhorn.io -o jsonpath='{range .items[?(@.spec.nodeID==%q)]}{.status.currentState}{" "}{end}' 2>/dev/null`, node))
		if err != nil {
			return false, fmt.Errorf("reading the Longhorn instance managers: %w", err)
		}
		states := strings.Fields(out)
		for _, s := range states {
			if s != "running" {
				return false, fmt.Errorf("the Longhorn instance manager is not running on %s (%s)", node, s)
			}
		}
		if len(states) == 0 {
			return false, fmt.Errorf("the Longhorn instance manager is not running on %s", node)
		}
		return true, nil
	}
	return pollUntil(t.ready, t.interval, running)
}

// UncordonWorker enables workload and replica scheduling, removes the check
// taint, and waits for Longhorn to report the node as schedulable.
func UncordonWorker(master CommandRunner, node string) error {
	return uncordonWorker(master, node, defaultJoinTimeouts)
}

func uncordonWorker(master CommandRunner, node string, t joinTimeouts) error {
	if _, err := master.Run("kubectl uncordon " + node + " >/dev/null"); err != nil {
		return fmt.Errorf("uncordoning %s: %w", node, err)
	}
	if err := setLonghornScheduling(master, node, true, t); err != nil {
		return fmt.Errorf("enabling Longhorn replica scheduling on %s: %w", node, err)
	}
	if _, err := master.Run(removeTaintCommand(node, checkingTaint)); err != nil {
		return fmt.Errorf("removing the %s taint from %s: %w", checkingTaint, node, err)
	}
	schedulable := func() (bool, error) {
		out, err := master.Run(fmt.Sprintf(`kubectl -n longhorn-system get nodes.longhorn.io %s -o jsonpath='{.status.conditions[?(@.type=="Schedulable")].status}' 2>/dev/null`, node))
		if err != nil {
			return false, fmt.Errorf("reading the Longhorn node %s: %w", node, err)
		}
		if strings.TrimSpace(out) != "True" {
			return false, fmt.Errorf("the Longhorn node %s does not accept replicas yet", node)
		}
		return true, nil
	}
	return pollUntil(t.schedulable, t.interval, schedulable)
}
