package cmd

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/controller/pkg/projectenv"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var projectCmd = &cobra.Command{
	Use:   "project",
	Short: "Manage projects",
}

var projectCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a project with optional environments",
	Long: `Create a project. If --environments is specified, a namespace is created
for each environment (e.g. blog-test, blog-acc, blog-prod).
Without --environments, a single namespace is created.`,
	Args: cobra.ExactArgs(1),
	RunE: runProjectCreate,
}

var projectListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all projects",
	RunE:  runProjectList,
}

var projectDeleteCmd = &cobra.Command{
	Use:   "delete [name]",
	Short: "Delete a project and all its environments",
	Args:  cobra.ExactArgs(1),
	RunE:  runProjectDelete,
}

var projectAddEnvCmd = &cobra.Command{
	Use:   "add-env [project] [env]",
	Short: "Add an environment to an existing project",
	Long: `Add a new environment to an existing project. The reconciler creates a
matching namespace (<project>-<env>) and copies registry secrets into it.

If [project] is omitted the active project from 'kip project use' is used.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runProjectAddEnv,
}

var projectRemoveEnvCmd = &cobra.Command{
	Use:   "remove-env [project] [env]",
	Short: "Remove an environment from a project (destroys the namespace)",
	Long: `Remove an environment from a project. The matching namespace and every
app, service, secret and volume inside it is deleted.

You must type the environment name to confirm. If [project] is omitted the
active project from 'kip project use' is used.`,
	Args: cobra.RangeArgs(1, 2),
	RunE: runProjectRemoveEnv,
}

var projectUseCmd = &cobra.Command{
	Use:   "use [name[/env]]",
	Short: "Set the active project (and environment) for the current cluster",
	Long: `Set a persistent project context for the current cluster, so that
subsequent commands do not need --project / --environment flags.

The chosen project applies until you switch with another 'kip project use',
or until you switch clusters with 'kip cluster use'. Each cluster remembers
its own active project.

Examples:
  kip project use hrportal           # project=hrportal, default environment
  kip project use hrportal/test      # project=hrportal, environment=test
  kip project use hrportal test      # same, with a space instead of slash
  kip project use --clear           # forget the active project on this cluster`,
	Args: cobra.MaximumNArgs(2),
	RunE: runProjectUse,
}

func init() {
	projectCreateCmd.Flags().StringSlice("environments", nil, "comma-separated environment names in promotion order (e.g. test,acc,prod)")
	projectCreateCmd.Flags().String("display-name", "", "human-readable project name (e.g. \"example.com Domain Platform\")")
	projectUseCmd.Flags().Bool("clear", false, "clear the active project for this cluster")

	projectCmd.AddCommand(projectCreateCmd)
	projectCmd.AddCommand(projectListCmd)
	projectCmd.AddCommand(projectDeleteCmd)
	projectCmd.AddCommand(projectUseCmd)
	projectCmd.AddCommand(projectAddEnvCmd)
	projectCmd.AddCommand(projectRemoveEnvCmd)
	rootCmd.AddCommand(projectCmd)
}

func runProjectCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	envs, _ := cmd.Flags().GetStringSlice("environments")
	displayName, _ := cmd.Flags().GetString("display-name")

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	dynClient := k8sClient.Dynamic()

	if len(envs) == 0 {
		envs = []string{"default"}
	}

	// Resolve the CR name — includes org prefix if configured
	crName := cluster.ResolveNamespace(name, "")

	// Check if project already exists
	_, getErr := dynClient.Resource(manifest.ProjectGVR).Get(ctx, crName, metav1.GetOptions{})
	if getErr == nil {
		return fmt.Errorf("project %q already exists", name)
	}
	if !errors.IsNotFound(getErr) {
		return fmt.Errorf("checking project %q: %w", name, getErr)
	}

	// Build environment list for the CR
	envList := make([]interface{}, len(envs))
	for i, e := range envs {
		envList[i] = map[string]interface{}{"name": e}
	}

	spec := map[string]interface{}{
		"environments": envList,
	}
	if displayName != "" {
		spec["displayName"] = displayName
	}

	projectCR := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Project",
			"metadata": map[string]interface{}{
				"name": crName,
				"labels": map[string]interface{}{
					labels.ManagedBy: labels.Kipper,
				},
			},
			"spec": spec,
		},
	}

	fmt.Printf("\n  Creating project %q...\n", name)

	if err := createProjectCR(ctx, dynClient, projectCR, crName, envs); err != nil {
		return err
	}
	fmt.Printf("  ✔  Project CR created\n")

	// Wait for the ProjectReconciler to create namespaces
	expectedNamespaces := make([]string, len(envs))
	for i, env := range envs {
		expectedNamespaces[i] = cluster.ResolveNamespace(name, env)
	}

	fmt.Printf("  ...  Waiting for namespaces\n")
	clientset := k8sClient.Clientset()
	missing := expectedNamespaces
	for attempt := 0; attempt < 30; attempt++ {
		var stillMissing []string
		for _, ns := range missing {
			if _, err := clientset.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
				stillMissing = append(stillMissing, ns)
			}
		}
		missing = stillMissing
		if len(missing) == 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	// Report what is actually there. A tick beside a namespace the reconciler
	// refused — one another project already owns, or one it did not create —
	// sends the operator away believing the environment is ready, and the only
	// account of what happened is a condition on the project they have no
	// reason to look at.
	absent := make(map[string]bool, len(missing))
	for _, ns := range missing {
		absent[ns] = true
	}
	for _, ns := range expectedNamespaces {
		if absent[ns] {
			fmt.Printf("  ⚠  %s did not appear\n", ns)
			continue
		}
		fmt.Printf("  ✔  %s\n", ns)
	}
	if len(missing) > 0 {
		fmt.Printf("\n     A namespace another project already uses, or one Kipper did not create,\n")
		fmt.Printf("     is refused rather than taken over. The project records why as a\n")
		fmt.Printf("     NamespaceConflict condition, which the console shows on the project.\n")
	}

	if len(envs) > 1 {
		fmt.Printf("\n  Promotion pipeline: %s\n", strings.Join(envs, " → "))
	}
	fmt.Println()

	return nil
}

func runProjectList(cmd *cobra.Command, args []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	dynClient := k8sClient.Dynamic()

	projectList, err := dynClient.Resource(manifest.ProjectGVR).List(ctx, metav1.ListOptions{
		LabelSelector: labels.KipperManagedSelector,
	})
	if err != nil {
		return fmt.Errorf("listing projects: %w", err)
	}

	if len(projectList.Items) == 0 {
		fmt.Printf("\n  No projects found\n\n")
		return nil
	}

	fmt.Printf("\n  %-25s %-25s %s\n", "PROJECT", "DISPLAY NAME", "ENVIRONMENTS")
	for _, p := range projectList.Items {
		name := p.GetName()
		displayName, _, _ := unstructured.NestedString(p.Object, "spec", "displayName")
		if displayName == "" {
			displayName = "-"
		}

		envSlice, _, _ := unstructured.NestedSlice(p.Object, "spec", "environments")
		var envNames []string
		for _, e := range envSlice {
			if envMap, ok := e.(map[string]interface{}); ok {
				if envName, ok := envMap["name"].(string); ok {
					envNames = append(envNames, envName)
				}
			}
		}

		fmt.Printf("  %-25s %-25s %s\n", name, displayName, strings.Join(envNames, ", "))
	}
	fmt.Println()

	return nil
}

func runProjectDelete(cmd *cobra.Command, args []string) error {
	name := args[0]

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	// Resolve the CR name — includes org prefix if configured
	crName := cluster.ResolveNamespace(name, "")

	ctx := context.Background()
	dynClient := k8sClient.Dynamic()

	// Verify it exists
	_, getErr := dynClient.Resource(manifest.ProjectGVR).Get(ctx, crName, metav1.GetOptions{})
	if errors.IsNotFound(getErr) {
		return fmt.Errorf("project %q not found", name)
	}
	if getErr != nil {
		return fmt.Errorf("checking project: %w", getErr)
	}

	if !confirmPrompt(fmt.Sprintf("Delete project %q and ALL its environments, apps, services, and data?", name)) {
		fmt.Println("  Cancelled.")
		return nil
	}

	fmt.Printf("\n  Deleting project %q...\n", name)

	// Delete the Project CR — the controller's finalizer and cascade handles cleanup
	if err := dynClient.Resource(manifest.ProjectGVR).Delete(ctx, crName, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("deleting project: %w", err)
	}

	// Also delete the namespaces (the console-api delete handler does this too)
	clientset := k8sClient.Clientset()
	namespaces, _ := clientset.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", labels.Project, crName),
	})
	if namespaces != nil {
		for _, ns := range namespaces.Items {
			if err := clientset.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{}); err != nil {
				fmt.Printf("  ✗  %s: %v\n", ns.Name, err)
			} else {
				fmt.Printf("  ✔  %s\n", ns.Name)
			}
		}
	}
	fmt.Println()

	return nil
}

func runProjectUse(cmd *cobra.Command, args []string) error {
	clear, _ := cmd.Flags().GetBool("clear")

	if !clear && len(args) == 0 {
		return fmt.Errorf("project name required (or use --clear)")
	}

	var project, environment string
	if !clear {
		project, environment = parseProjectArg(args)
	}

	var clusterName string
	if err := setActiveProject(&clusterName, project, environment); err != nil {
		return err
	}
	cluster := &config.Cluster{Name: clusterName}

	if clear {
		fmt.Printf("\n  ✔  Cleared active project for cluster %q\n\n", cluster.Name)
		return nil
	}

	if environment != "" {
		fmt.Printf("\n  ✔  Active project: %s/%s on cluster %q\n\n", project, environment, cluster.Name)
	} else {
		fmt.Printf("\n  ✔  Active project: %s on cluster %q\n\n", project, cluster.Name)
	}
	return nil
}

// resolveProjectAndEnvironment returns the (project, environment) the
// current command should operate on. Precedence: explicit --project /
// --environment flags first, then the per-cluster persisted context set by
// `kip project use`, then the flag's default value. Pass cluster=nil when
// no cluster context is available (the function then returns whatever the
// flags hold).
func resolveProjectAndEnvironment(cmd *cobra.Command, cluster *config.Cluster) (string, string) {
	project, _ := cmd.Flags().GetString("project")
	environment, _ := cmd.Flags().GetString("environment")

	if cluster == nil {
		return project, environment
	}

	pf := cmd.Flag("project")
	projectChanged := pf != nil && pf.Changed
	ef := cmd.Flag("environment")
	envChanged := ef != nil && ef.Changed

	if !projectChanged && cluster.CurrentProject != "" {
		project = cluster.CurrentProject
	}
	// Persisted environment only applies when the user is also using the
	// persisted project. If they explicitly switched project, the env
	// should reset to the flag default rather than inheriting a stale one.
	if !projectChanged && !envChanged && cluster.CurrentEnvironment != "" {
		environment = cluster.CurrentEnvironment
	}
	return project, environment
}

// parseProjectArg accepts either "name/env" as a single arg or "name" "env"
// as two args, and returns the resolved (project, environment).
func parseProjectArg(args []string) (string, string) {
	if len(args) == 2 {
		return args[0], args[1]
	}
	if strings.Contains(args[0], "/") {
		parts := strings.SplitN(args[0], "/", 2)
		return parts[0], parts[1]
	}
	return args[0], ""
}

func confirmPrompt(message string) bool {
	fmt.Printf("  %s [y/N] ", message)
	var response string
	_, _ = fmt.Scanln(&response)
	return strings.EqualFold(response, "y")
}

// confirmTypePrompt requires the user to type the expected token verbatim.
// Used as a guard in front of destructive operations like environment
// removal where a single keystroke could destroy production data.
func confirmTypePrompt(message, expected string) bool {
	fmt.Printf("  %s\n  Type %q to confirm: ", message, expected)
	var response string
	_, _ = fmt.Scanln(&response)
	return response == expected
}

// parseEnvCmdArgs splits the args between explicit "<project> <env>" and
// "<env>" (with the project taken from the active context).
func parseEnvCmdArgs(cluster *config.Cluster, args []string) (project, env string, err error) {
	switch len(args) {
	case 2:
		return args[0], args[1], nil
	case 1:
		if cluster.CurrentProject == "" {
			return "", "", fmt.Errorf("no project specified — pass it as the first argument or run 'kip project use <name>' first")
		}
		return cluster.CurrentProject, args[0], nil
	}
	return "", "", fmt.Errorf("expected 1 or 2 arguments, got %d", len(args))
}

// loadProjectCR fetches a Project CR by its resolved name and returns the raw
// unstructured object plus the environments the project has.
//
// The environments it has, not the ones it declares. A project declaring none
// still runs in one, and every caller here goes on to write the list back — so
// starting from the declared slice replaced that environment rather than
// extending it, and the reconciler then deleted its namespace along with every
// workload in it.
func loadProjectCR(ctx context.Context, dynClient dynamic.Interface, crName string) (*unstructured.Unstructured, []string, error) {
	project, err := dynClient.Resource(manifest.ProjectGVR).Get(ctx, crName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, nil, fmt.Errorf("project %q not found", crName)
		}
		return nil, nil, fmt.Errorf("loading project: %w", err)
	}

	envSlice, _, _ := unstructured.NestedSlice(project.Object, "spec", "environments")
	envs := make([]string, 0, len(envSlice))
	for _, e := range envSlice {
		if envMap, ok := e.(map[string]interface{}); ok {
			if name, ok := envMap["name"].(string); ok {
				envs = append(envs, name)
			}
		}
	}
	return project, projectenv.Effective(envs), nil
}

// writeProjectEnvironments overwrites spec.environments on the project CR.
// Surviving environments keep their full existing entry (quota override and
// any future fields); only genuinely new environments get a fresh name-only
// entry.
func writeProjectEnvironments(ctx context.Context, dynClient dynamic.Interface, project *unstructured.Unstructured, envs []string) error {
	existingSlice, _, _ := unstructured.NestedSlice(project.Object, "spec", "environments")
	existing := make(map[string]interface{}, len(existingSlice))
	for _, e := range existingSlice {
		if envMap, ok := e.(map[string]interface{}); ok {
			if name, ok := envMap["name"].(string); ok {
				existing[name] = envMap
			}
		}
	}

	envList := make([]interface{}, len(envs))
	for i, e := range envs {
		if entry, ok := existing[e]; ok {
			envList[i] = entry
		} else {
			envList[i] = map[string]interface{}{"name": e}
		}
	}
	if err := unstructured.SetNestedSlice(project.Object, envList, "spec", "environments"); err != nil {
		return fmt.Errorf("setting environments: %w", err)
	}
	if _, err := dynClient.Resource(manifest.ProjectGVR).Update(ctx, project, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating project: %w", err)
	}
	return nil
}

func runProjectAddEnv(cmd *cobra.Command, args []string) error {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	projectName, envName, err := parseEnvCmdArgs(cluster, args)
	if err != nil {
		return err
	}

	ctx := context.Background()
	dynClient := k8sClient.Dynamic()
	crName := cluster.ResolveNamespace(projectName, "")

	project, envs, err := loadProjectCR(ctx, dynClient, crName)
	if err != nil {
		return err
	}
	for _, existing := range envs {
		if existing == envName {
			return fmt.Errorf("environment %q already exists in project %q", envName, projectName)
		}
	}

	envs = append(envs, envName)
	if err := addProjectEnvironment(ctx, dynClient, project, envs, envName); err != nil {
		return err
	}

	expectedNs := cluster.ResolveNamespace(projectName, envName)
	fmt.Printf("\n  Adding environment %q to project %q...\n", envName, projectName)

	clientset := k8sClient.Clientset()
	for attempt := 0; attempt < 30; attempt++ {
		if _, err := clientset.CoreV1().Namespaces().Get(ctx, expectedNs, metav1.GetOptions{}); err == nil {
			fmt.Printf("  ✔  %s\n\n", expectedNs)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("  ⚠  Namespace %s not yet visible — controller may still be reconciling\n\n", expectedNs)
	return nil
}

func runProjectRemoveEnv(cmd *cobra.Command, args []string) error {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	projectName, envName, err := parseEnvCmdArgs(cluster, args)
	if err != nil {
		return err
	}

	ctx := context.Background()
	dynClient := k8sClient.Dynamic()
	crName := cluster.ResolveNamespace(projectName, "")

	project, envs, err := loadProjectCR(ctx, dynClient, crName)
	if err != nil {
		return err
	}

	remaining := make([]string, 0, len(envs))
	found := false
	for _, e := range envs {
		if e == envName {
			found = true
			continue
		}
		remaining = append(remaining, e)
	}
	if !found {
		return fmt.Errorf("environment %q not found in project %q (have: %s)", envName, projectName, strings.Join(envs, ", "))
	}
	if len(remaining) == 0 {
		return fmt.Errorf("cannot remove the last environment — delete the project instead with 'kip project delete %s'", projectName)
	}

	ns := cluster.ResolveNamespace(projectName, envName)

	// Surface what's about to be destroyed so the user knows the blast radius.
	clientset := k8sClient.Clientset()
	apps, _ := dynClient.Resource(manifest.AppGVR).Namespace(ns).List(ctx, metav1.ListOptions{})
	appCount := 0
	if apps != nil {
		appCount = len(apps.Items)
	}

	fmt.Printf("\n  Removing %q from project %q will delete namespace %s and everything in it.\n", envName, projectName, ns)
	if appCount > 0 {
		fmt.Printf("  This destroys %d running app(s):\n", appCount)
		for _, a := range apps.Items {
			fmt.Printf("    • %s\n", a.GetName())
		}
	}

	if !confirmTypePrompt("This action cannot be undone.", envName) {
		fmt.Println("\n  Cancelled.")
		return nil
	}

	if err := writeProjectEnvironments(ctx, dynClient, project, remaining); err != nil {
		return err
	}

	fmt.Printf("\n  Removing environment %q...\n", envName)
	for attempt := 0; attempt < 30; attempt++ {
		_, err := clientset.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			fmt.Printf("  ✔  Namespace %s deleted\n\n", ns)
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("  ⚠  Namespace %s still present — controller may still be tearing it down\n\n", ns)
	return nil
}

// refuseNamespaceCollision stops a project whose environments would use a
// namespace another project already uses.
//
// The reconciler refuses to adopt a namespace another project owns, so a
// collision cannot silently merge two projects. What it cannot do is prevent
// the second project from being created: it lands, half its namespaces work,
// and the rest carry a conflict condition. Catching it here means the operator
// is told before that happens rather than after.
//
// It is a check, not a guarantee. A direct kubectl write or two concurrent
// creates still reach the reconciler, which is where the actual protection is.
func refuseNamespaceCollision(ctx context.Context, dynClient dynamic.Interface, projectName string, envs []string) error {
	wanted := make(map[string]string, len(envs))
	for _, env := range envs {
		wanted[projectNamespace(projectName, env)] = env
	}

	list, err := dynClient.Resource(manifest.ProjectGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		// Stop. A check that could not run is not a check that passed, and the
		// two outcomes are not equally cheap to undo: this command is a moment
		// away from retrying, whereas a project created onto a name it cannot
		// have has to be recreated under another one, because a project cannot
		// be renamed.
		return fmt.Errorf("could not check whether another project already uses these namespaces: %w\n"+
			"    Nothing was created. Try again", err)
	}
	for i := range list.Items {
		other := list.Items[i].GetName()
		if other == projectName {
			continue
		}
		for _, name := range projectEnvironmentNames(&list.Items[i]) {
			ns := projectNamespace(other, name)
			if env, clash := wanted[ns]; clash {
				return fmt.Errorf(
					"environment %q would use namespace %q, which project %q already uses.\n"+
						"    Two projects cannot share a namespace, so one of them has to be named differently",
					env, ns, other)
			}
		}
	}
	return nil
}

// projectNamespace mirrors the namespace the project reconciler creates for an
// environment. It is deliberately the same rule, and the reconciler is the one
// that enforces it.
func projectNamespace(projectName, envName string) string {
	if envName == "" || envName == "default" {
		return projectName
	}
	return projectName + "-" + envName
}

// defaultEnvironmentName is the environment the project reconciler creates for
// a project that declares none. Mirrored here for the same reason
// projectNamespace is: resolving a project to its namespaces takes both rules,
// and a check holding only one of them guards a set the reconciler never builds.
const defaultEnvironmentName = "test"

// projectEnvironmentNames reads another project's environments as the
// reconciler will build them, default included. A project written with an empty
// list still gets a namespace, and a collision check that cannot see it
// approves a name that namespace already occupies.
func projectEnvironmentNames(project *unstructured.Unstructured) []string {
	raw, _, _ := unstructured.NestedSlice(project.Object, "spec", "environments")
	names := make([]string, 0, len(raw))
	for _, entry := range raw {
		envMap, ok := entry.(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := envMap["name"].(string); ok {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return []string{defaultEnvironmentName}
	}
	return names
}

// createProjectCR refuses a colliding project before it is written, then writes
// it.
//
// The check and the write are one function so a test can pin them together. The
// check on its own proves nothing about whether anything calls it, and this
// began as a bypass: the CLI wrote the Project straight through the dynamic
// client while the check lived only in the console API.
func createProjectCR(ctx context.Context, dynClient dynamic.Interface, projectCR *unstructured.Unstructured, crName string, envs []string) error {
	if err := refuseNamespaceCollision(ctx, dynClient, crName, envs); err != nil {
		return err
	}
	if _, err := dynClient.Resource(manifest.ProjectGVR).Create(ctx, projectCR, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating project: %w", err)
	}
	return nil
}

// addProjectEnvironment refuses an environment whose namespace another project
// already uses, then writes the new environment list. Paired with
// createProjectCR for the same reason.
func addProjectEnvironment(ctx context.Context, dynClient dynamic.Interface, project *unstructured.Unstructured, envs []string, adding string) error {
	if err := refuseNamespaceCollision(ctx, dynClient, project.GetName(), []string{adding}); err != nil {
		return err
	}
	return writeProjectEnvironments(ctx, dynClient, project, envs)
}

// setActiveProject records the per-cluster project context on the current
// cluster, and reports which cluster that was.
//
// Only those two fields are written. Saving a whole config read beforehand would
// restore every other cluster as it looked then, including a gateway credential
// a concurrent uninstall had mirrored in the meantime.
func setActiveProject(clusterName *string, project, environment string) error {
	return config.Update(func(cfg *config.Config) error {
		if cfg.CurrentCluster == "" {
			return fmt.Errorf("no current cluster — run 'kip cluster use <name>' first")
		}
		cluster := cfg.GetCluster(cfg.CurrentCluster)
		if cluster == nil {
			return fmt.Errorf("current cluster %q not found in config", cfg.CurrentCluster)
		}
		*clusterName = cluster.Name
		cluster.CurrentProject = project
		cluster.CurrentEnvironment = environment
		return nil
	})
}
