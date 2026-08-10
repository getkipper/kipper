package installer

import (
	"regexp"
	"strings"

	"golang.org/x/mod/semver"
)

// gitDescribeSuffix matches what `git describe --tags --always --dirty` appends
// to a tag once the tree has moved past it: the commit count and the abbreviated
// hash. It has to match at the end rather than as the whole prerelease, because
// describing a commit after a tag that already carries one appends to it —
// v0.9.0-rc.1 becomes v0.9.0-rc.1-5-gabc1234, whose prerelease is
// "rc.1-5-gabc1234". Anchoring only at the start missed that, and semver then
// ordered it ABOVE v0.9.0-rc.2, because a non-numeric identifier outranks a
// numeric one. The upgrade to rc.2 was refused as a downgrade.
var gitDescribeSuffix = regexp.MustCompile(`(?:^|-)\d+-g[0-9a-f]{4,}$`)

// comparableVersion normalises a kip version for ordering and reports whether
// ordering it means anything.
//
// A version that cannot be ordered is not an error. A source build reports
// "dev", and a build from a repository with no tags reports a bare commit hash;
// refusing on either would make the guard impossible to develop against. The
// caller allows those and says so rather than pretending a comparison happened.
func ComparableVersion(v string) (string, bool) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", false
	}
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return "", false
	}
	// `git describe` renders a commit *after* v0.8.3 as "v0.8.3-5-gabc1234",
	// which semver reads as a prerelease of v0.8.3 and therefore orders BEFORE
	// it. The truth is the opposite, so ordering such a build would refuse
	// upgrades that are actually moving forward. A genuine prerelease
	// ("v0.9.0-rc.1") carries no such inversion and is left comparable.
	pre := strings.TrimPrefix(semver.Prerelease(v), "-")
	if pre != "" && gitDescribeSuffix.MatchString(pre) {
		return "", false
	}
	// A dirty tree is not the version it claims to be, and semver orders
	// "v0.9.0-dirty" below "v0.9.0", so comparing it would refuse an upgrade
	// from a modified local build to the release it was built from.
	if pre != "" && strings.Contains(pre, "dirty") {
		return "", false
	}
	// Canonical keeps the prerelease and drops build metadata, which carries no
	// ordering by specification. It also settles a two-part tag: "v0.9" and
	// "v0.9.0" are the same version and must not read as different ones.
	return semver.Canonical(v), true
}

// clusterIsNewerThan reports whether a CRD's recorded writer is a later kip than
// the one running, along with the two versions for the message. It answers false
// whenever either side cannot be ordered, which includes a CRD written before
// stamping existed.
func ClusterIsNewerThan(stamp, running string) (cluster string, mine string, newer bool) {
	live, liveOK := ComparableVersion(stamp)
	if !liveOK {
		return "", "", false
	}
	self, selfOK := ComparableVersion(running)
	if !selfOK {
		return live, "", false
	}
	return live, self, semver.Compare(self, live) < 0
}
