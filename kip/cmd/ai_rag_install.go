package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/ai"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/installer"
)

var aiRagInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Install Qdrant + AnythingLLM on top of the chat bundle",
	Long: `Install the Phase 2 RAG bundle: Qdrant for the vector store and
AnythingLLM for the chat-with-your-docs UI. Both colocate with Phase 1's
Ollama on the same node so embedding traffic stays in-cluster.

Refuses to install when Phase 1 (Ollama + LibreChat) is not yet up.
Run 'kip ai install' first.

The bundle adds roughly 4 GiB of memory pressure on top of Phase 1
(Qdrant ~512 MiB, AnythingLLM ~2 GiB, embedding model ~1 GiB plus
slack). The preflight refuses install when the target node does not
have that headroom available.

After install completes, this prints a one-time bootstrap AUTH_TOKEN.
Use it to log in on the Ask UI for the first time and create the
admin account through the setup wizard. The token gates the public
Ingress so an internet scanner cannot claim the admin slot before you
do.

Examples:
  kip ai rag install
  kip ai rag install --host ask.example.com
  kip ai rag install --embedding-model nomic-embed-text:latest`,
	RunE: runAIRagInstall,
}

func init() {
	aiRagInstallCmd.Flags().String("host", "", "external Ask UI hostname (defaults to ask.<cluster-domain>)")
	aiRagInstallCmd.Flags().String("embedding-model", "", "Ollama embedding model tag (defaults to nomic-embed-text:latest)")
	aiRagInstallCmd.Flags().String("qdrant-pvc-size", "", "Qdrant data volume size (defaults to 10Gi)")
	aiRagInstallCmd.Flags().String("anythingllm-pvc-size", "", "AnythingLLM storage volume size (defaults to 10Gi)")
	aiRagCmd.AddCommand(aiRagInstallCmd)
}

func runAIRagInstall(cmd *cobra.Command, _ []string) error {
	cluster, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	host, _ := cmd.Flags().GetString("host")
	if host == "" {
		host, err = defaultAskHost(cluster)
		if err != nil {
			return err
		}
	}
	embeddingModel, _ := cmd.Flags().GetString("embedding-model")
	qdrantPVC, _ := cmd.Flags().GetString("qdrant-pvc-size")
	anyllmPVC, _ := cmd.Flags().GetString("anythingllm-pvc-size")

	ctx := context.Background()

	fmt.Printf("\n  Installing AI RAG bundle on %s\n\n", cluster.Name)

	installer := &ai.Installer{
		Clientset: client.Clientset(),
		Dynamic:   client.Dynamic(),
		Out:       os.Stdout,
	}
	if err := installer.InstallRAG(ctx, ai.RAGOptions{
		Host:               host,
		EmbeddingModel:     embeddingModel,
		QdrantPVCSize:      qdrantPVC,
		AnythingLLMPVCSize: anyllmPVC,
	}); err != nil {
		return err
	}

	creds, err := installer.AnythingLLMCredentials(ctx)
	if err != nil {
		return fmt.Errorf("reading AnythingLLM credentials after install: %w", err)
	}

	askURL := "https://" + host
	fmt.Printf("\n  ✔  AI RAG bundle installed\n")
	fmt.Printf("  Ask UI: %s\n", askURL)
	fmt.Println()
	printRAGBootstrapInstructions(os.Stdout, creds.AuthToken)

	return nil
}

// printRAGBootstrapInstructions tells the operator how to claim the
// admin account on the Ask UI. The AUTH_TOKEN gates every request to
// AnythingLLM so the first-visit-admin slot cannot be claimed by an
// internet scanner that finds the predictable ask.<domain> URL.
func printRAGBootstrapInstructions(out io.Writer, authToken string) {
	_, _ = fmt.Fprintln(out, "  Bootstrap authentication token (required for first login):")
	_, _ = fmt.Fprintf(out, "    %s\n\n", authToken)
	_, _ = fmt.Fprintln(out, "  Visit the Ask UI now and provide this token to log in for the")
	_, _ = fmt.Fprintln(out, "  first time. Then create the admin account through the setup")
	_, _ = fmt.Fprintln(out, "  wizard. The token stops a passing internet scanner from claiming")
	_, _ = fmt.Fprintln(out, "  the admin slot before you do.")
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintln(out, "  To rotate the token later, edit the Secret and restart the pod")
	_, _ = fmt.Fprintln(out, "  so AnythingLLM re-reads the env var (it is not picked up live):")
	_, _ = fmt.Fprintf(out, "    kubectl edit -n %s secret %s\n", ai.Namespace, ai.AnythingLLMCredentialsSecretName)
	_, _ = fmt.Fprintf(out, "    kubectl rollout restart -n %s deployment %s\n\n", ai.Namespace, ai.AnythingLLMDeploymentName)
}

// defaultAskHost derives the Ask UI hostname from the cluster's
// domain via the SubdomainFor convention, the same way defaultChatHost
// does for the chat UI.
//
// `ask` was chosen over `docs` because Kipper reserves the `docs`
// subdomain for the project's own documentation (e.g.
// docs.getkipper.com); reusing it for the per-cluster RAG UI would
// conflate "the Kipper docs" with "the user's RAG-backed knowledge
// base".
func defaultAskHost(cluster *config.Cluster) (string, error) {
	if cluster.Domain == "" {
		return "", fmt.Errorf("--host is required (cluster has no domain configured)")
	}
	return installer.SubdomainFor("ask", cluster.Domain), nil
}
