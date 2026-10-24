package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

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

// copiesUpdateHelp describes --copies on the commands that change it.
const copiesUpdateHelp = "copies of the data, each on a different node: 1 or 2. Kipper converts the live volume one step at a time. A reduction keeps the server's copy and stops the service until the extra copy is gone"

// sharedCopiesCreateHelp describes --copies when a shared volume is created.
const sharedCopiesCreateHelp = "copies of the data: 1 is the only count supported for shared storage in this Kipper release"

// sharedCopiesHelp describes --copies on the commands that change the copies
// of shared volumes and project shared storage, which keep one copy in this
// release.
const sharedCopiesHelp = sharedCopiesCreateHelp + ". Kipper reduces a live volume with more copies one step at a time once no app mounts it, and keeps the server's copy. Use 2 to stop a blocked or pending reduction and keep the copies the volume has. On a cluster that offers one copy, this requires an admin"

// refuseCopiesOutOfRange refuses a --copies value outside 1 and 2.
func refuseCopiesOutOfRange(copies int32) error {
	if copies < 1 || copies > platform.MaxCopies {
		return fmt.Errorf("--copies must be 1 or 2, got %d", copies)
	}
	return nil
}

var errSharedCopies = errors.New("shared volumes cannot have two copies in this Kipper release. Use --copies 1")

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
		return errSharedCopies
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

// Polling limits allow a short wait for the first controller response or a
// longer wait for healthy copies when --wait is set.
var (
	copiesVerdictPoll  = 2 * time.Second
	copiesVerdictLimit = 30 * time.Second
	copiesWaitPoll     = 10 * time.Second
	copiesWaitLimit    = 3 * time.Hour
)

// copiesChange is a change of copies on an existing Service, Volume or
// Project. A Project is cluster-scoped, so its namespace is empty, and its
// copies are under field.
type copiesChange struct {
	gvr             schema.GroupVersionResource
	namespace, name string
	field           []string
	want            int32
	shared          bool
	wait            bool
	infoCommand     string
}

// specCopies is where a Service or Volume holds its copies.
var specCopies = []string{"spec", "copies"}

func (ch copiesChange) copiesField() []string {
	if ch.field == nil {
		return specCopies
	}
	return ch.field
}

// copiesPatch is a merge patch setting want at field.
func copiesPatch(field []string, want int32) ([]byte, error) {
	var value interface{} = want
	for i := len(field) - 1; i >= 0; i-- {
		value = map[string]interface{}{field[i]: value}
	}
	return json.Marshal(value)
}

// setCopies writes copies on an existing object and prints the controller's
// response. A change to two copies runs the creation checks unless the
// volume already has two or more, and warns about in-cluster backups. Blocked
// conversions return an error. Running conversions refuse a different count
// but allow waiting for the current target.
func setCopies(ctx context.Context, out io.Writer, dyn dynamic.Interface, ch copiesChange) error {
	if err := refuseCopiesOutOfRange(ch.want); err != nil {
		return err
	}
	res := dyn.Resource(ch.gvr).Namespace(ch.namespace)
	obj, err := res.Get(ctx, ch.name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading %s: %w", ch.name, err)
	}
	var cluster *copiesCluster
	if ch.want == 2 {
		c := readCopiesCluster(ctx, dyn)
		cluster = &c
	}
	unchanged, err := checkCopiesChange(obj, ch.copiesField(), int64(ch.want), cluster, ch.shared)
	if err != nil {
		return err
	}
	generation := obj.GetGeneration()
	if unchanged {
		say(out, "  ✔  Copies are already %d\n", ch.want)
	} else {
		if cluster != nil {
			warnAboutCopies(out, ch.want, *cluster)
		}
		patch, err := copiesPatch(ch.copiesField(), ch.want)
		if err != nil {
			return err
		}
		written, err := res.Patch(ctx, ch.name, types.MergePatchType, patch, metav1.PatchOptions{})
		if err != nil {
			return fmt.Errorf("setting copies on %s: %w", ch.name, err)
		}
		generation = written.GetGeneration()
		say(out, "  ✔  Copies set to %d\n", ch.want)
		if notice := ch.stopNotice(obj); notice != "" {
			say(out, "     %s\n", notice)
		}
	}
	return awaitCopies(ctx, out, dyn, ch, obj.GetUID(), generation)
}

// stopNotice explains that a shared reduction waits for apps to unmount, or
// that canceling a stopped service's reduction restarts it. It returns an
// empty string for other changes, leaving the controller to explain whether
// the service stops. The live count determines whether a copy is removed,
// falling back to the configured count when live status is absent. While RWX
// copies are unqualified, requesting two shared copies only cancels a reduction
// and preserves the copies present.
func (ch copiesChange) stopNotice(obj *unstructured.Unstructured) string {
	want := int64(ch.want)
	current, set, _ := unstructured.NestedInt64(obj.Object, ch.copiesField()...)
	if c := copiesCondition(obj); !ch.shared && set && current < want && (c.reason == "Blocked" || c.reason == "ConversionPending") {
		if !strings.Contains(c.message, "is stopped for the copy change") {
			return ""
		}
		return fmt.Sprintf("This sets the waiting copy change back, and Kipper starts %s again once the change has ended.", ch.name)
	}
	have, known, _ := unstructured.NestedInt64(obj.Object, "status", "storage", "copies")
	if !known {
		have, known, _ = unstructured.NestedInt64(obj.Object, ch.copiesField()...)
	}
	if !known || have <= want || !ch.shared || (want >= 2 && !platform.RWXCopiesQualified) {
		return ""
	}
	return "Longhorn removes the extra copy only while Kipper holds the volume with nothing writing to it, so the change waits until no app mounts the volume. 'kip app stop <app>' stops an app that does."
}

// checkCopiesChange returns unchanged when want matches the copies at field. A change
// is refused while the object's copies are being converted, and a change to
// two copies is refused for shared volumes until RWX copies are qualified,
// unless it stops a blocked or pending reduction to one. console-api then
// decides what the volume keeps, since the reported live count is Longhorn's,
// which the held reduction lowers before it removes a copy.
// Other changes to two copies run the cluster checks unless the live volume
// already has two copies or more, or cluster is nil.
func checkCopiesChange(obj *unstructured.Unstructured, field []string, want int64, cluster *copiesCluster, shared bool) (unchanged bool, err error) {
	current, set, _ := unstructured.NestedInt64(obj.Object, field...)
	if set && current == want {
		return true, nil
	}
	c := copiesCondition(obj)
	if c.reason == "Converting" {
		return false, fmt.Errorf("the copies of %s are being converted, and can change again once that ends: %s", obj.GetName(), c.message)
	}
	live, known, _ := unstructured.NestedInt64(obj.Object, "status", "storage", "copies")
	if want == 2 && shared && !platform.RWXCopiesQualified {
		if !set || current != 1 || (c.reason != "Blocked" && c.reason != "ConversionPending") {
			return false, errSharedCopies
		}
		return false, nil
	}
	if want == 2 && cluster != nil && (!known || live < 2) {
		if err := refuseCopiesOnCreate(int32(want), shared, *cluster); err != nil {
			return false, err
		}
	}
	return false, nil
}

type storageCopiesCondition struct {
	status, reason, message string
	observedGeneration      int64
}

func copiesSetting(copies int64, set bool) string {
	if !set {
		return "unset"
	}
	return strconv.FormatInt(copies, 10)
}

func copiesCondition(obj *unstructured.Unstructured) storageCopiesCondition {
	conditions, _, _ := unstructured.NestedSlice(obj.Object, "status", "conditions")
	for _, raw := range conditions {
		c, _ := raw.(map[string]interface{})
		if c["type"] != "StorageCopies" {
			continue
		}
		status, _ := c["status"].(string)
		reason, _ := c["reason"].(string)
		message, _ := c["message"].(string)
		generation, _, _ := unstructured.NestedInt64(c, "observedGeneration")
		return storageCopiesCondition{status: status, reason: reason, message: message, observedGeneration: generation}
	}
	return storageCopiesCondition{}
}

// awaitCopies prints StorageCopies updates that have observed generation.
// Without wait it returns after the first update or copiesVerdictLimit. With
// wait it follows the conversion until copies are healthy or copiesWaitLimit
// expires. Blocked or RWXNotQualified conditions, read failures, cancellation
// and the wait timeout return errors. A changed UID or copy setting different
// from ch.want returns an error because the verdict would describe a different
// object or request.
func awaitCopies(ctx context.Context, out io.Writer, dyn dynamic.Interface, ch copiesChange, uid types.UID, generation int64) error {
	poll, limit := copiesVerdictPoll, copiesVerdictLimit
	if ch.wait {
		poll, limit = copiesWaitPoll, copiesWaitLimit
	}
	deadline := time.Now().Add(limit)
	printed := ""
	for {
		obj, err := dyn.Resource(ch.gvr).Namespace(ch.namespace).Get(ctx, ch.name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading %s: %w", ch.name, err)
		}
		if obj.GetUID() != uid {
			return fmt.Errorf("%s was deleted and created again while waiting, so its copies are not the ones this command set", ch.name)
		}
		if copies, set, _ := unstructured.NestedInt64(obj.Object, ch.copiesField()...); !set || copies != int64(ch.want) {
			return fmt.Errorf("the copies of %s were changed to %s while waiting. '%s' shows the current state", ch.name, copiesSetting(copies, set), ch.infoCommand)
		}
		c := copiesCondition(obj)
		if c.reason != "" && c.observedGeneration >= generation {
			switch {
			case c.reason == "Blocked" || c.reason == "RWXNotQualified":
				return fmt.Errorf("the copies of %s cannot change now: %s", ch.name, c.message)
			case c.status == "True":
				say(out, "  ✔  %s\n", c.message)
				return nil
			case c.message != printed:
				say(out, "  …  %s\n", c.message)
				printed = c.message
			}
			if !ch.wait {
				say(out, "     '%s' shows the progress, and --wait waits for it.\n", ch.infoCommand)
				return nil
			}
		}
		if time.Now().After(deadline) {
			if ch.wait {
				return fmt.Errorf("the copies of %s are still converting after %s. '%s' shows the progress", ch.name, limit, ch.infoCommand)
			}
			say(out, "     Kipper converts the copies in the background. '%s' shows the progress.\n", ch.infoCommand)
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(poll):
		}
	}
}

func pluralCopies(n int64) string {
	if n == 1 {
		return "copy"
	}
	return "copies"
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
