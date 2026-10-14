package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
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

	backupCmd.AddCommand(backupCreateCmd)
	backupCmd.AddCommand(backupListCmd)
	backupCmd.AddCommand(backupRestoreCmd)
	backupCmd.AddCommand(backupScheduleCmd)
	rootCmd.AddCommand(backupCmd)
}

func runBackupCreate(cmd *cobra.Command, args []string) error {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
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

// buildBackupCR assembles the velero.io/v1 Backup unstructured object
// for a single manual backup run. Pulled out of runBackupCreate so the
// spec construction is unit-testable without an apiserver.
//
// Scoping rules:
//   - project set → backup includes only that namespace (system
//     exclusions are redundant here; Velero already restricts by include
//     list).
//   - project unset, includeSystem false → cluster-wide backup with the
//     default exclusion list applied. Velero would otherwise recurse
//     into its own MinIO PVC and hang.
//   - project unset, includeSystem true → no exclusions. Escape hatch
//     for the rare case where the operator genuinely wants everything.
func buildBackupCR(name, project, environment, ttl string, includeSystem bool, cluster *config.Cluster) *unstructured.Unstructured {
	spec := map[string]interface{}{
		"ttl":                      ttl,
		"storageLocation":          "default",
		"defaultVolumesToFsBackup": true,
		"excludedResources":        toAnySlice(installer.DefaultBackupExcludedResources()),
	}

	switch {
	case project != "":
		ns := cluster.ResolveNamespace(project, environment)
		spec["includedNamespaces"] = []interface{}{ns}
	case !includeSystem:
		spec["excludedNamespaces"] = toAnySlice(installer.DefaultBackupExcludedNamespaces())
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
			// Default manual + scheduled cluster-wide backups exclude
			// system namespaces. Distinguish from a true "all"
			// (`--include-system`) so operators don't expect Velero or
			// monitoring data in these backups when restoring.
			namespaces = "all except system"
		}

		created := b.GetCreationTimestamp().Format("2006-01-02 15:04")

		fmt.Printf("  %-35s %-12s %-20s %s\n", b.GetName(), status, namespaces, created)
	}
	fmt.Println()
	return nil
}

func runBackupRestore(cmd *cobra.Command, args []string) error {
	backupName := args[0]
	nsMapping, _ := cmd.Flags().GetString("namespace-mapping")
	namespace, _ := cmd.Flags().GetString("namespace")
	resources, _ := cmd.Flags().GetString("resources")
	allowPartial, _ := cmd.Flags().GetBool("allow-partial")

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
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

	spec, err := restoreSpec(backupName, namespace, resources, nsMapping)
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

	_, err = k8sClient.Dynamic().Resource(restoreGVR).Namespace("velero").Create(ctx, restore, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("creating restore: %w", err)
	}

	fmt.Printf("  ✔  Restore %s started\n", restoreName)
	fmt.Printf("  This may take several minutes for large datasets.\n\n")
	fmt.Print(restoreFollowUpFor(restoreTarget(namespace, nsMapping), cluster.Org))
	return nil
}

// restoreTarget returns the mapped namespace, or an empty string for an unscoped restore.
func restoreTarget(namespace, mapping string) string {
	if namespace == "" {
		return ""
	}
	if source, target, ok := strings.Cut(mapping, ":"); ok && source == namespace && target != "" {
		return target
	}
	return namespace
}

// restoreFollowUp prompts for credentials ownership repair, which requires explicit opt-in.
func restoreFollowUp(namespace string) string {
	project := namespace
	if project == "" {
		project = "<project>"
	}
	return fmt.Sprintf("  Once kip service list --project %[1]s shows the restored services, repair their credentials ownership:\n"+
		"    kip service credentials --project %[1]s --repair\n\n", project)
}

// restoreFollowUpFor prints a project command only when kip resolves that project to the namespace the
// restore wrote to; a project of an organization always resolves to <org>-<project>.
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

func restoreSpec(backupName, namespace, resources, mapping string) (map[string]interface{}, error) {
	spec := map[string]interface{}{
		"backupName": backupName,
		// Apply certificate-issuance exclusions to older backups as well.
		"excludedResources": toAnySlice(installer.DefaultBackupExcludedResources()),
	}
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
		// Limit the restore to the mapped source to avoid restoring unrelated namespaces.
		spec["includedNamespaces"] = []interface{}{source}
		spec["namespaceMapping"] = map[string]interface{}{source: target}
	}
	return spec, nil
}

func runBackupSchedules(cmd *cobra.Command, args []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
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
