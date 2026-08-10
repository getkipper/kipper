package cmd

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/installer"
)

// crdWrittenByAnnotation records the kip version that last applied a CRD, so a
// later run can tell whether it is about to overwrite a schema newer than
// itself. Nothing else in a CRD answers that: the version names it declares say
// what shapes exist, not which build wrote them. It is defined in the installer
// package because the install path writes it too, over kubectl rather than the
// API, and the two must agree on the key.
const crdWrittenByAnnotation = installer.CRDWrittenByAnnotation

// corev1LastAppliedConfig is kubectl's record of the object it last applied. It
// is bookkeeping for kubectl's own three-way merge rather than state anything
// else owns.
const corev1LastAppliedConfig = "kubectl.kubernetes.io/last-applied-configuration"

// carryOverMetadata copies labels and annotations the cluster holds onto the
// object about to replace it, without letting them override what the embedded
// manifest sets. Only metadata is carried: the spec is this build's to define,
// which is the whole point of applying it.
func carryOverMetadata(existing, incoming *unstructured.Unstructured) {
	if existing == nil || incoming == nil {
		return
	}
	if live := existing.GetAnnotations(); len(live) > 0 {
		merged := map[string]string{}
		for k, v := range live {
			// kubectl records the object it last applied here and diffs the
			// next apply against it. Carrying a copy forward across an Update
			// that changed the schema leaves kubectl reconciling against a
			// snapshot that never existed, and a three-way merge then deletes
			// fields the stale copy does not mention. Dropping it is what the
			// replace did before anything was carried over at all, and kubectl
			// rebuilds it on its next apply.
			if k == corev1LastAppliedConfig {
				continue
			}
			merged[k] = v
		}
		for k, v := range incoming.GetAnnotations() {
			merged[k] = v
		}
		incoming.SetAnnotations(merged)
	}
	if live := existing.GetLabels(); len(live) > 0 {
		merged := map[string]string{}
		for k, v := range live {
			merged[k] = v
		}
		for k, v := range incoming.GetLabels() {
			merged[k] = v
		}
		incoming.SetLabels(merged)
	}
	// Finalizers and owner references are lifecycle metadata a client owns, and
	// an Update that omits them removes them. Dropping a finalizer hands a
	// controller's cleanup away silently, and on a terminating object it lets
	// deletion run ahead of the cleanup it was holding open. The embedded
	// manifest sets neither, so the live values are simply kept.
	if live := existing.GetFinalizers(); len(live) > 0 && len(incoming.GetFinalizers()) == 0 {
		incoming.SetFinalizers(live)
	}
	if live := existing.GetOwnerReferences(); len(live) > 0 && len(incoming.GetOwnerReferences()) == 0 {
		incoming.SetOwnerReferences(live)
	}
}

// crdWriterVersion returns the kip version recorded on a live CRD, or empty when
// it carries no stamp — which every cluster written before stamping existed does.
func crdWriterVersion(crd *unstructured.Unstructured) string {
	if crd == nil {
		return ""
	}
	return crd.GetAnnotations()[crdWrittenByAnnotation]
}

// stampCRDWriter records the running kip version on a CRD about to be written.
//
// The annotation means "the newest kip known to have written this schema",
// which is the only thing the guard needs and the only thing it can trust. So a
// build whose version cannot be ordered — a source build, or a commit after a
// tag — must not replace a stamp that can be. Overwriting `v0.11.0` with `dev`
// would turn one permitted local apply into a permanent hole: every later run
// reads `dev`, discards it, and silently skips the check, including the older
// release the guard exists to stop.
func stampCRDWriter(crd *unstructured.Unstructured, kipVersion string) {
	kipVersion = strings.TrimSpace(kipVersion)
	if crd == nil || kipVersion == "" {
		return
	}
	annotations := crd.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	if _, ordered := installer.ComparableVersion(kipVersion); !ordered {
		if _, existingOrdered := installer.ComparableVersion(annotations[crdWrittenByAnnotation]); existingOrdered {
			return
		}
	}
	annotations[crdWrittenByAnnotation] = kipVersion
	crd.SetAnnotations(annotations)
}
