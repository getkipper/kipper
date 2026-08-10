package installer

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// A fresh install writes CRDs through kubectl rather than the API, so it is a
// second writer. Leaving it unstamped left the newest clusters unprotected: an
// absent stamp has to be read as "written before stamping existed", so an older
// kip run against a brand-new cluster would have been waved straight through.
func TestStampCRDManifestRecordsTheWritingVersion(t *testing.T) {
	entries, err := crdManifests.ReadDir("crds")
	if err != nil {
		t.Fatalf("reading embedded CRDs: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no embedded CRDs; this test is reading the wrong place")
	}

	raw, err := crdManifests.ReadFile("crds/" + entries[0].Name())
	if err != nil {
		t.Fatalf("reading %s: %v", entries[0].Name(), err)
	}

	stamped, err := StampCRDManifest(raw, "v0.11.0")
	if err != nil {
		t.Fatalf("stamping: %v", err)
	}

	var obj map[string]any
	if err := yaml.Unmarshal(stamped, &obj); err != nil {
		t.Fatalf("the stamped manifest no longer parses: %v", err)
	}
	metadata, _ := obj["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	if got := annotations[CRDWrittenByAnnotation]; got != "v0.11.0" {
		t.Errorf("stamp = %v, want v0.11.0", got)
	}
	// The schema itself has to survive intact, or the install applies a
	// different CRD than the one this build ships.
	if _, ok := obj["spec"]; !ok {
		t.Error("the manifest lost its spec")
	}
	if name, _ := metadata["name"].(string); !strings.Contains(name, ".") {
		t.Errorf("the manifest lost its name, got %q", name)
	}
}

// An empty version must leave the manifest alone. Writing an empty annotation
// would read as a stamp and defeat the "absent means pre-stamping" rule.
func TestStampCRDManifestLeavesAnUnversionedBuildUnstamped(t *testing.T) {
	raw := []byte("apiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: apps.kipper.run\n")
	stamped, err := StampCRDManifest(raw, "")
	if err != nil {
		t.Fatalf("stamping: %v", err)
	}
	if !bytes.Equal(stamped, raw) {
		t.Errorf("an unversioned build must not touch the manifest, got %q", stamped)
	}
}

// recordingRunner captures the commands the install path would send.
type recordingRunner struct{ commands []string }

func (r *recordingRunner) Run(command string) (string, error) {
	r.commands = append(r.commands, command)
	return "", nil
}

func (r *recordingRunner) RunStdin(command string, _ io.Reader) (string, error) {
	return r.Run(command)
}

// The seam that matters: not whether StampCRDManifest works, but whether the
// install path uses it. Reverting the call must fail here.
func TestInstallCRDsSendsStampedManifests(t *testing.T) {
	runner := &recordingRunner{}
	if err := InstallCRDs(runner, "v0.11.0"); err != nil {
		t.Fatalf("InstallCRDs: %v", err)
	}
	applied := 0
	for _, cmd := range runner.commands {
		if !strings.Contains(cmd, "kubectl apply") {
			continue
		}
		applied++
		if !strings.Contains(cmd, CRDWrittenByAnnotation) || !strings.Contains(cmd, "v0.11.0") {
			t.Errorf("an applied CRD carries no version stamp:\n%s", firstLines(cmd, 12))
		}
	}
	if applied == 0 {
		t.Fatal("no CRDs were applied at all")
	}
}

func firstLines(s string, n int) string {
	lines := strings.SplitN(s, "\n", n+1)
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "\n")
}

// Silently applying the first of several schemas would ship a subset of the
// build's CRDs and say nothing. Every embedded file is single-document today,
// so this guards the day one is not.
func TestStampCRDManifestRefusesMultipleDocuments(t *testing.T) {
	single := []byte("---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: apps.kipper.run\n")
	if _, err := StampCRDManifest(single, "v0.11.0"); err != nil {
		t.Fatalf("a leading separator is not a second document: %v", err)
	}

	multi := append(append([]byte{}, single...),
		[]byte("---\napiVersion: apiextensions.k8s.io/v1\nkind: CustomResourceDefinition\nmetadata:\n  name: jobs.kipper.run\n")...)
	if _, err := StampCRDManifest(multi, "v0.11.0"); err == nil {
		t.Error("a multi-document manifest must be refused, not truncated to its first document")
	}
}

// Every embedded CRD must survive the round trip, not just the first one.
func TestStampCRDManifestHandlesEveryEmbeddedCRD(t *testing.T) {
	entries, err := crdManifests.ReadDir("crds")
	if err != nil {
		t.Fatalf("reading embedded CRDs: %v", err)
	}
	for _, entry := range entries {
		raw, readErr := crdManifests.ReadFile("crds/" + entry.Name())
		if readErr != nil {
			t.Fatalf("reading %s: %v", entry.Name(), readErr)
		}
		stamped, stampErr := StampCRDManifest(raw, "v0.11.0")
		if stampErr != nil {
			t.Errorf("%s: %v", entry.Name(), stampErr)
			continue
		}
		if len(stamped) == 0 || stamped[len(stamped)-1] != '\n' {
			t.Errorf("%s: stamped manifest must end with a newline, or the heredoc that applies it never terminates", entry.Name())
		}
		var obj map[string]any
		if err := yaml.Unmarshal(stamped, &obj); err != nil {
			t.Errorf("%s: no longer parses after stamping: %v", entry.Name(), err)
		}
		if _, ok := obj["spec"]; !ok {
			t.Errorf("%s: lost its spec", entry.Name())
		}
	}
}

// runnerScript answers each command from a table, so a test can stand in for
// the cluster's replies as well as record what was sent.
type runnerScript struct {
	reply    func(command string) string
	commands []string
}

func (r *runnerScript) Run(command string) (string, error) {
	r.commands = append(r.commands, command)
	if r.reply == nil {
		return "", nil
	}
	return r.reply(command), nil
}

func (r *runnerScript) RunStdin(command string, _ io.Reader) (string, error) {
	return r.Run(command)
}

// `kip install` is supported against an existing cluster, so it is a downgrade
// path too. It used to stamp without ever reading what was already there, which
// let an older kip replace a newer schema and move the stamp backwards.
func TestInstallCRDsRefusesToDowngradeAClusterWrittenByANewerKip(t *testing.T) {
	runner := &runnerScript{reply: func(cmd string) string {
		if strings.Contains(cmd, "kubectl get crd") {
			return "v0.11.0"
		}
		return ""
	}}

	err := InstallCRDs(runner, "v0.9.0")
	if err == nil {
		t.Fatal("an older kip must not re-install over a newer cluster's schemas")
	}
	if !strings.Contains(err.Error(), "v0.11.0") || !strings.Contains(err.Error(), "Upgrade kip first") {
		t.Errorf("the refusal must name the cluster's version and the remedy, got: %v", err)
	}
	for _, cmd := range runner.commands {
		if strings.Contains(cmd, "kubectl apply") {
			t.Error("refusing has to mean nothing was applied")
		}
	}
}

// A source build may re-install, but it must not move an orderable stamp
// backwards to a value every later run discards.
func TestInstallCRDsKeepsAnOrderableStampWhenTheBuildIsNot(t *testing.T) {
	runner := &runnerScript{reply: func(cmd string) string {
		if strings.Contains(cmd, "kubectl get crd") {
			return "v0.11.0"
		}
		return ""
	}}

	if err := InstallCRDs(runner, "dev"); err != nil {
		t.Fatalf("a source build must still be able to install: %v", err)
	}
	applied := 0
	for _, cmd := range runner.commands {
		if !strings.Contains(cmd, "kubectl apply") {
			continue
		}
		applied++
		if !strings.Contains(cmd, "v0.11.0") {
			t.Errorf("the orderable stamp must survive, got:\n%s", firstLines(cmd, 14))
		}
		if strings.Contains(cmd, CRDWrittenByAnnotation+": dev") {
			t.Error("an unorderable version must not become the recorded writer")
		}
	}
	if applied == 0 {
		t.Fatal("nothing was applied at all")
	}
}

// A cluster that has never been stamped is the ordinary case, and must install.
func TestInstallCRDsStampsAnUnstampedCluster(t *testing.T) {
	runner := &runnerScript{}
	if err := InstallCRDs(runner, "v0.11.0"); err != nil {
		t.Fatalf("InstallCRDs: %v", err)
	}
	for _, cmd := range runner.commands {
		if strings.Contains(cmd, "kubectl apply") && !strings.Contains(cmd, "v0.11.0") {
			t.Errorf("an unstamped cluster must come out stamped:\n%s", firstLines(cmd, 14))
		}
	}
}

// A CRD description can contain anything, including a line that looks like a
// document separator. A line-based count refused such a manifest as
// multi-document and would have broken install outright.
func TestDocumentCountIgnoresSeparatorsInsideText(t *testing.T) {
	manifest := []byte(`---
apiVersion: apiextensions.k8s.io/v1
kind: CustomResourceDefinition
metadata:
  name: apps.kipper.run
spec:
  versions:
    - name: v1alpha1
      schema:
        openAPIV3Schema:
          description: |
            Usage:
            ---
            some example
          type: object
`)
	n, err := documentCount(manifest)
	if err != nil {
		t.Fatalf("documentCount: %v", err)
	}
	if n != 1 {
		t.Errorf("a separator inside description text is not a document boundary, got %d", n)
	}
	if _, err := StampCRDManifest(manifest, "v0.11.0"); err != nil {
		t.Errorf("such a manifest must still stamp: %v", err)
	}
}

// A refusal has to mean nothing happened. Checking and applying in one pass let
// a refusal on a later CRD land after earlier ones had already been replaced
// with older schemas, leaving the cluster half-downgraded.
func TestInstallCRDsWritesNothingWhenALaterCRDWouldBeDowngraded(t *testing.T) {
	// Only one CRD reports a newer writer, and it is not the first one looked
	// at. Everything before it must still be unwritten when the refusal lands.
	runner := &runnerScript{reply: func(cmd string) string {
		if strings.Contains(cmd, "kubectl get crd") && strings.Contains(cmd, "volumes.kipper.run") {
			return "v0.11.0"
		}
		return ""
	}}

	if err := InstallCRDs(runner, "v0.9.0"); err == nil {
		t.Fatal("a newer schema on any CRD must refuse the whole install")
	}
	for _, cmd := range runner.commands {
		if strings.Contains(cmd, "kubectl apply") {
			t.Errorf("no CRD may be applied before the refusal, but one was:\n%s", firstLines(cmd, 6))
		}
	}
}

// A cluster that cannot be asked is not a cluster with no stamp. Reading the
// failure as "unstamped" waves the apply through on exactly the conditions the
// guard should refuse under.
func TestInstallCRDsRefusesWhenTheClusterCannotBeAsked(t *testing.T) {
	runner := &failingLookupRunner{}
	err := InstallCRDs(runner, "v0.9.0")
	if err == nil {
		t.Fatal("a failed lookup must not be treated as an unstamped cluster")
	}
	if !strings.Contains(err.Error(), "which kip wrote") {
		t.Errorf("the error must name what could not be established, got: %v", err)
	}
	if runner.applied {
		t.Error("nothing may be applied when the cluster could not be asked")
	}
}

type failingLookupRunner struct{ applied bool }

func (r *failingLookupRunner) Run(command string) (string, error) {
	if strings.Contains(command, "kubectl apply") {
		r.applied = true
		return "", nil
	}
	return "", errors.New("dial tcp: i/o timeout")
}

func (r *failingLookupRunner) RunStdin(command string, _ io.Reader) (string, error) {
	return r.Run(command)
}

// The first pass proves nothing was newer when it looked; it does not prove the
// cluster stayed that way. An upgrade landing while the install is still
// planning would otherwise be overwritten by a schema already judged safe.
func TestInstallCRDsRefusesAnUpgradeThatLandsMidInstall(t *testing.T) {
	// Every lookup answers "unstamped" until the applies begin, then the cluster
	// reports a newer writer — an upgrade that landed in between.
	upgraded := false
	runner := &runnerScript{}
	runner.reply = func(cmd string) string {
		if strings.Contains(cmd, "kubectl apply") {
			upgraded = true
			return ""
		}
		if strings.Contains(cmd, "kubectl get crd") && upgraded {
			return "v0.11.0"
		}
		return ""
	}

	err := InstallCRDs(runner, "v0.9.0")
	if err == nil {
		t.Fatal("a newer schema appearing mid-install must stop the remaining writes")
	}
	if !strings.Contains(err.Error(), "while this install was running") {
		t.Errorf("the error must say the cluster changed under it, got: %v", err)
	}

	applies := 0
	for _, cmd := range runner.commands {
		if strings.Contains(cmd, "kubectl apply") {
			applies++
		}
	}
	if applies > 1 {
		t.Errorf("the install must stop at the first CRD it can no longer vouch for, applied %d", applies)
	}
}
