// Package projectenv resolves declared environments, supplying test when empty.
// Mutations must start from this effective set so adding or removing an explicit
// environment preserves existing namespaces intended to remain.
package projectenv

// Default is the environment a project gets when it declares none.
const Default = "test"

// Effective returns the environments a project has, given the ones it declares.
//
// The declared list is returned untouched when it holds anything at all: an
// explicit list is the whole answer, and a project that names only "prod" has
// only "prod".
func Effective(declared []string) []string {
	if len(declared) == 0 {
		return []string{Default}
	}
	return declared
}
