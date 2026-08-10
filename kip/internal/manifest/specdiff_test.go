package manifest

import (
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func paths(changes []Change, kind ChangeKind) []string {
	var out []string
	for _, c := range changes {
		if c.Kind == kind {
			out = append(out, c.Path)
		}
	}
	return out
}

// The case this exists for. A redirect set with `kip app update` and never
// written into the manifest is removed by the next apply, and the old diff said
// only "exists, will be updated".
func TestDiffSpec_NamesTheFieldAnApplyWouldRemove(t *testing.T) {
	live := map[string]interface{}{
		"image": "shop:v1",
		"route": map[string]interface{}{
			"host":         "example.com",
			"redirectFrom": []interface{}{"www.example.com"},
		},
	}
	desired := map[string]interface{}{
		"image": "shop:v1",
		"route": map[string]interface{}{"host": "example.com"},
	}

	changes := DiffSpec(live, desired, nil, nil)
	assert.Equal(t, []string{"route.redirectFrom"}, paths(changes, Cleared))
	assert.Empty(t, paths(changes, Changed))
	assert.Empty(t, paths(changes, Added))

	cleared := Clears(changes)
	require.Len(t, cleared, 1)
	assert.Equal(t, "[www.example.com]", cleared[0].Live, "the value going away is named, not just the path")
}

// A manifest that drops a whole block should say which values go, not that
// "route" is different.
func TestDiffSpec_ReportsTheLeavesOfADroppedBlock(t *testing.T) {
	live := map[string]interface{}{
		"route": map[string]interface{}{
			"host": "example.com", "rateLimit": int64(100), "basicAuth": true,
		},
	}
	changes := DiffSpec(live, map[string]interface{}{}, nil, nil)
	assert.ElementsMatch(t,
		[]string{"route.host", "route.rateLimit", "route.basicAuth"},
		paths(changes, Cleared))
}

// A git app's built image is build output apply carries forward, so it is not
// being cleared and must not be reported as if it were.
func TestDiffSpec_PreservedPathsAreNotReportedAsCleared(t *testing.T) {
	live := map[string]interface{}{
		"image": "ghcr.io/acme/shop@sha256:abc",
		"git":   map[string]interface{}{"url": "https://git.example.com/shop"},
	}
	desired := map[string]interface{}{
		"git": map[string]interface{}{"url": "https://git.example.com/shop"},
	}

	withGuard := DiffSpec(live, desired, []string{"image"}, nil)
	assert.Empty(t, paths(withGuard, Cleared), "apply keeps the built image, so nothing is lost")

	withoutGuard := DiffSpec(live, desired, nil, nil)
	assert.Equal(t, []string{"image"}, paths(withoutGuard, Cleared),
		"and without the guard it would be, which is why the guard is passed")
}

func TestDiffSpec_AddedAndChanged(t *testing.T) {
	live := map[string]interface{}{"image": "shop:v1", "replicas": int64(2)}
	desired := map[string]interface{}{"image": "shop:v2", "replicas": int64(2), "port": int64(3000)}

	changes := DiffSpec(live, desired, nil, nil)
	assert.Equal(t, []string{"image"}, paths(changes, Changed))
	assert.Equal(t, []string{"port"}, paths(changes, Added))
	assert.Empty(t, paths(changes, Cleared))
}

// A manifest is YAML and the cluster answers JSON, so the same number arrives
// as float64 one way and int64 the other. Reporting that as a change would
// make every apply look like it rewrites every port.
func TestDiffSpec_NumbersFromYAMLAndFromTheAPIServerAgree(t *testing.T) {
	live := map[string]interface{}{"port": int64(3000), "replicas": int64(2)}
	desired := map[string]interface{}{"port": float64(3000), "replicas": float64(2)}
	assert.Empty(t, DiffSpec(live, desired, nil, nil), "3000 is 3000 whichever type carries it")
}

func TestDiffSpec_IdenticalSpecsReportNothing(t *testing.T) {
	spec := map[string]interface{}{
		"image": "shop:v1",
		"route": map[string]interface{}{"host": "example.com", "redirectFrom": []interface{}{"www.example.com"}},
	}
	other := map[string]interface{}{
		"image": "shop:v1",
		"route": map[string]interface{}{"host": "example.com", "redirectFrom": []interface{}{"www.example.com"}},
	}
	assert.Empty(t, DiffSpec(spec, other, nil, nil))
}

// A long value is worth knowing the shape of rather than the whole of.
func TestDiffSpec_LongValuesAreTruncated(t *testing.T) {
	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	changes := DiffSpec(map[string]interface{}{"image": long}, map[string]interface{}{}, nil, nil)
	require.Len(t, changes, 1)
	assert.LessOrEqual(t, len([]rune(changes[0].Live)), maxRenderedValue)
	assert.Contains(t, changes[0].Live, "…")

	// And a multi-byte value is cut between characters, not through one.
	multi := ""
	for i := 0; i < 200; i++ {
		multi += "ü"
	}
	got := DiffSpec(map[string]interface{}{"image": multi}, map[string]interface{}{}, nil, nil)
	require.Len(t, got, 1)
	assert.True(t, utf8.ValidString(got[0].Live), "truncation must not split a character")
}

// Display must not decide equality. Two values sharing a long prefix truncate
// to the same string, and comparing those reported no change while apply
// replaced the field anyway.
func TestDiffSpec_LongValuesDifferingLateAreStillAChange(t *testing.T) {
	prefix := ""
	for i := 0; i < 120; i++ {
		prefix += "a"
	}
	changes := DiffSpec(
		map[string]interface{}{"cmd": prefix + "-one"},
		map[string]interface{}{"cmd": prefix + "-two"},
		nil,
		nil,
	)
	require.Len(t, changes, 1, "they differ past the truncation point and that is still a difference")
	assert.Equal(t, Changed, changes[0].Kind)
}

// Rendering flattens, so it cannot be the equality key: these two are different
// values that read identically.
func TestDiffSpec_ValuesThatRenderAlikeAreNotEqual(t *testing.T) {
	changes := DiffSpec(
		map[string]interface{}{"args": []interface{}{"a, b"}},
		map[string]interface{}{"args": []interface{}{"a", "b"}},
		nil,
		nil,
	)
	require.Len(t, changes, 1, "one argument containing a comma is not two arguments")
	assert.Equal(t, Changed, changes[0].Kind)
}

// Nested structures compare on the whole value, not on their first 60 runes.
func TestDiffSpec_NestedSlicesOfMapsCompareWholly(t *testing.T) {
	live := map[string]interface{}{"serviceBindings": []interface{}{
		map[string]interface{}{"name": "db", "prefix": "DB_", "database": "shop_prod"},
	}}
	desired := map[string]interface{}{"serviceBindings": []interface{}{
		map[string]interface{}{"name": "db", "prefix": "DB_", "database": "shop_test"},
	}}
	require.Len(t, DiffSpec(live, desired, nil, nil), 1, "a differing nested field is a change")
	assert.Empty(t, DiffSpec(live, live, nil, nil))
}

// Admission writes a CRD default into the stored object, so a manifest that
// omits an optional field produces a live spec carrying one it never mentioned.
// Calling that a field the apply would remove made every ordinary manifest
// refuse: replicas, storage, a function's runtime, a job's backoffLimit.
func TestDiffSpec_ADefaultedFieldTheManifestOmitsIsNotAClear(t *testing.T) {
	live := map[string]interface{}{"image": "nginx", "replicas": int64(1)}
	desired := map[string]interface{}{"image": "nginx"}
	defaults := map[string]interface{}{"replicas": int64(1)}

	assert.Empty(t, DiffSpec(live, desired, nil, defaults),
		"assigning a spec without it makes admission write the same value back")
	assert.Equal(t, []string{"replicas"}, paths(DiffSpec(live, desired, nil, nil), Cleared),
		"and without the schema it reads as a clear, which is the defect")
}

// A live value that is not the default does change: it goes back to the
// default. That is worth saying, and saying accurately.
func TestDiffSpec_ANonDefaultValueTheManifestOmitsTakesTheDefault(t *testing.T) {
	live := map[string]interface{}{"image": "nginx", "replicas": int64(4)}
	desired := map[string]interface{}{"image": "nginx"}
	defaults := map[string]interface{}{"replicas": int64(1)}

	changes := DiffSpec(live, desired, nil, defaults)
	require.Len(t, changes, 1)
	assert.Equal(t, Reset, changes[0].Kind, "it is not removed, it is reset")
	require.Len(t, Clears(changes), 1, "the operator's value goes either way, so it is confirmed")
	assert.Equal(t, "replicas", changes[0].Path)
	assert.Equal(t, "4", changes[0].Live)
	assert.Equal(t, "1", changes[0].New)
}

// The YAML/API numeric split applies to defaults too: a schema default parsed
// from JSON is a float64 where the API server answers int64.
func TestDiffSpec_ADefaultMatchesWhateverTypeCarriesIt(t *testing.T) {
	live := map[string]interface{}{"replicas": int64(1)}
	desired := map[string]interface{}{}
	assert.Empty(t, DiffSpec(live, desired, nil, map[string]interface{}{"replicas": float64(1)}))
}

// Nested paths are dotted, the same as everywhere else here.
func TestDiffSpec_DefaultsApplyToNestedPaths(t *testing.T) {
	live := map[string]interface{}{"git": map[string]interface{}{"url": "https://example.com/x.git", "branch": "main"}}
	desired := map[string]interface{}{"git": map[string]interface{}{"url": "https://example.com/x.git"}}
	assert.Empty(t, DiffSpec(live, desired, nil, map[string]interface{}{"git.branch": "main"}))
}

// A default below a block the manifest drops is not applied: admission does not
// rebuild an absent parent because a child declares one, so the live value goes
// and must be confirmed.
func TestDiffSpec_ADefaultDoesNotExcuseADroppedParent(t *testing.T) {
	live := map[string]interface{}{"git": map[string]interface{}{"url": "https://example.com/x.git", "branch": "main"}}
	desired := map[string]interface{}{"image": "nginx"}
	changes := DiffSpec(live, desired, nil, map[string]interface{}{"git.branch": "main"})
	assert.Contains(t, paths(changes, Cleared), "git.branch", "the block is going, and so is everything in it")
	assert.Contains(t, paths(changes, Cleared), "git.url")
}

// A default on an object is a whole value, settled before its members are.
func TestDiffSpec_AnObjectDefaultIsComparedWhole(t *testing.T) {
	settings := map[string]interface{}{"mode": "safe"}
	live := map[string]interface{}{"settings": settings}
	defaults := map[string]interface{}{"settings": map[string]interface{}{"mode": "safe"}}
	assert.Empty(t, DiffSpec(live, map[string]interface{}{}, nil, defaults),
		"admission writes the same object straight back")

	changed := map[string]interface{}{"settings": map[string]interface{}{"mode": "loud"}}
	got := DiffSpec(changed, map[string]interface{}{}, nil, defaults)
	require.Len(t, got, 1)
	assert.Equal(t, Reset, got[0].Kind)
	require.Len(t, Clears(got), 1, "the operator's object goes, so it is confirmed")
}

// An environment value is the operator's own text and can be a credential
// outright. The path is what the warning is for; printing the value puts it in
// terminal scrollback and, from a GitOps job, in durable CI logs.
func TestDiffSpec_EnvValuesAreNamedButNotPrinted(t *testing.T) {
	//nolint:gosec // G101: credential-shaped fixtures are the subject of this test, not credentials.
	live := map[string]interface{}{"env": map[string]interface{}{
		"API_TOKEN":    "ghp_0123456789abcdefghijklmnopqrstuvwxyz",
		"DATABASE_URL": "postgresql://kipper:s3cret@db:5432/app",
		"LOG_LEVEL":    "debug",
	}}
	changes := DiffSpec(live, map[string]interface{}{}, nil, nil)
	require.Len(t, changes, 3)
	for _, c := range changes {
		assert.Equal(t, "(value hidden)", c.Live, c.Path)
	}
	assert.Equal(t, []string{"env.API_TOKEN", "env.DATABASE_URL", "env.LOG_LEVEL"}, paths(changes, Cleared),
		"the operator still needs to know which variables go")
}

// A changed env value says it changed and nothing more.
func TestDiffSpec_AChangedEnvValueShowsNeitherSide(t *testing.T) {
	live := map[string]interface{}{"env": map[string]interface{}{"API_TOKEN": "old-secret"}}
	desired := map[string]interface{}{"env": map[string]interface{}{"API_TOKEN": "new-secret"}}
	changes := DiffSpec(live, desired, nil, nil)
	require.Len(t, changes, 1)
	assert.Equal(t, Changed, changes[0].Kind)
	assert.Equal(t, "(value hidden)", changes[0].Live)
	assert.Equal(t, "(value hidden)", changes[0].New)
}

// Everything else still reads normally: hiding the image or the route would
// make the warning useless.
func TestDiffSpec_OnlyEnvValuesAreHidden(t *testing.T) {
	live := map[string]interface{}{"route": map[string]interface{}{"host": "shop.example.com"}}
	changes := DiffSpec(live, map[string]interface{}{}, nil, nil)
	require.Len(t, changes, 1)
	assert.Equal(t, "shop.example.com", changes[0].Live)
}

// A git URL can carry a token in its userinfo — the console API scrubs exactly
// this before showing an App — and the host and repository are most of why the
// line is worth printing, so only the credential goes.
func TestDiffSpec_AGitURLKeepsItsHostAndLosesItsCredential(t *testing.T) {
	//nolint:gosec // G101: a credential-shaped fixture is the subject of this test.
	live := map[string]interface{}{"git": map[string]interface{}{
		"url": "https://oauth2:glpat-0123456789abcdefghij@gitlab.example/acme/shop.git",
	}}
	changes := DiffSpec(live, map[string]interface{}{}, nil, nil)
	require.Len(t, changes, 1)
	assert.Equal(t, "git.url", changes[0].Path)
	assert.NotContains(t, changes[0].Live, "glpat-0123456789abcdefghij")
	assert.Contains(t, changes[0].Live, "***@", "and it is visible that one was there")
	assert.Contains(t, changes[0].Live, "gitlab.example/acme/shop.git", "the repository is why the line is there")
}

// A build argument is passed straight to the build and is a normal place to put
// a token, so it is named and not printed.
func TestDiffSpec_BuildArgumentsAreNamedButNotPrinted(t *testing.T) {
	live := map[string]interface{}{"git": map[string]interface{}{
		"buildArgs": map[string]interface{}{"NPM_TOKEN": "npm_0123456789abcdef"},
	}}
	changes := DiffSpec(live, map[string]interface{}{}, nil, nil)
	require.Len(t, changes, 1)
	assert.Equal(t, "git.buildArgs.NPM_TOKEN", changes[0].Path)
	assert.Equal(t, "(value hidden)", changes[0].Live)
}

// A URL with no credential in it reads exactly as it is.
func TestDiffSpec_APlainGitURLIsUntouched(t *testing.T) {
	live := map[string]interface{}{"git": map[string]interface{}{"url": "https://github.com/acme/shop.git"}}
	changes := DiffSpec(live, map[string]interface{}{}, nil, nil)
	require.Len(t, changes, 1)
	assert.Equal(t, "https://github.com/acme/shop.git", changes[0].Live)
}

// A token is a valid username on its own, which is what a personal access token
// looks like in a URL. A rule needing a colon to find a credential prints those.
func TestScrubURLCredential_RemovesUserinfoWithNoPassword(t *testing.T) {
	//nolint:gosec // G101: credential-shaped fixtures are the subject of this test.
	cases := []struct{ in, wantAbsent, wantPresent string }{
		{"https://ghp_0123456789abcdef@github.com/acme/shop.git", "ghp_0123456789abcdef", "github.com/acme/shop.git"},
		{"https://oauth2:glpat-0123456789@gitlab.example/acme/shop.git", "glpat-0123456789", "gitlab.example/acme/shop.git"},
		{"http://token@registry.example:8443/acme/shop.git", "token@", "registry.example:8443"},
	}
	for _, c := range cases {
		got := ScrubURLCredential(c.in)
		assert.NotContains(t, got, c.wantAbsent, c.in)
		assert.Contains(t, got, c.wantPresent, c.in)
	}
}

// Userinfo is one of the places a URL can carry a credential, not the only one.
// A provider that takes a token as a query parameter puts it after the path,
// where removing the userinfo finds nothing and the whole URL was printed —
// into terminal scrollback, and for a GitOps run into durable CI logs.
func TestScrubURLCredential_RemovesACredentialOutsideUserinfo(t *testing.T) {
	//nolint:gosec // G101: credential-shaped fixtures are the subject of this test.
	cases := []struct{ in, wantAbsent, wantPresent string }{
		{"https://github.example/acme/private.git?access_token=ghp_0123456789abcdef",
			"ghp_0123456789abcdef", "github.example/acme/private.git"},
		{"https://dev.azure.example/acme/_git/shop?api-version=6.0&token=glpat-0123456789",
			"glpat-0123456789", "dev.azure.example/acme/_git/shop"},
		{"https://github.example/acme/shop.git#sk-0123456789",
			"sk-0123456789", "github.example/acme/shop.git"},
		// Both at once: neither may survive the other's removal.
		{"https://ghp_0123456789abcdef@github.example/acme/shop.git?token=glpat-9876543210",
			"glpat-9876543210", "github.example/acme/shop.git"},
	}
	for _, c := range cases {
		got := ScrubURLCredential(c.in)
		assert.NotContains(t, got, c.wantAbsent, c.in)
		assert.Contains(t, got, c.wantPresent, c.in)
	}
	//nolint:gosec // G101: credential-shaped fixture.
	assert.NotContains(t, ScrubURLCredential("https://ghp_0123456789abcdef@github.example/acme/shop.git?token=glpat-9876543210"),
		"ghp_0123456789abcdef", "the userinfo still goes when a query is present too")
}

// An ssh remote's username is the convention rather than a secret, and a plain
// URL has nothing to remove.
func TestScrubURLCredential_LeavesWhatCarriesNoCredential(t *testing.T) {
	for _, in := range []string{
		"https://github.com/acme/shop.git",
		"ssh://git@github.com/acme/shop.git",
		"git@github.com:acme/shop.git",
		"https://gitlab.example:8443/acme/shop.git",
	} {
		assert.Equal(t, in, ScrubURLCredential(in))
	}
}

// A command line is the normal place a credential appears on the command line,
// a function's source is arbitrary text, and a dependency can be an
// authenticated package URL.
func TestDiffSpec_CommandSourceAndDependenciesAreNamedButNotPrinted(t *testing.T) {
	//nolint:gosec // G101: credential-shaped fixtures are the subject of this test.
	live := map[string]interface{}{
		"command": []interface{}{"curl", "-H", "Authorization: Bearer sk-0123456789"},
		"args":    []interface{}{"--password=hunter2"},
		"source": map[string]interface{}{
			"code":         "const key = 'sk-0123456789'",
			"dependencies": map[string]interface{}{"private-lib": "https://tok@npm.example/private-lib"},
		},
	}
	for _, c := range DiffSpec(live, map[string]interface{}{}, nil, nil) {
		assert.Equal(t, "(value hidden)", c.Live, c.Path)
	}
}

// The list of places a credential cannot hide is short and stops growing; the
// list of places it can hide grew every time somebody looked. So a path nobody
// has vouched for is named and not printed, including anything inside an array
// that the path scheme cannot reach.
func TestDiffSpec_APathNobodyVouchedForIsNotPrinted(t *testing.T) {
	live := map[string]interface{}{
		"somethingNew":    "whatever a later CRD adds",
		"serviceBindings": []interface{}{map[string]interface{}{"name": "db", "password": "hunter2"}},
	}
	for _, c := range DiffSpec(live, map[string]interface{}{}, nil, nil) {
		assert.Equal(t, "(value hidden)", c.Live, c.Path)
	}
}

// And the ordinary configuration a diff exists to show still reads.
func TestDiffSpec_TheOrdinaryConfigurationStillReads(t *testing.T) {
	live := map[string]interface{}{
		"image":    "nginx:1.27",
		"replicas": int64(4),
		"route":    map[string]interface{}{"host": "shop.example.com", "rateLimit": int64(100)},
	}
	shown := map[string]string{}
	for _, c := range DiffSpec(live, map[string]interface{}{}, nil, nil) {
		shown[c.Path] = c.Live
	}
	assert.Equal(t, "nginx:1.27", shown["image"])
	assert.Equal(t, "4", shown["replicas"])
	assert.Equal(t, "shop.example.com", shown["route.host"])
	assert.Equal(t, "100", shown["route.rateLimit"])
}

// A URL that will not parse is not proof that there is nothing in it.
func TestScrubURLCredential_HidesAnUnparseableURLWithUserinfo(t *testing.T) {
	assert.Equal(t, "(value hidden)", ScrubURLCredential("https://tok en:p@ss@exa mple/repo.git"))
}

// The allowlist is only useful if its keys are the paths Convert actually
// produces. Written from memory it named resources.cpu and autoscale.min, which
// exist nowhere, so a resource reduction printed as (value hidden) -> (value
// hidden) — the one moment the number matters.
func TestDiffSpec_TheAllowlistMatchesWhatConvertProduces(t *testing.T) {
	live := map[string]interface{}{
		"resources": map[string]interface{}{
			"cpuRequest": "500m", "cpuLimit": "1",
			"memoryRequest": "512Mi", "memoryLimit": "1Gi",
		},
		"autoscale": map[string]interface{}{
			"enabled": true, "minReplicas": int64(2), "maxReplicas": int64(6),
			"cpuTarget": int64(70), "memoryTarget": int64(80),
		},
		"git":   map[string]interface{}{"dockerfilePath": "docker/Dockerfile", "context": "."},
		"route": map[string]interface{}{"noInstanceHeader": true},
	}
	shown := map[string]string{}
	for _, c := range DiffSpec(live, map[string]interface{}{}, nil, nil) {
		shown[c.Path] = c.Live
	}
	for path, want := range map[string]string{
		"resources.cpuRequest": "500m", "resources.cpuLimit": "1",
		"resources.memoryRequest": "512Mi", "resources.memoryLimit": "1Gi",
		"autoscale.enabled": "true", "autoscale.minReplicas": "2",
		"autoscale.maxReplicas": "6", "autoscale.cpuTarget": "70",
		"autoscale.memoryTarget": "80",
		"git.dockerfilePath":     "docker/Dockerfile", "git.context": ".",
		"route.noInstanceHeader": "true",
	} {
		assert.Equal(t, want, shown[path], path)
	}
}

// An unguessable prefix is a documented way to protect a webhook, so the path
// is the operator's secret as often as it is their layout.
func TestDiffSpec_ARoutePathIsNotPrinted(t *testing.T) {
	live := map[string]interface{}{"route": map[string]interface{}{
		"host": "shop.example.com",
		"path": "/hooks/provider/6f9c2b1e8a",
	}}
	shown := map[string]string{}
	for _, c := range DiffSpec(live, map[string]interface{}{}, nil, nil) {
		shown[c.Path] = c.Live
	}
	assert.Equal(t, "shop.example.com", shown["route.host"], "a hostname is public")
	assert.Equal(t, "(value hidden)", shown["route.path"])
}
