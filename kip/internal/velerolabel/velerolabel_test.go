package velerolabel

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/validation"
)

func TestValidName(t *testing.T) {
	t.Run("short name passes through", func(t *testing.T) {
		got := ValidName("snap")
		assert.Equal(t, "snap", got)
		assert.LessOrEqual(t, len(got), 63)
		assert.Empty(t, validation.IsValidLabelValue(got))
	})
	t.Run("63-char name passes through", func(t *testing.T) {
		name := strings.Repeat("a", 63)
		got := ValidName(name)
		assert.Equal(t, name, got)
		assert.Equal(t, 63, len(got))
	})
	t.Run("64-char name is truncated and hashed", func(t *testing.T) {
		name := strings.Repeat("a", 64)
		got := ValidName(name)
		assert.Equal(t, 63, len(got), "result must fit in label-value limit")
		assert.Empty(t, validation.IsValidLabelValue(got))
	})
	t.Run("different long names produce different labels", func(t *testing.T) {
		a := strings.Repeat("a", 200)
		b := strings.Repeat("a", 200) + "y"
		assert.NotEqual(t, ValidName(a), ValidName(b))
	})
	t.Run("deterministic for the same input", func(t *testing.T) {
		name := strings.Repeat("a", 200)
		assert.Equal(t, ValidName(name), ValidName(name))
	})
}

// Fixed hash suffixes keep the expectations independent of the implementation.
func TestValidNameMatchesIndependentlyComputedValues(t *testing.T) {
	assert.Equal(t, strings.Repeat("a", 63), ValidName(strings.Repeat("a", 63)))
	assert.Equal(t, strings.Repeat("c", 57)+"52b641", ValidName(strings.Repeat("c", 64)))
	assert.Equal(t, strings.Repeat("b", 57)+"b94440", ValidName(strings.Repeat("b", 70)))
}
