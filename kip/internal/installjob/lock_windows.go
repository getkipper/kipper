//go:build windows

package installjob

import "errors"

// lock is only needed on the server, which runs Linux.
func lock(string) (func(), error) {
	return nil, errors.New("install jobs run on the Linux server, not on Windows")
}
