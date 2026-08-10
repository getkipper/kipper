package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/auth"
	"github.com/getkipper/kipper/kip/internal/config"
)

var serviceShareCmd = &cobra.Command{
	Use:   "share [service]",
	Short: "Create and manage shareable links to a service's web UI",
	Long: `Create a signed, expiring link that opens a service's web UI (e.g. the
MailHog inbox) without a Kipper login. Hand the link to someone who needs to
see the UI but should not have console access.

Minting, listing, and revoking all run through the console API, so the link
is backed by a revocable grant rather than a bare signed token. Revoke a
single link by its id, revoke every link for the cluster at once, or rotate
the signing key if one leaks.

  kip service share mailhog                       mint a link (default 72h)
  kip service share mailhog --expires 24h         mint a shorter-lived link
  kip service share mailhog --list                list the service's links
  kip service share mailhog --revoke <id>         revoke one link
  kip service share --revoke-all                  revoke every link (emergency)
  kip service share --rotate-key                  rotate the signing key`,
	Args: cobra.MaximumNArgs(1),
	RunE: runServiceShare,
}

func init() {
	serviceShareCmd.Flags().String("project", "default", "project namespace")
	serviceShareCmd.Flags().String("environment", "", "target environment")
	serviceShareCmd.Flags().Duration("expires", 72*time.Hour, "how long the link stays valid (max 720h)")
	serviceShareCmd.Flags().String("label", "", "a note shown in the link listing (e.g. \"PO review\")")
	serviceShareCmd.Flags().Bool("list", false, "list the service's live share links")
	serviceShareCmd.Flags().String("revoke", "", "revoke a single share link by its id")
	serviceShareCmd.Flags().Bool("revoke-all", false, "revoke every share link in the cluster")
	serviceShareCmd.Flags().Bool("rotate-key", false, "rotate the share signing key (retires leaked keys over two rotations)")
	serviceCmd.AddCommand(serviceShareCmd)
}

func runServiceShare(cmd *cobra.Command, args []string) error {
	rotateKey, _ := cmd.Flags().GetBool("rotate-key")
	revokeAll, _ := cmd.Flags().GetBool("revoke-all")
	list, _ := cmd.Flags().GetBool("list")
	revokeID, _ := cmd.Flags().GetString("revoke")

	if err := validateShareOperation(cmd, args, rotateKey, revokeAll, list, revokeID); err != nil {
		return err
	}

	client, err := newShareClient()
	if err != nil {
		return err
	}
	ctx := context.Background()

	// Cluster-wide operations take no service argument.
	switch {
	case rotateKey:
		return client.rotateKey(ctx)
	case revokeAll:
		return client.revokeAll(ctx)
	}

	name := args[0]
	namespace := resolveServiceNamespace(cmd)

	switch {
	case list:
		return client.list(ctx, name, namespace)
	case revokeID != "":
		return client.revoke(ctx, name, namespace, revokeID)
	default:
		lifetime, _ := cmd.Flags().GetDuration("expires")
		label, _ := cmd.Flags().GetString("label")
		return client.mint(ctx, name, namespace, lifetime, label)
	}
}

// validateShareOperation refuses ambiguous invocations before anything runs.
// The operations are mutually exclusive, cluster-wide ones take no service
// argument, and the mint-only flags (--expires/--label) belong to a mint.
// Priority-based dispatch would otherwise silently run one action and drop
// the rest — dangerous when a stray flag turns a mint into a cluster-wide
// revoke.
func validateShareOperation(cmd *cobra.Command, args []string, rotateKey, revokeAll, list bool, revokeID string) error {
	var ops []string
	if list {
		ops = append(ops, "--list")
	}
	if revokeID != "" {
		ops = append(ops, "--revoke")
	}
	if revokeAll {
		ops = append(ops, "--revoke-all")
	}
	if rotateKey {
		ops = append(ops, "--rotate-key")
	}
	if len(ops) > 1 {
		return fmt.Errorf("choose only one of %s", strings.Join(ops, ", "))
	}

	clusterWide := revokeAll || rotateKey
	if clusterWide {
		if len(args) != 0 {
			return fmt.Errorf("%s operates on the whole cluster; drop the service argument", ops[0])
		}
	} else if len(args) != 1 {
		return fmt.Errorf("a service name is required (e.g. kip service share mailhog)")
	}

	// --expires and --label only shape a fresh link; reject them on any
	// non-mint operation so a mistaken combination fails loudly.
	if len(ops) == 1 {
		for _, f := range []string{"expires", "label"} {
			if cmd.Flags().Changed(f) {
				return fmt.Errorf("--%s only applies when minting a link, not with %s", f, ops[0])
			}
		}
	}
	return nil
}

// shareLink mirrors the console API's share link response. URL is only
// populated on mint; listings identify links without exposing the token.
type shareLink struct {
	ID        string    `json:"id"`
	URL       string    `json:"url,omitempty"`
	Label     string    `json:"label,omitempty"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (c *shareClient) mint(ctx context.Context, name, namespace string, lifetime time.Duration, label string) error {
	body := map[string]string{"expires_in": lifetime.String()}
	if label != "" {
		body["label"] = label
	}
	var link shareLink
	path := fmt.Sprintf("/services/%s/shares?namespace=%s", url.PathEscape(name), url.QueryEscape(namespace))
	if err := c.do(ctx, http.MethodPost, path, body, &link); err != nil {
		return err
	}

	fmt.Printf("\n  Share link for %s (valid until %s):\n\n", name, link.ExpiresAt.Local().Format("2 Jan 2006 15:04"))
	fmt.Printf("  %s\n\n", link.URL)
	fmt.Printf("  Anyone with this link can open the UI until it expires.\n")
	fmt.Printf("  Revoke it:      kip service share %s --revoke %s\n", name, link.ID)
	fmt.Printf("  Revoke all:     kip service share --revoke-all\n\n")
	return nil
}

func (c *shareClient) list(ctx context.Context, name, namespace string) error {
	var links []shareLink
	path := fmt.Sprintf("/services/%s/shares?namespace=%s", url.PathEscape(name), url.QueryEscape(namespace))
	if err := c.do(ctx, http.MethodGet, path, nil, &links); err != nil {
		return err
	}

	if len(links) == 0 {
		fmt.Printf("\n  No share links for %s.\n\n", name)
		return nil
	}

	fmt.Printf("\n  Share links for %s:\n\n", name)
	fmt.Printf("  %-34s  %-20s  %-24s  %s\n", "ID", "EXPIRES", "CREATED BY", "LABEL")
	for _, l := range links {
		fmt.Printf("  %-34s  %-20s  %-24s  %s\n",
			l.ID,
			l.ExpiresAt.Local().Format("2 Jan 2006 15:04"),
			orDash(l.CreatedBy),
			orDash(printable(l.Label)))
	}
	fmt.Println()
	return nil
}

// printable strips control characters from a stored label before it reaches
// the terminal: another admin's label must not carry ANSI escapes or
// newlines into this one's output.
func printable(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func (c *shareClient) revoke(ctx context.Context, name, namespace, id string) error {
	path := fmt.Sprintf("/services/%s/shares/%s?namespace=%s", url.PathEscape(name), url.PathEscape(id), url.QueryEscape(namespace))
	if err := c.do(ctx, http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	fmt.Printf("\n  ✔  Revoked share link %s for %s.\n\n", id, name)
	return nil
}

func (c *shareClient) revokeAll(ctx context.Context) error {
	if err := c.do(ctx, http.MethodDelete, "/shares", nil, nil); err != nil {
		return err
	}
	fmt.Printf("\n  ✔  Revoked every share link in the cluster.\n")
	fmt.Printf("  If a signing key leaked, also rotate it twice: kip service share --rotate-key\n\n")
	return nil
}

func (c *shareClient) rotateKey(ctx context.Context) error {
	var out struct {
		CurrentKID string `json:"current_kid"`
	}
	if err := c.do(ctx, http.MethodPost, "/shares/rotate-key", nil, &out); err != nil {
		return err
	}
	fmt.Printf("\n  ✔  Rotated the share signing key (now %s).\n", out.CurrentKID)
	fmt.Printf("  Links signed before the rotation stay valid until they expire or the\n")
	fmt.Printf("  next rotation. Rotate a second time to retire a leaked key completely.\n\n")
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// shareClient calls the console API's admin-only share endpoints with the
// caller's Dex-issued token.
type shareClient struct {
	baseURL string
	token   string
	cluster *config.Cluster
}

func newShareClient() (*shareClient, error) {
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
	return &shareClient{
		baseURL: fmt.Sprintf("https://%s/api/v1", cluster.ConsoleAPIHost()),
		token:   token,
		cluster: cluster,
	}, nil
}

func (c *shareClient) do(ctx context.Context, method, path string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("calling share API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusUnauthorized {
		return authRejectedError(ctx, c.cluster)
	}
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("%s", shareAPIError(respBody, resp.StatusCode))
	}
	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("parsing response: %w", err)
		}
	}
	return nil
}

// shareAPIError pulls the human message out of the console API's
// {"error": "..."} envelope, falling back to the raw body.
func shareAPIError(body []byte, status int) string {
	var env struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Error != "" {
		return env.Error
	}
	if len(body) > 0 {
		return string(body)
	}
	return fmt.Sprintf("share API returned status %d", status)
}
