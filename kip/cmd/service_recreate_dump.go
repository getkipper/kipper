package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/backupscope"
	"github.com/getkipper/kipper/kip/internal/ai"
	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

var serviceRecreateDumpCmd = &cobra.Command{
	Use:   "recreate-dump <name>",
	Short: "Replace the faulted dump volume of a Postgres service",
	Long: `Deletes the faulted dump volume of a Postgres service with consistent
backups and restarts its database pod, so the controller creates a new, empty
dump volume and the pod can start again.

The dump volume holds only dumps that backups write and then copy, and the
next backup writes a new one. The data volume is left unchanged.

kip checks that Longhorn reports the dump volume as faulted and that the dump
volume, StatefulSet and any existing pod belong to the service. It refuses
while a backup of the namespace or a restore into it is still running, or if
it cannot read their status. It repeats these checks after confirmation, even
when --yes skips the prompt. It waits up to three minutes for the new dump
volume. A pod on a node that is down stays terminating, and then
'kip node fence' is the next step.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE:         runServiceRecreateDump,
}

func init() {
	serviceRecreateDumpCmd.Flags().String("project", "default", "project namespace")
	serviceRecreateDumpCmd.Flags().String("environment", "", "target environment")
	serviceRecreateDumpCmd.Flags().Bool("yes", false, "recreate the dump volume without asking for confirmation")
	serviceCmd.AddCommand(serviceRecreateDumpCmd)
}

const recreateDumpWithin = 3 * time.Minute

func runServiceRecreateDump(cmd *cobra.Command, args []string) error {
	name := args[0]
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx := context.Background()
	namespace := resolveServiceNamespace(cmd)
	if !cmd.Flags().Changed("project") && !cmd.Flags().Changed("environment") {
		if namespace, err = findServiceNamespace(ctx, k8sClient.Dynamic(), name); err != nil {
			return err
		}
	}

	mgr := &service.Manager{Client: k8sClient.Clientset(), Dynamic: k8sClient.Dynamic()}
	d := &dumpRecreate{
		client: k8sClient.Clientset(), dyn: k8sClient.Dynamic(),
		robustness: func(ctx context.Context, pvc *corev1.PersistentVolumeClaim) (string, error) {
			c, err := mgr.ClaimCopies(ctx, pvc)
			return c.Robustness, err
		},
		within: recreateDumpWithin, poll: 2 * time.Second,
	}
	target, err := d.check(ctx, namespace, name)
	if err != nil {
		return err
	}
	writeRecreateDumpPlan(os.Stdout, name, target)
	yes, _ := cmd.Flags().GetBool("yes")
	if !yes {
		if !term.IsTerminal(int(os.Stdin.Fd())) { //nolint:gosec // file descriptors fit an int
			return fmt.Errorf("recreating the dump volume restarts the database. Pass --yes to confirm")
		}
		if !confirmPrompt("Recreate the dump volume?") {
			return fmt.Errorf("recreating the dump volume canceled")
		}
	}
	return d.recreate(ctx, os.Stdout, namespace, name, target)
}

// dumpRecreate replaces a service's faulted dump claim. robustness reads the
// Longhorn robustness of a claim's volume, or "" when Longhorn does not report
// one.
type dumpRecreate struct {
	client     kubernetes.Interface
	dyn        dynamic.Interface
	robustness func(context.Context, *corev1.PersistentVolumeClaim) (string, error)
	within     time.Duration
	poll       time.Duration
}

// dumpTarget is a dump claim proven to belong to its service, with the
// service's pod when one exists.
type dumpTarget struct {
	claim       *corev1.PersistentVolumeClaim
	pod         *corev1.Pod
	dataFaulted bool
}

// check verifies that a Postgres service with consistent backups owns a
// faulted dump claim and its StatefulSet. An existing pod must belong to that
// StatefulSet and reference the dump claim. It returns the first validation or
// read error without changing resources, including errors checking for unfinished
// backups or restores in the namespace. Data-volume read errors are ignored.
func (d *dumpRecreate) check(ctx context.Context, namespace, name string) (*dumpTarget, error) {
	cr, err := d.dyn.Resource(manifest.ServiceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil, fmt.Errorf("service %q not found", name)
		}
		return nil, fmt.Errorf("reading service %s: %w", name, err)
	}
	svcType, _, _ := unstructured.NestedString(cr.Object, "spec", "type")
	consistent, _, _ := unstructured.NestedBool(cr.Object, "spec", "consistentBackups")
	if svcType != "postgres" || !consistent {
		return nil, fmt.Errorf("only a Postgres service with consistent backups has a dump volume, and %s is not one", name)
	}
	if err := refuseDuringVeleroWork(ctx, d.dyn, namespace); err != nil {
		return nil, err
	}

	claimName := name + "-dump"
	claim, err := d.client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, claimName, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, fmt.Errorf("service %s has no dump volume yet. The controller creates %s", name, claimName)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", claimName, err)
	}
	ref := metav1.GetControllerOf(claim)
	if ref == nil {
		return nil, fmt.Errorf("%s has no owner yet, which happens right after a restore. Please wait for the controller to adopt it, then run this again", claimName)
	}
	if ref.Kind != "Service" || ref.UID != cr.GetUID() {
		return nil, fmt.Errorf("%s does not belong to service %s, so kip leaves it alone", claimName, name)
	}
	robustness, err := d.robustness(ctx, claim)
	if err != nil {
		return nil, err
	}
	if robustness != "faulted" {
		state := "dump volume " + claimName + " is " + robustness
		if robustness == "" {
			state = "the state of dump volume " + claimName + " is unknown"
		}
		return nil, fmt.Errorf("%s, and only a faulted dump volume is recreated", state)
	}

	sts, err := d.client.AppsV1().StatefulSets(namespace).Get(ctx, name, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return nil, fmt.Errorf("service %s has no StatefulSet, so kip cannot tell which pod uses %s", name, claimName)
	}
	if err != nil {
		return nil, fmt.Errorf("reading StatefulSet %s: %w", name, err)
	}
	if ref := metav1.GetControllerOf(sts); ref == nil || ref.Kind != "Service" || ref.UID != cr.GetUID() {
		return nil, fmt.Errorf("StatefulSet %s does not belong to service %s, so kip leaves it alone", name, name)
	}

	target := &dumpTarget{claim: claim}
	pod, err := d.client.CoreV1().Pods(namespace).Get(ctx, name+"-0", metav1.GetOptions{})
	switch {
	case errors.IsNotFound(err):
	case err != nil:
		return nil, fmt.Errorf("reading pod %s-0: %w", name, err)
	default:
		if ref := metav1.GetControllerOf(pod); ref == nil || ref.Kind != "StatefulSet" || ref.UID != sts.UID {
			return nil, fmt.Errorf("pod %s does not belong to StatefulSet %s, so kip leaves it alone", pod.Name, name)
		}
		if !mountsClaim(pod, claimName) {
			return nil, fmt.Errorf("pod %s does not mount %s, so kip leaves it alone", pod.Name, claimName)
		}
		target.pod = pod
	}

	data, err := d.client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, "data-"+name+"-0", metav1.GetOptions{})
	if err == nil {
		if r, err := d.robustness(ctx, data); err == nil && r == "faulted" {
			target.dataFaulted = true
		}
	}
	return target, nil
}

// refuseDuringVeleroWork refuses while a backup may be copying the namespace's
// volumes or a restore may be writing into it, because deleting the pod and dump
// claim would interrupt that work. It also returns errors reading Velero state.
func refuseDuringVeleroWork(ctx context.Context, dyn dynamic.Interface, namespace string) error {
	backups, err := unfinishedBackups(ctx, dyn, namespace)
	if err != nil {
		return fmt.Errorf("checking for running backups, which recreating the dump volume must not interrupt: %w", err)
	}
	if len(backups) > 0 {
		return fmt.Errorf("backup %s is still running and covers %s. Recreate the dump volume once it has finished", strings.Join(backups, ", "), namespace)
	}
	restores, err := restoresInto(ctx, dyn, namespace)
	if err != nil {
		return fmt.Errorf("checking for running restores, which recreating the dump volume must not interrupt: %w", err)
	}
	if len(restores) > 0 {
		return fmt.Errorf("a restore into %s is still running (%s). Recreate the dump volume once it has finished. 'kip backup restore --cancel <restore>' ends a stuck restore", namespace, strings.Join(restores, ", "))
	}
	return nil
}

var veleroRestoreGVR = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "restores"}

// restoresInto returns sorted, labelled names of unfinished Velero work that may
// write into namespace. It includes restores mapped into or selecting namespace
// without mapping it elsewhere, plus volume restores and data downloads targeting
// it, which can outlive their parent restore. Unserved kinds contribute no entries.
// Other read errors are returned.
func restoresInto(ctx context.Context, dyn dynamic.Interface, namespace string) ([]string, error) {
	var running []string
	restores, err := listVelero(ctx, dyn, veleroRestoreGVR)
	if err != nil {
		return nil, err
	}
	for i := range restores {
		r := &restores[i]
		if restoreFinished(r) {
			continue
		}
		included, _, _ := unstructured.NestedStringSlice(r.Object, "spec", "includedNamespaces")
		excluded, _, _ := unstructured.NestedStringSlice(r.Object, "spec", "excludedNamespaces")
		mapping, _, _ := unstructured.NestedStringMap(r.Object, "spec", "namespaceMapping")
		_, mappedAway := mapping[namespace]
		mappedHere := false
		for _, target := range mapping {
			mappedHere = mappedHere || target == namespace
		}
		if mappedHere || (!mappedAway && backupscope.MayCover(included, excluded, namespace)) {
			running = append(running, "restore "+r.GetName())
		}
	}
	for _, child := range []struct {
		gvr   schema.GroupVersionResource
		label string
		field []string
	}{
		{podVolumeRestoreGVR, "volume restore", []string{"spec", "pod", "namespace"}},
		{dataDownloadGVR, "data download", []string{"spec", "targetVolume", "namespace"}},
	} {
		items, err := listVelero(ctx, dyn, child.gvr)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			phase, _, _ := unstructured.NestedString(item.Object, "status", "phase")
			target, _, _ := unstructured.NestedString(item.Object, child.field...)
			switch phase {
			case "Completed", "Failed", "Canceled":
				continue
			}
			if target == namespace {
				running = append(running, child.label+" "+item.GetName())
			}
		}
	}
	sort.Strings(running)
	return running, nil
}

// listVelero lists gvr in Velero's namespace, returning none when the cluster
// does not serve the kind.
func listVelero(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource) ([]unstructured.Unstructured, error) {
	list, err := dyn.Resource(gvr).Namespace(ai.VeleroNamespace).List(ctx, metav1.ListOptions{})
	if errors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return list.Items, nil
}

func mountsClaim(pod *corev1.Pod, claim string) bool {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == claim {
			return true
		}
	}
	return false
}

func writeRecreateDumpPlan(out io.Writer, name string, t *dumpTarget) {
	say(out, "\n  Recreating the dump deletes the dump volume %s and restarts the database pod of %s.\n", t.claim.Name, name)
	say(out, "  The data volume is left unchanged, and the next backup writes a new dump.\n")
	if t.dataFaulted {
		say(out, "  The data volume is faulted too, so the database pod still cannot start afterwards.\n")
		say(out, "  See \"When a node dies\" in the maintenance guide.\n")
	}
	say(out, "\n")
}

// recreate checks again that the dump volume is faulted and that the claim and
// pod are the ones confirmed, since a prompt can wait long enough for either to
// change. It then deletes the dump claim and the pod, each only while it is
// still that object, and waits for the controller's new claim.
func (d *dumpRecreate) recreate(ctx context.Context, out io.Writer, namespace, name string, confirmed *dumpTarget) error {
	t, err := d.check(ctx, namespace, name)
	if err != nil {
		return err
	}
	if t.claim.UID != confirmed.claim.UID || podUID(t.pod) != podUID(confirmed.pod) {
		return fmt.Errorf("the dump volume or the pod of %s changed since the confirmation. Run the command again", name)
	}
	if err := d.client.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, t.claim.Name, uidPrecondition(t.claim.UID)); err != nil && !errors.IsNotFound(err) {
		return fmt.Errorf("deleting %s: %w", t.claim.Name, err)
	}
	if t.pod != nil {
		if err := d.client.CoreV1().Pods(namespace).Delete(ctx, t.pod.Name, uidPrecondition(t.pod.UID)); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("deleting pod %s: %w", t.pod.Name, err)
		}
	}
	say(out, "  Waiting for the new dump volume...\n")

	deadline := time.Now().Add(d.within)
	for {
		claim, err := d.client.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, t.claim.Name, metav1.GetOptions{})
		if err == nil && claim.UID != t.claim.UID {
			say(out, "  ✔  The new dump volume %s exists. The database starts once the new volume is attached and any copy change that stopped the database has finished. Watch it with: kip service list\n\n", claim.Name)
			return nil
		}
		if time.Now().After(deadline) {
			return d.leftover(ctx, t, err == nil)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d.poll):
		}
	}
}

// leftover explains why no new claim appeared in time.
func (d *dumpRecreate) leftover(ctx context.Context, t *dumpTarget, oldClaim bool) error {
	var left []string
	if oldClaim {
		left = append(left, t.claim.Name+" is still being deleted")
	} else {
		left = append(left, "the controller has not created a new "+t.claim.Name+" yet")
	}
	if t.pod != nil {
		pod, err := d.client.CoreV1().Pods(t.pod.Namespace).Get(ctx, t.pod.Name, metav1.GetOptions{})
		if err == nil && pod.UID == t.pod.UID {
			left = append(left, fmt.Sprintf("pod %s on %s is still there. A pod on a node that is down stays until the node is fenced with 'kip node fence %s'", pod.Name, pod.Spec.NodeName, pod.Spec.NodeName))
		}
	}
	return fmt.Errorf("no new dump volume within %s: %s", d.within, strings.Join(left, ". "))
}

func podUID(pod *corev1.Pod) types.UID {
	if pod == nil {
		return ""
	}
	return pod.UID
}

func uidPrecondition(uid types.UID) metav1.DeleteOptions {
	return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}
}
