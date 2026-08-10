package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/kip/internal/auth"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/function"
)

var functionCmd = &cobra.Command{
	Use:     "function",
	Aliases: []string{"fn"},
	Short:   "Manage serverless functions",
}

var functionCreateCmd = &cobra.Command{
	Use:   "create [name]",
	Short: "Create a serverless function",
	Args:  cobra.ExactArgs(1),
	RunE:  runFunctionCreate,
}

var functionListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all functions",
	RunE:  runFunctionList,
}

var functionLogsCmd = &cobra.Command{
	Use:   "logs [name]",
	Short: "Stream function logs",
	Args:  cobra.ExactArgs(1),
	RunE:  runFunctionLogs,
}

var functionDeleteCmd = &cobra.Command{
	Use:   "delete [name...]",
	Short: "Delete one or more functions",
	Args:  cobra.MinimumNArgs(1),
	RunE:  runFunctionDelete,
}

var functionBindCmd = &cobra.Command{
	Use:   "bind [function] [service]",
	Short: "Bind a service to a function, injecting connection credentials",
	Long: `Binds a Service CR to a Function CR. The function's Pod (and its
CronJob Pod, if any) gets the bound Service's credentials injected as
prefixed environment variables (e.g. DB_HOST, DB_PASSWORD).

Examples:
  kip function bind my-fn mydb
  kip function bind my-fn mydb --prefix MAIN_DB_
  kip function bind my-fn mydb --database custom_db_name`,
	Args: cobra.ExactArgs(2),
	RunE: runFunctionBind,
}

var functionUnbindCmd = &cobra.Command{
	Use:   "unbind [function] [service]",
	Short: "Remove a service binding from a function",
	Args:  cobra.ExactArgs(2),
	RunE:  runFunctionUnbind,
}

// Function-side env / secret commands. These reuse the run* functions already
// implemented for apps, which read the workload kind off the command tree they
// were invoked through: `kip function env set api` addresses
// `function-api-env`, `function-api-secrets` and the Function CR, `kip app env
// set api` addresses `app-api-env`, `app-api-secrets` and the App CR.

var functionEnvCmd = &cobra.Command{
	Use:   "env",
	Short: "Manage environment variables for a function",
}

var functionEnvSetCmd = &cobra.Command{
	Use:   "set [function-name] KEY=VALUE...",
	Short: "Set environment variables on a function",
	Args:  cobra.MinimumNArgs(2),
	RunE:  runEnvSet,
}

var functionEnvListCmd = &cobra.Command{
	Use:   "list [function-name]",
	Short: "List environment variables for a function",
	Args:  cobra.ExactArgs(1),
	RunE:  runEnvList,
}

var functionEnvDeleteCmd = &cobra.Command{
	Use:   "delete [function-name] KEY",
	Short: "Delete an environment variable from a function",
	Args:  cobra.ExactArgs(2),
	RunE:  runEnvDelete,
}

var functionSecretCmd = &cobra.Command{
	Use:   "secret",
	Short: "Manage secret environment variables for a function",
}

var functionSecretSetCmd = &cobra.Command{
	Use:   "set [function-name] [key]",
	Short: "Set a secret on a function (prompts for the value)",
	Args:  cobra.ExactArgs(2),
	RunE:  runSecretSet,
}

var functionSecretListCmd = &cobra.Command{
	Use:   "list [function-name]",
	Short: "List secret keys on a function (values are masked)",
	Args:  cobra.ExactArgs(1),
	RunE:  runSecretList,
}

var functionSecretDeleteCmd = &cobra.Command{
	Use:   "delete [function-name] [key]",
	Short: "Delete a secret from a function",
	Args:  cobra.ExactArgs(2),
	RunE:  runSecretDelete,
}

func init() {
	functionCreateCmd.Flags().String("image", "", "container image for the function (mutually exclusive with --code-file)")
	functionCreateCmd.Flags().String("code-file", "", "path to a file containing the function source for an inline function (Node: index.js style; Python: handler.py style)")
	functionCreateCmd.Flags().Int("port", 8080, "port the function listens on")
	functionCreateCmd.Flags().String("runtime", "", "runtime for inline functions (node, python). Required with --code-file.")
	functionCreateCmd.Flags().String("trigger", "http", "trigger type (http, cron, postgres, mysql, redis, minio)")
	functionCreateCmd.Flags().String("schedule", "", "cron expression when --trigger=cron (e.g. \"0 2 * * *\")")
	functionCreateCmd.Flags().StringArray("dependency", nil, "third-party dependency for an inline function (repeatable, name@version, e.g. --dependency pg@8.11.5)")
	functionCreateCmd.Flags().StringArray("volume", nil, "shared volume to mount into the function (repeatable, name:/container/path, e.g. --volume cache:/data)")
	functionCreateCmd.Flags().String("project", "", "project name")
	functionCreateCmd.Flags().String("environment", "", "target environment")
	functionCreateCmd.Flags().String("source", "", "service name for event trigger (e.g. mydb, cache)")
	functionCreateCmd.Flags().String("query", "", "SQL query for postgres/mysql triggers")
	functionCreateCmd.Flags().String("mark-done", "", "SQL to mark rows processed (e.g. \"UPDATE x SET done=true WHERE id={{id}}\")")
	functionCreateCmd.Flags().String("list", "", "Redis list name for redis trigger")
	functionCreateCmd.Flags().String("bucket", "", "MinIO bucket name for minio trigger")

	functionCreateCmd.MarkFlagsMutuallyExclusive("image", "code-file")

	functionListCmd.Flags().String("project", "", "project name")
	functionListCmd.Flags().String("environment", "", "target environment")

	functionLogsCmd.Flags().String("project", "", "project name")
	functionLogsCmd.Flags().String("environment", "", "target environment")

	functionDeleteCmd.Flags().String("project", "", "project name")
	functionDeleteCmd.Flags().String("environment", "", "target environment")

	functionBindCmd.Flags().String("project", "", "project name")
	functionBindCmd.Flags().String("environment", "", "target environment")
	functionBindCmd.Flags().String("prefix", "", "env var prefix (defaults to a type-based prefix like DB_, REDIS_)")
	functionBindCmd.Flags().String("database", "", "custom per-function database name (database services only)")

	functionUnbindCmd.Flags().String("project", "", "project name")
	functionUnbindCmd.Flags().String("environment", "", "target environment")

	restartConfigFlag(functionEnvSetCmd, functionEnvDeleteCmd, functionSecretSetCmd, functionSecretDeleteCmd)

	functionEnvSetCmd.Flags().String("from-file", "", "load env vars from a file (KEY=VALUE per line)")
	functionEnvSetCmd.Flags().String("project", "", "project name")
	functionEnvSetCmd.Flags().String("environment", "", "target environment")
	functionEnvListCmd.Flags().String("project", "", "project name")
	functionEnvListCmd.Flags().String("environment", "", "target environment")
	functionEnvDeleteCmd.Flags().String("project", "", "project name")
	functionEnvDeleteCmd.Flags().String("environment", "", "target environment")

	functionSecretSetCmd.Flags().String("project", "", "project name")
	functionSecretSetCmd.Flags().String("environment", "", "target environment")
	functionSecretListCmd.Flags().String("project", "", "project name")
	functionSecretListCmd.Flags().String("environment", "", "target environment")
	functionSecretDeleteCmd.Flags().String("project", "", "project name")
	functionSecretDeleteCmd.Flags().String("environment", "", "target environment")

	functionEnvCmd.AddCommand(functionEnvSetCmd, functionEnvListCmd, functionEnvDeleteCmd)
	functionSecretCmd.AddCommand(functionSecretSetCmd, functionSecretListCmd, functionSecretDeleteCmd)

	functionCmd.AddCommand(functionCreateCmd)
	functionCmd.AddCommand(functionListCmd)
	functionCmd.AddCommand(functionLogsCmd)
	functionCmd.AddCommand(functionDeleteCmd)
	functionCmd.AddCommand(functionBindCmd)
	functionCmd.AddCommand(functionUnbindCmd)
	functionCmd.AddCommand(functionEnvCmd)
	functionCmd.AddCommand(functionSecretCmd)
	rootCmd.AddCommand(functionCmd)
}

func runFunctionCreate(cmd *cobra.Command, args []string) error {
	name := args[0]
	image, _ := cmd.Flags().GetString("image")
	codeFile, _ := cmd.Flags().GetString("code-file")
	port, _ := cmd.Flags().GetInt("port")
	runtime, _ := cmd.Flags().GetString("runtime")
	trigger, _ := cmd.Flags().GetString("trigger")
	schedule, _ := cmd.Flags().GetString("schedule")

	if image == "" && codeFile == "" {
		return fmt.Errorf("either --image or --code-file is required")
	}
	if codeFile != "" && runtime == "" {
		return fmt.Errorf("--runtime is required with --code-file (e.g. --runtime node)")
	}
	if trigger == string(function.TriggerCron) && schedule == "" {
		return fmt.Errorf("--schedule is required when --trigger=cron (e.g. --schedule \"0 2 * * *\")")
	}

	var code string
	if codeFile != "" {
		raw, readErr := os.ReadFile(codeFile)
		if readErr != nil {
			return fmt.Errorf("reading --code-file %s: %w", codeFile, readErr)
		}
		code = string(raw)
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	project, environment := resolveProjectAndEnvironment(cmd, cluster)
	var ns string
	if project != "" {
		ns = cluster.ResolveNamespace(project, environment)
	} else {
		// No flag, no persisted context — try to find a function of this
		// name, then fall back to default. Users with kip project use
		// set will not hit this path.
		ctx := context.Background()
		ns, err = findFunctionNamespaceOrDefault(ctx, k8sClient.Clientset(), k8sClient.Dynamic(), name)
		if err != nil {
			return err
		}
	}

	source, _ := cmd.Flags().GetString("source")
	query, _ := cmd.Flags().GetString("query")
	markDone, _ := cmd.Flags().GetString("mark-done")
	redisList, _ := cmd.Flags().GetString("list")
	bucket, _ := cmd.Flags().GetString("bucket")
	deps, _ := cmd.Flags().GetStringArray("dependency")
	volumes, _ := cmd.Flags().GetStringArray("volume")

	parsedDeps, depErr := parseDependencies(deps)
	if depErr != nil {
		return depErr
	}
	parsedVolumes, volErr := parseVolumeMounts(volumes)
	if volErr != nil {
		return volErr
	}

	mgr := &function.Manager{Dynamic: k8sClient.Dynamic()}

	opts := function.Options{
		Name:         name,
		Namespace:    ns,
		Image:        image,
		Port:         int32(port), //nolint:gosec // CLI flag values are bounded
		Runtime:      runtime,
		Code:         code,
		Dependencies: parsedDeps,
		Volumes:      parsedVolumes,
		Trigger:      function.TriggerType(trigger),
		Schedule:     schedule,
		SourceName:   source,
		Query:        query,
		MarkDone:     markDone,
		RedisList:    redisList,
		MinioBucket:  bucket,
	}

	fmt.Printf("\n  Creating function %s...\n", name)
	fmt.Printf("  Trigger: %s\n", trigger)
	if schedule != "" {
		fmt.Printf("  Schedule: %s\n", schedule)
	}
	if image != "" {
		fmt.Printf("  Image: %s\n", image)
	} else {
		fmt.Printf("  Inline source: %s (%s runtime, %d byte(s))\n", codeFile, runtime, len(code))
		if len(parsedDeps) > 0 {
			fmt.Printf("  Dependencies: %d package(s)\n", len(parsedDeps))
		}
	}
	if source != "" {
		fmt.Printf("  Source: %s\n", source)
	}

	ctx := context.Background()
	if err := mgr.Create(ctx, opts); err != nil {
		return err
	}

	fmt.Printf("  ✔  Function %s created\n", name)
	fmt.Printf("  Run `kip function list` to see status.\n")
	fmt.Println()
	return nil
}

func runFunctionList(cmd *cobra.Command, args []string) error {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	project, environment := resolveProjectAndEnvironment(cmd, cluster)

	ctx := context.Background()
	mgr := &function.Manager{Dynamic: k8sClient.Dynamic()}

	if project != "" {
		ns := cluster.ResolveNamespace(project, environment)
		return printFunctions(ctx, mgr, ns)
	}

	// List across all kipper namespaces
	namespaces, err := k8sClient.Clientset().CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=kipper",
	})
	if err != nil {
		return fmt.Errorf("listing namespaces: %w", err)
	}

	found := false
	for _, ns := range namespaces.Items {
		if err := printFunctions(ctx, mgr, ns.Name); err == nil {
			found = true
		}
	}

	if !found {
		// Try default namespace
		return printFunctions(ctx, mgr, "default")
	}

	return nil
}

func printFunctions(ctx context.Context, mgr *function.Manager, namespace string) error {
	functions, err := mgr.List(ctx, namespace)
	if err != nil {
		return err
	}

	if len(functions) == 0 {
		fmt.Printf("\n  No functions in %s\n\n", namespace)
		return nil
	}

	fmt.Printf("\n  %-25s %-10s %-40s %-10s\n", "NAME", "TRIGGER", "IMAGE", "STATUS")
	for _, fn := range functions {
		fmt.Printf("  %-25s %-10s %-40s %-10s\n", fn.Name, fn.Trigger, fn.Image, fn.Status)
	}
	fmt.Println()

	return nil
}

func runFunctionLogs(cmd *cobra.Command, args []string) error {
	name := args[0]

	ns, k8sClient, err := resolveAppNamespace(cmd, name)
	if err != nil {
		return err
	}

	ctx := context.Background()
	clientset := k8sClient.Clientset()

	pods, err := clientset.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", name),
	})
	if err != nil || len(pods.Items) == 0 {
		return fmt.Errorf("no running pods for function %q: it may be scaled to zero", name)
	}

	tailLines := int64(100)
	req := clientset.CoreV1().Pods(ns).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{
		Container: workloadContainerName(&pods.Items[0], name),
		Follow:    true,
		TailLines: &tailLines,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("opening log stream: %w", err)
	}
	defer func() { _ = stream.Close() }()

	_, err = io.Copy(os.Stdout, stream)
	return err
}

func runFunctionDelete(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	var firstErr error
	for _, name := range args {
		ns, k8sClient, err := resolveAppNamespace(cmd, name)
		if err != nil {
			fmt.Printf("\n  ✗  %s: %v\n", name, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		mgr := &function.Manager{Dynamic: k8sClient.Dynamic()}
		fmt.Printf("\n  Deleting function %s...\n", name)
		if err := mgr.Delete(ctx, ns, name); err != nil {
			fmt.Printf("  ✗  %s: %v\n", name, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		fmt.Printf("  ✔  Function %s deleted\n", name)
	}
	fmt.Println()
	return firstErr
}

func runFunctionBind(cmd *cobra.Command, args []string) error {
	functionName := args[0]
	serviceName := args[1]
	prefix, _ := cmd.Flags().GetString("prefix")
	database, _ := cmd.Flags().GetString("database")

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	project, environment := resolveProjectAndEnvironment(cmd, cluster)
	namespace := cluster.ResolveNamespace(project, environment)
	if namespace == "" {
		ctx := context.Background()
		namespace, err = findFunctionNamespaceOrDefault(ctx, k8sClient.Clientset(), k8sClient.Dynamic(), functionName)
		if err != nil {
			return err
		}
	}

	reqBody := map[string]string{
		"service":   serviceName,
		"app":       functionName,
		"namespace": namespace,
		"target":    "function",
	}
	if prefix != "" {
		reqBody["prefix"] = prefix
	}
	if database != "" {
		reqBody["database"] = database
	}

	resp, err := callBindAPI(cluster, "/api/v1/bind", reqBody)
	if err != nil {
		return err
	}

	fmt.Printf("\n  ✔  Bound %s (%s) → %s\n", serviceName, resp.Type, functionName)
	if resp.Database != "" {
		fmt.Printf("  ✔  Database: %s\n", resp.Database)
	}
	if len(resp.Injected) > 0 {
		fmt.Printf("\n  Injected environment variables:\n")
		for key, val := range resp.Injected {
			fmt.Printf("    %s=%s\n", key, val)
		}
	}
	fmt.Println()
	return nil
}

func runFunctionUnbind(cmd *cobra.Command, args []string) error {
	functionName := args[0]
	serviceName := args[1]

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	project, environment := resolveProjectAndEnvironment(cmd, cluster)
	namespace := cluster.ResolveNamespace(project, environment)
	if namespace == "" {
		ctx := context.Background()
		namespace, err = findFunctionNamespaceOrDefault(ctx, k8sClient.Clientset(), k8sClient.Dynamic(), functionName)
		if err != nil {
			return err
		}
	}

	reqBody := map[string]string{
		"service":   serviceName,
		"app":       functionName,
		"namespace": namespace,
		"target":    "function",
	}

	if _, err := callBindAPI(cluster, "/api/v1/unbind", reqBody); err != nil {
		return err
	}
	fmt.Printf("\n  ✔  Unbound %s from %s\n\n", serviceName, functionName)
	return nil
}

// parseVolumeMounts turns repeated --volume name:/path flags into a
// []VolumeMount. Each value must be name:path; an empty name or path
// is rejected loudly so the user notices a typo before deploying.
func parseVolumeMounts(specs []string) ([]function.VolumeMount, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make([]function.VolumeMount, 0, len(specs))
	for _, raw := range specs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		idx := strings.Index(raw, ":")
		if idx <= 0 || idx == len(raw)-1 {
			return nil, fmt.Errorf("invalid --volume value %q (expected name:/path)", raw)
		}
		out = append(out, function.VolumeMount{
			Name:      raw[:idx],
			MountPath: raw[idx+1:],
		})
	}
	return out, nil
}

// parseDependencies turns repeated --dependency name@version flags into
// a map. Versions are optional ("--dependency pg" → pg=latest).
func parseDependencies(specs []string) (map[string]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(specs))
	for _, raw := range specs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		// Split on the LAST @ so scoped npm packages like @scope/name@1.0.0 work.
		idx := strings.LastIndex(raw, "@")
		// idx==0 means raw begins with @ (npm scoped package without a version).
		if idx <= 0 {
			out[raw] = "latest"
			continue
		}
		name := raw[:idx]
		version := raw[idx+1:]
		if name == "" {
			return nil, fmt.Errorf("invalid --dependency value %q", raw)
		}
		if version == "" {
			version = "latest"
		}
		out[name] = version
	}
	return out, nil
}

// bindAPIResponse mirrors the console-api bindResponse.
type bindAPIResponse struct {
	Service  string            `json:"service"`
	App      string            `json:"app"`
	Type     string            `json:"type"`
	Database string            `json:"database,omitempty"`
	Injected map[string]string `json:"injected"`
}

// callBindAPI POSTs to the console-api bind/unbind endpoints. Shared by
// kip service bind / unbind and kip function bind / unbind.
func callBindAPI(cluster *config.Cluster, path string, reqBody map[string]string) (*bindAPIResponse, error) {
	store, err := auth.Load()
	if err != nil {
		return nil, fmt.Errorf("loading auth: %w", err)
	}
	token, err := store.Token(cluster.Domain, cluster.DexHost())
	if err != nil {
		return nil, err
	}

	body, _ := json.Marshal(reqBody)
	url := fmt.Sprintf("https://%s%s", cluster.ConsoleAPIHost(), path)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return nil, authRejectedError(req.Context(), cluster)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API %s: %s", path, string(respBody))
	}

	var out bindAPIResponse
	if len(respBody) > 0 {
		_ = json.Unmarshal(respBody, &out)
	}
	return &out, nil
}
