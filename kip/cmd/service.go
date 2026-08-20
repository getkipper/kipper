package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/auth"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

var serviceCmd = &cobra.Command{
	Use:   "service",
	Short: "Manage stateful services (databases, caches)",
}

var serviceAddCmd = &cobra.Command{
	Use:   "add [type] --name [name]",
	Short: "Deploy a stateful service",
	Long: fmt.Sprintf(`Deploy a stateful service with persistent storage.

Supported types: %s

Examples:
  kip service add postgres --name mydb
  kip service add redis --name cache --storage 2Gi`, strings.Join(service.SupportedTypes(), ", ")),
	Args: cobra.ExactArgs(1),
	RunE: runServiceAdd,
}

var serviceListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all stateful services",
	RunE:  runServiceList,
}

var serviceInfoCmd = &cobra.Command{
	Use:   "info [name]",
	Short: "Show connection details for a service",
	Args:  cobra.ExactArgs(1),
	RunE:  runServiceInfo,
}

var serviceCredentialsCmd = &cobra.Command{
	Use:   "credentials",
	Short: "Check that each service owns its credentials, and repair what does not",
	Long: `Report whether each service's credentials Secret is owned by that service.

Ownership is what admits a service's credentials into a workload bound to it. A
Secret that lost its owner leaves the service running while everything bound to
it is refused, so this is the question to ask before a controller rollout and
after restoring a backup.

--repair gives an unowned Secret back to its service and removes per-binding
Secrets nothing owns, which the workloads render again for themselves.`,
	RunE: runServiceCredentials,
}

var serviceUpdateCmd = &cobra.Command{
	Use:   "update [name]",
	Short: "Update a service's resources, storage, or version",
	Long: `Update a running service without losing data.

Storage expansion is live (no restart needed).
Memory, CPU, and version changes restart the service pod.

Examples:
  kip service update mydb --storage 20Gi
  kip service update mydb --memory 2Gi --cpu 2
  kip service update mydb --version 17`,
	Args: cobra.ExactArgs(1),
	RunE: runServiceUpdate,
}

var serviceDeleteCmd = &cobra.Command{
	Use:   "delete [name]",
	Short: "Delete a stateful service and its data",
	Args:  cobra.ExactArgs(1),
	RunE:  runServiceDelete,
}

var serviceBindCmd = &cobra.Command{
	Use:   "bind [service] [app]",
	Short: "Bind a service to an app, injecting connection credentials",
	Long: `Binds a service to an application by injecting connection credentials
as environment variables. For database services (postgres, mysql, mongodb),
a per-app database is created automatically.

Examples:
  kip service bind mydb api
  kip service bind mydb api --prefix MAIN_DB_
  kip service bind mydb api --database custom_db_name`,
	Args: cobra.ExactArgs(2),
	RunE: runServiceBind,
}

var serviceUnbindCmd = &cobra.Command{
	Use:   "unbind [service] [app]",
	Short: "Remove a service binding from an app",
	Args:  cobra.ExactArgs(2),
	RunE:  runServiceUnbind,
}

func init() {
	serviceAddCmd.Flags().String("name", "", "service name")
	serviceAddCmd.Flags().String("project", "default", "project namespace")
	serviceAddCmd.Flags().String("environment", "", "target environment (e.g. test, acc, prod)")
	serviceAddCmd.Flags().String("storage", "", "storage size (e.g. 5Gi, 10Gi)")
	serviceAddCmd.Flags().String("memory", "", "memory limit (e.g. 256Mi, 1Gi)")
	serviceAddCmd.Flags().String("cpu", "", "CPU limit (e.g. 500m, 1)")
	serviceAddCmd.Flags().String("version", "", "image version override (e.g. 16, 8.0, 7)")
	_ = serviceAddCmd.MarkFlagRequired("name")

	serviceListCmd.Flags().String("project", "default", "project namespace")
	serviceListCmd.Flags().String("environment", "", "target environment")

	serviceInfoCmd.Flags().String("project", "default", "project namespace")
	serviceInfoCmd.Flags().String("environment", "", "target environment")

	serviceUpdateCmd.Flags().String("project", "default", "project namespace")
	serviceUpdateCmd.Flags().String("environment", "", "target environment")
	serviceUpdateCmd.Flags().String("storage", "", "new storage size (can only increase)")
	serviceUpdateCmd.Flags().String("memory", "", "new memory limit (e.g. 1Gi)")
	serviceUpdateCmd.Flags().String("cpu", "", "new CPU limit (e.g. 1)")
	serviceUpdateCmd.Flags().String("version", "", "new image version")

	serviceDeleteCmd.Flags().String("project", "default", "project namespace")
	serviceDeleteCmd.Flags().String("environment", "", "target environment")
	serviceDeleteCmd.Flags().Bool("delete-data", false, "confirm permanent data deletion")

	serviceBindCmd.Flags().String("project", "default", "project namespace")
	serviceBindCmd.Flags().String("environment", "", "target environment")
	serviceBindCmd.Flags().String("prefix", "", "env var prefix (e.g. DB_, REDIS_), auto-detected from service type if omitted")
	serviceBindCmd.Flags().String("database", "", "custom database name (auto-derived for database services if omitted)")

	serviceUnbindCmd.Flags().String("project", "default", "project namespace")
	serviceUnbindCmd.Flags().String("environment", "", "target environment")

	serviceCmd.AddCommand(serviceAddCmd)
	serviceCmd.AddCommand(serviceListCmd)
	serviceCmd.AddCommand(serviceInfoCmd)
	serviceCmd.AddCommand(serviceUpdateCmd)
	serviceCmd.AddCommand(serviceDeleteCmd)
	serviceCmd.AddCommand(serviceBindCmd)
	serviceCmd.AddCommand(serviceUnbindCmd)
	serviceCredentialsCmd.Flags().String("project", "default", "project namespace")
	serviceCredentialsCmd.Flags().String("environment", "", "target environment")
	serviceCredentialsCmd.Flags().Bool("repair", false, "give unowned credentials back to their service and remove projections nothing owns")
	serviceCmd.AddCommand(serviceCredentialsCmd)
	rootCmd.AddCommand(serviceCmd)
}

func resolveServiceNamespace(cmd *cobra.Command) string {
	cluster, _, err := loadCurrentCluster()
	if err != nil {
		project, environment := resolveProjectAndEnvironment(cmd, nil)
		if environment != "" {
			return fmt.Sprintf("%s-%s", project, environment)
		}
		return project
	}
	project, environment := resolveProjectAndEnvironment(cmd, cluster)
	return cluster.ResolveNamespace(project, environment)
}

func runServiceAdd(cmd *cobra.Command, args []string) error {
	serviceType := args[0]
	name, _ := cmd.Flags().GetString("name")
	namespace := resolveServiceNamespace(cmd)
	storage, _ := cmd.Flags().GetString("storage")
	memory, _ := cmd.Flags().GetString("memory")
	cpu, _ := cmd.Flags().GetString("cpu")
	version, _ := cmd.Flags().GetString("version")

	// Validate service type
	if !service.IsSupported(serviceType) {
		return fmt.Errorf("unsupported service type %q (supported: %v)", serviceType, service.SupportedTypes())
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	dynClient := k8sClient.Dynamic()

	// Check if service already exists
	_, getErr := dynClient.Resource(manifest.ServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if getErr == nil {
		return fmt.Errorf("service %q already exists in %s", name, namespace)
	}
	if !errors.IsNotFound(getErr) {
		return fmt.Errorf("checking service %q: %w", name, getErr)
	}

	if err := refuseServiceNameSharingAnAppCredential(ctx, dynClient, namespace, name); err != nil {
		return err
	}

	// Build Service CR
	spec := map[string]interface{}{
		"type": serviceType,
	}
	if storage != "" {
		spec["storage"] = storage
	}
	if version != "" {
		spec["version"] = version
	}
	resources := map[string]interface{}{}
	if memory != "" {
		resources["memoryRequest"] = memory
		resources["memoryLimit"] = memory
	}
	if cpu != "" {
		resources["cpuRequest"] = cpu
		resources["cpuLimit"] = cpu
	}
	if len(resources) > 0 {
		spec["resources"] = resources
	}

	serviceCR := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Service",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": namespace,
				"labels": map[string]interface{}{
					"app":                          name,
					"app.kubernetes.io/managed-by": "kipper",
					"kipper.run/service-type":      serviceType,
				},
			},
			"spec": spec,
		},
	}

	fmt.Printf("\n  Creating %s service %q in %s...\n", serviceType, name, namespace)

	if _, err := dynClient.Resource(manifest.ServiceGVR).Namespace(namespace).Create(ctx, serviceCR, metav1.CreateOptions{}); err != nil {
		return fmt.Errorf("creating service CR: %w", err)
	}
	fmt.Printf("  ✔  Service CR created\n")

	// Wait for the ServiceReconciler to create the credentials secret
	fmt.Printf("  ...  Waiting for service to initialise\n")
	clientset := k8sClient.Clientset()
	secretName := secretname.ServiceCredentials(name)
	var secret *unstructured.Unstructured
	for attempt := 0; attempt < 30; attempt++ {
		s, err := clientset.CoreV1().Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
		if err == nil {
			// Read credentials from the secret created by the reconciler
			secret = &unstructured.Unstructured{}
			secret.Object = map[string]interface{}{
				"HOST":       string(s.Data["HOST"]),
				"PORT":       string(s.Data["PORT"]),
				"USERNAME":   string(s.Data["USERNAME"]),
				"PASSWORD":   string(s.Data["PASSWORD"]),
				"NAME":       string(s.Data["NAME"]),
				"ENDPOINT":   string(s.Data["ENDPOINT"]),
				"ACCESS_KEY": string(s.Data["ACCESS_KEY"]),
				"SECRET_KEY": string(s.Data["SECRET_KEY"]),
			}
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	fmt.Printf("  ✔  Credentials generated\n")
	fmt.Printf("  ✔  Persistent storage provisioned\n")

	if secret != nil {
		fmt.Printf("\n")
		fmt.Printf("  Connection details:\n")
		if endpoint, _ := secret.Object["ENDPOINT"].(string); endpoint != "" {
			// S3 service (minio): endpoint URL + access key / secret key.
			accessKey, _ := secret.Object["ACCESS_KEY"].(string)
			secretKey, _ := secret.Object["SECRET_KEY"].(string)
			fmt.Printf("    Endpoint:   %s\n", endpoint)
			fmt.Printf("    Access Key: %s\n", accessKey)
			fmt.Printf("    Secret Key: %s\n", secretKey)
		} else {
			host, _ := secret.Object["HOST"].(string)
			port, _ := secret.Object["PORT"].(string)
			username, _ := secret.Object["USERNAME"].(string)
			password, _ := secret.Object["PASSWORD"].(string)
			database, _ := secret.Object["NAME"].(string)

			fmt.Printf("    Host:     %s\n", host)
			fmt.Printf("    Port:     %s\n", port)
			if username != "" {
				fmt.Printf("    Username: %s\n", username)
				fmt.Printf("    Password: %s\n", password)
				fmt.Printf("    Database: %s\n", database)
			}
		}
	}

	fmt.Printf("\n")
	fmt.Printf("  To bind to an app:\n")
	fmt.Printf("    kip service bind %s <app>\n", name)
	fmt.Printf("\n")

	return nil
}

func runServiceList(cmd *cobra.Command, args []string) error {
	namespace := resolveServiceNamespace(cmd)

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	mgr := &service.Manager{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	ctx := context.Background()

	services, err := mgr.List(ctx, namespace)
	if err != nil {
		return err
	}

	if len(services) == 0 {
		fmt.Printf("\n  No services in %q\n\n", namespace)
		return nil
	}

	fmt.Printf("\n  %-20s %-12s %-10s %-10s %s\n", "NAME", "TYPE", "STATUS", "READY", "STORAGE")
	for _, s := range services {
		fmt.Printf("  %-20s %-12s %-10s %-10s %s\n", s.Name, s.Type, s.Status, s.Ready, s.Storage)
	}
	fmt.Println()

	return nil
}

func runServiceInfo(cmd *cobra.Command, args []string) error {
	name := args[0]
	namespace := resolveServiceNamespace(cmd)

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	mgr := &service.Manager{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	ctx := context.Background()

	conn, err := mgr.Info(ctx, namespace, name)
	if err != nil {
		return err
	}

	fmt.Printf("\n  Service: %s\n\n", name)
	if conn.Type == "minio" {
		// S3 service: endpoint URL + access key / secret key.
		fmt.Printf("  Endpoint:   %s\n", conn.URL)
		fmt.Printf("  Access Key: %s\n", conn.Username)
		fmt.Printf("  Secret Key: %s\n", conn.Password)
		fmt.Println()
		return nil
	}
	fmt.Printf("  Host:     %s\n", conn.Host)
	fmt.Printf("  Port:     %d\n", conn.Port)
	if conn.Username != "" {
		fmt.Printf("  Username: %s\n", conn.Username)
		fmt.Printf("  Password: %s\n", conn.Password)
		fmt.Printf("  Database: %s\n", conn.Database)
	}
	if conn.URL != "" {
		fmt.Printf("  URL:      %s\n", conn.URL)
	}
	fmt.Println()

	return nil
}

func runServiceUpdate(cmd *cobra.Command, args []string) error {
	name := args[0]
	namespace := resolveServiceNamespace(cmd)
	storage, _ := cmd.Flags().GetString("storage")
	memory, _ := cmd.Flags().GetString("memory")
	cpu, _ := cmd.Flags().GetString("cpu")
	version, _ := cmd.Flags().GetString("version")

	if storage == "" && memory == "" && cpu == "" && version == "" {
		return fmt.Errorf("specify at least one of --storage, --memory, --cpu, or --version")
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	mgr := &service.Manager{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	ctx := context.Background()

	fmt.Printf("\n  Updating service %s...\n", name)

	result, err := mgr.Update(ctx, namespace, name, service.Options{
		Storage:      storage,
		MemoryLimit:  memory,
		CPULimit:     cpu,
		ImageVersion: version,
	})
	if err != nil {
		return err
	}

	if result.StorageExpanded {
		fmt.Printf("  ✔  Storage expanded to %s (live, no restart)\n", storage)
	}
	if result.ResourcesChanged {
		fmt.Printf("  ✔  Resource limits updated")
		if memory != "" {
			fmt.Printf(" (memory: %s", memory)
			if cpu != "" {
				fmt.Printf(", cpu: %s", cpu)
			}
			fmt.Printf(")")
		} else if cpu != "" {
			fmt.Printf(" (cpu: %s)", cpu)
		}
		fmt.Println()
	}
	if result.ImageChanged {
		fmt.Printf("  ✔  Image version updated to %s\n", version)
	}
	if result.NeedsRestart {
		fmt.Printf("  ⚠  Pod will restart to apply changes\n")
	}
	fmt.Println()

	return nil
}

func runServiceDelete(cmd *cobra.Command, args []string) error {
	name := args[0]
	namespace := resolveServiceNamespace(cmd)
	deleteData, _ := cmd.Flags().GetBool("delete-data")

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	dynClient := k8sClient.Dynamic()

	// Try to delete the Service CR first (proper path)
	crErr := dynClient.Resource(manifest.ServiceGVR).Namespace(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if crErr == nil {
		fmt.Printf("\n  ✔  Service %q deleted\n\n", name)
		return nil
	}

	// Fall back to direct resource deletion for services created before CR support
	if !errors.IsNotFound(crErr) && !deleteData {
		return fmt.Errorf("refusing to delete service %q without --delete-data flag (this permanently destroys all data)", name)
	}

	mgr := &service.Manager{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	if err := mgr.Delete(ctx, namespace, name, deleteData); err != nil {
		return err
	}

	fmt.Printf("\n  ✔  Service %q deleted (data destroyed)\n\n", name)
	return nil
}

func runServiceBind(cmd *cobra.Command, args []string) error {
	serviceName := args[0]
	appName := args[1]
	namespace := resolveServiceNamespace(cmd)
	prefix, _ := cmd.Flags().GetString("prefix")
	database, _ := cmd.Flags().GetString("database")

	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	store, err := auth.Load()
	if err != nil {
		return fmt.Errorf("loading auth: %w", err)
	}

	token, err := store.Token(cluster.Domain, cluster.DexHost())
	if err != nil {
		return err
	}

	// Call the console-api bind endpoint
	consoleAPI := fmt.Sprintf("https://%s", cluster.ConsoleAPIHost())
	reqBody := map[string]string{
		"service":   serviceName,
		"app":       appName,
		"namespace": namespace,
	}
	if prefix != "" {
		reqBody["prefix"] = prefix
	}
	if database != "" {
		reqBody["database"] = database
	}

	body, _ := json.Marshal(reqBody)
	ctx := context.Background()
	url := consoleAPI + "/api/v1/bind"

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	fmt.Printf("\n  Binding %s → %s...\n", serviceName, appName)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling bind API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return authRejectedError(ctx, cluster)
	}

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bind failed: %s", string(respBody))
	}

	var result struct {
		Service  string            `json:"service"`
		App      string            `json:"app"`
		Type     string            `json:"type"`
		Database string            `json:"database,omitempty"`
		Injected map[string]string `json:"injected"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}

	fmt.Printf("  ✔  Bound %s (%s) → %s\n", serviceName, result.Type, appName)
	if result.Database != "" {
		fmt.Printf("  ✔  Database: %s\n", result.Database)
	}
	fmt.Printf("\n  Injected environment variables:\n")
	for key, val := range result.Injected {
		fmt.Printf("    %s=%s\n", key, val)
	}
	fmt.Println()

	return nil
}

func runServiceUnbind(cmd *cobra.Command, args []string) error {
	serviceName := args[0]
	appName := args[1]
	namespace := resolveServiceNamespace(cmd)

	cluster, _, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	store, err := auth.Load()
	if err != nil {
		return fmt.Errorf("loading auth: %w", err)
	}

	token, err := store.Token(cluster.Domain, cluster.DexHost())
	if err != nil {
		return err
	}

	consoleAPI := fmt.Sprintf("https://%s", cluster.ConsoleAPIHost())
	reqBody := map[string]string{
		"service":   serviceName,
		"app":       appName,
		"namespace": namespace,
	}

	body, _ := json.Marshal(reqBody)
	ctx := context.Background()
	url := consoleAPI + "/api/v1/unbind"

	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	fmt.Printf("\n  Unbinding %s from %s...\n", serviceName, appName)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling unbind API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return authRejectedError(ctx, cluster)
	}
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("unbind failed: %s", string(respBody))
	}

	fmt.Printf("  ✔  Unbound %s from %s\n\n", serviceName, appName)
	return nil
}

func runServiceCredentials(cmd *cobra.Command, args []string) error {
	namespace := resolveServiceNamespace(cmd)

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	mgr := &service.Manager{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	ctx := context.Background()

	audit, err := mgr.AuditCredentials(ctx, namespace)
	if err != nil {
		return err
	}

	if len(audit.Services) == 0 && len(audit.Projections) == 0 {
		fmt.Printf("\n  No services in %q\n\n", namespace)
		return nil
	}

	fmt.Printf("\n  %-24s %-34s %s\n", "SERVICE", "SECRET", "STATE")
	for _, s := range audit.Services {
		state := string(s.State)
		if s.Owner != "" {
			state = fmt.Sprintf("%s (%s)", state, s.Owner)
		}
		fmt.Printf("  %-24s %-34s %s\n", s.Service, s.Secret, state)
	}
	for _, p := range audit.Projections {
		fmt.Printf("  %-24s %-34s %s\n", "-", p.Name, "unowned binding secret")
	}
	fmt.Println()

	if !audit.NeedsRepair() {
		fmt.Printf("  Every service owns its credentials.\n\n")
		return nil
	}

	repair, _ := cmd.Flags().GetBool("repair")
	if !repair {
		fmt.Printf("  Run again with --repair to fix these.\n\n")
		return nil
	}

	done, err := mgr.RepairCredentials(ctx, namespace, audit)
	for _, line := range done {
		fmt.Printf("  ✔  %s\n", line)
	}
	if err != nil {
		fmt.Println()
		return err
	}
	fmt.Println()
	return nil
}

// refuseServiceNameSharingAnAppCredential stops a service being created whose
// credentials Secret is the object an app already keeps its git token in. The
// console refuses the same name for the same reason; the check lives in
// secretname so the two cannot drift.
func refuseServiceNameSharingAnAppCredential(ctx context.Context, dyn dynamic.Interface, namespace, name string) error {
	app, collides := secretname.AppSharingServiceCredentialName(name)
	if !collides {
		return nil
	}
	existing, err := dyn.Resource(deployer.AppGVR).Namespace(namespace).Get(ctx, app, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking whether app %s exists: %w", app, err)
	}
	// The app existing is not the collision: an app names its credential after a
	// digest of the pair now, so only one still on the older name has anything
	// at the object this service would take.
	// The error is dropped because it says nothing this decision needs: a field
	// that is absent and a field that is not a string both come back not-found,
	// and either way the app is not on the legacy name.
	credential, named, _ := unstructured.NestedString(existing.Object, "spec", "git", "credentialsSecret")
	if !named || credential != secretname.LegacyGitCredential(app) {
		return nil
	}
	return fmt.Errorf("a service named %q would keep its credentials in %s, which is where the app %q keeps its git token. Pick another name for the service",
		name, secretname.ServiceCredentials(name), app)
}
