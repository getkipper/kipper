package installer

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCheckLocalPathStorage(t *testing.T) {
	assert.NoError(t, checkLocalPathStorage(&scriptedRunner{out: "storageclass.storage.k8s.io/local-path\n"}))
	assert.ErrorContains(t, checkLocalPathStorage(&scriptedRunner{out: ""}), "local-path")
	assert.ErrorContains(t, checkLocalPathStorage(&scriptedRunner{err: errors.New("ssh: connection lost")}), "checking local storage")
}
