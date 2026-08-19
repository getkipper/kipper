package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/controller/pkg/authncfg"
)

var userCmd = &cobra.Command{
	Use:   "user",
	Short: "Manage cluster users and roles",
}

var userAddCmd = &cobra.Command{
	Use:   "add [email]",
	Short: "Add a new user to the cluster",
	Long: `Creates a user in Dex and assigns a role.

Roles:
  admin:    full access
  deployer: deploy, scale, manage apps and services
  viewer:   read-only access

Examples:
  kip user add dev@example.com --role deployer
  kip user add pm@example.com --role viewer`,
	Args: cobra.ExactArgs(1),
	RunE: runUserAdd,
}

var userListCmd = &cobra.Command{
	Use:   "list",
	Short: "List all users and their roles",
	RunE:  runUserList,
}

var userRoleCmd = &cobra.Command{
	Use:   "role [email] [role]",
	Short: "Change a user's role",
	Args:  cobra.ExactArgs(2),
	RunE:  runUserRole,
}

var userRemoveCmd = &cobra.Command{
	Use:   "remove [email]",
	Short: "Remove a user from the cluster",
	Args:  cobra.ExactArgs(1),
	RunE:  runUserRemove,
}

func init() {
	userAddCmd.Flags().String("role", "deployer", "role: admin, deployer, or viewer")
	userAddCmd.Flags().String("password", "", "password (prompted if not provided)")

	userCmd.AddCommand(userAddCmd)
	userCmd.AddCommand(userListCmd)
	userCmd.AddCommand(userRoleCmd)
	userCmd.AddCommand(userRemoveCmd)
	rootCmd.AddCommand(userCmd)
}

func runUserAdd(cmd *cobra.Command, args []string) error {
	email := args[0]
	role, _ := cmd.Flags().GetString("role")
	password, _ := cmd.Flags().GetString("password")

	if role != "admin" && role != "deployer" && role != "viewer" {
		return fmt.Errorf("role must be admin, deployer, or viewer")
	}

	if password == "" {
		fmt.Print("  Password: ")
		reader := bufio.NewReader(os.Stdin)
		password, _ = reader.ReadString('\n')
		password = strings.TrimSpace(password)
		if password == "" {
			return fmt.Errorf("password is required")
		}
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	cs := k8sClient.Clientset()

	if err := addToDex(ctx, cs, email, password); err != nil {
		return fmt.Errorf("adding to Dex: %w", err)
	}

	if err := setRole(ctx, cs, email, role); err != nil {
		return fmt.Errorf("setting role: %w", err)
	}

	fmt.Printf("\n  ✔  User %s added with role %s\n\n", email, role)
	return nil
}

func runUserList(_ *cobra.Command, _ []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	roles := getRoles(ctx, k8sClient.Clientset())

	if len(roles) == 0 {
		fmt.Print("\n  No users configured. Run 'kip user add' to create one.\n\n")
		return nil
	}

	access, accessErr := clusterAccessByEmail(ctx, k8sClient.Clientset(), k8sClient.Dynamic())

	fmt.Println()
	fmt.Printf("  %-40s %-10s %s\n", "EMAIL", "CONSOLE", "CLUSTER ACCESS")
	emails := make([]string, 0, len(roles))
	for email := range roles {
		emails = append(emails, email)
	}
	sort.Strings(emails)
	for _, email := range emails {
		grant := "none"
		if accessErr != nil {
			grant = "unknown"
		} else if held, ok := access[email]; ok {
			grant = held
		}
		fmt.Printf("  %-40s %-10s %s\n", email, roles[email], grant)
	}
	fmt.Println()

	// The two columns are separate systems, and reading the first as the second
	// is what left five console admins unable to run a single kip command.
	fmt.Printf("  Console is the web UI. Cluster access is what kip and kubectl use, and\n")
	fmt.Printf("  comes from project membership ('kip project members add') or the\n")
	fmt.Printf("  bootstrap admin binding. A console role grants neither.\n")
	if accessErr != nil {
		fmt.Printf("\n  Cluster access could not be read (%v), so the last column is unknown.\n", accessErr)
	} else if noneHaveAccess(access, emails) {
		fmt.Printf("\n  ⚠  Nobody listed here can use kip or kubectl against this cluster.\n")
		fmt.Printf("     Add them to a project with 'kip project members add <project> <email> deployer'.\n")
	}
	fmt.Println()
	return nil
}

// noneHaveAccess reports whether not one listed user holds any grant, which is
// a cluster running entirely on its shared admin certificate.
func noneHaveAccess(access map[string]string, emails []string) bool {
	for _, email := range emails {
		if access[email] != "" {
			return false
		}
	}
	return true
}

// clusterAccessByEmail describes what each identity may actually do at the
// Kubernetes API, which is a different question from the console role beside
// it.
//
// Two sources, because there are two ways to hold access: the bootstrap
// binding grants cluster-admin outright, and project membership is projected
// into namespaced RoleBindings by the reconciler.
//
// The RoleBindings are read rather than the Project's member list, because
// membership is desired state and the projection is asynchronous. Reporting
// the desired list as "cluster access" would repeat the defect this whole
// change set is about: a plausible statement that is not evidence. An operator
// added a moment ago is reported as pending, which is true and actionable,
// rather than as working, which is a guess.
func clusterAccessByEmail(ctx context.Context, clientset kubernetes.Interface, dyn dynamic.Interface) (map[string]string, error) {
	access := map[string]string{}

	binding, err := clientset.RbacV1().ClusterRoleBindings().Get(ctx, initialAdminBindingName, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	if err == nil {
		for _, subject := range binding.Subjects {
			if email, ok := strings.CutPrefix(subject.Name, authncfg.UsernamePrefix); ok {
				access[email] = "cluster-admin"
			}
		}
	}

	// Effective grants: what the API server would actually consult.
	bindings, err := clientset.RbacV1().RoleBindings(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	granted := map[string]map[string]bool{}
	for _, rb := range bindings.Items {
		role := strings.TrimPrefix(rb.RoleRef.Name, "kipper:project-")
		if role == rb.RoleRef.Name {
			continue
		}
		for _, subject := range rb.Subjects {
			email, ok := strings.CutPrefix(subject.Name, authncfg.UsernamePrefix)
			if !ok || access[email] == "cluster-admin" {
				continue
			}
			if granted[email] == nil {
				granted[email] = map[string]bool{}
			}
			granted[email][fmt.Sprintf("%s in %s", role, rb.Namespace)] = true
		}
	}
	for email, where := range granted {
		access[email] = joinSorted(where)
	}

	// Membership the reconciler has not projected yet. Named as pending rather
	// than folded in, so "added but not yet effective" is distinguishable from
	// "working".
	projects, err := dyn.Resource(projectGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	for _, project := range projects.Items {
		members, _, _ := unstructured.NestedSlice(project.Object, "spec", "members")
		for _, raw := range members {
			member, ok := raw.(map[string]interface{})
			if !ok {
				continue
			}
			email, _ := member["email"].(string)
			if email == "" || access[email] != "" {
				continue
			}
			access[email] = fmt.Sprintf("pending on %s", project.GetName())
		}
	}
	return access, nil
}

// joinSorted renders a set of grants in a stable order, so two runs against an
// unchanged cluster print the same thing.
func joinSorted(set map[string]bool) string {
	out := make([]string, 0, len(set))
	for item := range set {
		out = append(out, item)
	}
	sort.Strings(out)
	return strings.Join(out, ", ")
}

func runUserRole(_ *cobra.Command, args []string) error {
	email, role := args[0], args[1]

	if role != "admin" && role != "deployer" && role != "viewer" {
		return fmt.Errorf("role must be admin, deployer, or viewer")
	}

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	if err := setRole(context.Background(), k8sClient.Clientset(), email, role); err != nil {
		return err
	}

	fmt.Printf("\n  ✔  %s is now %s\n\n", email, role)
	return nil
}

func runUserRemove(_ *cobra.Command, args []string) error {
	email := args[0]

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	cs := k8sClient.Clientset()

	_ = removeFromDex(ctx, cs, email)

	if err := removeRole(ctx, cs, email); err != nil {
		return fmt.Errorf("removing role: %w", err)
	}

	fmt.Printf("\n  ✔  User %s removed\n\n", email)
	return nil
}

func getRoles(ctx context.Context, cs kubernetes.Interface) map[string]string {
	cm, err := cs.CoreV1().ConfigMaps("kipper-system").Get(ctx, "kipper-users", metav1.GetOptions{})
	if err != nil {
		return map[string]string{}
	}
	var roles map[string]string
	if err := json.Unmarshal([]byte(cm.Data["users"]), &roles); err != nil {
		return map[string]string{}
	}
	return roles
}

func setRole(ctx context.Context, cs kubernetes.Interface, email, role string) error {
	cm, err := cs.CoreV1().ConfigMaps("kipper-system").Get(ctx, "kipper-users", metav1.GetOptions{})
	if err != nil {
		roles := map[string]string{email: role}
		data, _ := json.Marshal(roles)
		_, err = cs.CoreV1().ConfigMaps("kipper-system").Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "kipper-users", Namespace: "kipper-system",
				Labels: map[string]string{"app.kubernetes.io/managed-by": "kipper"},
			},
			Data: map[string]string{"users": string(data)},
		}, metav1.CreateOptions{})
		return err
	}

	var roles map[string]string
	if err := json.Unmarshal([]byte(cm.Data["users"]), &roles); err != nil {
		roles = map[string]string{}
	}
	roles[email] = role
	data, _ := json.Marshal(roles)
	cm.Data["users"] = string(data)
	_, err = cs.CoreV1().ConfigMaps("kipper-system").Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func removeRole(ctx context.Context, cs kubernetes.Interface, email string) error {
	cm, err := cs.CoreV1().ConfigMaps("kipper-system").Get(ctx, "kipper-users", metav1.GetOptions{})
	if err != nil {
		return nil //nolint:nilerr // no store means nothing to remove
	}
	var roles map[string]string
	if err := json.Unmarshal([]byte(cm.Data["users"]), &roles); err != nil {
		return nil //nolint:nilerr // corrupt store, nothing to remove
	}
	delete(roles, email)
	data, _ := json.Marshal(roles)
	cm.Data["users"] = string(data)
	_, err = cs.CoreV1().ConfigMaps("kipper-system").Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func addToDex(ctx context.Context, cs kubernetes.Interface, email, password string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	userID := make([]byte, 16)
	_, _ = rand.Read(userID)

	cm, err := cs.CoreV1().ConfigMaps("dex").Get(ctx, "dex-config", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading Dex config: %w", err)
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &config); err != nil {
		return err
	}

	passwords, _ := config["staticPasswords"].([]interface{})
	for _, p := range passwords {
		if entry, ok := p.(map[string]interface{}); ok {
			if entry["email"] == email {
				return fmt.Errorf("user %s already exists in Dex", email)
			}
		}
	}

	passwords = append(passwords, map[string]interface{}{
		"email":    email,
		"hash":     string(hash),
		"username": strings.Split(email, "@")[0],
		"userID":   hex.EncodeToString(userID),
	})
	config["staticPasswords"] = passwords

	newYAML, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	cm.Data["config.yaml"] = string(newYAML)
	_, err = cs.CoreV1().ConfigMaps("dex").Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func removeFromDex(ctx context.Context, cs kubernetes.Interface, email string) error {
	cm, err := cs.CoreV1().ConfigMaps("dex").Get(ctx, "dex-config", metav1.GetOptions{})
	if err != nil {
		return err
	}

	var config map[string]interface{}
	if err := yaml.Unmarshal([]byte(cm.Data["config.yaml"]), &config); err != nil {
		return err
	}

	passwords, _ := config["staticPasswords"].([]interface{})
	var filtered []interface{}
	for _, p := range passwords {
		if entry, ok := p.(map[string]interface{}); ok {
			if entry["email"] == email {
				continue
			}
		}
		filtered = append(filtered, p)
	}
	config["staticPasswords"] = filtered

	newYAML, err := yaml.Marshal(config)
	if err != nil {
		return err
	}
	cm.Data["config.yaml"] = string(newYAML)
	_, err = cs.CoreV1().ConfigMaps("dex").Update(ctx, cm, metav1.UpdateOptions{})
	return err
}
