package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/crypto/bcrypt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/dexcfg"
	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/kip/internal/auth"
)

// Where Dex keeps the login config Kipper edits.
const (
	dexNamespace      = "dex"
	dexConfigMapName  = "dex-config"
	dexConfigKey      = "config.yaml"
	dexDeploymentName = "dex"
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
prints the new credentials, and restarts Dex so they take effect.

The credentials are printed as soon as Dex's configuration holds them, so a
restart that fails still leaves a password you can read and use.`,
	SilenceUsage: true,
	RunE:         runAuthResetPassword,
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
		fmt.Printf("  Status:  expired. Run: kip auth login\n")
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
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	return resetAdminPassword(context.Background(), k8sClient.Clientset(), cmd.OutOrStdout())
}

// resetAdminPassword generates a new admin password, writes its bcrypt hash to
// the Dex config, discloses the credentials, and restarts Dex.
//
// The order is the contract. The ConfigMap write is the durable half of the
// change, exactly as it is for the ClusterIdentity reconciler's writeDexConfig,
// so the credentials are printed the moment it succeeds. The restart can fail
// and still leave a truthful cluster: the password is set, the operator has read
// it, and it takes effect the next time Dex restarts. Printing last is what
// locked an operator out of a cluster whose restart lost a race. Disclosure is
// the one step after the write that is not allowed to fail quietly, because a
// live hash nobody has read is the same lockout by another route.
//
// The restart is a patch rather than a get-modify-update because the reconciler
// rolls Dex too, via its own config-hash annotation. A patch carries no
// resourceVersion, so that concurrent write cannot make this one fail. This
// command stays independent of the reconciler because it is the break-glass
// path and has to work on a cluster whose control plane is degraded; the
// reconciler stamping its hash as well costs at most one extra rollout.
func resetAdminPassword(ctx context.Context, clientset kubernetes.Interface, out io.Writer) error {
	pwBytes := make([]byte, 16)
	if _, err := rand.Read(pwBytes); err != nil {
		return fmt.Errorf("generating password: %w", err)
	}
	newPassword := hex.EncodeToString(pwBytes)

	hash, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
	if err != nil {
		return fmt.Errorf("hashing password: %w", err)
	}

	configMaps := clientset.CoreV1().ConfigMaps(dexNamespace)
	var email string
	// The reconciler server-side applies this same ConfigMap, so an unretried
	// get-modify-update loses the race whenever the two land together.
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := configMaps.Get(ctx, dexConfigMapName, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading dex config: %w", err)
		}
		raw := cm.Data[dexConfigKey]
		if raw == "" {
			return fmt.Errorf("dex config.yaml not found in ConfigMap")
		}
		cfg, err := dexcfg.Load(raw)
		if err != nil {
			return err
		}
		// Read before the write, because a password nobody can name is no use:
		// Dex matches a static password on its email, so an admin entry without
		// one cannot be logged into whatever hash it carries.
		configured, ok, err := cfg.AdminEmail()
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("the admin entry in the %s ConfigMap has no email address, so it has no login to reset", dexConfigMapName)
		}
		if err := cfg.SetAdminHash(string(hash)); err != nil {
			return err
		}
		updated, err := cfg.Marshal()
		if err != nil {
			return err
		}
		cm.Data[dexConfigKey] = updated
		if _, err := configMaps.Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return err
		}
		email = configured
		return nil
	})
	if err != nil {
		return fmt.Errorf("updating dex config: %w", err)
	}

	// One write, and its failure is the operation failing. The hash is live from
	// here on, so a disclosure that did not reach the operator locks them out
	// just as thoroughly as the lost restart this command was fixed for. The
	// password rides the error to give stderr a chance at what stdout dropped.
	if _, err := fmt.Fprintf(out, "\n  Admin password reset.\n  Email:    %s\n  Password: %s\n\n", email, newPassword); err != nil {
		return fmt.Errorf("the new password could not be displayed. it is set and it is %q: %w", newPassword, err)
	}

	patch, err := json.Marshal(map[string]any{
		"spec": map[string]any{"template": map[string]any{"metadata": map[string]any{
			"annotations": map[string]string{labels.AnnoRestartedAt: time.Now().Format(time.RFC3339Nano)},
		}}},
	})
	if err != nil {
		return fmt.Errorf("building the dex restart patch: %w", err)
	}
	if _, err := clientset.AppsV1().Deployments(dexNamespace).Patch(
		ctx, dexDeploymentName, types.StrategicMergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("restarting dex: the password above is set and applies at the next dex restart: %w", err)
	}
	return nil
}
