package cmd

import (
	"bytes"
	"errors"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
)

// runProbe runs a throwaway subcommand of kip with the given arguments and
// returns everything kip printed.
func runProbe(t *testing.T, args ...string) string {
	t.Helper()
	probe := &cobra.Command{
		Use:  "probe <name>",
		Args: cobra.ExactArgs(1),
		RunE: func(*cobra.Command, []string) error { return errors.New("the host refused") },
	}
	probe.Flags().String("host", "", "")
	_ = probe.MarkFlagRequired("host")
	probe.Flags().Bool("a", false, "")
	probe.Flags().Bool("b", false, "")
	probe.MarkFlagsMutuallyExclusive("a", "b")
	rootCmd.AddCommand(probe)
	t.Cleanup(func() { rootCmd.RemoveCommand(probe) })

	var out bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&out)
	t.Cleanup(func() { rootCmd.SetOut(nil); rootCmd.SetErr(nil); rootCmd.SetArgs(nil) })
	rootCmd.SetArgs(append([]string{"probe"}, args...))
	_ = execute(&out)
	return out.String()
}

func TestUsageIsPrintedOnlyForMistakesInTheCommandLine(t *testing.T) {
	t.Run("a failure while working prints only the error", func(t *testing.T) {
		out := runProbe(t, "x", "--host", "h")
		assert.Contains(t, out, "the host refused")
		assert.NotContains(t, out, "Usage:")
	})
	t.Run("a wrong argument count shows usage", func(t *testing.T) {
		assert.Contains(t, runProbe(t, "--host", "h"), "Usage:")
	})
	t.Run("a missing required flag shows usage", func(t *testing.T) {
		assert.Contains(t, runProbe(t, "x"), "Usage:")
	})
	t.Run("conflicting flags show usage", func(t *testing.T) {
		assert.Contains(t, runProbe(t, "x", "--host", "h", "--a", "--b"), "Usage:")
	})
}
