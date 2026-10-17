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

func TestReadStorageMode(t *testing.T) {
	for name, tc := range map[string]struct {
		out  string
		err  error
		want string
		bad  bool
	}{
		"local":                           {out: "'local'", want: StorageLocal},
		"longhorn":                        {out: "longhorn", want: StorageLonghorn},
		"no storage field":                {out: "", want: StorageLonghorn},
		"a cluster predating the CRD":     {out: `error: the server doesn't have a resource type "platformconfig"`, err: errors.New("exit status 1"), want: StorageLonghorn},
		"an API server that is not there": {out: "The connection to the server 127.0.0.1:6443 was refused", err: errors.New("exit status 1"), bad: true},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := ReadStorageMode(&scriptedRunner{out: tc.out, err: tc.err})
			if tc.bad {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
