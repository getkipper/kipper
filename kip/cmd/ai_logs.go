package cmd

import (
	"context"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/kip/internal/ai"
)

var aiLogsCmd = &cobra.Command{
	Use:   "logs <component>",
	Short: "Show the logs of an AI bundle component",
	Long: `Show the logs of an AI bundle component: ` + strings.Join(ai.LogComponents, ", ") + `.

model-probe checks that the chat model is available, and embedding-pull downloads
and checks the embedding model. For these two components, logs come from the
newest existing job, and kip keeps a finished job for an hour.
Use --chart to show logs from the Helm controller's job that installs or upgrades
the component's chart.`,
	Example: `  kip ai logs ollama --follow
  kip ai logs librechat --chart`,
	Args:      cobra.ExactArgs(1),
	ValidArgs: ai.LogComponents,
	RunE:      runAILogs,
}

func init() {
	addLogFlags(aiLogsCmd, 100)
	aiLogsCmd.Flags().Bool("chart", false, "show logs from the Helm job for the component's chart")
	aiCmd.AddCommand(aiLogsCmd)
}

func runAILogs(cmd *cobra.Command, args []string) error {
	_, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	chart, _ := cmd.Flags().GetBool("chart")
	return aiLogs(context.Background(), client.Clientset(), args[0], chart, readLogFlags(cmd), os.Stdout, os.Stderr)
}

func aiLogs(ctx context.Context, client kubernetes.Interface, component string, chart bool, req logRequest, out, errOut io.Writer) error {
	namespace, pods, err := ai.LogPods(ctx, client, component, chart)
	if err != nil {
		return err
	}
	return printPodLogs(ctx, clientsetLogOpener(client, namespace), pods, component, req, out, errOut)
}
