// Package projectenv holds the rule that decides which environments a project
// has, as opposed to which ones it declares.
//
// A Project that declares none still has one, and the difference is not
// cosmetic: the reconciler creates a namespace for it and workloads run there.
// Every surface that appends to or removes from that list has to start from the
// effective set, because the reconciler builds its keep-list the same way and
// deletes any namespace the list no longer accounts for.
//
// Getting that wrong deletes a live namespace and everything in it. It has
// already happened at two surfaces independently — the console's add-environment
// handler and the CLI's — which is why the rule lives here rather than beside
// either of them.
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
