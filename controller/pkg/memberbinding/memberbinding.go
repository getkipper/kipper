// Package memberbinding names the RoleBindings that project membership
// projects onto.
//
// Names are **generated, never parsed**. The reconcile can produce every name
// it would ever have used, which is what lets it find a binding for a role the
// project no longer lists: parsing a name to work out whose it is needs a
// separator that survives every project and role name, and no separator does.
// A project called `acme` and one called `acme-support` produce the same
// hyphenated string, and the two would take each other's bindings apart
// forever.
//
// So the name is two fixed-length digests, one for the project and one for the
// role. Fixed length is the whole point: it makes the project half a prefix the
// reconcile can list by, and it makes one project's prefix unable to be
// another's. That is a different claim from collisions being impossible, which
// no hash gives. At 128 bits, hitting a chosen project's prefix is a
// second-preimage search and hitting any two at random is a birthday problem
// over 2^64 names.
//
// It also puts the role name somewhere safe. A role name reaches this, and a
// digest is what stops a role called "Support Team!" producing an object name
// nothing can address.
package memberbinding

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// prefix is on every name this package generates, so a human reading a
// namespace can tell whose object it is even though the halves are opaque.
const prefix = "kipper-"

// digestLength is the hex characters kept from each SHA-256. Sixteen bytes,
// so 128 bits per half.
const digestLength = 32

// Name is the RoleBinding that grants role within project.
func Name(project, role string) string {
	return prefix + digest(project) + "-" + digest(role)
}

// Prefix is what every binding of a project's carries, and what a cluster-wide
// listing selects on.
func Prefix(project string) string {
	return prefix + digest(project) + "-"
}

// LegacyNames are the three fixed names every released build has written.
//
// They carry no project digest, so no prefix listing finds them, and the name
// is identical in every namespace so it says nothing about whose it is. They
// stay until a later release retires them, and until then both generations are
// written.
func LegacyNames() []string {
	return []string{
		"kipper-project-owner",
		"kipper-project-deployer",
		"kipper-project-viewer",
	}
}

// IsManaged reports whether a name is one this project's membership writes,
// in either generation.
//
// The cluster-admin listing enumerates by shape and applies no label selector,
// because a label is exactly what drifts and a selector would let drift hide a
// binding. So this has to be exact in both directions: loose enough to see
// both generations, tight enough that somebody else's object is never taken
// for ours.
func IsManaged(name string) bool {
	for _, legacy := range LegacyNames() {
		if name == legacy {
			return true
		}
	}
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return false
	}
	project, role, found := strings.Cut(rest, "-")
	if !found {
		return false
	}
	return isDigest(project) && isDigest(role)
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:digestLength]
}

func isDigest(s string) bool {
	if len(s) != digestLength {
		return false
	}
	for _, c := range s {
		isDigit := c >= '0' && c <= '9'
		isHexLetter := c >= 'a' && c <= 'f'
		if !isDigit && !isHexLetter {
			return false
		}
	}
	return true
}
