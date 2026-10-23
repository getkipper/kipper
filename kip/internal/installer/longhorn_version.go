package installer

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// longhornVersion is the Longhorn release kip installs on new clusters and
// moves existing clusters to with kip upgrade --longhorn. Moving it to a later
// minor needs a longhornPath entry for every minor in between.
const longhornVersion = "v1.11.3"

// longhornRelease is one stop on the Longhorn upgrade path: the latest patch
// of its minor, the oldest Kubernetes its release notes accept, and the
// oldest engine API versions its longhorn-manager accepts at start.
type longhornRelease struct {
	Version                string
	MinKubernetes          string
	EngineCLIAPIMin        int
	EngineControllerAPIMin int
}

// longhornPath holds one release per minor, oldest first, ending at
// longhornVersion. Longhorn upgrades one minor at a time and has no downgrade.
// The engine minimums come from longhorn-engine pkg/meta/version.go at each tag.
var longhornPath = []longhornRelease{
	{Version: "v1.7.3", MinKubernetes: "v1.21", EngineCLIAPIMin: 8, EngineControllerAPIMin: 4},
	{Version: "v1.8.2", MinKubernetes: "v1.25", EngineCLIAPIMin: 8, EngineControllerAPIMin: 4},
	{Version: "v1.9.2", MinKubernetes: "v1.25", EngineCLIAPIMin: 8, EngineControllerAPIMin: 4},
	{Version: "v1.10.2", MinKubernetes: "v1.25", EngineCLIAPIMin: 8, EngineControllerAPIMin: 4},
	{Version: "v1.11.3", MinKubernetes: "v1.34", EngineCLIAPIMin: 8, EngineControllerAPIMin: 4},
}

// LonghornTarget returns the Longhorn release this kip installs and upgrades to.
func LonghornTarget() string { return longhornVersion }

type longhornVer struct{ major, minor, patch int }

func (v longhornVer) less(o longhornVer) bool {
	if v.major != o.major {
		return v.major < o.major
	}
	if v.minor != o.minor {
		return v.minor < o.minor
	}
	return v.patch < o.patch
}

var longhornVersionPattern = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)(-hotfix-\d+)?$`)

// parseLonghornVersion reads a release tag such as v1.7.3. A hotfix suffix
// counts as its patch release. Release candidates and other suffixes are refused.
func parseLonghornVersion(s string) (longhornVer, error) {
	m := longhornVersionPattern.FindStringSubmatch(s)
	if m == nil {
		return longhornVer{}, fmt.Errorf("kip cannot read the Longhorn version %q", s)
	}
	var v longhornVer
	v.major, _ = strconv.Atoi(m[1])
	v.minor, _ = strconv.Atoi(m[2])
	v.patch, _ = strconv.Atoi(m[3])
	return v, nil
}

// longhornReleaseFor returns the path entry matching version's major and
// minor, using that entry's Kubernetes and engine minimums for every patch.
// It returns false for an invalid version or one outside the path.
func longhornReleaseFor(version string) (longhornRelease, bool) {
	v, err := parseLonghornVersion(version)
	if err != nil {
		return longhornRelease{}, false
	}
	for _, r := range longhornPath {
		if p, _ := parseLonghornVersion(r.Version); p.major == v.major && p.minor == v.minor {
			return r, true
		}
	}
	return longhornRelease{}, false
}

// LonghornPlan lists the releases kip upgrade --longhorn applies after From,
// in order. A plan returned with no hops already matches the target,
// ignoring hotfix suffixes.
type LonghornPlan struct {
	From string
	Hops []string
}

// Describe returns the plan as one line, such as "v1.7.3 → v1.8.2 → v1.9.2".
func (p LonghornPlan) Describe() string {
	return strings.Join(append([]string{p.From}, p.Hops...), " → ")
}

// PlanLonghornUpgrade returns the hops from the applied release to the target:
// the latest patch of each minor after it, and of its own minor when that is
// newer. It treats hotfix tags as their base patch release and returns no hops
// when that release matches the target. It rejects unreadable versions, other
// major versions, versions newer than the target and minors before the path.
func PlanLonghornUpgrade(applied string) (LonghornPlan, error) {
	from, err := parseLonghornVersion(applied)
	if err != nil {
		return LonghornPlan{}, err
	}
	target, _ := parseLonghornVersion(longhornVersion)
	if target.less(from) || from.major != target.major {
		if from.major < target.major {
			return LonghornPlan{}, fmt.Errorf("this cluster runs Longhorn %s, which kip cannot upgrade to %s", applied, longhornVersion)
		}
		return LonghornPlan{}, fmt.Errorf("this cluster runs Longhorn %s, newer than the %s this kip installs. Nothing was changed. Upgrade kip to manage this cluster's Longhorn", applied, longhornVersion)
	}
	first, _ := parseLonghornVersion(longhornPath[0].Version)
	if from.minor < first.minor {
		return LonghornPlan{}, fmt.Errorf("this cluster runs Longhorn %s, older than %s where kip's upgrade path starts. Nothing was changed. Upgrade Longhorn to %s first, following the Longhorn upgrade documentation", applied, longhornPath[0].Version, longhornPath[0].Version)
	}
	plan := LonghornPlan{From: applied}
	for _, r := range longhornPath {
		v, _ := parseLonghornVersion(r.Version)
		if from.less(v) {
			plan.Hops = append(plan.Hops, r.Version)
		}
	}
	return plan, nil
}
