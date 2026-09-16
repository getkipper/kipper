// Package memberbinding derives RoleBinding names from fixed-length project
// and role digests. Generate expected names to identify bindings; original
// project and role names cannot be recovered from them. The project digest
// forms a stable prefix, avoiding ambiguity from hyphens in source names.
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

// ProjectPrefixOf returns the project prefix carried by a generated name.
//
// ok is false for anything else, including the fixed legacy names: those carry
// no digest, so any prefix returned for one would be a guess. This is what lets
// a binding be attributed to its project from the name alone, after every
// mutable trail back to it has been edited away.
func ProjectPrefixOf(name string) (string, bool) {
	rest, ok := strings.CutPrefix(name, prefix)
	if !ok {
		return "", false
	}
	project, role, found := strings.Cut(rest, "-")
	if !found || !isDigest(project) || !isDigest(role) {
		return "", false
	}
	return prefix + project + "-", true
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
