package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/twofa"
)

var twofaCmd = &cobra.Command{
	Use:   "2fa",
	Short: "Manage two-factor authentication for console users",
}

var twofaBootstrapCmd = &cobra.Command{
	Use:   "bootstrap [email]",
	Short: "Issue a one-time 2FA enrollment code for a console user",
	Long: `Issues a one-time code that authorises a console user to enroll their
2FA factor. The user enters it in Console → Settings → Two-factor
authentication, scans the QR code, and confirms.

Enrollment is gated on this code because it can only be issued with
kubeconfig access to the cluster. A stolen console login alone can then
never enroll an attacker's device.

The code is valid for 15 minutes and single-use. Issuing a new code for
the same email replaces any unused one.

A lost phone without recovery codes is recovered at host level:
  kip 2fa remove admin@example.com
  kip 2fa bootstrap admin@example.com
Then the user re-enrolls with the fresh code.

Example:
  kip 2fa bootstrap admin@example.com`,
	Args: cobra.ExactArgs(1),
	RunE: runTwofaBootstrap,
}

var twofaRemoveCmd = &cobra.Command{
	Use:   "remove [email]",
	Short: "Remove a console user's 2FA factor",
	Long: `Deletes the user's enrolled 2FA factor, leaving the account unenrolled.
Re-enrollment requires a fresh bootstrap code, and the new factor waits
out the full eligibility period before it can authorise a migration.

This is the host-level recovery path for a lost phone when no recovery
codes are left.

Example:
  kip 2fa remove admin@example.com`,
	Args: cobra.ExactArgs(1),
	RunE: runTwofaRemove,
}

func init() {
	twofaCmd.AddCommand(twofaBootstrapCmd)
	twofaCmd.AddCommand(twofaRemoveCmd)
	rootCmd.AddCommand(twofaCmd)
}

func runTwofaBootstrap(cmd *cobra.Command, args []string) error {
	email := args[0]

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	code, err := twofa.NewCode()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	secret := twofa.BuildSecret(email, code, time.Now())
	secrets := k8sClient.Clientset().CoreV1().Secrets(twofa.Namespace)
	if _, err := secrets.Create(ctx, secret, metav1.CreateOptions{}); err != nil {
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("storing enrollment code: %w", err)
		}
		existing, getErr := secrets.Get(ctx, secret.Name, metav1.GetOptions{})
		if getErr != nil {
			return fmt.Errorf("replacing enrollment code: %w", getErr)
		}
		existing.Data = secret.Data
		existing.Labels = secret.Labels
		if _, updateErr := secrets.Update(ctx, existing, metav1.UpdateOptions{}); updateErr != nil {
			return fmt.Errorf("replacing enrollment code: %w", updateErr)
		}
	}

	fmt.Printf("  ✔  Enrollment code for %s\n\n", email)
	fmt.Printf("     %s\n\n", code)
	fmt.Printf("  Valid for %d minutes, single-use.\n", int(twofa.BootstrapTTL.Minutes()))
	fmt.Println("  Enter it in Console → Settings → Two-factor authentication to enroll.")
	return nil
}

func runTwofaRemove(cmd *cobra.Command, args []string) error {
	email := args[0]

	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Factor Secrets are keyed by a hash of the token identity, so the
	// user's is found by matching the email recorded in the payload.
	secrets := k8sClient.Clientset().CoreV1().Secrets(twofa.Namespace)
	list, err := secrets.List(ctx, metav1.ListOptions{LabelSelector: "kipper.run/twofa=true"})
	if err != nil {
		return fmt.Errorf("listing 2FA factors: %w", err)
	}

	for i := range list.Items {
		secret := &list.Items[i]
		var payload struct {
			Email string `json:"email"`
		}
		if err := json.Unmarshal(secret.Data["factor"], &payload); err != nil || payload.Email != email {
			continue
		}
		if err := secrets.Delete(ctx, secret.Name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("removing 2FA factor: %w", err)
		}
		fmt.Printf("  ✔  Removed the 2FA factor of %s\n", email)
		fmt.Println("  Re-enrollment needs a fresh code: kip 2fa bootstrap " + email)
		return nil
	}

	return fmt.Errorf("no 2FA factor found for %s", email)
}
