package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestHardenOptionsKeepKeyOnlySSHWithoutHostHardening(t *testing.T) {
	opts := newHardenOptions(false, false, false, nil)
	assert.False(t, opts.harden)
	assert.True(t, opts.keyOnlySSH, "--allow-ssh-passwords controls SSH hardening even when --harden=false")
}

func TestHardenOptionsAllowSSHPasswordsSkipsKeyOnlySSH(t *testing.T) {
	opts := newHardenOptions(true, false, true, nil)
	assert.True(t, opts.harden)
	assert.False(t, opts.keyOnlySSH)
}
