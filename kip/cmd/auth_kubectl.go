package cmd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/auth"
	"github.com/getkipper/kipper/kip/internal/config"
)

var authKubectlTokenCmd = &cobra.Command{
	Use:   "kubectl-token",
	Short: "Emit an ExecCredential for kubectl (called by the exec kubeconfig, not by hand)",
	Long: `Prints the session token for the cluster named by --cluster-domain in the
Kubernetes ExecCredential format. The kubeconfig kip writes references this
command as its credential plugin, the direct analogue of aws eks get-token:
kubectl runs it whenever it needs a token, the token is minutes-lived, and
refresh happens automatically from the stored session.

The cluster comes from the kubeconfig's own exec args rather than from
'kip cluster use', because kubectl tells a credential plugin nothing about
which kubeconfig invoked it. Each kubeconfig therefore serves the session for
its own cluster, whichever cluster the rest of kip is pointed at.

When the session has expired entirely, run: kip auth login`,
	// stdout is the wire protocol; anything human-facing must go to stderr.
	// kubectl surfaces that stderr verbatim to whoever ran kubectl, so a
	// failure here has to read as one line carrying the fix: cobra's own
	// "Error:" copy on top of Execute's would print the same sentence three
	// times to someone who is already stuck.
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runAuthKubectlToken,
}

// clusterDomainFlag names the cluster whose session this invocation serves.
// kip writes it into every exec kubeconfig it renders; an operator has no
// reason to pass it by hand.
var clusterDomainFlag string

func init() {
	authKubectlTokenCmd.Flags().StringVar(&clusterDomainFlag, "cluster-domain", "",
		"domain of the cluster this kubeconfig authenticates to (written by kip)")
}

// execCredential is the client.authentication.k8s.io/v1 object kubectl
// consumes; hand-rolled because the three fields are the entire contract.
type execCredential struct {
	APIVersion string               `json:"apiVersion"`
	Kind       string               `json:"kind"`
	Status     execCredentialStatus `json:"status"`
}

type execCredentialStatus struct {
	Token               string `json:"token"`
	ExpirationTimestamp string `json:"expirationTimestamp"`
}

func runAuthKubectlToken(cmd *cobra.Command, args []string) error {
	// Config-only load: this command IS the credential plugin, so it must
	// never construct a client from the kubeconfig that invokes it.
	cluster, err := clusterForKubectlToken(clusterDomainFlag)
	if err != nil {
		return err
	}

	store, err := auth.Load()
	if err != nil {
		return fmt.Errorf("loading auth store: %w", err)
	}

	token, err := store.Token(cluster.Domain, cluster.DexHost())
	if err != nil {
		return err
	}
	creds := store.Credential(cluster.Domain)
	if creds == nil {
		return fmt.Errorf("not authenticated. Run: kip auth login")
	}

	out, err := json.Marshal(execCredential{
		APIVersion: "client.authentication.k8s.io/v1",
		Kind:       "ExecCredential",
		Status: execCredentialStatus{
			Token:               token,
			ExpirationTimestamp: creds.ExpiresAt.UTC().Format(time.RFC3339),
		},
	})
	if err != nil {
		return err
	}
	fmt.Println(string(out))
	return nil
}

// clusterForKubectlToken resolves the cluster whose session this invocation
// serves, from the domain the calling kubeconfig pinned into its exec args.
//
// It deliberately does not fall back to current_cluster when the pin is
// absent. That fallback is the defect this replaces: it serves whichever
// cluster kip happens to be pointed at, so kubectl against one cluster's
// kubeconfig sends another cluster's token, and the API server rejects it as
// an invalid bearer token no matter how many times the operator logs in.
// Refusing names the file that needs regenerating instead.
func clusterForKubectlToken(domain string) (*config.Cluster, error) {
	if domain == "" {
		return nil, fmt.Errorf("this kubeconfig does not say which cluster it authenticates to. Regenerate it with: kip auth kubeconfig")
	}

	cfg, err := config.Load()
	if err != nil {
		return nil, fmt.Errorf("loading config: %w", err)
	}

	cluster := cfg.GetClusterByDomain(domain)
	if cluster == nil {
		return nil, fmt.Errorf("no cluster for domain %q in ~/.kip/config.yaml. Add it with: kip cluster add <file>", domain)
	}
	return cluster, nil
}
