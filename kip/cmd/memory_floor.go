package cmd

import "github.com/spf13/cobra"

// skipMemoryFloorFlag installs Longhorn below installer.LonghornMinRAMMB. It
// exists to measure smaller servers and is left out of help and docs.
const skipMemoryFloorFlag = "skip-memory-floor"

func addSkipMemoryFloorFlag(cmd *cobra.Command) {
	cmd.Flags().Bool(skipMemoryFloorFlag, false, "install Longhorn below the RAM floor and record the override")
	_ = cmd.Flags().MarkHidden(skipMemoryFloorFlag)
}

func skipMemoryFloor(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool(skipMemoryFloorFlag)
	return v
}
