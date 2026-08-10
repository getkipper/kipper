package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/getkipper/kipper/kip/internal/auth"
)

var projectGVR = schema.GroupVersionResource{
	Group:    "kipper.run",
	Version:  "v1alpha1",
	Resource: "projects",
}

var projectAllowLinksCmd = &cobra.Command{
	Use:   "allow-links [from-project]",
	Short: "Let another project's apps link to apps in this one",
	Long: `Records this project's consent to being linked to from another.

An app in the calling project can then reach a named app here directly over the
cluster network. That path goes past the ingress, so anything enforced on a
public route — an API key, forward auth, a rate limit — is not in the way of it.
That is why the decision belongs to this project rather than the one asking.

Without this, a link from the other side is recorded and no traffic is allowed.

  kip project allow-links hrportal --project docuseal

Use --remove to withdraw it. Withdrawing closes the path on the next reconcile.`,
	Args: cobra.ExactArgs(1),
	RunE: runProjectAllowLinks,
}

var projectLinksCmd = &cobra.Command{
	Use:   "links",
	Short: "Show which projects may link to this one",
	Args:  cobra.NoArgs,
	RunE:  runProjectLinks,
}

func init() {
	projectAllowLinksCmd.Flags().String("project", "", "the project granting consent")
	projectAllowLinksCmd.Flags().Bool("remove", false, "withdraw consent instead of granting it")
	projectLinksCmd.Flags().String("project", "", "project name")

	projectCmd.AddCommand(projectAllowLinksCmd)
	projectCmd.AddCommand(projectLinksCmd)
}

// resolveProjectName returns the project a project-scoped command acts on:
// --project when given, otherwise the one currently selected.
func resolveProjectName(cmd *cobra.Command) (string, error) {
	if name, _ := cmd.Flags().GetString("project"); name != "" {
		return name, nil
	}
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return "", err
	}
	if cluster.CurrentProject == "" {
		return "", fmt.Errorf("no project selected; pass --project or run 'kip project use <name>'")
	}
	return cluster.CurrentProject, nil
}

func runProjectAllowLinks(cmd *cobra.Command, args []string) error {
	from := args[0]
	remove, _ := cmd.Flags().GetBool("remove")

	target, err := resolveProjectName(cmd)
	if err != nil {
		return err
	}
	if from == target {
		return fmt.Errorf("a project already reaches its own apps; consent is only needed from another project")
	}

	// Console-api rather than the Project resource directly. Granting is the
	// target project owner's decision, and a project owner holds a namespaced
	// role — the Project is cluster-scoped, so letting them edit it would mean
	// letting them edit every project. The API resolves their membership of
	// this one and requires the owner role.
	body, err := json.Marshal(map[string]any{"project": from, "allow": !remove})
	if err != nil {
		return err
	}
	allowed, err := callLinkConsent(cmd.Context(), target, http.MethodPut, body)
	if err != nil {
		return err
	}

	if remove {
		fmt.Printf("\n  ✔  %s may no longer link to %s\n", from, target)
		fmt.Printf("     Any path its apps had into %s is closed.\n\n", target)
		return nil
	}
	fmt.Printf("\n  ✔  %s may link to %s\n", from, target)
	fmt.Printf("     Apps in %s can now be linked to, one at a time, with:\n", target)
	fmt.Printf("       kip app link %s/<app> <their-app> --project %s\n\n", target, from)
	_ = allowed
	return nil
}

// callLinkConsent talks to console-api's per-project consent endpoint and
// returns the resulting list.
func callLinkConsent(ctx context.Context, project, method string, body []byte) ([]string, error) {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return nil, err
	}
	store, err := auth.Load()
	if err != nil {
		return nil, fmt.Errorf("loading auth: %w", err)
	}
	token, err := store.Token(cluster.Domain, cluster.DexHost())
	if err != nil {
		return nil, err
	}

	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://%s/api/v1/projects/%s/link-consent", cluster.ConsoleAPIHost(), project)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("calling console API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	payload, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, fmt.Errorf("reading the console API response: %w", readErr)
	}
	if resp.StatusCode == http.StatusUnauthorized {
		return nil, authRejectedError(ctx, cluster)
	}
	if resp.StatusCode == http.StatusForbidden {
		if method == http.MethodGet {
			return nil, fmt.Errorf("you do not have access to project %s", project)
		}
		return nil, fmt.Errorf("you need the owner role on %s to change who may link to it", project)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("%s", shareAPIError(payload, resp.StatusCode))
	}

	var decoded struct {
		AllowLinksFrom []string `json:"allowLinksFrom"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}
	return decoded.AllowLinksFrom, nil
}

func runProjectLinks(cmd *cobra.Command, _ []string) error {
	target, err := resolveProjectName(cmd)
	if err != nil {
		return err
	}
	allowed, err := callLinkConsent(cmd.Context(), target, http.MethodGet, nil)
	if err != nil {
		return err
	}

	fmt.Printf("\n  Projects allowed to link to %s\n\n", target)
	if len(allowed) == 0 {
		fmt.Printf("    None. Apps elsewhere cannot reach apps in %s.\n", target)
		fmt.Printf("    Allow one with:  kip project allow-links <project> --project %s\n\n", target)
		return nil
	}
	for _, name := range allowed {
		fmt.Printf("    %s\n", name)
	}
	fmt.Printf("\n  Each still needs its own 'kip app link' naming the app it reaches.\n\n")
	return nil
}
