package cmd

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsYes(t *testing.T) {
	for _, a := range []string{"y", "Y", "yes", "YES", "Yes", " yes \n", "y\r\n"} {
		assert.True(t, isYes(a), "%q", a)
	}
	for _, a := range []string{"", "n", "no", "yess", "ye", "sure", "1"} {
		assert.False(t, isYes(a), "%q", a)
	}
}

func TestConfirmUnboundHostTakesYes(t *testing.T) {
	assert.True(t, confirmUnboundHost(strings.NewReader("yes\n"), io.Discard, "203.0.113.10"))
}

func TestExecuteReportsAnErrorOnce(t *testing.T) {
	var out bytes.Buffer
	rootCmd.SetArgs([]string{"--no-such-flag-anywhere"})
	rootCmd.SetErr(&out)
	rootCmd.SetOut(io.Discard)
	t.Cleanup(func() {
		rootCmd.SetArgs(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetOut(nil)
	})

	code := execute(&out)

	assert.Equal(t, 1, code)
	assert.Equal(t, 1, strings.Count(out.String(), "no-such-flag-anywhere"), out.String())
	assert.True(t, strings.HasPrefix(out.String(), "Error: "), out.String())
}
