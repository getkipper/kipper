package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/velerolabel"
)

const drillNamespace = backupscope.DrillNamespace

var podVolumeRestoreGVR = schema.GroupVersionResource{
	Group:    "velero.io",
	Version:  "v1",
	Resource: "podvolumerestores",
}

var backupDrillCmd = &cobra.Command{
	Use:   "drill <namespace>",
	Short: "Restore a namespace's services from the latest backup into an isolated namespace and count their rows",
	Long: `Restores the database and other service pods of one namespace, with their
volumes and credentials, from its latest completed backup into the namespace
kipper-restore-drill. App pods, Ingresses and jobs are excluded to avoid running
applications against live services or claiming live hostnames. A NetworkPolicy
allows traffic between the restored pods and outgoing cluster DNS queries.
It requires a NetworkPolicy controller and allows the node traffic exempted by
Kubernetes. For Postgres and MySQL, the drill counts rows in user tables across
databases with count(*). The drill namespace is deleted afterwards unless
--keep is set or the restore is still running or its state cannot be checked.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runBackupDrill,
}

func init() {
	backupDrillCmd.Flags().String("backup", "", "backup to restore (default: the latest completed backup that covers the namespace)")
	backupDrillCmd.Flags().Bool("keep", false, "keep the drill namespace for inspection. Delete it later with --clean")
	backupDrillCmd.Flags().Bool("clean", false, "delete the drill namespace left by --keep or an interrupted drill after waiting for running drill restores")
	backupCmd.AddCommand(backupDrillCmd)
}

type tableCount struct {
	Table string
	Rows  int64
}

func runBackupDrill(cmd *cobra.Command, args []string) error {
	source := args[0]
	backupName, _ := cmd.Flags().GetString("backup")
	keep, _ := cmd.Flags().GetBool("keep")
	clean, _ := cmd.Flags().GetBool("clean")

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	// Cancel on Ctrl-C or SIGTERM so deferred cleanup can still run.
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	clientset := k8sClient.Clientset()
	dyn := k8sClient.Dynamic()
	if clean {
		return cleanDrill(ctx, clientset, dyn)
	}
	if !backupsConfiguredOn(ctx, dyn) {
		return errBackupsNotConfigured
	}

	if _, err := clientset.CoreV1().Namespaces().Get(ctx, drillNamespace, metav1.GetOptions{}); err == nil {
		return fmt.Errorf("namespace %s already exists from an earlier drill. Remove it with kip backup drill %s --clean", drillNamespace, source)
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("checking for namespace %s: %w", drillNamespace, err)
	}

	backup, err := chooseDrillBackup(ctx, dyn, source, backupName)
	if err != nil {
		return err
	}
	fmt.Printf("\n  Drilling %s from backup %s (completed %s)\n", source, backup.GetName(), completionTime(backup))

	if _, err := clientset.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: drillNamespace}}, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating namespace %s: %w", drillNamespace, err)
	}
	var restoreName string
	if !keep {
		defer func() {
			if err := finishDrill(context.Background(), clientset, dyn, restoreName, source); err != nil {
				fmt.Printf("  ⚠  %v\n", err)
			}
		}()
	}
	if _, err := clientset.NetworkingV1().NetworkPolicies(drillNamespace).Create(ctx, drillNetworkPolicy(), metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("isolating namespace %s: %w", drillNamespace, err)
	}

	if restores, err := dyn.Resource(restoreGVR).Namespace("velero").List(ctx, metav1.ListOptions{}); err == nil {
		for _, name := range unfinishedRestores(restores.Items) {
			fmt.Printf("  ·  Restore %s is unfinished. Velero processes restore requests one at a time, so the drill may wait for it\n", name)
		}
	}

	restoreName = "drill-" + source + "-" + time.Now().Format("20060102-150405")
	restore := &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "velero.io/v1",
		"kind":       "Restore",
		"metadata":   map[string]interface{}{"name": restoreName, "namespace": "velero"},
		"spec":       drillRestoreSpec(backup.GetName(), source),
	}}
	if _, err := dyn.Resource(restoreGVR).Namespace("velero").Create(ctx, restore, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating restore: %w", err)
	}
	fmt.Printf("  ·  Restore %s started\n", restoreName)
	phase, err := waitForRestorePhase(ctx, dyn, restoreName, time.Hour)
	if err != nil {
		return err
	}
	if phase != "Completed" {
		return fmt.Errorf("restore %s ended with phase %s. Use kubectl -n velero describe restore %s to inspect it, and --keep on the drill command to retain restored services", restoreName, phase, restoreName)
	}

	pods, err := clientset.CoreV1().Pods(drillNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing restored pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return fmt.Errorf("backup %s contains no service pods from namespace %s to test", backup.GetName(), source)
	}
	if err := requireVolumesRestored(ctx, dyn, restoreName, pods.Items); err != nil {
		return err
	}

	kubeconfig, err := clientcmd.BuildConfigFromFlags("", cluster.Kubeconfig)
	if err != nil {
		return fmt.Errorf("loading kubeconfig: %w", err)
	}
	execClient, err := kubernetes.NewForConfig(k8s.WithoutPodSecurityWarnings(kubeconfig))
	if err != nil {
		return fmt.Errorf("creating the Kubernetes client: %w", err)
	}
	failed := false
	for i := range pods.Items {
		pod := &pods.Items[i]
		if err := reportServiceRows(ctx, execClient, kubeconfig, pod); err != nil {
			fmt.Printf("  ✘  %s: %v\n", pod.Name, err)
			failed = true
		}
	}

	fmt.Printf("\n  Drill of %s from backup %s, completed %s.\n", source, backup.GetName(), completionTime(backup))
	if keep {
		fmt.Printf("  The restored services stay in namespace %s. Remove it with kip backup drill %s --clean.\n\n", drillNamespace, source)
	}
	if failed {
		return errors.New("the drill could not check every restored service")
	}
	return nil
}

// chooseDrillBackup returns a Completed backup covering source. An empty name
// selects the latest by completion timestamp. An ineligible or missing backup
// returns an error.
func chooseDrillBackup(ctx context.Context, dyn dynamic.Interface, source, name string) (*unstructured.Unstructured, error) {
	backups := dyn.Resource(backupGVR).Namespace("velero")
	if name != "" {
		b, err := backups.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("reading backup %s: %w", name, err)
		}
		if phase, _, _ := unstructured.NestedString(b.Object, "status", "phase"); phase != "Completed" {
			return nil, fmt.Errorf("backup %s is %s. A drill requires a Completed backup", name, phase)
		}
		if !backupCovers(b, source) {
			return nil, fmt.Errorf("backup %s does not cover namespace %s", name, source)
		}
		return b, nil
	}
	list, err := backups.List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("listing backups: %w", err)
	}
	return latestBackupFor(list.Items, source)
}

// latestBackupFor returns the Completed backup with the latest completion
// timestamp whose namespace selection covers namespace, or an error if none does.
func latestBackupFor(backups []unstructured.Unstructured, namespace string) (*unstructured.Unstructured, error) {
	var best *unstructured.Unstructured
	for i := range backups {
		b := &backups[i]
		if phase, _, _ := unstructured.NestedString(b.Object, "status", "phase"); phase != "Completed" {
			continue
		}
		if !backupCovers(b, namespace) {
			continue
		}
		if best == nil || completionTime(b) > completionTime(best) {
			best = b
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no completed backup covers namespace %s", namespace)
	}
	return best, nil
}

func backupCovers(b *unstructured.Unstructured, namespace string) bool {
	included, _, _ := unstructured.NestedStringSlice(b.Object, "spec", "includedNamespaces")
	excluded, _, _ := unstructured.NestedStringSlice(b.Object, "spec", "excludedNamespaces")
	for _, ns := range excluded {
		if ns == namespace {
			return false
		}
	}
	if len(included) == 0 {
		return true
	}
	for _, ns := range included {
		if ns == namespace || ns == "*" {
			return true
		}
	}
	return false
}

func completionTime(b *unstructured.Unstructured) string {
	t, _, _ := unstructured.NestedString(b.Object, "status", "completionTimestamp")
	return t
}

func labelExists(key string) map[string]interface{} {
	return map[string]interface{}{"matchExpressions": []interface{}{map[string]interface{}{"key": key, "operator": "Exists"}}}
}

// drillRestoreSpec selects source's service-labelled pods, Secrets and ConfigMaps
// for the drill namespace. Velero adds each pod's PVC and PV as dependencies.
// Including PVs lets Velero clear the PVC's old binding so a fresh volume can
// receive the restored files.
func drillRestoreSpec(backupName, source string) map[string]interface{} {
	spec := map[string]interface{}{
		"backupName":         backupName,
		"includedNamespaces": []interface{}{source},
		"namespaceMapping":   map[string]interface{}{source: drillNamespace},
		"includedResources":  []interface{}{"pods", "persistentvolumeclaims", "persistentvolumes", "secrets", "configmaps"},
		// TLS services also need certificate Secrets and pg_hba ConfigMaps
		// labelled with their service name under tls-for. Include these
		// alongside the pods and credentials selected by service-type.
		"orLabelSelectors": []interface{}{
			labelExists("kipper.run/service-type"),
			labelExists("kipper.run/tls-for"),
		},
	}
	backupscope.Restore(spec, false)
	return spec
}

// drillNetworkPolicy allows traffic between drill pods and outgoing DNS queries
// to kube-dns in kube-system on TCP and UDP port 53. Enforcement requires a
// NetworkPolicy controller and retains Kubernetes' node-traffic exemptions.
func drillNetworkPolicy() *networkingv1.NetworkPolicy {
	udp, tcp := corev1.ProtocolUDP, corev1.ProtocolTCP
	dns := intstr.FromInt32(53)
	sameNamespace := []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "isolate-drill", Namespace: drillNamespace},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{From: sameNamespace}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{To: sameNamespace},
				{
					To: []networkingv1.NetworkPolicyPeer{{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"kubernetes.io/metadata.name": "kube-system"}},
						PodSelector:       &metav1.LabelSelector{MatchLabels: map[string]string{"k8s-app": "kube-dns"}},
					}},
					Ports: []networkingv1.NetworkPolicyPort{{Protocol: &udp, Port: &dns}, {Protocol: &tcp, Port: &dns}},
				},
			},
		},
	}
}

// finishDrill requests namespace deletion if the restore has finished or is absent.
// A running restore could recreate the namespace, so a running or unknown restore
// state returns an error with cleanup instructions instead.
func finishDrill(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface, restoreName, source string) error {
	if restoreName != "" {
		r, err := dyn.Resource(restoreGVR).Namespace("velero").Get(ctx, restoreName, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return fmt.Errorf("could not determine whether restore %s has finished (%w). Once it has, remove the drill with kip backup drill %s --clean", restoreName, err, source)
		case !restoreFinished(r):
			return fmt.Errorf("restore %s is still running. Once it finishes, remove the drill with kip backup drill %s --clean", restoreName, source)
		}
	}
	return deleteDrillNamespace(ctx, clientset)
}

// cleanDrill waits up to an hour for each unfinished restore named with the
// drill- prefix, then requests namespace deletion and waits up to five minutes
// for removal. A restore wait error leaves the namespace in place.
func cleanDrill(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface) error {
	list, err := dyn.Resource(restoreGVR).Namespace("velero").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing restores: %w", err)
	}
	for i := range list.Items {
		r := &list.Items[i]
		if !strings.HasPrefix(r.GetName(), "drill-") || restoreFinished(r) {
			continue
		}
		fmt.Printf("  ·  Waiting for restore %s to finish before removing the drill\n", r.GetName())
		if _, err := waitForRestorePhase(ctx, dyn, r.GetName(), time.Hour); err != nil {
			return err
		}
	}
	if err := deleteDrillNamespace(ctx, clientset); err != nil {
		return err
	}
	if err := waitForDrillNamespaceGone(ctx, clientset, 5*time.Minute); err != nil {
		return err
	}
	fmt.Printf("  ✔  Removed namespace %s\n", drillNamespace)
	return nil
}

// waitForDrillNamespaceGone waits for namespace removal so the next drill can
// recreate it. It polls every two seconds and returns an error on lookup failure,
// context cancellation or expiry of limit.
func waitForDrillNamespaceGone(ctx context.Context, clientset kubernetes.Interface, limit time.Duration) error {
	namespace := drillNamespace
	deadline := time.Now().Add(limit)
	for {
		_, err := clientset.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking namespace %s: %w", namespace, err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("namespace %s is still being removed after %s", namespace, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// unfinishedRestores returns names of restores that may delay a new restore,
// in input order. Missing or unknown phases count as unfinished.
func unfinishedRestores(restores []unstructured.Unstructured) []string {
	var names []string
	for i := range restores {
		if !restoreFinished(&restores[i]) {
			names = append(names, restores[i].GetName())
		}
	}
	return names
}

func restoreFinished(r *unstructured.Unstructured) bool {
	switch phase, _, _ := unstructured.NestedString(r.Object, "status", "phase"); phase {
	case "Completed", "PartiallyFailed", "Failed", "FailedValidation":
		return true
	}
	return false
}

func deleteDrillNamespace(ctx context.Context, clientset kubernetes.Interface) error {
	err := clientset.CoreV1().Namespaces().Delete(ctx, drillNamespace, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting namespace %s: %w", drillNamespace, err)
	}
	return nil
}

func waitForRestorePhase(ctx context.Context, dyn dynamic.Interface, name string, limit time.Duration) (string, error) {
	deadline := time.Now().Add(limit)
	for {
		r, err := dyn.Resource(restoreGVR).Namespace("velero").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", fmt.Errorf("reading restore %s: %w", name, err)
		}
		switch phase, _, _ := unstructured.NestedString(r.Object, "status", "phase"); phase {
		case "Completed", "PartiallyFailed", "Failed", "FailedValidation":
			return phase, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("restore %s did not finish within %s", name, limit)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// requireVolumesRestored checks that every PVC-backed volume of the restored
// pods has a completed PodVolumeRestore from this restore.
func requireVolumesRestored(ctx context.Context, dyn dynamic.Interface, restoreName string, pods []corev1.Pod) error {
	list, err := dyn.Resource(podVolumeRestoreGVR).Namespace("velero").List(ctx, metav1.ListOptions{
		LabelSelector: "velero.io/restore-name=" + velerolabel.ValidName(restoreName),
	})
	if err != nil {
		return fmt.Errorf("listing volume restores: %w", err)
	}
	done := map[string]bool{}
	for _, v := range list.Items {
		pod, _, _ := unstructured.NestedString(v.Object, "spec", "pod", "name")
		volume, _, _ := unstructured.NestedString(v.Object, "spec", "volume")
		if phase, _, _ := unstructured.NestedString(v.Object, "status", "phase"); phase == "Completed" {
			done[pod+"/"+volume] = true
		}
	}
	var missing []string
	for _, pod := range pods {
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim != nil && !done[pod.Name+"/"+vol.Name] {
				missing = append(missing, pod.Name+"/"+vol.Name)
			}
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return fmt.Errorf("these volumes were not restored from the backup: %s", strings.Join(missing, ", "))
	}
	return nil
}

func reportServiceRows(ctx context.Context, clientset kubernetes.Interface, restConfig *rest.Config, pod *corev1.Pod) error {
	serviceType := pod.Labels["kipper.run/service-type"]
	if err := waitForPodRunning(ctx, clientset, pod.Name, 5*time.Minute); err != nil {
		return err
	}
	command, script, ok := rowCountCommand(serviceType)
	if !ok {
		fmt.Printf("  ✔  %s (%s): volume restored. Row checks are unavailable for this service type\n", pod.Name, serviceType)
		return nil
	}
	exec := podExec(clientset, restConfig, drillNamespace, pod.Name, workloadContainerName(pod, pod.Labels["app"]))
	var stdout, stderr bytes.Buffer
	if err := exec(ctx, command, strings.NewReader(script), &stdout, &stderr); err != nil {
		return fmt.Errorf("counting rows: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	counts, err := parseRowCounts(stdout.String())
	if err != nil {
		return err
	}
	fmt.Printf("  ✔  %s (%s): %d tables\n", pod.Name, serviceType, len(counts))
	for _, c := range counts {
		fmt.Printf("       %-50s %d\n", c.Table, c.Rows)
	}
	if serviceType == "postgres" {
		return checkConsistentDump(ctx, exec, pod.Name)
	}
	return nil
}

func waitForPodRunning(ctx context.Context, clientset kubernetes.Interface, name string, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for {
		p, err := clientset.CoreV1().Pods(drillNamespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading pod %s: %w", name, err)
		}
		if podReady(p) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pod %s did not become ready within %s", name, limit)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func podReady(p *corev1.Pod) bool {
	if p.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range p.Status.ContainerStatuses {
		if !c.Ready {
			return false
		}
	}
	return true
}

// rowCountCommand returns a command and stdin script that count user-table rows
// with count(*). Postgres emits "database.schema.table|rows" for connectable,
// non-template databases. MySQL emits "database.table|rows" outside system schemas.
// Direct counts remain useful after crash recovery resets Postgres statistics
// during startup of a restored file-system copy. SQL names are escaped as literals
// and identifiers, and enumeration errors fail the command. The MySQL password
// stays in the environment rather than the command line. Unsupported service types
// return nil, an empty script and false.
func rowCountCommand(serviceType string) ([]string, string, bool) {
	switch serviceType {
	case "postgres":
		return []string{"sh", "-c", `sql=$(mktemp) && cat > "$sql" && dbs=$(mktemp) || exit 1
psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -X -A -t -q -v ON_ERROR_STOP=1 -c "SELECT datname FROM pg_database WHERE datallowconn AND NOT datistemplate ORDER BY 1" > "$dbs" || exit 1
while IFS= read -r db; do
  psql -U "$POSTGRES_USER" -d "$db" -X -A -t -q -v ON_ERROR_STOP=1 -f "$sql" || exit 1
done < "$dbs"`},
			`SELECT format('SELECT %L || ''|'' || count(*) FROM %I.%I', current_database() || '.' || schemaname || '.' || tablename, schemaname, tablename)
FROM pg_catalog.pg_tables WHERE schemaname NOT IN ('pg_catalog', 'information_schema')
ORDER BY 1 \gexec
`, true
	case "mysql":
		return []string{"sh", "-c", `export MYSQL_PWD="$MYSQL_ROOT_PASSWORD"; q=$(mktemp) || exit 1
mysql -uroot -N -B -r > "$q" || exit 1
mysql -uroot -N -B < "$q" || exit 1`},
			"SELECT CONCAT('SELECT CONCAT(', QUOTE(CONCAT(table_schema, '.', table_name)), ', ''|'', COUNT(*)) FROM `', " +
				"REPLACE(table_schema, '`', '``'), '`.`', REPLACE(table_name, '`', '``'), '`;') " +
				"FROM information_schema.tables WHERE table_type = 'BASE TABLE' " +
				"AND table_schema NOT IN ('mysql', 'sys', 'performance_schema', 'information_schema') ORDER BY table_schema, table_name;\n", true
	}
	return nil, "", false
}

// parseRowCounts reads "table|rows" lines. The count follows the last "|", so
// a table name may contain one.
func parseRowCounts(out string) ([]tableCount, error) {
	var counts []tableCount
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sep := strings.LastIndex(line, "|")
		if sep < 0 {
			return nil, fmt.Errorf("unexpected row count output: %q", line)
		}
		table, rows := line[:sep], line[sep+1:]
		n, err := strconv.ParseInt(strings.TrimSpace(rows), 10, 64)
		if err != nil {
			return nil, fmt.Errorf("unexpected row count output: %q", line)
		}
		counts = append(counts, tableCount{Table: table, Rows: n})
	}
	return counts, nil
}
