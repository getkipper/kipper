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
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/datavolume"
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

--repair gives a Secret back to its service and removes per-binding Secrets
nothing owns, which the workloads render again for themselves.

Two states are repaired: a Secret with no owner at all, and one whose owner
reference names an object that is not there, which is what a restore leaves
behind when the service comes back under a new identity. The second is the
urgent one, because Kubernetes deletes a dependent by exactly that reference and
the password its volume was written under goes with it.

A Secret an object that still exists controls is reported and left alone.`,
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
	Short: "Delete a stateful service, and its volume with --delete-data",
	Args:  cobra.ExactArgs(1),
	RunE:  runServiceDelete,
}

var serviceBindCmd = &cobra.Command{
	Use:   "bind [service] [app]",
	Short: "Bind a service to an app, injecting connection credentials",
	Long: `Binds a service to an application by injecting connection credentials
as environment variables. The app attaches to the service's own database unless
--database names one, which postgres, mysql, mongodb and rabbitmq create inside
the service. Every binding connects as the service's own user, so a named
database decides where an app's data sits rather than what it may reach.

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
	serviceBindCmd.Flags().String("database", "", "database this binding uses, created inside the service if it does not exist (a vhost for rabbitmq); omit to use the service's own")

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
	serviceCredentialsCmd.Flags().Bool("repair", false, "give credentials back to their service where no live object holds them, and remove projections nothing owns")
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
	existing, getErr := dynClient.Resource(manifest.ServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if getErr == nil {
		// A service on its way out still holds the name, and a delete that has
		// stopped holds it for good. "Already exists" would send an operator
		// looking for a service they have just deleted.
		if existing.GetDeletionTimestamp() != nil {
			return fmt.Errorf("service %q in %s is still being deleted and holds the name until it has finished; kip service list says what is holding it up", name, namespace)
		}
		return fmt.Errorf("service %q already exists in %s", name, namespace)
	}
	if !errors.IsNotFound(getErr) {
		return fmt.Errorf("checking service %q: %w", name, getErr)
	}

	if err := refuseServiceNameWhoseCredentialIsTaken(ctx, dynClient, namespace, name); err != nil {
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

	created, err := dynClient.Resource(manifest.ServiceGVR).Namespace(namespace).Create(ctx, serviceCR, metav1.CreateOptions{})
	if err != nil {
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
		// The Secret has to be this service's, not merely standing under its
		// name. One already there is what the reconciler refuses over, and
		// waiting on the name alone reads somebody else's credentials as this
		// service coming up.
		if err == nil && ownedBy(s, created.GetUID()) {
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

	snapshot, _ := (&service.Manager{Client: clientset, Dynamic: dynClient}).Read(ctx, namespace, name)
	workload, workloadErr := clientset.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	sayHowItCameUp(os.Stdout, snapshot, name, secret != nil, workloadErr == nil && ownedBy(workload, created.GetUID()))
	if snapshot.Blocked() {
		// The values in that Secret are whatever is standing under the name, and
		// printing them hands out another object's credentials.
		secret = nil
	}

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

	writeServiceList(os.Stdout, services)
	return nil
}

// writeServiceList prints the table, and under it what an operator has to do
// about anything the reconciler has refused.
//
// The remedy goes below rather than in a column: it is a sentence naming objects
// and commands, and a column wide enough for it would push everything else off
// the screen. A service with nothing wrong adds nothing here.
func writeServiceList(out io.Writer, services []service.Status) {
	say(out, "\n  %-20s %-12s %-10s %-10s %s\n", "NAME", "TYPE", "STATUS", "READY", "STORAGE")
	for _, s := range services {
		say(out, "  %-20s %-12s %-10s %-10s %s\n", s.Name, s.Type, s.Status, s.Ready, s.Storage)
	}
	say(out, "\n")

	for _, s := range services {
		if s.BlockedReason == "" {
			continue
		}
		say(out, "  !   %s (%s)\n", s.Name, s.BlockedReason)
		say(out, "      %s\n\n", s.BlockedMessage)
	}
}

// writeBlockedNotice says why a service was refused, and says nothing at all
// when it was not.
//
// The headline names no subsystem, because more than one refusal reaches it: the
// credentials the reconciler would not use, and a name that belongs to something
// else. The reason and the message say which.
func writeBlockedNotice(out io.Writer, name, reason, message string) {
	if reason == "" {
		return
	}
	say(out, "  !   %s is blocked (%s)\n", name, reason)
	say(out, "      %s\n\n", message)
}

func runServiceInfo(cmd *cobra.Command, args []string) error {
	name := args[0]
	namespace := resolveServiceNamespace(cmd)

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	mgr := &service.Manager{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	return writeServiceInfo(context.Background(), os.Stdout, mgr, namespace, name)
}

// writeServiceInfo prints what an operator asked to know about one service.
//
// A refusal is reported instead of the connection details rather than above
// them. The details are read out of the very Secret the refusal is about, so in
// the case this exists for there is nothing left to read and the command would
// fail with "not found", burying the remedy. Where something is still readable
// it is a credential the engine will not accept, and printing it sends an
// operator to debug a connection that was never going to work.
func writeServiceInfo(ctx context.Context, out io.Writer, mgr *service.Manager, namespace, name string) error {
	snapshot, err := mgr.Read(ctx, namespace, name)
	if err != nil {
		return err
	}
	if snapshot.Blocked() {
		say(out, "\n  Service: %s\n\n", name)
		writeBlockedNotice(out, name, snapshot.BlockedReason, snapshot.BlockedMessage)
		return nil
	}

	// The type comes from the snapshot that answered the question above, so the
	// command cannot decide it is healthy from one read of the service and then
	// describe a different one.
	conn, err := mgr.Connection(ctx, namespace, name, snapshot.Type)
	if err != nil {
		return err
	}

	say(out, "\n  Service: %s\n\n", name)
	if conn.Type == "minio" {
		// S3 service: endpoint URL + access key / secret key.
		say(out, "  Endpoint:   %s\n", conn.URL)
		say(out, "  Access Key: %s\n", conn.Username)
		say(out, "  Secret Key: %s\n", conn.Password)
		say(out, "\n")
		return nil
	}
	say(out, "  Host:     %s\n", conn.Host)
	say(out, "  Port:     %d\n", conn.Port)
	if conn.Username != "" {
		say(out, "  Username: %s\n", conn.Username)
		say(out, "  Password: %s\n", conn.Password)
		say(out, "  Database: %s\n", conn.Database)
	}
	if conn.URL != "" {
		say(out, "  URL:      %s\n", conn.URL)
	}
	say(out, "\n")

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

	mgr := &service.Manager{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	return deleteService(context.Background(), os.Stdout, mgr, k8sClient.Dynamic(), namespace, name, deleteData)
}

// deleteService removes a service, and its data when that was asked for.
//
// The two are separate steps because they finish at different times. Deleting
// the CR takes the workload with it through owner references, and only once that
// has happened can the volume go: a claim deleted while the StatefulSet is still
// there is written straight back from its template, so the operator would be
// left with an empty volume and the data still on disk.
func deleteService(ctx context.Context, out io.Writer, mgr *service.Manager, dyn dynamic.Interface, namespace, name string, deleteData bool) error {
	// The service is read first, and everything after is pinned to that read. A
	// set of volumes read before it, or a delete sent without it, would let a
	// service that took the name in between be the one this acts on.
	cr, err := readService(ctx, dyn, namespace, name)
	if err != nil {
		return err
	}
	if cr == nil {
		// Nothing to delete under that name. Whatever appears there now is
		// somebody's new service, not this one, so the CR is never touched and
		// only what an earlier delete left behind is cleared.
		return deleteLeftovers(ctx, out, mgr, dyn, namespace, name, deleteData)
	}

	var volumes service.DataVolumes
	var decided string
	if deleteData {
		if volumes, err = mgr.DataVolumes(ctx, namespace, name); err != nil {
			return err
		}
		if decided, err = markForDataDeletion(ctx, dyn, namespace, cr); err != nil {
			return err
		}
	} else if decided, err = clearTheDataMark(ctx, dyn, namespace, cr); err != nil {
		return err
	}

	// Pinned to the version the mark was decided on as well as to the service
	// itself. The identity alone would let somebody else's mark land in between
	// and turn a delete that says it kept the volume into one that destroyed it.
	uid := cr.GetUID()
	pinned := metav1.Preconditions{UID: &uid}
	if decided != "" {
		pinned.ResourceVersion = &decided
	}
	crErr := dyn.Resource(manifest.ServiceGVR).Namespace(namespace).Delete(ctx, name,
		metav1.DeleteOptions{Preconditions: &pinned})
	switch {
	case errors.IsConflict(crErr):
		// Careful about what this claims. The conflict says this delete was not
		// the one the API server took, and nothing more: another may have been
		// accepted a moment earlier and be running now.
		return fmt.Errorf("service %q changed while this delete was running, so this command did not delete it; run kip service list to see where it stands", name)
	case crErr == nil:
		if !deleteData {
			say(out, "\n  ✔  Service %q deleted\n", name)
			// Said plainly, because a volume nobody knows about is what a
			// service of the same name lands on later, and the reconciler then
			// refuses it for having data and no password to go with it.
			say(out, "      Its volume was kept. Run the same delete with --delete-data to remove it.\n\n")
			return nil
		}
		if err := mgr.DestroyVolumes(ctx, namespace, name, volumes); err != nil {
			// The CR has gone by now and is not coming back, so the failure has
			// to say so. An operator reading only about the volume would go
			// looking for a service that is no longer there.
			return fmt.Errorf("service %q was deleted, but its volume was not: %w", name, err)
		}
		sayWhatBecameOfTheVolume(out, name, volumes)
		return nil
	case !errors.IsNotFound(crErr):
		return fmt.Errorf("deleting service %q: %w", name, crErr)
	}

	// The CR went between the read and the delete, so there may still be a
	// volume under the name.
	return deleteLeftovers(ctx, out, mgr, dyn, namespace, name, deleteData)
}

// deleteLeftovers clears what is under a name that has no service record: a
// service from before those records existed, or one whose record has already
// gone and left the volume behind.
//
// Nothing here touches a service record, because there is none to touch. What it
// does touch is checked for being Kipper's and for having no owner, so a service
// created under the name while this was running keeps everything of its own.
func deleteLeftovers(ctx context.Context, out io.Writer, mgr *service.Manager, dyn dynamic.Interface, namespace, name string, deleteData bool) error {
	// The Secret this name derives can be where an app keeps its git token.
	// Creating such a service is refused for that reason; deleting one that was
	// never there must not take the app's token instead.
	if app, holds, err := appHoldingTheCredential(ctx, dyn, namespace, name); err != nil {
		return err
	} else if holds {
		return fmt.Errorf("there is no service %q, and %s is where the app %q keeps its git token, so this would delete that instead",
			name, secretname.ServiceCredentials(name), app)
	}

	volumes, err := mgr.DataVolumes(ctx, namespace, name)
	if err != nil {
		return err
	}
	if err := mgr.Delete(ctx, namespace, name, deleteData); err != nil {
		return err
	}
	sayWhatBecameOfTheVolume(out, name, volumes)
	return nil
}

// sayWhatBecameOfTheVolume reports the destroy for what it was.
//
// Nothing found is its own outcome: the volume may have gone in an earlier
// delete, or it may be standing there under a name or label this does not
// recognise as the service's. Saying it went either way is a claim about data
// nobody can check afterwards.
func sayWhatBecameOfTheVolume(out io.Writer, name string, volumes service.DataVolumes) {
	if len(volumes) == 0 {
		say(out, "\n  ✔  Service %q deleted. No volume of its own was found to remove.\n\n", name)
		return
	}
	say(out, "\n  ✔  Service %q deleted, and its volume with it\n\n", name)
}

// ownedBy says whether this object was made for the object that owns it, by the
// controller reference rather than by name.
func ownedBy(object metav1.Object, service types.UID) bool {
	owner := metav1.GetControllerOf(object)
	return owner != nil && owner.UID == service
}

// sayHowItCameUp reports what the wait actually found.
//
// The wait gives up after a while, and a service the reconciler refused never
// produces the Secret it is waiting for. Ticking both lines regardless tells an
// operator the service came up on the one flow where it most often has not: a
// volume left behind by an earlier delete, and a new service of the same name
// landing on it with no password recorded for the data already there.
func sayHowItCameUp(out io.Writer, snapshot service.Snapshot, name string, ready, provisioned bool) {
	// The refusal comes first, because the wait finds a Secret by name and a
	// name is all it proves. A Secret already standing there is the very thing
	// the reconciler refuses over, so a service can be blocked and look ready in
	// the same breath.
	if snapshot.Blocked() {
		say(out, "  !   Service %q was created but has not come up\n", name)
		writeBlockedNotice(out, name, snapshot.BlockedReason, snapshot.BlockedMessage)
		return
	}
	if ready {
		say(out, "  ✔  Credentials generated\n")
		// Asked separately, because the reconciler writes the credentials before
		// it makes the workload. Finding the Secret says nothing about whether
		// the volume exists.
		if provisioned {
			say(out, "  ✔  Persistent storage provisioned\n")
			return
		}
		say(out, "  !   Its volume is not there yet. Run kip service list to see where it has got to.\n\n")
		return
	}
	say(out, "  !   Service %q was created but has not come up\n", name)
	say(out, "      Nothing has said why yet. Run kip service list to see where it has got to.\n\n")
}

// markForDataDeletion asks the cluster to remove the service's volumes as well.
//
// A project's own operators can delete their services and read their volumes,
// and delete no volume at all: kipper:project-owner and kipper:project-deployer
// carry get, list and watch on claims and nothing more. The mark is what gets
// their data removed, by the Service finalizer, and it is set before the CR goes
// because afterwards there is nothing left to set it on.
//
// It returns the version the service stands at once the mark is on it, so the
// delete that follows can be pinned to the decision and not only to the object.
func markForDataDeletion(ctx context.Context, dyn dynamic.Interface, namespace string, cr *unstructured.Unstructured) (string, error) {
	if cr.GetAnnotations()[datavolume.DeleteAnnotation] == "true" {
		return cr.GetResourceVersion(), nil
	}

	// The resourceVersion goes in the patch, which the API server reads as a
	// condition: the mark lands on the service that was read, before its volumes
	// were, or on nothing at all.
	mark := []byte(fmt.Sprintf(`{"metadata":{"resourceVersion":%q,"annotations":{%q:"true"}}}`,
		cr.GetResourceVersion(), datavolume.DeleteAnnotation))
	services := dyn.Resource(manifest.ServiceGVR).Namespace(namespace)
	marked, err := services.Patch(ctx, cr.GetName(), types.MergePatchType, mark, metav1.PatchOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return cr.GetResourceVersion(), nil
		}
		return "", fmt.Errorf("asking the cluster to remove the volume of %q: %w", cr.GetName(), err)
	}
	return marked.GetResourceVersion(), nil
}

// readService reads the service a delete was asked about, and refuses one whose
// own deletion has already started. Nothing there is not an error: the CR-less
// path takes over from here.
func readService(ctx context.Context, dyn dynamic.Interface, namespace, name string) (*unstructured.Unstructured, error) {
	cr, err := dyn.Resource(manifest.ServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading service %q: %w", name, err)
	}
	if err := notAlreadyGoing(cr, name); err != nil {
		return nil, err
	}
	return cr, nil
}

// clearTheDataMark takes off a mark an earlier delete left behind.
//
// A mark outlives the command that set it whenever the delete that should have
// followed did not happen. Left there, this delete would say the volume was kept
// while the finalizer destroyed it, which is the one thing an operator cannot
// check afterwards. Saying the volume stays means making sure it does.
// It returns the version the service stands at once the mark is off it, so the
// delete that follows can be pinned to the decision and not only to the object.
// Nothing to clear is left alone: a write nobody needs is one more chance for
// this and a concurrent delete to disagree.
func clearTheDataMark(ctx context.Context, dyn dynamic.Interface, namespace string, cr *unstructured.Unstructured) (string, error) {
	if _, marked := cr.GetAnnotations()[datavolume.DeleteAnnotation]; !marked {
		return cr.GetResourceVersion(), nil
	}

	clear := []byte(fmt.Sprintf(`{"metadata":{"resourceVersion":%q,"annotations":{%q:null}}}`,
		cr.GetResourceVersion(), datavolume.DeleteAnnotation))
	services := dyn.Resource(manifest.ServiceGVR).Namespace(namespace)
	cleared, err := services.Patch(ctx, cr.GetName(), types.MergePatchType, clear, metav1.PatchOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return cr.GetResourceVersion(), nil
		}
		return "", fmt.Errorf("making sure the volume of %q is kept: %w", cr.GetName(), err)
	}
	return cleared.GetResourceVersion(), nil
}

// notAlreadyGoing refuses a service whose deletion has already started.
//
// Neither half of the delete has anything to say about a service that is on its
// way out: the mark cannot be added, because the API server will not take a
// finalizer onto a deleting object and the mark without one decides nothing, and
// taking the mark off cannot call back a volume the finalizer has already
// deleted.
func notAlreadyGoing(cr *unstructured.Unstructured, name string) error {
	if cr.GetDeletionTimestamp() == nil {
		return nil
	}
	return fmt.Errorf("service %q is already being deleted, and what happens to its volume was settled by that delete; kip service list says what is holding it up", name)
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

// refuseServiceNameWhoseCredentialIsTaken stops a service being created whose
// credentials Secret is somebody else's object. The console refuses the same
// name for the same reason; the collision arithmetic lives in secretname so the
// two cannot drift.
func refuseServiceNameWhoseCredentialIsTaken(ctx context.Context, dyn dynamic.Interface, namespace, name string) error {
	app, holds, err := appHoldingTheCredential(ctx, dyn, namespace, name)
	if err != nil {
		return err
	}
	if holds {
		return fmt.Errorf("a service named %q would keep its credentials in %s, which is where the app %q keeps its git token. Pick another name for the service",
			name, secretname.ServiceCredentials(name), app)
	}
	return credentialNameFree(ctx, dyn, namespace, name)
}

// appHoldingTheCredential says whether an app keeps its git token in the object
// this service name would take. The app existing is not the collision: an app
// names its credential after a digest of the pair now, so only one still on the
// older name has anything at that object.
func appHoldingTheCredential(ctx context.Context, dyn dynamic.Interface, namespace, name string) (string, bool, error) {
	app, collides := secretname.AppSharingServiceCredentialName(name)
	if !collides {
		return "", false, nil
	}
	existing, err := dyn.Resource(deployer.AppGVR).Namespace(namespace).Get(ctx, app, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("checking whether app %s exists: %w", app, err)
	}
	// The error is dropped because it says nothing this decision needs: a field
	// that is absent and a field that is not a string both come back not-found,
	// and either way the app is not on the legacy name.
	credential, named, _ := unstructured.NestedString(existing.Object, "spec", "git", "credentialsSecret")
	if !named || credential != secretname.LegacyGitCredential(app) {
		return "", false, nil
	}
	return app, true, nil
}

// credentialNameFree refuses the name while the object is still there.
//
// Nothing on a create path mints this Secret ahead of the CR, so one already in
// the namespace belongs to something else: an app that rotated onto a digest
// name and left its old token for a sweep that runs on a delay, a restore, or a
// hand-written object. Refusing now says what the reconciler would say later,
// while there is still a choice of name.
func credentialNameFree(ctx context.Context, dyn dynamic.Interface, namespace, name string) error {
	secret := secretname.ServiceCredentials(name)
	existing, err := dyn.Resource(secretGVR).Namespace(namespace).Get(ctx, secret, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("checking whether %s is free: %w", secret, err)
	}
	// A live controller is what makes this final: that Secret is one this service
	// can never take, no repair claims it away, and the only way out is another
	// name. Nothing else here is permanent, so nothing else is refused.
	//
	// With no owner at all, `kip service credentials --repair` hands the Secret
	// to the service that should have it, which is how a password gets back to
	// the volume it was written under.
	//
	// An owner that has gone is a weaker case, and it is allowed rather than
	// recommended. The reconciler will report SecretNotOwned and ask for the
	// reference to be pointed at this Service, which does keep the password, but
	// no kip command does that yet: the audit calls a Secret with any controller
	// reference foreign and repair claims only an unowned one. Garbage collection
	// is entitled to delete the Secret by that dangling reference in the
	// meantime. Refusing would not save it, and would take away the one window
	// where an operator can act.
	//
	// A Secret this very service already owns is not a collision at all: the
	// service exists, and saying so is the other check's job.
	ref := controllerOf(existing)
	if ref == nil {
		return nil
	}
	if ours(ref) && ref.Kind == "Service" && ref.Name == name {
		return nil
	}
	live, err := ownerIsLive(ctx, dyn, namespace, ref)
	if err != nil || !live {
		return err
	}
	return fmt.Errorf("a service named %q would keep its credentials in %s, and that secret already belongs to something else in this namespace. Pick another name for the service",
		name, secret)
}

// controllerOf is the reference naming the object that controls this one, which
// is the claim the service reconciler reads before it will use a Secret.
func controllerOf(obj *unstructured.Unstructured) *metav1.OwnerReference {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			return ref.DeepCopy()
		}
	}
	return nil
}

// ownerIsLive says whether the object a controller reference names is still
// there under the same identity.
//
// A reference outlives its object: garbage collection is not instant, and a
// restore brings back a dependent whose owner came back with a new UID. Reading
// the reference alone would take both for a live claim on the name.
//
// Only the two kinds Kipper creates are checked. Anything else holding this
// Secret belongs to a controller whose objects are not ours to look up, and a
// claim that cannot be disproved is treated as real.
func ownerIsLive(ctx context.Context, dyn dynamic.Interface, namespace string, ref *metav1.OwnerReference) (bool, error) {
	if !ours(ref) {
		return true, nil
	}
	var gvr schema.GroupVersionResource
	switch ref.Kind {
	case "Service":
		gvr = manifest.ServiceGVR
	case "App":
		gvr = deployer.AppGVR
	default:
		return true, nil
	}

	owner, err := dyn.Resource(gvr).Namespace(namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("checking whether %s %s still holds its credentials: %w", ref.Kind, ref.Name, err)
	}
	return owner.GetUID() == ref.UID, nil
}

// secretGVR reaches Secrets through the dynamic client the collision check
// already holds, rather than threading a second client through three callers.
var secretGVR = schema.GroupVersionResource{Version: "v1", Resource: "secrets"}

// ours says whether a controller reference names one of Kipper's own kinds. The
// kind alone does not: Service is a core kind too, and looking a core Service up
// as a kipper.run one answers not-found, which would read as a claim that has
// lapsed when it is somebody's live object.
func ours(ref *metav1.OwnerReference) bool {
	return schema.FromAPIVersionAndKind(ref.APIVersion, ref.Kind).Group == manifest.ServiceGVR.Group
}
