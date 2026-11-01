package cmd

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

var userInviteCmd = &cobra.Command{
	Use:   "invite",
	Short: "Generate an invite link for a new team member",
	Long: `Creates a one-time invite URL for one person. They open it, set a
password, and get the assigned role under the address you name here.

The address is required. An invite without one is a link that grants its role
to whoever opens it, under whatever identity they type.

Examples:
  kip user invite --email dev@example.com --role deployer
  kip user invite --email pm@example.com --role viewer --expires 7d
  kip user invite --email ops@example.com --role admin --expires 24h`,
	RunE: runUserInvite,
}

func init() {
	userInviteCmd.Flags().String("email", "", "email address of the person being invited (required)")
	userInviteCmd.Flags().String("role", "deployer", "role: admin, deployer, or viewer")
	userInviteCmd.Flags().String("expires", "48h", "expiry: 24h, 48h, 7d")
	_ = userInviteCmd.MarkFlagRequired("email")

	userCmd.AddCommand(userInviteCmd)
}

// inviteEntry matches the console API's stored invite format. Records use the
// token hash as their key and retain the first eight token characters as Prefix.
type inviteEntry struct {
	Prefix  string `json:"prefix,omitempty"`
	Role    string `json:"role"`
	Expires string `json:"expires"`
	Email   string `json:"email,omitempty"`
}

func runUserInvite(cmd *cobra.Command, _ []string) error {
	role, _ := cmd.Flags().GetString("role")
	expiresStr, _ := cmd.Flags().GetString("expires")
	email, _ := cmd.Flags().GetString("email")

	// MarkFlagRequired checks presence, so an explicitly empty address needs
	// the same validation here.
	email = strings.TrimSpace(email)
	if email == "" {
		return fmt.Errorf("--email is required: an invite is for one person, and the account is created under that address")
	}

	if role != "admin" && role != "deployer" && role != "viewer" {
		return fmt.Errorf("role must be admin, deployer, or viewer")
	}

	duration, err := parseDurationCLI(expiresStr)
	if err != nil {
		return fmt.Errorf("invalid expiry: %w", err)
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	cs := k8sClient.Clientset()

	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return fmt.Errorf("generating token: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	expires := time.Now().Add(duration).Format(time.RFC3339)

	entry := inviteEntry{Role: role, Expires: expires, Email: email}
	if err := storeInviteEntry(ctx, cs, token, entry); err != nil {
		return err
	}

	url := fmt.Sprintf("https://%s/invite/%s", cluster.ConsoleHost(), token)

	fmt.Printf("\n  Invite link created\n")
	fmt.Printf("  For: %s\n", email)
	fmt.Printf("  Role: %s\n", role)
	fmt.Printf("  Expires: %s\n\n", expiresStr)
	fmt.Printf("  %s\n\n", url)
	fmt.Printf("  Send this link to %s. It can only be used once, and only\n", email)
	fmt.Printf("  that address can accept it.\n\n")

	return nil
}

// inviteKey returns the token hash used by the console API to look up invites.
// Hashing keeps the redeemable token out of newly stored records.
func inviteKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func storeInviteEntry(ctx context.Context, cs kubernetes.Interface, token string, entry inviteEntry) error {
	entry.Prefix = token[:min(8, len(token))]
	cm, err := cs.CoreV1().ConfigMaps("kipper-system").Get(ctx, "kipper-invites", metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
		invites := map[string]inviteEntry{inviteKey(token): entry}
		data, _ := json.Marshal(invites)
		_, err = cs.CoreV1().ConfigMaps("kipper-system").Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{
				Name: "kipper-invites", Namespace: "kipper-system",
				Labels: map[string]string{"app.kubernetes.io/managed-by": "kipper"},
			},
			Data: map[string]string{"invites": string(data)},
		}, metav1.CreateOptions{})
		if err != nil {
			return fmt.Errorf("storing invite: %w", err)
		}
	case err != nil:
		return fmt.Errorf("reading invites: %w", err)
	default:
		// Raw messages preserve other invites' fields, including the token field
		// required to look up invites stored before hashing.
		var invites map[string]json.RawMessage
		if err := json.Unmarshal([]byte(cm.Data["invites"]), &invites); err != nil {
			invites = map[string]json.RawMessage{}
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			return fmt.Errorf("storing invite: %w", err)
		}
		invites[inviteKey(token)] = raw
		data, _ := json.Marshal(invites)
		cm.Data["invites"] = string(data)
		if _, err := cs.CoreV1().ConfigMaps("kipper-system").Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("storing invite: %w", err)
		}
	}
	return nil
}

func parseDurationCLI(s string) (time.Duration, error) {
	if len(s) > 1 && s[len(s)-1] == 'd' {
		var days int
		if _, err := fmt.Sscanf(s, "%dd", &days); err == nil {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	return time.ParseDuration(s)
}
