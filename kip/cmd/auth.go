package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/kip/internal/auth"
)

var authCmd = &cobra.Command{
	Use:   "auth",
	Short: "Manage cluster authentication",
}

var authLoginCmd = &cobra.Command{
	Use:   "login",
	Short: "Authenticate with the cluster via browser-based login",
	Long: `Opens a browser window to the cluster's identity provider (Dex).
After logging in, the CLI stores a session token for subsequent commands.

The token is automatically refreshed when it expires. If the refresh
token has expired, run kip auth login again.`,
	RunE: runAuthLogin,
}

var authStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show current authentication status",
	RunE:  runAuthStatus,
}

var authLogoutCmd = &cobra.Command{
	Use:   "logout",
	Short: "Clear stored authentication credentials",
	RunE:  runAuthLogout,
}

var authResetPasswordCmd = &cobra.Command{
	Use:   "reset-password",
	Short: "Reset the admin password",
	Long: `Generates a new admin password, updates the Dex configuration,
and restarts Dex. The new credentials are printed to the terminal.`,
	RunE: runAuthResetPassword,
}

func init() {
	authCmd.AddCommand(authLoginCmd)
	authCmd.AddCommand(authStatusCmd)
	authCmd.AddCommand(authLogoutCmd)
	authCmd.AddCommand(authResetPasswordCmd)
	authCmd.AddCommand(authKubectlTokenCmd)
	authCmd.AddCommand(authKubeconfigCmd)
	authCmd.AddCommand(authVerifyCmd)
	rootCmd.AddCommand(authCmd)
}

func runAuthLogin(cmd *cobra.Command, args []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}

	fmt.Printf("\n  Logging in to %s...\n\n", cluster.ConsoleHost())

	creds, err := auth.Login(cluster.DexHost())
	if err != nil {
		return err
	}

	if err := auth.Mutate(func(s *auth.Store) {
		s.Clusters[cluster.Domain] = creds
	}); err != nil {
		return fmt.Errorf("saving credentials: %w", err)
	}

	fmt.Printf("  ✔  Authenticated as %s\n\n", creds.Email)
	return nil
}

func runAuthStatus(cmd *cobra.Command, args []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}

	store, err := auth.Load()
	if err != nil {
		return fmt.Errorf("loading auth store: %w", err)
	}

	creds, ok := store.Clusters[cluster.Domain]
	if !ok {
		fmt.Printf("\n  Not authenticated. Run: kip auth login\n\n")
		return nil
	}

	_, tokenErr := store.Token(cluster.Domain, cluster.DexHost())

	fmt.Printf("\n  Cluster: %s\n", cluster.Domain)
	fmt.Printf("  Email:   %s\n", creds.Email)
	if tokenErr != nil {
		fmt.Printf("  Status:  expired — run: kip auth login\n")
	} else {
		fmt.Printf("  Status:  authenticated\n")
	}

	// Show both versions so a mismatch is visible when a request is unexpectedly
	// rejected. No automatic warning: kip and console-api can stamp their
	// versions from different schemes (a release tag vs a commit sha), so two
	// different strings don't reliably mean two different releases.
	fmt.Printf("  kip:     %s\n", cliVersion())
	if serverVer := consoleAPIVersion(context.Background(), cluster); serverVer != "" {
		fmt.Printf("  Cluster: console-api %s\n", serverVer)
	}
	fmt.Println()
	return nil
}

func runAuthLogout(cmd *cobra.Command, args []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}

	if err := auth.Mutate(func(s *auth.Store) {
		delete(s.Clusters, cluster.Domain)
	}); err != nil {
		return fmt.Errorf("saving auth store: %w", err)
	}

	fmt.Printf("\n  ✔  Logged out of %s\n\n", cluster.Domain)
	return nil
}

func runAuthResetPassword(cmd *cobra.Command, args []string) error {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	clientset := k8sClient.Clientset()

	// Generate new password
	pwBytes := make([]byte, 16)
	if _, err := rand.Read(pwBytes); err != nil {
		return fmt.Errorf("generating password: %w", err)
	}
	newPassword := hex.EncodeToString(pwBytes)

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	// Update the Dex ConfigMap
	cm, err := clientset.CoreV1().ConfigMaps("dex").Get(ctx, "dex-config", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading dex config: %w", err)
	}

	config := cm.Data["config.yaml"]
	if config == "" {
		return fmt.Errorf("dex config.yaml not found in ConfigMap")
	}

	// Replace the hash line in the config, preserving original indentation
	lines := strings.Split(config, "\n")
	updated := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "hash:") {
			indent := line[:len(line)-len(strings.TrimLeft(line, " "))]
			lines[i] = fmt.Sprintf(`%shash: %q`, indent, string(hash))
			updated = true
			break
		}
	}

	if !updated {
		return fmt.Errorf("could not find password hash in dex config")
	}

	cm.Data["config.yaml"] = strings.Join(lines, "\n")
	if _, err := clientset.CoreV1().ConfigMaps("dex").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("updating dex config: %w", err)
	}

	// Restart Dex to pick up the new config
	deploy, err := clientset.AppsV1().Deployments("dex").Get(ctx, "dex", metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("getting dex deployment: %w", err)
	}

	if deploy.Spec.Template.Annotations == nil {
		deploy.Spec.Template.Annotations = make(map[string]string)
	}
	deploy.Spec.Template.Annotations["kipper.run/restartedAt"] = fmt.Sprintf("%d", metav1.Now().Unix())
	if _, err := clientset.AppsV1().Deployments("dex").Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("restarting dex: %w", err)
	}

	// Find the admin email from the config
	email := "admin@" + cluster.Domain
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "email:") {
			email = strings.Trim(strings.TrimPrefix(trimmed, "email:"), ` "`)
			break
		}
	}

	fmt.Printf("\n  Admin password reset.\n")
	fmt.Printf("  Email:    %s\n", email)
	fmt.Printf("  Password: %s\n\n", newPassword)

	return nil
}
