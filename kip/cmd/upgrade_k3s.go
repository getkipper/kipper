package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

const k3sUpgradeCommand = "kip upgrade --k3s"

// upgradeClusterK3s moves the server and then each ready worker onto the k3s
// release pinned by this kip version, over SSH. It records the server if needed
// and checks the cluster upgrade plan before changing k3s.
func upgradeClusterK3s(cluster *config.Cluster, sshKey string) error {
	if cluster.Host == "" {
		return fmt.Errorf("--k3s needs SSH access to the server, and this cluster has no server address saved")
	}
	explicitKey, fallbackKey := resolveSSHKey(sshKey, cluster)
	provider, err := clusterProvider(cluster, cluster.Host, explicitKey, fallbackKey)
	if err != nil {
		return err
	}
	if err := provider.Connect(); err != nil {
		return fmt.Errorf("connecting to %s: %w", cluster.Host, err)
	}
	defer func() { _ = provider.Close() }()
	server := provider.Client()

	records, err := recordNodes(server, cluster)
	if err != nil {
		return err
	}
	fmt.Printf("  k3s\n")
	if err := installer.UpgradeServerK3s(os.Stdout, server, records, k3sUpgradeCommand); err != nil {
		return err
	}
	dial := func(host, _ string) (installer.FirewallRunner, func(), error) {
		conn, err := dialNodeSSH(host, sshKey, cluster)
		if err != nil {
			return nil, nil, err
		}
		return conn, func() { _ = conn.Close() }, nil
	}
	if err := upgradeAgentsAfterInstall(os.Stdout, server, dial, k3sUpgradeCommand); err != nil {
		return err
	}
	fmt.Println()
	return nil
}

// k3sAuthenticationWait allows time for OIDC authentication to recover after a
// restart. The authenticator retries every ten seconds and needs Traefik to reach Dex.
const k3sAuthenticationWait = 2 * time.Minute

// waitForAPIAuthentication uses SelfSubjectReview to check that the API accepts
// cs's credentials. OIDC authentication may still be initializing after the node
// reports Ready. It retries Unauthorized responses at interval until timeout or
// context cancellation, and returns other errors immediately. Every error names
// the command to run again.
func waitForAPIAuthentication(ctx context.Context, cs kubernetes.Interface, timeout, interval time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		_, err := cs.AuthenticationV1().SelfSubjectReviews().Create(ctx, &authnv1.SelfSubjectReview{}, metav1.CreateOptions{})
		if err == nil {
			return nil
		}
		rejected := fmt.Errorf("the Kubernetes API did not accept kip's credentials after k3s restarted (%w). Run %s again once 'kip auth verify' passes", err, k3sUpgradeCommand)
		if !apierrors.IsUnauthorized(err) {
			return rejected
		}
		select {
		case <-ctx.Done():
			return rejected
		case <-time.After(interval):
		}
	}
}

// preflightK3s checks the upgrade plan over SSH using the existing node records,
// after network recovery and before node and storage changes. The upgrade step
// checks again after refreshing the records.
func preflightK3s(cluster *config.Cluster, sshKey string) error {
	if cluster.Host == "" {
		return fmt.Errorf("--k3s needs SSH access to the server, and this cluster has no server address saved")
	}
	explicitKey, fallbackKey := resolveSSHKey(sshKey, cluster)
	provider, err := clusterProvider(cluster, cluster.Host, explicitKey, fallbackKey)
	if err != nil {
		return err
	}
	if err := provider.Connect(); err != nil {
		return fmt.Errorf("connecting to %s: %w", cluster.Host, err)
	}
	defer func() { _ = provider.Close() }()
	records, err := installer.ReadNodeRecords(provider.Client())
	if err != nil {
		return err
	}
	_, err = installer.CheckK3sUpgrade(provider.Client(), records, k3sUpgradeCommand)
	return err
}

// checkK3sUpgrade reports what --k3s would do, read from the API: whether the
// server needs the pinned release and how many workers recorded as ready follow.
// It returns an error for API, inventory or version checks that fail. This check
// uses Node versions, so it cannot detect a newer binary on disk or test SSH access.
func checkK3sUpgrade(ctx context.Context, cs kubernetes.Interface, out io.Writer) error {
	nodes, err := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}
	var records []installer.NodeRecord
	cm, err := cs.CoreV1().ConfigMaps(installer.NodeRecordsNamespace).Get(ctx, installer.NodeRecordsConfigMap, metav1.GetOptions{})
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("reading the node records: %w", err)
	default:
		if records, err = installer.ParseNodeRecordData(cm.Data); err != nil {
			return err
		}
	}
	kubelets := map[string]string{}
	server := ""
	identities := make([]installer.NodeIdentity, 0, len(nodes.Items))
	inventory := records
	for _, n := range nodes.Items {
		kubelets[n.Name] = n.Status.NodeInfo.KubeletVersion
		identities = append(identities, installer.NodeIdentity{Name: n.Name, MachineID: n.Status.NodeInfo.MachineID})
		if _, ok := n.Labels["node-role.kubernetes.io/control-plane"]; ok {
			server = n.Status.NodeInfo.KubeletVersion
			// kip upgrade records an unrecorded server before checking the
			// inventory, so the check counts it as recorded the same way.
			if _, recorded := installer.ServerRecord(records); !recorded {
				inventory = append(append([]installer.NodeRecord(nil), records...), installer.NodeRecord{
					Name: n.Name, MachineID: n.Status.NodeInfo.MachineID, Role: installer.NodeRoleServer, State: installer.NodeStateReady,
				})
			}
		}
	}
	if server == "" {
		return errors.New("no server node found, so kip cannot tell which k3s version the cluster runs")
	}
	unrecorded, err := installer.UnrecordedIdentities(identities, inventory)
	if err != nil {
		return err
	}
	if len(unrecorded) > 0 {
		_, _ = fmt.Fprintf(out, "  ✗  k3s: unrecorded nodes %s. Record each with 'kip node add --host <address>'\n", strings.Join(unrecorded, ", "))
		return fmt.Errorf("the cluster's k3s versions need a decision before '%s' can run", k3sUpgradeCommand)
	}

	pin := installer.PinnedK3sVersion()
	plan, err := installer.PlanK3sUpgrade(records, kubelets, server, k3sUpgradeCommand)
	if err != nil {
		_, _ = fmt.Fprintf(out, "  ✗  k3s: %v\n", err)
		return fmt.Errorf("the cluster's k3s versions need a decision before '%s' can run", k3sUpgradeCommand)
	}
	workers := len(plan.Workers)
	switch {
	case plan.MoveServer && workers > 0:
		_, _ = fmt.Fprintf(out, "  ✔  k3s: %s moves the server from %s to %s, then %d worker(s)\n", k3sUpgradeCommand, plan.Server, pin, workers)
	case plan.MoveServer:
		_, _ = fmt.Fprintf(out, "  ✔  k3s: %s moves the server from %s to %s\n", k3sUpgradeCommand, plan.Server, pin)
	case workers > 0:
		_, _ = fmt.Fprintf(out, "  ✔  k3s: the server already runs %s, and %s brings %d worker(s) to it\n", pin, k3sUpgradeCommand, workers)
	default:
		_, _ = fmt.Fprintf(out, "  ✔  k3s: every ready node already runs %s\n", pin)
	}
	return nil
}
