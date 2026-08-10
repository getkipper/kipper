package export

import "github.com/getkipper/kipper/datamover/internal/manifest"

// manifestBuild returns the digest of a freshly built dir manifest.
func manifestBuild(root string) (string, error) {
	m, err := manifest.BuildDir(root, 1024)
	if err != nil {
		return "", err
	}
	raw, err := manifest.Encode(m)
	if err != nil {
		return "", err
	}
	return manifest.Digest(raw), nil
}
