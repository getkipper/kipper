package cmd

import "github.com/spf13/cobra"

var aiRagCmd = &cobra.Command{
	Use:   "rag",
	Short: "Manage the RAG bundle (chat with your documents)",
	Long: `Phase 2 of the AI bundle adds Qdrant (vector store) and AnythingLLM
(chat-with-your-docs UI) on top of the Phase 1 chat bundle. Documents
uploaded through the AnythingLLM UI are embedded with a local model and
indexed in Qdrant; chat answers are grounded in those documents. No
data leaves the cluster.

The RAG bundle requires Phase 1 (Ollama + LibreChat) to be installed
first. It reuses the same Ollama Deployment for both generation and
embeddings, with a dedicated embedding model pulled at install time.

Examples:
  kip ai rag install
  kip ai rag install --host ask.example.com
  kip ai rag uninstall`,
}

func init() {
	aiCmd.AddCommand(aiRagCmd)
}
