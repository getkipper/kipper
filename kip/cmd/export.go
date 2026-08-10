package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

var exportCmd = &cobra.Command{
	Use:   "export",
	Short: "Export the current cluster state as a kipper.yaml manifest",
	RunE:  runExport,
}

func init() {
	exportCmd.Flags().String("project", "", "project name (required)")
	exportCmd.Flags().String("environment", "", "target environment. Mutually exclusive with --split.")
	exportCmd.Flags().StringP("output", "o", "", "output file (defaults to stdout). With --split, this is the output directory.")
	exportCmd.Flags().Bool("split", false, "export every environment of the project, one file per env, into the --output directory")
	_ = exportCmd.MarkFlagRequired("project")
	rootCmd.AddCommand(exportCmd)
}

func runExport(cmd *cobra.Command, args []string) error {
	project, _ := cmd.Flags().GetString("project")
	environment, _ := cmd.Flags().GetString("environment")
	output, _ := cmd.Flags().GetString("output")
	split, _ := cmd.Flags().GetBool("split")

	if split {
		if environment != "" {
			return fmt.Errorf("--split and --environment are mutually exclusive: --split writes every env, --environment writes one")
		}
		if output == "" {
			return fmt.Errorf("--split requires --output to be a target directory (will contain one file per environment)")
		}
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	dynClient := k8sClient.Dynamic()

	if split {
		return exportSplit(ctx, dynClient, cluster, project, output)
	}

	return exportSingle(ctx, dynClient, cluster, project, environment, output)
}

func exportSingle(ctx context.Context, dynClient dynamic.Interface, cluster *config.Cluster, project, environment, output string) error {
	namespace := cluster.ResolveNamespace(project, environment)
	m, err := manifest.Export(ctx, dynClient, project, environment, namespace)
	if err != nil {
		return err
	}

	data, err := manifest.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshalling manifest: %w", err)
	}

	if output != "" {
		if err := os.WriteFile(output, data, 0600); err != nil {
			return fmt.Errorf("writing %s: %w", output, err)
		}
		fmt.Printf("  ✔  Exported to %s\n", output)
	} else {
		fmt.Print(string(data))
	}
	return nil
}

// exportSplit reads the Project CR's spec.environments list and writes
// one manifest file per environment into outputDir. Use case is a
// migration export where every env needs to be captured in one go.
//
// The Project CR name follows the same resolved-namespace convention as
// `kip project create`, so org-prefixed projects (cluster.Org=acme,
// CR name `acme-deck`) are resolved here before the lookup; passing the
// raw user input would silently miss the CR and degrade to a single-file
// fallback.
func exportSplit(ctx context.Context, dynClient dynamic.Interface, cluster *config.Cluster, project, outputDir string) error {
	if err := os.MkdirAll(outputDir, 0o750); err != nil {
		return fmt.Errorf("creating output dir %s: %w", outputDir, err)
	}

	crName := cluster.ResolveNamespace(project, "")
	envs, err := projectEnvironments(ctx, dynClient, crName)
	if err != nil {
		return err
	}
	if len(envs) == 0 {
		// Project exists but has no environments declared, or no Project
		// CR is present (controllers older than the environments feature,
		// or a freshly created project before the first env). Fall back
		// to a single default-env export so the user gets something
		// useful even when --split has nothing to iterate.
		return exportSingle(ctx, dynClient, cluster, project, "", filepath.Join(outputDir, project+".yaml"))
	}

	for _, env := range envs {
		path := filepath.Join(outputDir, env+".yaml")
		if err := exportSingle(ctx, dynClient, cluster, project, env, path); err != nil {
			return fmt.Errorf("exporting env %s: %w", env, err)
		}
	}
	return nil
}

// projectEnvironments returns the env names declared on the Project CR.
// Returns an empty slice (no error) when the project's CR cannot be
// found, so the caller can fall back to a single-env export. Any other
// API error (RBAC, network, apiserver outage) is returned so a backup-
// grade --split run does not silently produce a partial export.
func projectEnvironments(ctx context.Context, dynClient dynamic.Interface, crName string) ([]string, error) {
	cr, err := dynClient.Resource(manifest.ProjectGVR).Get(ctx, crName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading project %q: %w", crName, err)
	}
	envsRaw, found, err := unstructured.NestedSlice(cr.Object, "spec", "environments")
	if err != nil {
		return nil, fmt.Errorf("reading project %q environments: %w", crName, err)
	}
	if !found {
		return nil, nil
	}
	envs := make([]string, 0, len(envsRaw))
	for _, e := range envsRaw {
		em, ok := e.(map[string]interface{})
		if !ok {
			continue
		}
		if name, ok := em["name"].(string); ok && name != "" {
			envs = append(envs, name)
		}
	}
	return envs, nil
}
