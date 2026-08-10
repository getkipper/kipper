package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/yaml"
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
  admin    — full access
  deployer — deploy, scale, manage apps and services
  viewer   — read-only access

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

	fmt.Println()
	for email, role := range roles {
		fmt.Printf("  %-40s %s\n", email, role)
	}
	fmt.Println()
	return nil
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
