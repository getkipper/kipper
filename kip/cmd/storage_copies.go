package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/service"
)

// unknownReplicaNodes is the replica node count when Longhorn's nodes could
// not be read, for example with a project-scoped kubeconfig.
const unknownReplicaNodes = -1

// copiesCluster is what kip knows about a cluster when a new object asks for
// copies. offered is 0 when the PlatformConfig could not be read.
type copiesCluster struct {
	mode         string
	offered      int32
	replicaNodes int
	backup       string
}

// readCopiesCluster reads the storage mode, copies offer, backup destination
// and usable replica nodes. A failed PlatformConfig read defaults to Longhorn
// and leaves the offer and backup unknown. A failed node read leaves the count
// unknown so admission and the controller can enforce the creation checks.
func readCopiesCluster(ctx context.Context, dyn dynamic.Interface) copiesCluster {
	c := copiesCluster{mode: platform.StorageLonghorn, replicaNodes: unknownReplicaNodes}
	if pc, err := dyn.Resource(platformConfigGVR).Get(ctx, "platform", metav1.GetOptions{}); err == nil {
		storage, _, _ := unstructured.NestedString(pc.Object, "spec", "storage")
		c.mode = platform.StorageMode(storage)
		c.backup, _, _ = unstructured.NestedString(pc.Object, "spec", "backup")
		c.offered = platform.OfferedCopies(maxCopiesOf(pc))
	}
	if c.mode == platform.StorageLonghorn {
		if n, err := (&service.Manager{Dynamic: dyn}).ReplicaNodes(ctx); err == nil {
			c.replicaNodes = n
		}
	}
	return c
}

func maxCopiesOf(pc *unstructured.Unstructured) *int32 {
	v, found, _ := unstructured.NestedInt64(pc.Object, "spec", "maxCopies")
	if !found {
		return nil
	}
	n := int32(v) //nolint:gosec // G115: the CRD bounds maxCopies to 1 or 2.
	return &n
}

// copiesFlagHelp describes --copies on the commands that create storage.
const copiesFlagHelp = "copies of the data, each on a different node: 1 or 2. Choosing 2 at creation avoids the cost of converting a live volume later"

// refuseCopiesOutOfRange refuses a --copies value outside 1 and 2.
func refuseCopiesOutOfRange(copies int32) error {
	if copies < 1 || copies > platform.MaxCopies {
		return fmt.Errorf("--copies must be 1 or 2, got %d", copies)
	}
	return nil
}

// refuseCopiesOnCreate returns why a new object cannot have copies on this
// cluster, or nil. shared is true for a shared volume. Unknown facts never
// refuse: the controller and the admission policy check them again.
func refuseCopiesOnCreate(copies int32, shared bool, c copiesCluster) error {
	if err := refuseCopiesOutOfRange(copies); err != nil {
		return err
	}
	if copies == 1 {
		return nil
	}
	if c.mode == platform.StorageLocal {
		return fmt.Errorf("two copies need Longhorn storage, and this cluster uses local storage. Use --copies 1")
	}
	if c.offered == 1 {
		return fmt.Errorf("this cluster offers one copy of each volume. An admin can offer two with 'kip cluster storage --max-copies 2'")
	}
	if shared && !platform.RWXCopiesQualified {
		return fmt.Errorf("shared volumes cannot have two copies in this Kipper release. Use --copies 1")
	}
	if c.replicaNodes != unknownReplicaNodes && c.replicaNodes < 2 {
		return fmt.Errorf("two copies need two ready nodes that can hold copies, and the cluster has %d. An admin adds one with 'kip node storage <node> --copies'", c.replicaNodes)
	}
	return nil
}

// warnAboutCopies warns about in-cluster backups when two copies are requested.
func warnAboutCopies(out io.Writer, copies int32, c copiesCluster) {
	if copies < 2 || c.backup != platform.BackupInCluster {
		return
	}
	say(out, "  ⚠  Backups on this cluster are stored inside the cluster. A second copy survives a lost node, and only an external backup destination survives a lost cluster.\n")
}

// checkCopiesOnCreate checks a new object's copies against the available cluster
// information and returns that information even on refusal. An unreadable offer
// is returned as zero so callers can consult admission. After a successful check,
// it warns if two copies are requested with in-cluster backups.
func checkCopiesOnCreate(ctx context.Context, out io.Writer, dyn dynamic.Interface, copies int32, shared bool) (copiesCluster, error) {
	c := readCopiesCluster(ctx, dyn)
	if err := refuseCopiesOnCreate(copies, shared, c); err != nil {
		return c, err
	}
	warnAboutCopies(out, copies, c)
	return c, nil
}

// adoptCopies writes copies on an existing object whose copies are unset,
// when want equals the copies its volume has now. It changes no volume:
// any other value needs live conversion and is refused. It reports whether
// it wrote anything.
func adoptCopies(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, namespace, name string, want int32) (bool, error) {
	if err := refuseCopiesOutOfRange(want); err != nil {
		return false, err
	}
	obj, err := dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", name, err)
	}
	unchanged, err := checkCopiesAdoption(obj, int64(want))
	if unchanged || err != nil {
		return false, err
	}
	patch := []byte(fmt.Sprintf(`{"spec":{"copies":%d}}`, want))
	if _, err := dyn.Resource(gvr).Namespace(namespace).Patch(ctx, name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return false, fmt.Errorf("setting copies on %s: %w", name, err)
	}
	return true, nil
}

// checkCopiesAdoption returns unchanged when want matches spec.copies.
// For unset copies, it permits adoption only when want matches the known live
// count. It returns an error for an unknown live count or a change requiring
// live conversion.
func checkCopiesAdoption(obj *unstructured.Unstructured, want int64) (unchanged bool, err error) {
	name := obj.GetName()
	current, set, _ := unstructured.NestedInt64(obj.Object, "spec", "copies")
	if set && current == want {
		return true, nil
	}
	live, known, _ := unstructured.NestedInt64(obj.Object, "status", "storage", "copies")
	switch {
	case set || (known && live != want):
		have := live
		if set {
			have = current
		}
		return false, fmt.Errorf("%s has %d %s. Changing the copies of an existing volume arrives with live conversion in a later Kipper release", name, have, pluralCopies(have))
	case !known:
		return false, fmt.Errorf("the copies %s has now are not known yet. Try again once its volume is provisioned", name)
	}
	return false, nil
}

func pluralCopies(n int64) string {
	if n == 1 {
		return "copy"
	}
	return "copies"
}

func sayCopiesAdopted(out io.Writer, changed bool, copies int32) {
	if changed {
		say(out, "  ✔  Copies set to %d. Kipper now keeps %d %s of the data.\n", copies, copies, pluralCopies(int64(copies)))
		return
	}
	say(out, "  ✔  Copies are already %d\n", copies)
}

// writeCopies prints the copies line of an info view after label, with the
// controller's explanation under it when the copies are not as wanted.
func writeCopies(out io.Writer, c service.Copies, label string) {
	summary := c.Summary()
	if summary == "" {
		return
	}
	say(out, "%s%s\n", label, summary)
	if problem := c.Problem(); problem != "" {
		say(out, "%s%s\n", strings.Repeat(" ", len(label)), problem)
	}
}

// recordedCopiesOffer reads the admin's copies offer from the PlatformConfig.
// A missing or unreadable PlatformConfig offers one copy.
func recordedCopiesOffer(ctx context.Context, dyn dynamic.Interface) int32 {
	pc, err := dyn.Resource(platformConfigGVR).Get(ctx, "platform", metav1.GetOptions{})
	if err != nil {
		return 1
	}
	return platform.OfferedCopies(maxCopiesOf(pc))
}
