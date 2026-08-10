package auth

import (
	"path/filepath"

	"github.com/getkipper/kipper/kip/internal/lockfile"
)

// lockStore takes an exclusive advisory lock guarding the auth store, so
// concurrent kip invocations (kubectl fans out exec-credential calls)
// serialize their refreshes: Dex rotates the refresh token on use, and two
// racing refreshes would leave one process holding a revoked token. The lock
// lives on a dedicated file — locking the store itself would break the
// atomic rename Save performs. The returned func releases the lock.
func lockStore() (func(), error) {
	path, err := storePath()
	if err != nil {
		return nil, err
	}
	return lockfile.Exclusive(filepath.Join(filepath.Dir(path), filepath.Base(path)+".lock"))
}
