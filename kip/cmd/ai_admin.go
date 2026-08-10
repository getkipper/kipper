package cmd

import (
	"github.com/spf13/cobra"
)

var aiAdminCmd = &cobra.Command{
	Use:   "admin",
	Short: "Manage admin accounts on the in-cluster LibreChat",
}

func init() {
	aiCmd.AddCommand(aiAdminCmd)
}
