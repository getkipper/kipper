package installer

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStepError_ReadsAsTheStepThenTheCause(t *testing.T) {
	err := &StepError{Step: "Installing k3s", Err: errors.New("exit status 1")}

	assert.Equal(t, "Installing k3s: exit status 1", err.Error())
}

func TestStepError_IsFoundThroughFurtherWrapping(t *testing.T) {
	host := HostOS{ID: "ubuntu", VersionID: "24.04", Family: FamilyDebian}
	wrapped := fmt.Errorf("install: %w", &StepError{Step: "Configuring firewall", Host: host, Profile: "small", Err: fs.ErrPermission})

	var stepErr *StepError
	require.True(t, errors.As(wrapped, &stepErr))
	assert.Equal(t, "Configuring firewall", stepErr.Step)
	assert.Equal(t, host, stepErr.Host)
	assert.Equal(t, "small", stepErr.Profile)
	assert.ErrorIs(t, wrapped, fs.ErrPermission)
}
