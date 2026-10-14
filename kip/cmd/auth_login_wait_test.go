package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitForLoginPageStopsBeforeTheBrowserWhenDexIsNotReady(t *testing.T) {
	asked := ""
	err := waitForLoginPage(context.Background(), "dex.example.com", func(_ context.Context, host string) error {
		asked = host
		return errors.New("/dex/theme/styles.css: issuer returned 502 Bad Gateway")
	})

	require.Error(t, err)
	assert.Equal(t, "dex.example.com", asked)
	assert.Contains(t, err.Error(), "kip auth login")
	assert.Contains(t, err.Error(), "styles.css")

	assert.NoError(t, waitForLoginPage(context.Background(), "dex.example.com", func(context.Context, string) error { return nil }))
}
