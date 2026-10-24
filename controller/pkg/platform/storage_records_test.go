package platform

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackoffDoublesFromOneMinuteToThirty(t *testing.T) {
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute, 30 * time.Minute}
	for i, d := range want {
		assert.Equal(t, d, BackoffDelay(i+1), "refusal %d", i+1)
	}
}

func TestReadRecord(t *testing.T) {
	var stop CopyChangeStop
	found, err := ReadRecord(map[string]string{CopyChangeStopAnnotation: `{"replicas":1,"at":"2026-10-24T12:00:00Z"}`}, CopyChangeStopAnnotation, &stop)
	require.NoError(t, err)
	assert.True(t, found)
	one := int32(1)
	assert.Equal(t, CopyChangeStop{Replicas: &one, At: "2026-10-24T12:00:00Z"}, stop)

	found, err = ReadRecord(nil, CopyChangeStopAnnotation, &stop)
	assert.False(t, found)
	assert.NoError(t, err)

	found, err = ReadRecord(map[string]string{CopyChangeStopAnnotation: "{"}, CopyChangeStopAnnotation, &stop)
	assert.True(t, found)
	assert.Error(t, err)
}
