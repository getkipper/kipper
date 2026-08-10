package installer

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// The per-package prefix tests assert the references those packages actually
// ship, which is what pins the wiring. They cannot catch a first-party image
// added somewhere new, because a constant nobody lists is a constant nobody
// checks — and a GitHub organisation rename does not redirect registry package
// paths, so a single missed reference is a 403 at pull time.
//
// This walks the repository instead, so a new image is covered the moment it is
// written rather than when someone remembers to add it to a list.
//
// Both this and the workflow test below read files outside their own package,
// which `go test` does not track: after editing one of those files, a local run
// can serve a cached pass from before the edit. Use -count=1 when checking a
// change by hand. CI always runs cold, so this affects the inner loop only.
func TestNoFirstPartyImageEscapesTheKipperOrg(t *testing.T) {
	root := repoRoot(t)

	// Only Kipper's own images are in scope. Third-party images (traefik, dex,
	// zot) live under organisations this project does not publish to.
	ref := regexp.MustCompile(`ghcr\.io/[A-Za-z0-9._-]+/kipper-[A-Za-z0-9._-]+`)

	skipDirs := map[string]bool{
		".git": true, "node_modules": true, "dist": true, "vendor": true,
		// Local working material: review hand-offs, plan files and agent
		// worktrees quote historical references on purpose, and none ship.
		"tmp": true, "plans": true, ".claude": true,
	}
	scanExt := map[string]bool{
		".go": true, ".yaml": true, ".yml": true, ".json": true, ".sh": true,
		".ts": true, ".vue": true, ".md": true, ".tmpl": true, ".tpl": true,
		".conf": true, ".toml": true, ".env": true, ".txt": true, ".js": true,
	}

	// Reads go through a root-scoped handle so a symlink cannot walk the read
	// outside the repository between the walk seeing an entry and this opening
	// it.
	scope, err := os.OpenRoot(root)
	if err != nil {
		t.Fatalf("opening the repository root: %v", err)
	}
	defer func() { _ = scope.Close() }()

	checked := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		// A symlink carries no content of its own, and a broken one cannot be
		// read at all. Its target is walked separately when it lives in the repo.
		if !d.Type().IsRegular() {
			return nil
		}
		// Extension-less files that carry image references anyway. A Dockerfile
		// FROM line is exactly the kind of reference that ships and pulls.
		scanByName := map[string]bool{
			"Dockerfile": true, "Containerfile": true, "Makefile": true,
			"Caddyfile": true, "Procfile": true, "Justfile": true,
		}
		base := filepath.Base(path)
		if !scanExt[strings.ToLower(filepath.Ext(path))] && !scanByName[base] &&
			!strings.HasPrefix(base, "Dockerfile.") && !strings.HasPrefix(base, "Containerfile.") {
			return nil
		}
		// Test files are skipped because a wrong organisation is deliberate
		// there and has to be: retiredSidecarRef in kip/cmd exists precisely so
		// a repoint test compares against something this build does not ship,
		// and a tree-wide rename that quietly corrected it would make those
		// tests pass on a no-op. Nothing under a test file is pulled by a
		// cluster, so the shipping references are the ones in scope here.
		if strings.HasSuffix(path, "_test.go") || strings.HasSuffix(path, ".test.ts") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		// os.Root.Open takes slash-separated paths on every platform, while
		// filepath.Rel returns the host separator.
		f, openErr := scope.Open(filepath.ToSlash(rel))
		if openErr != nil {
			return openErr
		}
		body, readErr := io.ReadAll(f)
		_ = f.Close()
		if readErr != nil {
			return readErr
		}
		for _, got := range ref.FindAllString(string(body), -1) {
			checked++
			if !strings.HasPrefix(got, FirstPartyImagePrefix) {
				t.Errorf("%s references %q, which is not published under %s", rel, got, FirstPartyImagePrefix)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking the repository: %v", err)
	}

	// A walk that finds nothing would pass while proving nothing, which is the
	// failure mode this whole class of test exists to avoid.
	if checked == 0 {
		t.Fatal("found no first-party image references at all; this test is no longer looking where they live")
	}
	t.Logf("checked %d first-party reference(s)", checked)
}

// Every other test here checks what Kipper pulls. None checks where Kipper
// publishes, so moving the workflow's destination organisation leaves the whole
// suite green while the release lands somewhere no cluster looks. The producer
// and the consumers have to name the same organisation, and this is the only
// place the two are compared.
func TestTheImageWorkflowPublishesWhereTheCodePullsFrom(t *testing.T) {
	root := repoRoot(t)
	workflow := filepath.Join(root, ".github", "workflows", "build-images.yml")

	body, err := os.ReadFile(workflow)
	if err != nil {
		t.Fatalf("reading the image workflow: %v", err)
	}

	// Every ORG the workflow declares, not just the first. A job-level or
	// step-level env block overriding the top-level one would otherwise publish
	// elsewhere with this test green.
	orgLine := regexp.MustCompile(`(?m)^\s*ORG:\s*["']?([A-Za-z0-9._-]+)["']?\s*$`)
	matches := orgLine.FindAllSubmatch(body, -1)
	if len(matches) == 0 {
		t.Fatal("the image workflow no longer declares an ORG; this test cannot see where images publish")
	}

	wantOrg := strings.TrimSuffix(strings.TrimPrefix(FirstPartyImagePrefix, "ghcr.io/"), "/")
	for _, m := range matches {
		if publishOrg := string(m[1]); publishOrg != wantOrg {
			t.Errorf("the workflow publishes to %q but the code pulls from %q; a release would land where nothing looks for it",
				publishOrg, wantOrg)
		}
	}

	// The registry itself has to agree too, or the prefix is right and the host
	// is not.
	if !regexp.MustCompile(`(?m)^\s*REGISTRY:\s*["']?ghcr\.io["']?\s*$`).Match(body) {
		t.Error("the workflow no longer publishes to ghcr.io, which is the registry every first-party reference names")
	}

	// Agreeing on ORG proves nothing if the tags that actually publish are
	// composed from something else. Every destination must be built from those
	// two values.
	composed := regexp.MustCompile(`\$\{\{\s*env\.REGISTRY\s*\}\}/\$\{\{\s*env\.ORG\s*\}\}/`)
	if !composed.Match(body) {
		t.Error("no publish tag is composed from env.REGISTRY and env.ORG; the destination is set somewhere this test cannot see")
	}
	// A destination spelled out rather than composed sidesteps both checks
	// above, however carefully they agree. Looking for the image name is not
	// enough, because the matrix supplies it: `ghcr.io/elsewhere/${{ matrix.image
	// }}` contains no image name to find. So the registry may be written
	// literally in exactly one place, its own declaration, and nowhere else.
	registryLiteral := regexp.MustCompile(`ghcr\.io`)
	declaration := regexp.MustCompile(`(?m)^\s*REGISTRY:\s*["']?ghcr\.io["']?\s*$`)
	if got, want := len(registryLiteral.FindAll(body, -1)), len(declaration.FindAll(body, -1)); got != want {
		t.Errorf("the workflow names ghcr.io %d time(s) but declares it %d time(s); a destination written literally rather than composed from env.REGISTRY and env.ORG would publish where this test cannot see it",
			got, want)
	}
}

// repoRoot walks up from this source file until it finds the directory holding
// the repository's go.work, which is the only marker that does not move when a
// module is added.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source file")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("walked past the filesystem root without finding go.work")
		}
		dir = parent
	}
}
