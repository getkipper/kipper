package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestScaleCountTakesThePositionalOrTheFlag(t *testing.T) {
	n, err := scaleCount([]string{"web", "3"}, 0, false)
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	n, err = scaleCount([]string{"web"}, 2, true)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	n, err = scaleCount([]string{"web"}, 0, true)
	require.NoError(t, err)
	assert.Equal(t, 0, n, "an explicit --replicas 0 scales to zero")

	_, err = scaleCount([]string{"web"}, 0, false)
	assert.ErrorContains(t, err, "how many replicas", "a missing count never means zero")

	_, err = scaleCount([]string{"web", "3"}, 2, true)
	assert.ErrorContains(t, err, "3 and --replicas 2")

	_, err = scaleCount([]string{"web", "three"}, 0, false)
	assert.ErrorContains(t, err, "three")

	_, err = scaleCount([]string{"web", "-1"}, 0, false)
	assert.Error(t, err)
}

func TestAppScaleAcceptsTheCountAsAnArgument(t *testing.T) {
	assert.NoError(t, appScaleCmd.Args(appScaleCmd, []string{"web", "3"}))
	assert.Error(t, appScaleCmd.Args(appScaleCmd, []string{"web", "3", "4"}))
	assert.NoError(t, appScaleCmd.ValidateRequiredFlags(), "the count can come without --replicas")

	_, err := scaleCount([]string{"web", "99999999999"}, 0, false)
	assert.Error(t, err, "a count beyond the API's int32 is refused")
	n, err := scaleCount([]string{"web", "2"}, 2, true)
	require.NoError(t, err)
	assert.Equal(t, 2, n, "the same count twice is fine")
}
