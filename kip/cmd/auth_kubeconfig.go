package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

var authKubeconfigCmd = &cobra.Command{
	Use:   "kubeconfig",
	Short: "Rewrite this cluster's kubeconfig to use your OIDC identity",
	Long: `Replaces the cluster's stored kubeconfig with one that carries no
credential at all: kubectl obtains short-lived tokens through
"kip auth kubectl-token" from your logged-in session, so every action is
attributed to you personally and access ends with your account.

Your login is proven against this cluster's API server first, the same check
"kip auth verify" runs. A cluster that does not accept it keeps the credential
it has, because the file being replaced is often the only one that reaches the
cluster from this machine.

The shared admin certificate the previous kubeconfig carried stays on the
server as the documented break-glass credential; this command removes it
from this machine.`,
	// The refusals below are already a written explanation, so cobra's own
	// "Error:" line on top of the one Execute prints says the same sentence
	// three times to someone who is already stuck.
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runAuthKubeconfig,
}

// loginProof reports whether the stored session authenticates against this
// cluster's API server as the operator who holds it, and is granted access.
type loginProof func(ctx context.Context, cluster *config.Cluster, server string, caData []byte) (installer.ProofResult, string, error)

// proveOperatorLogin is the gate the conversion runs before giving up the
// cluster's credential. It is a variable so tests can drive every answer the
// API server can give, including the ones a live cluster rarely produces.
var proveOperatorLogin loginProof = liveOperatorLogin

func liveOperatorLogin(ctx context.Context, cluster *config.Cluster, server string, caData []byte) (installer.ProofResult, string, error) {
	return installer.VerifyOperatorIdentity(ctx, cluster.Domain, cluster.DexHost(), cluster.Domain, server, caData)
}

// clusterEndpoint is what a kubeconfig says about the cluster it reaches,
// alongside the exact bytes that said it. The snapshot travels with the answer
// so a proof and the rewrite that follows it act on one read of the file rather
// than two.
type clusterEndpoint struct {
	server   string
	caData   []byte
	snapshot []byte
}

// readClusterEndpoint returns the server address and cluster certificate
// authority the kubeconfig's active context names.
//
// It resolves that context through installer.ActiveContext, which is what the
// rewrite itself uses. Both have to pick the same entry: a proof made against
// one server while the rewrite keeps another attests to a cluster nobody asked
// about.
func readClusterEndpoint(path string) (clusterEndpoint, error) {
	snapshot, err := os.ReadFile(path)
	if err != nil {
		return clusterEndpoint{}, fmt.Errorf("reading kubeconfig %s: %w", path, err)
	}
	cfg, err := clientcmd.Load(snapshot)
	if err != nil {
		return clusterEndpoint{}, fmt.Errorf("reading kubeconfig %s: %w", path, err)
	}
	kubeContext, _ := installer.ActiveContext(cfg)
	if kubeContext.Cluster == "" {
		return clusterEndpoint{}, fmt.Errorf("kubeconfig %s has no usable current context; set one before rewriting", path)
	}
	cluster := cfg.Clusters[kubeContext.Cluster]
	if cluster == nil || cluster.Server == "" || len(cluster.CertificateAuthorityData) == 0 {
		return clusterEndpoint{}, fmt.Errorf("kubeconfig %s carries no server and CA to verify against", path)
	}
	return clusterEndpoint{server: cluster.Server, caData: cluster.CertificateAuthorityData, snapshot: snapshot}, nil
}

func runAuthKubeconfig(cmd *cobra.Command, args []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}

	// Read the endpoint from the file about to be replaced: the proof uses the
	// operator's token against that same server, not this file's credential.
	endpoint, err := readClusterEndpoint(cluster.Kubeconfig)
	if err != nil {
		return err
	}

	fmt.Printf("\n  ...  Checking that your login reaches this cluster (up to a minute)\n")
	result, detail, err := proveOperatorLogin(context.Background(), cluster, endpoint.server, endpoint.caData)
	if err != nil {
		// Reached before the API server is asked anything, a missing session
		// being the usual cause. The error carries the fix; this says what was
		// left alone, which the other two refusals also say.
		fmt.Fprintf(os.Stderr, "\n  ✗  Your login could not be checked, so %s is unchanged.\n\n", cluster.Kubeconfig)
		return err
	}

	switch result {
	case installer.ProofPass, installer.ProofPassNonAdmin:
	case installer.ProofTransportError:
		fmt.Fprintf(os.Stderr, "\n  ⚠  Could not reach the API server to check your login: %s\n", detail)
		fmt.Fprintf(os.Stderr, "     %s is unchanged and still works.\n\n", cluster.Kubeconfig)
		return fmt.Errorf("login could not be checked, kubeconfig unchanged")
	default:
		fmt.Fprintf(os.Stderr, "\n  ✗  This cluster did not accept your login: %s\n", detail)
		fmt.Fprintf(os.Stderr, "     %s is unchanged, so it still reaches the cluster.\n", cluster.Kubeconfig)
		fmt.Fprintf(os.Stderr, "     Run 'kip cluster ca status' to see what the API server has loaded,\n")
		fmt.Fprintf(os.Stderr, "     and 'kip auth verify' to re-check the login once it is fixed.\n\n")
		return fmt.Errorf("this cluster does not accept your login, kubeconfig unchanged")
	}

	path, err := installer.RewriteKubeconfigToExec(cluster.Domain, cluster.Kubeconfig, endpoint.snapshot)
	if err != nil {
		return err
	}

	fmt.Printf("\n  ✔  %s now authenticates as your OIDC identity\n", path)
	fmt.Printf("     kubectl runs `kip auth kubectl-token` for short-lived tokens.\n")
	if result == installer.ProofPassNonAdmin {
		fmt.Printf("     %s\n", detail)
	}
	fmt.Println()
	return nil
}
