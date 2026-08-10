package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getkipper/kipper/kip/internal/installer"
)

var authVerifyCmd = &cobra.Command{
	Use:   "verify",
	Short: "Prove your OIDC identity authenticates and authorizes against the cluster",
	Long: `Confirms that the Kubernetes API server accepts your login token as your
own identity and grants it access. This is the check the installer runs
inline; run it after a headless install (kip auth login && kip auth verify),
or any time you want to confirm the OIDC path works end to end.

Exits non-zero when the token is rejected by the API server or an admin
identity is denied cluster access. Never changes your kubeconfig.`,
	SilenceUsage: true,
	RunE:         runAuthVerify,
}

func runAuthVerify(cmd *cobra.Command, args []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}

	// Read the server address and cluster CA from the on-disk kubeconfig; the
	// proof uses the operator's token, not this file's credential.
	cfg, err := clientcmd.LoadFromFile(cluster.Kubeconfig)
	if err != nil {
		return fmt.Errorf("reading kubeconfig %s: %w", cluster.Kubeconfig, err)
	}
	var server string
	var caData []byte
	if ctxEntry, ok := cfg.Contexts[cfg.CurrentContext]; ok {
		if c, ok := cfg.Clusters[ctxEntry.Cluster]; ok {
			server, caData = c.Server, c.CertificateAuthorityData
		}
	}
	if server == "" || len(caData) == 0 {
		return fmt.Errorf("kubeconfig %s carries no server and CA to verify against", cluster.Kubeconfig)
	}

	result, detail, err := installer.VerifyOperatorIdentity(
		context.Background(), cluster.Domain, cluster.DexHost(), cluster.Domain, server, caData)
	if err != nil {
		return err
	}

	switch result {
	case installer.ProofPass:
		fmt.Printf("\n  ✔  Authenticated and authorized as %s\n\n", detail)
		return nil
	case installer.ProofPassNonAdmin:
		fmt.Printf("\n  ✔  %s\n\n", detail)
		return nil
	case installer.ProofTransportError:
		fmt.Fprintf(os.Stderr, "\n  ⚠  Could not reach the API server to verify: %s\n\n", detail)
		return fmt.Errorf("verification could not complete")
	case installer.ProofAuthzDeniedAsAdmin:
		fmt.Fprintf(os.Stderr, "\n  ✗  Authenticated, but denied cluster access: %s\n\n", detail)
		return fmt.Errorf("authorization failed")
	default:
		fmt.Fprintf(os.Stderr, "\n  ✗  The API server rejected your identity: %s\n\n", detail)
		return fmt.Errorf("authentication failed")
	}
}
