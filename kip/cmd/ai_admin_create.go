package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/kip/internal/ai"
)

var aiAdminCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create the first LibreChat admin account",
	Long: `Create an admin account on the in-cluster LibreChat. The bundle ships
with open registration disabled, so an admin must be seeded once after
install before anyone can log in.

This command runs LibreChat's create-user script inside the running
librechat pod via the Kubernetes API. No kubectl required.

Examples:
  kip ai admin create --email me@example.com --name "Alice Smith" --password 'a-strong-password'
  kip ai admin create --email me@example.com --name "Alice" --username alice --password 'a-strong-password'`,
	RunE: runAIAdminCreate,
}

func init() {
	aiAdminCreateCmd.Flags().String("email", "", "admin email address (required)")
	aiAdminCreateCmd.Flags().String("password", "", "admin password (required, at least 8 characters)")
	aiAdminCreateCmd.Flags().String("name", "", "admin display name (required)")
	aiAdminCreateCmd.Flags().String("username", "", "admin username (defaults to the local part of the email)")
	aiAdminCmd.AddCommand(aiAdminCreateCmd)
}

func runAIAdminCreate(cmd *cobra.Command, _ []string) error {
	cluster, client, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	email, _ := cmd.Flags().GetString("email")
	password, _ := cmd.Flags().GetString("password")
	name, _ := cmd.Flags().GetString("name")
	username, _ := cmd.Flags().GetString("username")

	fmt.Printf("\n  Creating LibreChat admin on %s\n\n", cluster.Name)

	if err := ai.CreateAdmin(context.Background(), client.Clientset(), client.RESTConfig(), os.Stdout, ai.AdminCreateOptions{
		Email:    email,
		Password: password,
		Name:     name,
		Username: username,
	}); err != nil {
		return err
	}

	fmt.Println()
	fmt.Println("  Open the chat URL and log in with these credentials.")
	fmt.Println("  Run 'kip ai status' for the chat URL if you forgot it.")
	fmt.Println()
	return nil
}
