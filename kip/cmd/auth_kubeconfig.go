package cmd

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

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
from this machine.

A login this cluster grants no access to is refused, because the file being
replaced is often the only way in. Use --output to write the OIDC kubeconfig
somewhere else and try it before giving anything up.`,
	// The refusals below are already a written explanation, so cobra's own
	// "Error:" line on top of the one Execute prints says the same sentence
	// three times to someone who is already stuck.
	SilenceUsage:  true,
	SilenceErrors: true,
	RunE:          runAuthKubeconfig,
}

func init() {
	authKubeconfigCmd.Flags().String("output", "", "write the OIDC kubeconfig to this path instead of replacing the cluster's")
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
	outputPath, _ := cmd.Flags().GetString("output")

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

	// Whatever the proof says, the rewrite renders a fresh single-context file
	// over the whole path. Anything else the file describes goes with it, and
	// a replaceable entry is still one the operator has to rebuild by hand — so
	// the count is what decides this, not whether each entry could in principle
	// be re-rendered.
	// Two different losses, both collateral. Another cluster in the file goes
	// when the rewrite keeps only the active one, and a credential parked in an
	// unused entry of *this* cluster goes with it — a break-glass certificate
	// beside the OIDC entry being the case that matters. Checking only the
	// first would let a proven admin destroy the second.
	collateral := append(otherClusters(endpoint.snapshot), irreplaceableEntries(endpoint.snapshot, exceptActive)...)
	if len(collateral) > 0 && outputPath == "" {
		sort.Strings(collateral)
		extra := collateral
		fmt.Fprintf(os.Stderr, "\n  ✗  %s also describes %s, and this rewrites the whole file.\n", cluster.Kubeconfig, describeHeld(extra))
		fmt.Fprintf(os.Stderr, "     Nothing was changed. Write the new file somewhere else instead:\n")
		fmt.Fprintf(os.Stderr, "       kip auth kubeconfig --output ~/kip-oidc.yaml\n")
		fmt.Fprintf(os.Stderr, "     then point KUBECONFIG at it, or merge it in yourself.\n\n")
		return fmt.Errorf("%s describes other clusters, kubeconfig unchanged", cluster.Kubeconfig)
	}

	switch result {
	case installer.ProofPass:
	case installer.ProofPassNonAdmin:
		// The login authenticated and the access review said no. That proves
		// who the operator is and nothing about what they may do, so it cannot
		// justify giving up access that already works — the replacement could
		// be an identity able to do nothing at all.
		//
		// It is still the ordinary answer for a project member, whose grant is
		// namespaced and so never satisfies a cluster-wide review. Their file
		// carries no credential, and converting it costs nothing, which is why
		// this asks what would be destroyed rather than who is asking.
		// Writing elsewhere destroys nothing, so the refusal does not apply:
		// the operator keeps what works and gets a file to try. This is the
		// answer for a project member whose grant is namespaced and so never
		// satisfies a cluster-wide review — their login may be perfectly good
		// and this check cannot tell.
		if held := irreplaceableEntries(endpoint.snapshot, everyEntry); len(held) > 0 && outputPath == "" {
			fmt.Fprintf(os.Stderr, "\n  ✗  Your login works, but this cluster grants it no access: %s\n", detail)
			fmt.Fprintf(os.Stderr, "     %s is unchanged, because replacing it would give up %s\n", cluster.Kubeconfig, describeHeld(held))
			fmt.Fprintf(os.Stderr, "     and leave this machine with an identity that cannot use the cluster.\n")
			if installer.IsKipExecAuthInfo(activeAuthInfo(endpoint.snapshot)) {
				fmt.Fprintf(os.Stderr, "     That plugin is kip's, with settings kip did not write, so re-rendering it\n")
				fmt.Fprintf(os.Stderr, "     would drop them.\n")
			}
			fmt.Fprintf(os.Stderr, "     If your access is namespaced, a cluster-wide check cannot see it, so this\n")
			fmt.Fprintf(os.Stderr, "     is the route: write the new file elsewhere, try it, and move it into place\n")
			fmt.Fprintf(os.Stderr, "     yourself once you are satisfied.\n")
			fmt.Fprintf(os.Stderr, "       kip auth kubeconfig --output ~/kip-oidc.yaml\n")
			fmt.Fprintf(os.Stderr, "       KUBECONFIG=~/kip-oidc.yaml kubectl get pods\n")
			fmt.Fprintf(os.Stderr, "     'kip auth verify' re-checks the login once your access is granted.\n\n")
			return fmt.Errorf("this cluster grants your login no access, kubeconfig unchanged")
		}
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

	write := func() (string, error) {
		if outputPath != "" {
			return installer.WriteExecKubeconfigTo(cluster.Domain, outputPath, endpoint.snapshot)
		}
		return installer.RewriteKubeconfigToExec(cluster.Domain, cluster.Kubeconfig, endpoint.snapshot)
	}
	path, err := write()
	if err != nil {
		return err
	}

	fmt.Printf("\n  ✔  %s now authenticates as your OIDC identity\n", path)
	fmt.Printf("     kubectl runs `kip auth kubectl-token` for short-lived tokens.\n")
	if outputPath != "" {
		fmt.Printf("     %s is untouched. Try the new file with:\n", cluster.Kubeconfig)
		fmt.Printf("       KUBECONFIG=%s kubectl get pods\n", path)
	}
	if result == installer.ProofPassNonAdmin {
		fmt.Printf("     %s\n", detail)
	}
	fmt.Println()
	return nil
}

// irreplaceableEntries names the auth infos whose access the rewrite would
// destroy, sorted so the message is the same on every run.
//
// Every entry is examined rather than the active one alone: the rewrite renders
// a fresh single-context file over the whole path, so a certificate parked in a
// context nobody is using is lost just as completely as the one in front.
//
// A file that cannot be parsed here was already parsed by readClusterEndpoint,
// so the error is folded into "something is held" rather than reported twice:
// refusing on an unreadable file is the safe answer either way.
func irreplaceableEntries(snapshot []byte, scope entryScope) []string {
	cfg, err := clientcmd.Load(snapshot)
	if err != nil {
		return []string{"the credential it holds"}
	}
	active, _ := installer.ActiveContext(cfg)
	var held []string
	for name, authInfo := range cfg.AuthInfos {
		if authInfo == nil || !irreplaceableAccess(authInfo) {
			continue
		}
		if scope == exceptActive && name == active.AuthInfo {
			continue
		}
		held = append(held, name)
	}
	sort.Strings(held)
	return held
}

// otherClusters names every cluster in the file except the active one.
//
// The rewrite keeps one cluster and discards the rest, so a shared kubeconfig
// loses every other entry it holds. Whether those entries carry an
// irreplaceable credential is beside the point: a context an operator has to
// rebuild by hand was still destroyed by a command they ran for a different
// cluster.
func otherClusters(snapshot []byte) []string {
	cfg, err := clientcmd.Load(snapshot)
	if err != nil {
		return nil
	}
	active, _ := installer.ActiveContext(cfg)
	var others []string
	for name := range cfg.Clusters {
		if name != active.Cluster {
			others = append(others, name)
		}
	}
	sort.Strings(others)
	return others
}

// entryScope selects which entries a check is about.
//
// Two questions are asked of the same file and they have different answers.
// Replacing the active context's credential is the trade the command exists to
// make, and it is gated on the login being proven. Replacing another context's
// credential is not a trade at all: nobody asked for it, and no proof makes it
// acceptable.
type entryScope int

const (
	everyEntry entryScope = iota
	exceptActive
)

// describeHeld renders the entry names for the refusal, naming them so the
// operator can find what is at stake in the file rather than guessing.
func describeHeld(held []string) string {
	if len(held) == 1 {
		return fmt.Sprintf("the credential for %q", held[0])
	}
	return fmt.Sprintf("the credentials for %s", strings.Join(quoteAll(held), ", "))
}

func quoteAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, name := range names {
		out = append(out, fmt.Sprintf("%q", name))
	}
	return out
}

// activeAuthInfo returns the entry the active context names, so a refusal can
// say which kind of thing is in the way rather than describing them all the
// same way.
func activeAuthInfo(snapshot []byte) *clientcmdapi.AuthInfo {
	cfg, err := clientcmd.Load(snapshot)
	if err != nil {
		return nil
	}
	active, _ := installer.ActiveContext(cfg)
	return cfg.AuthInfos[active.AuthInfo]
}
