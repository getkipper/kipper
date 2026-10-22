package installer

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKeyOnlySSHStepsRunWithoutHostHardening(t *testing.T) {
	steps := keyOnlySSHSteps(Options{Harden: false}, nil)
	require.Len(t, steps, 1, "--allow-ssh-passwords controls SSH hardening even when --harden=false")
	assert.Equal(t, "Turning off SSH password login", steps[0].name)
}

func TestKeyOnlySSHStepsSkippedWithAllowSSHPasswords(t *testing.T) {
	assert.Empty(t, keyOnlySSHSteps(Options{Harden: true, AllowSSHPasswords: true}, nil))
}
