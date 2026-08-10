package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/auth"
)

var authSessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage service-UI browser sessions",
}

var authSessionsRevokeAllCmd = &cobra.Command{
	Use:   "revoke-all",
	Short: "Revoke every service-UI session across the cluster",
	Long: `Deletes every service-UI session record and rotates the session
signing key. Every open service UI (MailHog, RabbitMQ Management, and so on)
signs out within the caches' refresh window, and outstanding single-use SSO
codes stop working. The console session is untouched; operators simply open a
service UI again to get a fresh session.

Use this after a suspected cookie or signing-key compromise. Admin only.`,
	RunE: runAuthSessionsRevokeAll,
}

func init() {
	authSessionsCmd.AddCommand(authSessionsRevokeAllCmd)
	authCmd.AddCommand(authSessionsCmd)
}

func runAuthSessionsRevokeAll(cmd *cobra.Command, args []string) error {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return err
	}
	store, err := auth.Load()
	if err != nil {
		return fmt.Errorf("loading auth: %w", err)
	}
	token, err := store.Token(cluster.Domain, cluster.DexHost())
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	url := fmt.Sprintf("https://%s/api/v1/sessions/revoke-all", cluster.ConsoleAPIHost())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling console API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return authRejectedError(ctx, cluster)
	}
	if resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s", shareAPIError(body, resp.StatusCode))
	}

	fmt.Printf("\n  ✔  All service-UI sessions on %s revoked\n\n", cluster.Domain)
	return nil
}
