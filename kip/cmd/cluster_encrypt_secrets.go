package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/installer"
)

var clusterEncryptSecretsCmd = &cobra.Command{
	Use:          "encrypt-secrets",
	SilenceUsage: true,
	Args:         cobra.NoArgs,
	Short:        "Encrypt the cluster's Secrets at rest",
	Long: `Turn on k3s secrets encryption on the server and rewrite every stored
Secret with the new key. Clusters installed by this version of kip already
encrypt their Secrets, and the command brings an older cluster to the same
state.

kip follows the k3s procedure for an existing cluster: it creates the key,
adds secrets-encryption: true to /etc/rancher/k3s/config.yaml, restarts k3s,
rotates to a fresh key while k3s re-encrypts every Secret, and restarts k3s
again. The API server is unavailable for a short time at each restart, while
running pods keep serving. A run that stops partway can be repeated. On a
cluster that encrypted from the start, the first run still rotates the key
once, and later runs change nothing.

The key sits on the server's disk in /var/lib/rancher/k3s/server/cred/.
Older copies of the Secrets stay in the datastore file until k3s compacts its
history and reuses the space, and backups taken before this run keep them
unencrypted.

Examples:
  kip cluster encrypt-secrets
  kip cluster encrypt-secrets --ssh-key ~/.ssh/kipper_ed25519`,
	RunE: runClusterEncryptSecrets,
}

func init() {
	clusterEncryptSecretsCmd.Flags().String("ssh-key", "", "path to SSH private key (defaults to ~/.ssh/id_ed25519)")
	clusterCmd.AddCommand(clusterEncryptSecretsCmd)
}

var encryptSecrets = installer.EncryptSecrets

func runClusterEncryptSecrets(cmd *cobra.Command, _ []string) error {
	lc, err := leaseClusterServer(cmd, "kip cluster encrypt-secrets")
	if err != nil {
		return err
	}
	defer lc.close()
	return encryptSecretsOn(lc.server)
}

func encryptSecretsOn(server installer.CommandRunner) error {
	fmt.Printf("\n  Encrypting Secrets at rest\n\n")
	already, err := encryptSecrets(server, func(m string) {
		fmt.Printf("  ...  %s\n", m)
	})
	if err != nil {
		return err
	}
	if already {
		fmt.Printf("  ✔  k3s already encrypts every Secret it stores\n\n")
		return nil
	}
	fmt.Printf("  ✔  k3s encrypts the Secrets it stores, and every existing Secret was rewritten with the new key\n")
	fmt.Printf("     Older copies stay in the datastore file until k3s compacts its history and reuses the\n")
	fmt.Printf("     space, and earlier backups keep them unencrypted. Change any credential that must not\n")
	fmt.Printf("     survive such a copy.\n\n")
	return nil
}
