package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/service"
	"github.com/getkipper/kipper/kip/internal/velerolabel"
)

var backupGVR = schema.GroupVersionResource{
	Group:    "velero.io",
	Version:  "v1",
	Resource: "backups",
}

var restoreGVR = schema.GroupVersionResource{
	Group:    "velero.io",
	Version:  "v1",
	Resource: "restores",
}

var podVolumeBackupGVR = schema.GroupVersionResource{
	Group:    "velero.io",
	Version:  "v1",
	Resource: "podvolumebackups",
}

var scheduleGVR = schema.GroupVersionResource{
	Group:    "velero.io",
	Version:  "v1",
	Resource: "schedules",
}

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Manage cluster backups",
}

var backupCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a backup",
	Args:  cobra.MaximumNArgs(1),
	RunE:  runBackupCreate,
}

var backupListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all backups",
	RunE:  runBackupList,
}

var backupRestoreCmd = &cobra.Command{
	Use:   "restore [backup-name]",
	Short: "Restore from a backup",
	Args:  cobra.ExactArgs(1),
	RunE:  runBackupRestore,
}

var backupScheduleCmd = &cobra.Command{
	Use:   "schedules",
	Short: "List backup schedules",
	RunE:  runBackupSchedules,
}

func init() {
	backupCreateCmd.Flags().String("project", "", "backup a specific project only")
	backupCreateCmd.Flags().String("environment", "", "backup a specific environment only")
	backupCreateCmd.Flags().String("ttl", "168h", "backup retention period (default 7 days)")
	backupCreateCmd.Flags().Bool("include-system", false, "include system namespaces (kube-*, traefik, longhorn-system, monitoring, keda, velero) in the backup. Off by default because Velero recurses into its own MinIO PVC otherwise and the backup hangs.")

	backupRestoreCmd.Flags().String("namespace-mapping", "", "restore only the source namespace into the target namespace (format: source:target)")
	backupRestoreCmd.Flags().String("namespace", "", "restore only a specific namespace")
	backupRestoreCmd.Flags().String("resources", "", "restore only specific resource types (comma-separated, e.g. deployments,services,secrets)")
	backupRestoreCmd.Flags().Bool("allow-partial", false, "restore a PartiallyFailed backup despite possible missing data")
	backupRestoreCmd.Flags().Bool("repair-credentials", false, "re-attach restored service credentials Secrets without prompting")
	backupRestoreCmd.Flags().Bool("include-ingresses", false, "With --namespace-mapping, also restore Ingresses and cert-manager Certificates. The copy will claim the source's hostnames and renew certificates for them.")
	backupRestoreCmd.Flags().Bool("cluster-resources", false, "also restore projects and their resource adjustments. Existing ones remain unchanged.")

	backupCmd.AddCommand(backupCreateCmd)
	backupCmd.AddCommand(backupListCmd)
	backupCmd.AddCommand(backupRestoreCmd)
	backupCmd.AddCommand(backupScheduleCmd)
	rootCmd.AddCommand(backupCmd)
}

// errBackupsNotConfigured is the CLI's refusal on a cluster without backups.
var errBackupsNotConfigured = errors.New(strings.TrimSuffix(platform.BackupsNotConfigured, "."))

// backupsConfiguredOn checks the recorded backup mode. Read failures count as
// configured for compatibility with clusters that lack a readable PlatformConfig.
func backupsConfiguredOn(ctx context.Context, dyn dynamic.Interface) bool {
	pc, err := dyn.Resource(platformConfigGVR).Get(ctx, "platform", metav1.GetOptions{})
	if err != nil {
		return true
	}
	backup, _, _ := unstructured.NestedString(pc.Object, "spec", "backup")
	return platform.BackupsConfigured(backup)
}

func runBackupCreate(cmd *cobra.Command, args []string) error {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	if !backupsConfiguredOn(context.Background(), k8sClient.Dynamic()) {
		return errBackupsNotConfigured
	}

	project, _ := cmd.Flags().GetString("project")
	environment, _ := cmd.Flags().GetString("environment")
	ttl, _ := cmd.Flags().GetString("ttl")
	includeSystem, _ := cmd.Flags().GetBool("include-system")

	name := "manual-" + time.Now().Format("20060102-150405")
	if len(args) > 0 {
		name = args[0]
	}

	backup := buildBackupCR(name, project, environment, ttl, includeSystem, cluster)

	ctx := context.Background()
	fmt.Printf("\n  Creating backup %s...\n", name)

	_, err = k8sClient.Dynamic().Resource(backupGVR).Namespace("velero").Create(ctx, backup, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating backup: %w", err)
	}

	fmt.Printf("  ✔  Backup %s started\n", name)
	fmt.Printf("  Run 'kip backup list' to check progress\n\n")
	return nil
}

// toAnySlice converts a string slice into the []interface{} form the
// unstructured Velero specs require.
func toAnySlice(items []string) []interface{} {
	out := make([]interface{}, len(items))
	for i, item := range items {
		out[i] = item
	}
	return out
}

// buildBackupCR builds a manual backup with the shared resource exclusions.
func buildBackupCR(name, project, environment, ttl string, includeSystem bool, cluster *config.Cluster) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"ttl":                      ttl,
		"storageLocation":          "default",
		"defaultVolumesToFsBackup": true,
	}
	if includeSystem && project == "" {
		backupscope.BackupWholeCluster(spec)
	} else {
		backupscope.Backup(spec)
	}

	switch {
	case project != "":
		ns := cluster.ResolveNamespace(project, environment)
		spec["includedNamespaces"] = []interface{}{ns}
	case !includeSystem:
		spec["excludedNamespaces"] = toAnySlice(backupscope.ExcludedNamespaces())
	}

	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "velero.io/v1",
			"kind":       "Backup",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "velero",
			},
			"spec": spec,
		},
	}
}

func runBackupList(cmd *cobra.Command, args []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	if !backupsConfiguredOn(ctx, k8sClient.Dynamic()) {
		fmt.Printf("\n  %s\n\n", platform.BackupsNotConfigured)
		return nil
	}
	backups, err := k8sClient.Dynamic().Resource(backupGVR).Namespace("velero").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing backups: %w", err)
	}

	if len(backups.Items) == 0 {
		fmt.Printf("\n  No backups found\n\n")
		return nil
	}

	fmt.Printf("\n  %-35s %-12s %-20s %s\n", "NAME", "STATUS", "NAMESPACES", "CREATED")
	for _, b := range backups.Items {
		status := "Unknown"
		if s, ok, _ := unstructured.NestedString(b.Object, "status", "phase"); ok {
			status = s
		}

		namespaces := "all"
		if ns, ok, _ := unstructured.NestedStringSlice(b.Object, "spec", "includedNamespaces"); ok && len(ns) > 0 {
			namespaces = strings.Join(ns, ", ")
		} else if ex, ok, _ := unstructured.NestedStringSlice(b.Object, "spec", "excludedNamespaces"); ok && len(ex) > 0 {
			// Distinguish namespace exclusions from an unrestricted backup.
			namespaces = "all except system"
		}

		created := b.GetCreationTimestamp().Format("2006-01-02 15:04")

		fmt.Printf("  %-35s %-12s %-20s %s\n", b.GetName(), status, namespaces, created)
	}
	if bsl, err := k8sClient.Dynamic().Resource(backupStorageLocationGVR).Namespace("velero").Get(ctx, "default", metav1.GetOptions{}); err == nil {
		config, _, _ := unstructured.NestedStringMap(bsl.Object, "spec", "config")
		defaultKey := false
		if secret, err := k8sClient.Clientset().CoreV1().Secrets("velero").Get(ctx, "velero-repo-credentials", metav1.GetOptions{}); err == nil {
			defaultKey = installer.IsDefaultRepositoryKey(secret.Data["repository-password"])
		}
		fmt.Println()
		for _, line := range backupProtection(config, defaultKey) {
			fmt.Printf("  %s\n", line)
		}
	}
	fmt.Println()
	return nil
}

// backupProtection describes, from the backup location's config and whether
// the repository key is Velero's published default, how backup objects and
// volume data are encrypted.
func backupProtection(config map[string]string, defaultKey bool) []string {
	objects := "Backup objects: the store's own settings apply"
	switch {
	case config["customerKeyEncryptionSecret"] != "":
		objects = "Backup objects: encrypted by the store with this cluster's SSE-C key (kip backup sse-key export)"
	case config["serverSideEncryption"] != "":
		objects = "Backup objects: encrypted at rest by AWS S3 (SSE-S3)"
	}
	volumes := "Volume data:    encrypted with the backup repository key (kip backup repository-key export)"
	if defaultKey {
		volumes = "Volume data:    encrypted with Velero's published default key, so only access to the bucket protects it"
	}
	return []string{objects, volumes}
}

func runBackupRestore(cmd *cobra.Command, args []string) error {
	backupName := args[0]
	nsMapping, _ := cmd.Flags().GetString("namespace-mapping")
	namespace, _ := cmd.Flags().GetString("namespace")
	resources, _ := cmd.Flags().GetString("resources")
	allowPartial, _ := cmd.Flags().GetBool("allow-partial")
	repairCredentials, _ := cmd.Flags().GetBool("repair-credentials")
	clusterResources, _ := cmd.Flags().GetBool("cluster-resources")
	includeIngresses, _ := cmd.Flags().GetBool("include-ingresses")

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	if !backupsConfiguredOn(ctx, k8sClient.Dynamic()) {
		return errBackupsNotConfigured
	}
	backup, err := k8sClient.Dynamic().Resource(backupGVR).Namespace("velero").Get(ctx, backupName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading backup %s: %w", backupName, err)
	}
	// Volume backup details make partial-restore messages more specific.
	// If the lookup fails, a general warning still explains the risk.
	var volumes []unstructured.Unstructured
	partial := false
	if phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase"); phase == "PartiallyFailed" {
		partial = true
		list, err := k8sClient.Dynamic().Resource(podVolumeBackupGVR).Namespace("velero").List(ctx, metav1.ListOptions{
			LabelSelector: "velero.io/backup-name=" + velerolabel.ValidName(backupName),
		})
		if err == nil {
			volumes = list.Items
		}
	}
	if err := checkRestorable(backup, volumes, allowPartial); err != nil {
		return err
	}
	if partial {
		fmt.Print(partialRestoreWarning(backupName, volumes))
	}

	restoreName := "restore-" + backupName + "-" + time.Now().Format("150405")

	spec, err := restoreSpec(backupName, namespace, resources, nsMapping, clusterResources, includeIngresses)
	if err != nil {
		return err
	}

	restore := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "velero.io/v1",
			"kind":       "Restore",
			"metadata": map[string]interface{}{
				"name":      restoreName,
				"namespace": "velero",
			},
			"spec": spec,
		},
	}

	fmt.Printf("\n  Restoring from backup %s...\n", backupName)
	if nsMapping != "" {
		fmt.Printf("  ⚠  The copy's Secrets and connection settings retain references to the source environment's services. Check them before allowing the copy to connect.\n")
	}

	_, err = k8sClient.Dynamic().Resource(restoreGVR).Namespace("velero").Create(ctx, restore, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating restore: %w", err)
	}

	fmt.Printf("  ✔  Restore %s started. Waiting for completion.\n  Press Ctrl-C to stop waiting; the restore continues in the cluster.\n", restoreName)

	follow := restoreFollowUpFor(restoreTarget(namespace, nsMapping), cluster.Org)
	interrupts := make(chan os.Signal, 1)
	signal.Notify(interrupts, os.Interrupt)
	go func() {
		<-interrupts
		fmt.Printf("\n\n  Stopped waiting. Restore %s continues on the cluster.\n", restoreName)
		fmt.Print(follow)
		os.Exit(130)
	}()
	defer signal.Stop(interrupts)

	var ask func(string) bool
	if term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) { //nolint:gosec // file descriptors fit an int
		ask = confirm
	}
	mgr := &service.Manager{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	waitCtx, cancel := context.WithTimeout(cmd.Context(), restoreWaitLimit)
	defer cancel()
	steps := clusterRestoreSteps(k8sClient.Dynamic(), mgr, restoreName, ask)
	steps.waited = func() { signal.Stop(interrupts) }
	err = finishRestore(waitCtx, steps, os.Stdout, repairCredentials, cluster.Org)
	if errors.Is(err, errWaitStopped) {
		fmt.Print(follow)
	}
	return err
}

// restoreTarget selects the destination namespace for follow-up commands; empty means unscoped.
func restoreTarget(namespace, mapping string) string {
	if source, target, ok := strings.Cut(mapping, ":"); ok && (namespace == "" || source == namespace) && target != "" {
		return target
	}
	return namespace
}

// restoreFollowUp suggests an explicit credentials repair after the services appear.
func restoreFollowUp(namespace string) string {
	project := namespace
	if project == "" {
		project = "<project>"
	}
	return fmt.Sprintf("  Once kip service list --project %[1]s shows the restored services, repair their credentials ownership:\n"+
		"    kip service credentials --project %[1]s --repair\n\n", project)
}

// restoreFollowUpFor suggests a repair command only when project resolution reaches the target namespace.
func restoreFollowUpFor(target, org string) string {
	if target != "" && org != "" && !strings.HasPrefix(target, org+"-") {
		return fmt.Sprintf("  The restored services are in namespace %s, outside organization %s, so kip cannot repair\n"+
			"  their credentials ownership there. Restore into a namespace that starts with %s- to use\n"+
			"  kip service credentials --repair.\n\n", target, org, org)
	}
	return restoreFollowUp(target)
}

// checkRestorable requires explicit opt-in for backups that may be incomplete.
func checkRestorable(backup *unstructured.Unstructured, volumes []unstructured.Unstructured, allowPartial bool) error {
	phase, _, _ := unstructured.NestedString(backup.Object, "status", "phase")
	switch phase {
	case "Completed":
		return nil
	case "PartiallyFailed":
		if allowPartial {
			return nil
		}
		failed := failedVolumes(volumes)
		if len(failed) == 0 {
			return fmt.Errorf("backup %s is PartiallyFailed: restored data or resources may be incomplete. Choose a Completed backup or pass --allow-partial to proceed", backup.GetName())
		}
		return fmt.Errorf("backup %s is PartiallyFailed: volume backups failed or were canceled for %s. Restored volumes may be missing data. Choose a Completed backup or pass --allow-partial to proceed", backup.GetName(), strings.Join(failed, ", "))
	case "", "New", "Queued", "ReadyToStart", "InProgress", "WaitingForPluginOperations",
		"WaitingForPluginOperationsPartiallyFailed", "Finalizing", "FinalizingPartiallyFailed":
		return fmt.Errorf("backup %s is not ready to restore; wait for it to finish, then try again", backup.GetName())
	default:
		return fmt.Errorf("backup %s is %s and cannot be restored; choose a Completed backup", backup.GetName(), phase)
	}
}

func failedVolumes(volumes []unstructured.Unstructured) []string {
	var failed []string
	for _, v := range volumes {
		if p, _, _ := unstructured.NestedString(v.Object, "status", "phase"); p != "Failed" && p != "Canceled" {
			continue
		}
		ns, _, _ := unstructured.NestedString(v.Object, "spec", "pod", "namespace")
		pod, _, _ := unstructured.NestedString(v.Object, "spec", "pod", "name")
		volume, _, _ := unstructured.NestedString(v.Object, "spec", "volume")
		failed = append(failed, fmt.Sprintf("%s/%s volume %s", ns, pod, volume))
	}
	return failed
}

func partialRestoreWarning(backupName string, volumes []unstructured.Unstructured) string {
	if failed := failedVolumes(volumes); len(failed) > 0 {
		return fmt.Sprintf("  ⚠  Restoring PartiallyFailed backup %s: data may be missing from %s.\n", backupName, strings.Join(failed, ", "))
	}
	return fmt.Sprintf("  ⚠  Restoring PartiallyFailed backup %s: restored data or resources may be incomplete.\n", backupName)
}

func restoreSpec(backupName, namespace, resources, mapping string, clusterResources, includeIngresses bool) (map[string]interface{}, error) {
	spec := map[string]interface{}{"backupName": backupName}
	backupscope.Restore(spec, clusterResources)
	if namespace != "" {
		spec["includedNamespaces"] = []interface{}{namespace}
	}
	if resources != "" {
		spec["includedResources"] = strings.Split(resources, ",")
	}
	if mapping != "" {
		source, target, ok := strings.Cut(mapping, ":")
		if !ok || source == target || len(validation.IsDNS1123Label(source)) > 0 || len(validation.IsDNS1123Label(target)) > 0 {
			return nil, fmt.Errorf("--namespace-mapping %q must use source:target with two different valid namespace names", mapping)
		}
		if namespace != "" && namespace != source {
			return nil, fmt.Errorf("--namespace-mapping %q requires --namespace %s; got %s", mapping, source, namespace)
		}
		if clusterResources {
			return nil, fmt.Errorf("--namespace-mapping restores a copy of one namespace and cannot be combined with --cluster-resources")
		}
		// Limit the restore to the mapped source to avoid restoring unrelated namespaces.
		spec["includedNamespaces"] = []interface{}{source}
		spec["namespaceMapping"] = map[string]interface{}{source: target}
		// A copy's Ingresses would claim the live app's hostnames, and its
		// cert-manager Certificates would keep renewing certificates for them.
		if !includeIngresses {
			excluded, _ := spec["excludedResources"].([]interface{})
			spec["excludedResources"] = append(excluded, "ingresses.networking.k8s.io", "certificates.cert-manager.io")
		}
	}
	return spec, nil
}

func runBackupSchedules(cmd *cobra.Command, args []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	if !backupsConfiguredOn(ctx, k8sClient.Dynamic()) {
		fmt.Printf("\n  %s\n\n", platform.BackupsNotConfigured)
		return nil
	}
	schedules, err := k8sClient.Dynamic().Resource(scheduleGVR).Namespace("velero").List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing schedules: %w", err)
	}

	if len(schedules.Items) == 0 {
		fmt.Printf("\n  No backup schedules configured\n\n")
		return nil
	}

	fmt.Printf("\n  %-25s %-15s %-12s %s\n", "NAME", "SCHEDULE", "STATUS", "LAST BACKUP")
	for _, s := range schedules.Items {
		schedule, _, _ := unstructured.NestedString(s.Object, "spec", "schedule")

		phase := "Active"
		if p, ok, _ := unstructured.NestedString(s.Object, "status", "phase"); ok {
			phase = p
		}

		lastBackup := "never"
		if lb, ok, _ := unstructured.NestedString(s.Object, "status", "lastBackup"); ok {
			lastBackup = lb
		}

		fmt.Printf("  %-25s %-15s %-12s %s\n", s.GetName(), schedule, phase, lastBackup)
	}
	fmt.Println()
	return nil
}
