package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestExecAsksForATTYOnlyFromATerminal(t *testing.T) {
	interactive := execOptions("app", []string{"/bin/sh"}, true)
	assert.True(t, interactive.TTY)
	assert.True(t, interactive.Stdin)

	scripted := execOptions("app", []string{"psql", "-c", "select 1"}, false)
	assert.False(t, scripted.TTY)
	assert.True(t, scripted.Stdin, "Stdin must remain enabled for piped input.")
	assert.Equal(t, "app", scripted.Container)
	assert.Equal(t, []string{"psql", "-c", "select 1"}, scripted.Command)
}
