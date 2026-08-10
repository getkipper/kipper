package installer

import (
	"bytes"
	"embed"
	"errors"
	"fmt"
	"io"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

//go:embed crds/*.yaml
var crdManifests embed.FS

// CRDWrittenByAnnotation records the kip version that last wrote a CRD, so a
// later run can refuse to replace a newer schema with an older one. Both writers
// set it: `kip upgrade` through the API, and this install path through kubectl.
// A fresh install that skipped it would leave the newest clusters unprotected,
// since an absent stamp has to be read as "written before stamping existed".
const CRDWrittenByAnnotation = "kipper.run/written-by-kip-version"

// InstallCRDs registers the Kipper Custom Resource Definitions in the cluster,
// recording kipVersion as the version that wrote them.
//
// It takes the runner interface rather than *ssh.Client so a test can read the
// command this actually sends. Asserting on StampCRDManifest alone proves the
// helper works and says nothing about whether the install path calls it, which
// is the half that was missing.
func InstallCRDs(client commandRunner, kipVersion string) error {
	entries, err := crdManifests.ReadDir("crds")
	if err != nil {
		return fmt.Errorf("reading embedded CRD manifests: %w", err)
	}

	// Two passes, matching the upgrade writer. Checking and applying in one
	// pass means a refusal on the fifth CRD leaves the first four already
	// replaced with older schemas, so "refused" would not mean "nothing
	// happened" — which is the only thing that makes a refusal safe.
	type plannedCRD struct {
		name     string
		manifest []byte
	}
	var plan []plannedCRD

	for _, entry := range entries {
		data, readErr := crdManifests.ReadFile("crds/" + entry.Name())
		if readErr != nil {
			return fmt.Errorf("reading CRD %s: %w", entry.Name(), readErr)
		}

		name, nameErr := crdNameFromManifest(data)
		if nameErr != nil {
			return fmt.Errorf("reading CRD %s: %w", entry.Name(), nameErr)
		}

		// `kip install` is supported against an existing cluster, so this is a
		// downgrade path as much as the upgrade command is, and kubectl apply
		// would replace a newer schema without anything noticing.
		live, liveErr := liveCRDWriterVersion(client, name)
		if liveErr != nil {
			return fmt.Errorf("reading the recorded writer of CRD %s: %w", name, liveErr)
		}
		if cluster, mine, newer := ClusterIsNewerThan(live, kipVersion); newer {
			return fmt.Errorf("refusing to apply CRD %s: the cluster's schema was written by kip %s and this is kip %s, "+
				"so applying it would replace a newer schema with an older one. Upgrade kip first", name, cluster, mine)
		}

		// An unorderable build must not move an orderable stamp backwards; the
		// annotation means the newest kip known to have written the schema, and
		// replacing it with "dev" would disable the check for every later run.
		stampWith := kipVersion
		if _, ordered := ComparableVersion(kipVersion); !ordered {
			if _, liveOrdered := ComparableVersion(live); liveOrdered {
				stampWith = live
				// The upgrade path says the same thing. Silence here would read
				// as a guard that ran and passed, when it could not run at all.
				fmt.Printf("  !   %s was written by kip %s; this build reports %q, which cannot be ordered against it, so the schema-age check was skipped.\n",
					name, live, kipVersion)
			}
		}

		stamped, stampErr := StampCRDManifest(data, stampWith)
		if stampErr != nil {
			return fmt.Errorf("stamping CRD %s: %w", entry.Name(), stampErr)
		}
		plan = append(plan, plannedCRD{name: name, manifest: stamped})
	}

	for _, p := range plan {
		// The first pass established the age of every CRD before writing any of
		// them, which is what makes a refusal safe. It does not make the
		// observation current: another operator can upgrade this cluster while
		// the first pass is still running, and kubectl apply carries no
		// precondition tying the write to what was checked. Re-reading here
		// narrows the window from the whole first pass to a single command.
		//
		// It does not close it. A concurrent upgrade landing between this read
		// and the apply below is still overwritten, silently. Closing that needs
		// a write conditioned on the observed object, which kubectl apply cannot
		// express — `kip upgrade` gets it from carrying resourceVersion into
		// Update. Two operators writing schemas to one cluster at the same time
		// is the situation this cannot make safe.
		live, liveErr := liveCRDWriterVersion(client, p.name)
		if liveErr != nil {
			return fmt.Errorf("re-checking the recorded writer of CRD %s: %w", p.name, liveErr)
		}
		if cluster, mine, newer := ClusterIsNewerThan(live, kipVersion); newer {
			return fmt.Errorf("refusing to apply CRD %s: the cluster's schema was written by kip %s while this install was running, and this is kip %s. Upgrade kip first",
				p.name, cluster, mine)
		}

		cmd := fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", string(p.manifest))
		if _, applyErr := client.Run(cmd); applyErr != nil {
			return fmt.Errorf("applying CRD %s: %w", p.name, applyErr)
		}
	}

	return nil
}

// StampCRDManifest returns the CRD manifest with the writing kip version
// recorded on it. An empty version leaves the manifest untouched rather than
// writing an empty annotation, which would read as a stamp and defeat the
// "absent means pre-stamping" rule the upgrade guard relies on.
func StampCRDManifest(manifest []byte, kipVersion string) ([]byte, error) {
	if kipVersion == "" {
		return manifest, nil
	}

	// Every embedded CRD is one document, and this only ever reads one. A file
	// with more would be re-marshalled down to its first, applying a subset of
	// the schemas the build ships without saying so — so refuse instead. The
	// leading "---" controller-gen emits is a separator, not a second document.
	docs, docErr := documentCount(manifest)
	if docErr != nil {
		return nil, docErr
	}
	if docs > 1 {
		return nil, fmt.Errorf("manifest holds %d YAML documents, and this writer would truncate it to the first", docs)
	}

	var obj map[string]any
	if err := yaml.Unmarshal(manifest, &obj); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	if obj == nil {
		return manifest, nil
	}

	metadata, _ := obj["metadata"].(map[string]any)
	if metadata == nil {
		metadata = map[string]any{}
		obj["metadata"] = metadata
	}
	annotations, _ := metadata["annotations"].(map[string]any)
	if annotations == nil {
		annotations = map[string]any{}
		metadata["annotations"] = annotations
	}
	annotations[CRDWrittenByAnnotation] = kipVersion

	out, err := yaml.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("rendering manifest: %w", err)
	}
	return out, nil
}

// documentCount reports how many YAML documents a manifest holds, using the
// YAML parser rather than scanning for separator lines. A line-based scan would
// count a "---" appearing inside a CRD's own description text, and refuse a
// perfectly ordinary single-document manifest as if it held several.
func documentCount(manifest []byte) (int, error) {
	decoder := yamlv3.NewDecoder(bytes.NewReader(manifest))
	count := 0
	for {
		var doc any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return 0, fmt.Errorf("parsing manifest: %w", err)
		}
		// A separator with nothing after it decodes as an empty document and is
		// not a schema, which is what controller-gen's leading "---" produces.
		if doc == nil {
			continue
		}
		count++
	}
	return count, nil
}

// crdNameFromManifest reads the metadata.name a CRD manifest declares, which is
// how the live copy is looked up before it is replaced.
func crdNameFromManifest(manifest []byte) (string, error) {
	var obj struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if err := yaml.Unmarshal(manifest, &obj); err != nil {
		return "", fmt.Errorf("parsing manifest: %w", err)
	}
	if obj.Metadata.Name == "" {
		return "", fmt.Errorf("manifest declares no metadata.name")
	}
	return obj.Metadata.Name, nil
}

// liveCRDWriterVersion returns the kip version recorded on the cluster's copy of
// a CRD, or empty when the CRD is absent or carries no stamp. A cluster that has
// never seen this annotation is the ordinary case on a fresh install and on
// every cluster written before stamping existed.
func liveCRDWriterVersion(client commandRunner, name string) (string, error) {
	// The jsonpath yields nothing rather than failing when the annotation is
	// absent, so an unstamped CRD and a missing one both read as empty. The
	// error is reserved for a cluster that could not be asked.
	//
	// The key's dots are escaped and its slash is left bare. This is the form
	// kubectl's parser accepts, verified against a live cluster rather than
	// assumed: reading controller-gen.kubebuilder.io/version this way returns
	// the value, while the bracket-quoted spelling
	// {.metadata.annotations["controller-gen\.kubebuilder\.io/version"]} is
	// rejected with `invalid array index`. A test with a fake runner cannot
	// tell these apart, so changing this line needs a real kubectl to confirm.
	// --ignore-not-found makes an absent CRD an empty success, so the only
	// remaining non-zero exit is a cluster that could not be asked: a timeout, a
	// transport reset, an authentication or authorization failure. Swallowing
	// those with `|| true` made every one of them read as "unstamped", which is
	// the one answer that waves the apply through — a safety gate that fails
	// open on exactly the conditions it should refuse under.
	// stderr is discarded but the exit status is not. The runner returns
	// combined output, so a successful kubectl that also emits an API warning
	// would otherwise hand back "Warning: ...\nv0.11.0" — which parses as no
	// version at all, and an unparseable stamp reads as an unstamped cluster.
	// That is the same fail-open this redirect used to cause with `|| true`,
	// reached from the opposite direction, and no fake runner can show it
	// because a fake returns one clean stream.
	cmd := fmt.Sprintf(
		"kubectl get crd %s --ignore-not-found -o jsonpath='{.metadata.annotations.%s}' 2>/dev/null",
		name, strings.ReplaceAll(CRDWrittenByAnnotation, ".", `\.`))
	out, err := client.Run(cmd)
	if err != nil {
		return "", fmt.Errorf("asking the cluster which kip wrote %s: %w", name, err)
	}
	return strings.TrimSpace(strings.Trim(out, "'")), nil
}
