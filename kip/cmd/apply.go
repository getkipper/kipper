package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"

	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/workload"
)

var applyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply a kipper.yaml manifest to the cluster",
	RunE:  runApply,
}

func init() {
	applyCmd.Flags().StringP("file", "f", "kipper.yaml", "path to manifest file or directory")
	applyCmd.Flags().String("project", "", "override the project name from the manifest")
	applyCmd.Flags().String("environment", "", "override the environment from the manifest")
	applyCmd.Flags().Bool("dry-run", false, "print what would be applied without making changes")
	applyCmd.Flags().Bool("force", false, "apply even when it would clear fields the manifest does not carry")
	rootCmd.AddCommand(applyCmd)
}

func runApply(cmd *cobra.Command, args []string) error {
	filePath, _ := cmd.Flags().GetString("file")
	projectOverride, _ := cmd.Flags().GetString("project")
	envOverride, _ := cmd.Flags().GetString("environment")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	force, _ := cmd.Flags().GetBool("force")

	manifests, err := manifest.Parse(filePath)
	if err != nil {
		return err
	}

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	dynClient := k8sClient.Dynamic()

	plans := make([]applyPlan, 0, len(manifests))
	for _, m := range manifests {
		project := m.Project
		if projectOverride != "" {
			project = projectOverride
		}
		environment := m.Environment
		if envOverride != "" {
			environment = envOverride
		}
		namespace := cluster.ResolveNamespace(project, environment)
		plans = append(plans, applyPlan{m: m, project: project, namespace: namespace, resources: manifest.Convert(m, namespace)})
	}

	if dryRun {
		// The same refusal the real run makes, since a dry run whose report
		// cannot be produced accurately should say so rather than print one.
		if err := refuseDuplicateResources(plans); err != nil {
			return err
		}
		defaults := newSchemaDefaults()
		var shown []resourceChange
		for _, p := range plans {
			fmt.Printf("\n  Dry run: %s (%s)\n", p.project, p.namespace)
			if len(p.m.Environments) > 0 {
				fmt.Printf("    Project/%s (environments: %v)\n", p.project, p.m.Environments)
			}
			for _, res := range p.resources {
				fmt.Printf("    %s/%s (%s)\n", res.Object.GetKind(), res.Object.GetName(), res.GVR.Resource)
			}
			changes, scanErr := scanChangesWith(ctx, dynClient, p.namespace, p.resources, defaults)
			if scanErr != nil {
				return scanErr
			}
			clears := clearsOf(changes)
			shown = append(shown, clears...)
			printClears(clears)
		}
		printSchemaCaveat(defaults, shown)
		fmt.Println()
		return nil
	}

	totalCreated, totalUpdated, err := applyPlans(ctx, dynClient, k8sClient.Clientset(), plans, force)
	if err != nil {
		return err
	}

	fmt.Printf("\n  Done: %d created, %d updated\n\n", totalCreated, totalUpdated)
	return nil
}

// applyPlan is one manifest resolved to the namespace and resources it writes.
type applyPlan struct {
	m         *manifest.Manifest
	project   string
	namespace string
	resources []manifest.Resource
}

// applyPlans scans every plan for fields an apply would remove, and writes
// nothing at all if it finds any without force.
//
// Scanning all of them before writing any of them is what makes the refusal a
// preflight. Scanned per plan, a directory whose last file cleared a field had
// already created projects, namespaces and workloads for the files ahead of it,
// so the error arrived after a partial apply nobody asked for — and the second
// run then saw a cluster the first had already changed.
func applyPlans(ctx context.Context, dyn dynamic.Interface, clientset kubernetes.Interface, plans []applyPlan, force bool) (created, updated int, err error) {
	// Two manifests naming one resource make the preflight unanswerable: each is
	// compared against the live object, so a field the first adds and the second
	// omits is invisible until the first has already been written and the second
	// refuses. Nothing legitimate needs it — the later spec would simply win —
	// so it is refused before anything is read.
	if err := refuseDuplicateResources(plans); err != nil {
		return 0, 0, err
	}

	defaults := newSchemaDefaults()
	var allClears []resourceChange
	for _, p := range plans {
		changes, scanErr := scanChangesWith(ctx, dyn, p.namespace, p.resources, defaults)
		if scanErr != nil {
			return 0, 0, scanErr
		}
		allClears = append(allClears, clearsOf(changes)...)
	}
	if len(allClears) > 0 {
		printClears(allClears)
		printSchemaCaveat(defaults, allClears)
		if !force {
			return 0, 0, fmt.Errorf("refusing to clear %d field(s) the manifest does not carry; add them to the manifest, run 'kip export' to fold the live state in, or re-run with --force", len(allClears))
		}
		fmt.Printf("  --force given, clearing them.\n")
	}

	// Everything below writes. Kubernetes has no transaction across objects, so
	// any error from here on can leave the cluster part-way, and every one of
	// these paths has to say what it wrote before it stopped.
	var projectsWritten, namespacesCreated int
	stopped := func(e error) (int, int, error) {
		printPartialApply(created, updated, projectsWritten, namespacesCreated)
		return created, updated, e
	}

	for _, p := range plans {
		if len(p.m.Environments) > 0 {
			if err := ensureProject(ctx, dyn, p.m, p.project); err != nil {
				return stopped(fmt.Errorf("ensuring project: %w", err))
			}
			projectsWritten++
		}

		if _, nsErr := clientset.CoreV1().Namespaces().Get(ctx, p.namespace, metav1.GetOptions{}); errors.IsNotFound(nsErr) {
			// Wait for the Project reconciler to create it, or create it manually
			ns := &corev1.Namespace{
				ObjectMeta: metav1.ObjectMeta{
					Name: p.namespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "kipper",
						"kipper.run/project":           p.project,
					},
				},
			}
			_, createErr := clientset.CoreV1().Namespaces().Create(ctx, ns, metav1.CreateOptions{})
			switch {
			case createErr == nil:
				namespacesCreated++
				fmt.Printf("  ✔  Namespace %s created\n", p.namespace)
			case errors.IsAlreadyExists(createErr):
				// The Project reconciler or another apply got there between the
				// Get and the Create. Carrying on is right — the namespace is
				// there either way — but this run did not create it, so neither
				// the tick nor the partial-apply count may claim it did.
			default:
				return stopped(fmt.Errorf("creating namespace %s: %w", p.namespace, createErr))
			}
		}

		fmt.Printf("\n  Applying to %s (%s)...\n", p.project, p.namespace)

		for _, res := range p.resources {
			kind := res.Object.GetKind()
			name := res.Object.GetName()
			kindDefaults, defErr := defaults.forResource(ctx, dyn, res.Object.GetKind(), res.GVR)
			if defErr != nil {
				return stopped(defErr)
			}
			action, applyErr := applyResource(ctx, dyn, p.namespace, res, force, kindDefaults, defaults.unreadKinds[kind])
			if applyErr != nil {
				return stopped(fmt.Errorf("applying %s/%s: %w", kind, name, applyErr))
			}
			fmt.Printf("    ✔  %s/%s %s\n", kind, name, action)
			switch action {
			case "created":
				created++
			case "updated":
				updated++
			}
		}
	}
	return created, updated, nil
}

// refuseDuplicateResources reports manifests that write the same resource twice
// in one invocation.
func refuseDuplicateResources(plans []applyPlan) error {
	seen := map[string]struct{}{}
	var dupes []string
	for _, p := range plans {
		for _, res := range p.resources {
			id := fmt.Sprintf("%s/%s in %s", res.Object.GetKind(), res.Object.GetName(), p.namespace)
			if _, found := seen[id]; found {
				dupes = append(dupes, id)
				continue
			}
			seen[id] = struct{}{}
		}
	}
	if len(dupes) == 0 {
		return nil
	}
	sort.Strings(dupes)
	return fmt.Errorf("%s named more than once across these manifests; each resource needs one definition, or the last one silently wins", strings.Join(dupes, ", "))
}

// resourceChange is one field an apply would touch, and the resource it is on.
type resourceChange struct {
	kind, name string
	change     manifest.Change
}

// scanChanges reads each resource that already exists and reports what applying
// the manifest would do to it, field by field. Read-only.
//
// One function for both callers on purpose: `kip diff` prints all of it and
// `kip apply` acts on the cleared half, and the two answering differently is
// how a diff ends up promising something apply does not do. It is also what
// makes the preserved-path rule testable — a test that called DiffSpec with its
// own arguments would prove the parameter works and not that production passes
// it.
// schemaDefaults caches a cluster's CRD defaults per kind, so a directory of
// twenty apps asks for the App schema once.
type schemaDefaults struct {
	byKind map[schema.GroupVersionResource]map[string]interface{}
	// unreadKinds names the kinds whose schema could not be read, so a caller
	// can say it could not tell rather than that a field is being destroyed.
	// Keyed by Kind, which is what a reported change carries.
	unreadKinds map[string]bool
}

func newSchemaDefaults() *schemaDefaults {
	return &schemaDefaults{
		byKind:      map[schema.GroupVersionResource]map[string]interface{}{},
		unreadKinds: map[string]bool{},
	}
}

func (d *schemaDefaults) forResource(ctx context.Context, dyn dynamic.Interface, kind string, gvr schema.GroupVersionResource) (map[string]interface{}, error) {
	if cached, read := d.byKind[gvr]; read {
		return cached, nil
	}
	found, known, err := installer.SpecDefaults(ctx, dyn, gvr)
	if err != nil {
		return nil, err
	}
	d.byKind[gvr] = found
	if !known {
		d.unreadKinds[kind] = true
	}
	return found, nil
}

// unreadAmong reports whether any of these changes is on a kind whose schema
// could not be read. Only those are uncertain, so only those are caveated —
// warning about a list that is entirely certain teaches an operator to skip the
// warning.
func (d *schemaDefaults) unreadAmong(changes []resourceChange) bool {
	for _, c := range changes {
		if d.unreadKinds[c.kind] {
			return true
		}
	}
	return false
}

func scanChanges(ctx context.Context, dyn dynamic.Interface, namespace string, resources []manifest.Resource) ([]resourceChange, error) {
	return scanChangesWith(ctx, dyn, namespace, resources, newSchemaDefaults())
}

func scanChangesWith(ctx context.Context, dyn dynamic.Interface, namespace string, resources []manifest.Resource, defaults *schemaDefaults) ([]resourceChange, error) {
	var out []resourceChange
	for _, res := range resources {
		name := res.Object.GetName()
		live, err := dyn.Resource(res.GVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			continue // being created; nothing to lose
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s/%s: %w", res.Object.GetKind(), name, err)
		}
		kindDefaults, defErr := defaults.forResource(ctx, dyn, res.Object.GetKind(), res.GVR)
		if defErr != nil {
			return nil, defErr
		}
		liveSpec, _, _ := unstructured.NestedMap(live.Object, "spec")
		newSpec, _ := res.Object.Object["spec"].(map[string]interface{})
		// What the apply will write, not what the manifest says. The two differ
		// where a manifest carries a credential name the cluster owns, and a
		// scan that reported the manifest's version showed a change the write
		// never makes and passed a preflight the write then refuses.
		effective := effectiveSpecForDiff(name, newSpec)
		for _, c := range manifest.DiffSpec(liveSpec, effective, preservedPaths(name, effective, liveSpec), kindDefaults) {
			out = append(out, resourceChange{kind: res.Object.GetKind(), name: name, change: c})
		}
	}
	return out, nil
}

// objectForCreate returns what a create should write. There is nothing live to
// carry forward, so this is the manifest minus anything it does not assert: a
// credential name the app owns is machine state, and creating an App onto one
// names a Secret that is not there.
//
// The manifest is left alone, because the update path reads it afterwards.
func objectForCreate(name string, object *unstructured.Unstructured) *unstructured.Unstructured {
	spec, isSpec := object.Object["spec"].(map[string]interface{})
	if !isSpec || !assertsNothingByNamingItsOwnCredential(name, spec) {
		return object
	}
	out := object.DeepCopy()
	out.Object["spec"] = effectiveSpecForDiff(name, spec)
	return out
}

// assertsNothingByNamingItsOwnCredential reports whether a spec carries a
// credential name this app owns, which every surface treats as machine state
// rather than something the manifest is asking for.
func assertsNothingByNamingItsOwnCredential(name string, spec map[string]interface{}) bool {
	git, isGit := spec["git"].(map[string]interface{})
	if !isGit {
		return false
	}
	value, named := git["credentialsSecret"].(string)
	return named && value != "" && secretname.IsGitCredentialOf(name, value)
}

// effectiveSpecForDiff returns the spec an apply would write, so a diff shows
// what will happen rather than what the manifest says.
//
// Only the credential differs, and only where the manifest carries a name this
// app owns. The manifest is left alone: the write path reads it afterwards and
// derives its own value per attempt.
func effectiveSpecForDiff(name string, newSpec map[string]interface{}) map[string]interface{} {
	git, isGit := newSpec["git"].(map[string]interface{})
	if !isGit {
		return newSpec
	}
	if !assertsNothingByNamingItsOwnCredential(name, newSpec) {
		return newSpec
	}
	effective := make(map[string]interface{}, len(newSpec))
	for k, v := range newSpec {
		effective[k] = v
	}
	gitCopy := make(map[string]interface{}, len(git))
	for k, v := range git {
		gitCopy[k] = v
	}
	delete(gitCopy, "credentialsSecret")
	effective["git"] = gitCopy
	return effective
}

// preservedPaths are the spec paths apply carries forward rather than
// replacing, so neither a diff nor a warning may report them as changing.
//
// A git app's built image is build output owned by the build controller. Apply
// keeps it so that applying a git-only spec cannot reset a running app to the
// busybox build placeholder, which serves a static page and 502s the real
// service. Convert always stamps an image, so this never shows up as a clear —
// what it prevents is a diff announcing that the running image is about to
// become the placeholder, which would be alarming and untrue.
func preservedPaths(name string, newSpec, liveSpec map[string]interface{}) []string {
	if newSpec == nil {
		return nil
	}
	git, isGit := newSpec["git"].(map[string]interface{})
	if !isGit {
		return nil
	}
	paths := []string{"image"}
	v, named := git["credentialsSecret"].(string)
	live, _, _ := unstructured.NestedString(liveSpec, "git", "credentialsSecret")
	// Suppressed only where the apply would write back exactly what is live, so
	// a diff never offers to clear what the cluster owns and never hides a
	// change the operator asked for. Without this the round trip the docs
	// describe — export, which omits the credential, then apply — is refused on
	// every app that has a token, and the refusal tells the operator to pin the
	// name that rotates.
	if effective, present := effectiveGitCredential(name, v, named, live); (present && effective == live) || (!present && live == "") {
		paths = append(paths, "git.credentialsSecret")
	}
	return paths
}

// effectiveGitCredential returns what git.credentialsSecret should be for this
// app, and whether the field belongs in the spec at all.
//
// The manifest asserts a credential only by naming a shared one. Absent, empty,
// and a name this app owns are the same thing: no assertion. An app-owned name
// is machine state that a manifest exported before credentials were named this
// way happens to carry, and by now it points at a Secret the sweep has
// collected, so honouring it would move the app onto nothing.
//
// With no assertion the live value decides. One the app owns is the cluster's
// and carries forward; a shared one is the operator's, so its absence from the
// manifest is them taking it away, which the ordinary clear reporting handles.
func effectiveGitCredential(app, value string, named bool, live string) (string, bool) {
	if named && value != "" && !secretname.IsGitCredentialOf(app, value) {
		return value, true
	}
	if live != "" && secretname.IsGitCredentialOf(app, live) {
		return live, true
	}
	return "", false
}

// applyGitCredential puts the effective credential into a manifest's git block.
//
// Every attempt starts from the manifest as written, because the retry closure
// mutates this map in place: an attempt that carried a live credential in would
// otherwise leave it there for the next one, which then writes it over whatever
// the cluster has moved to.
func applyGitCredential(git map[string]interface{}, app, value string, named bool, live string) {
	delete(git, "credentialsSecret")
	if effective, present := effectiveGitCredential(app, value, named, live); present {
		git["credentialsSecret"] = effective
	}
}

// manifestGitCredential reads git.credentialsSecret straight from a converted
// manifest, before an apply attempt writes the live value into it.
func manifestGitCredential(object map[string]interface{}) (string, bool) {
	spec, _ := object["spec"].(map[string]interface{})
	git, _ := spec["git"].(map[string]interface{})
	v, named := git["credentialsSecret"].(string)
	return v, named
}

// clearsOf keeps only the changes that take a value away.
func clearsOf(changes []resourceChange) []resourceChange {
	var out []resourceChange
	for _, c := range changes {
		if c.change.Kind == manifest.Cleared || c.change.Kind == manifest.Reset {
			out = append(out, c)
		}
	}
	return out
}

// printPartialApply says what a run wrote before it stopped. Silence here reads
// as "nothing happened", which is the one thing that is not true.
//
// It names each kind of write separately rather than totalling them, because
// "2 updated" that quietly included a Project and a Namespace would be a second
// way of misreporting the same run.
func printPartialApply(created, updated, projectsWritten, namespacesCreated int) {
	var parts []string
	for _, part := range []struct {
		n     int
		label string
	}{
		{created, "created"},
		{updated, "updated"},
		{projectsWritten, "project(s) written"},
		{namespacesCreated, "namespace(s) created"},
	} {
		if part.n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", part.n, part.label))
		}
	}
	if len(parts) == 0 {
		return
	}
	fmt.Printf("\n  Stopped part-way: %s. Those writes stand, because Kubernetes\n", strings.Join(parts, ", "))
	fmt.Printf("  applies one object at a time. Re-run once the manifest accounts for what\n")
	fmt.Printf("  stopped it.\n")
}

// printSchemaCaveat says when the list above may be overstating itself.
//
// Without a kind's schema there is no way to tell a value the cluster fills in
// from one the apply destroys, so both are listed. Saying which case this is
// costs a sentence and is the difference between an operator who understands
// the question and one who reaches for --force to make it go away.
func printSchemaCaveat(defaults *schemaDefaults, shown []resourceChange) {
	if !defaults.unreadAmong(shown) {
		return
	}
	fmt.Printf("\n  Some of these may not be losses at all: this cluster's resource schemas\n")
	fmt.Printf("  could not be read, so a field the cluster fills in for itself cannot be told\n")
	fmt.Printf("  from one the manifest is about to remove. Reading them needs get on\n")
	fmt.Printf("  customresourcedefinitions, which a project-scoped role does not have.\n")
}

func printClears(clears []resourceChange) {
	if len(clears) == 0 {
		return
	}
	fmt.Printf("\n  These are set on the cluster and absent from the manifest, so applying takes them away:\n")
	for _, c := range clears {
		if c.change.Kind == manifest.Reset {
			fmt.Printf("    - %s/%s  %s: %s -> %s (the cluster's default)\n", c.kind, c.name, c.change.Path, c.change.Live, c.change.New)
			continue
		}
		fmt.Printf("    - %s/%s  %s: %s (removed)\n", c.kind, c.name, c.change.Path, c.change.Live)
	}
}

// clearedUnderApplyError reports fields that appeared between the preflight
// scan and the write, and would be removed by it.
type clearedUnderApplyError struct {
	kind, name string
	clears     []manifest.Change
	// schemaUnread carries the same uncertainty the preflight discloses. This
	// guard is the authoritative one, so an explanation that leaves it out
	// sends the operator to --force without saying the answer was a guess.
	schemaUnread bool
}

func (e *clearedUnderApplyError) Error() string {
	paths := make([]string, len(e.clears))
	for i, c := range e.clears {
		paths[i] = c.Path
	}
	msg := fmt.Sprintf("%s/%s changed while applying: %s %s set on the cluster and absent from the manifest, so applying would remove %s; re-run to see the current difference, or re-run with --force",
		e.kind, e.name, strings.Join(paths, ", "), plural(len(e.clears), "is", "are"), plural(len(e.clears), "it", "them"))
	if e.schemaUnread {
		msg += "\n  This may not be a loss at all: this cluster's schema for " + e.kind +
			" could not be read, so a field the cluster fills in for itself cannot be told from one being removed"
	}
	return msg
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// applyResource creates the resource if it is absent, or replaces its spec if
// it already exists. kip apply is declarative: the manifest is the desired
// spec, so a field left out of the manifest is cleared. Metadata and status are
// preserved. It returns "created" or "updated". The update retries on conflict
// because the reconciler bumps the object's resourceVersion (finalizers,
// status) between the Get and the Update.
//
// Unless force is set, it refuses to write a spec that would clear a field of
// the object it just read. The preflight scan in runApply is a separate read
// from this write, so anything the console, another CLI run or a controller
// added in between was invisible to it — including a resource that did not
// exist at scan time and lost the create race below. Re-checking against the
// object actually being replaced is what makes the refusal a property of the
// write rather than of a read that preceded it.
func applyResource(ctx context.Context, dyn dynamic.Interface, namespace string, res manifest.Resource, force bool, defaults map[string]interface{}, schemaUnread bool) (string, error) {
	name := res.Object.GetName()
	if _, getErr := dyn.Resource(res.GVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{}); getErr != nil {
		if !errors.IsNotFound(getErr) {
			return "", getErr
		}
		// A manifest can declare any kind, so this is the one create path that
		// reaches all three workload kinds. Checking here rather than in each
		// kind's own command is what keeps a manifest from doing what the
		// commands refuse.
		if res.GVR == manifest.ServiceGVR {
			if err := refuseServiceNameSharingAnAppCredential(ctx, dyn, namespace, name); err != nil {
				return "", err
			}
		}
		release := func() {}
		if kind := workloadKindOf(res.GVR); kind != "" {
			var reserveErr error
			release, reserveErr = workload.Reserve(ctx, dyn, namespace, name, kind)
			if reserveErr != nil {
				return "", reserveErr
			}
		}
		_, err := dyn.Resource(res.GVR).Namespace(namespace).Create(ctx, objectForCreate(name, res.Object), metav1.CreateOptions{})
		if err == nil {
			return "created", nil
		}
		// AlreadyExists proves the workload is there, so the reservation just
		// made is its own backfill and stands; the update below is what this
		// call actually does.
		if !errors.IsAlreadyExists(err) {
			release()
		}
		if !errors.IsAlreadyExists(err) {
			return "", err
		}
		// Lost a create race — another apply created it first. Fall through and
		// replace its spec instead of failing.
	}

	// Read once, before any attempt mutates the spec below. A retry re-runs the
	// closure over the same map, so deciding from the map would read back what
	// the previous attempt injected and pin a credential the cluster has since
	// moved off.
	manifestCredential, manifestNamesCredential := manifestGitCredential(res.Object.Object)

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		existing, err := dyn.Resource(res.GVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		newSpec, _ := res.Object.Object["spec"].(map[string]interface{})
		// A git app's built image is build output owned by the build controller,
		// not manifest state. Carry the live image forward so applying a git-only
		// spec never resets a running app to the busybox build placeholder (which
		// serves a static page and 502s the real service).
		if newSpec != nil {
			if git, isGit := newSpec["git"].(map[string]interface{}); isGit {
				if liveImage, found, _ := unstructured.NestedString(existing.Object, "spec", "image"); found && liveImage != "" {
					newSpec["image"] = liveImage
				}
				// The app's own credential is build input the console and the
				// CLI own, one object per token-and-host pair and a new name on
				// every rotation. The cluster decides it whenever the manifest
				// does not name a shared one — including when the manifest
				// names an app-owned name, which only a manifest exported
				// before credentials were named this way can carry, and which
				// by now points at a Secret the sweep has collected.
				live, _, _ := unstructured.NestedString(existing.Object, "spec", "git", "credentialsSecret")
				applyGitCredential(git, name, manifestCredential, manifestNamesCredential, live)
			}
		}
		if !force {
			liveSpec, _, _ := unstructured.NestedMap(existing.Object, "spec")
			if clears := manifest.Clears(manifest.DiffSpec(liveSpec, newSpec, preservedPaths(name, newSpec, liveSpec), defaults)); len(clears) > 0 {
				return &clearedUnderApplyError{kind: res.Object.GetKind(), name: name, clears: clears, schemaUnread: schemaUnread}
			}
		}
		existing.Object["spec"] = newSpec
		_, err = dyn.Resource(res.GVR).Namespace(namespace).Update(ctx, existing, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return "", err
	}
	return "updated", nil
}

// unionEnvironments returns the live environment entries plus any manifest
// environment names not already present. Existing entries are preserved
// verbatim (keeping per-environment quota and other fields), and nothing is
// ever removed — apply adds environments, it never prunes them.
func unionEnvironments(existing []interface{}, manifestEnvs []string) []interface{} {
	seen := make(map[string]bool, len(existing))
	out := make([]interface{}, 0, len(existing)+len(manifestEnvs))
	for _, e := range existing {
		if em, ok := e.(map[string]interface{}); ok {
			if name, ok := em["name"].(string); ok {
				seen[name] = true
			}
		}
		out = append(out, e)
	}
	for _, name := range manifestEnvs {
		if !seen[name] {
			out = append(out, map[string]interface{}{"name": name})
			seen[name] = true
		}
	}
	return out
}

func ensureProject(ctx context.Context, dynClient dynamic.Interface, m *manifest.Manifest, project string) error {
	envs := make([]interface{}, len(m.Environments))
	for i, e := range m.Environments {
		envs[i] = map[string]interface{}{"name": e}
	}

	spec := map[string]interface{}{
		"environments": envs,
	}
	if m.DisplayName != "" {
		spec["displayName"] = m.DisplayName
	}

	projectObj := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Project",
			"metadata": map[string]interface{}{
				"name": project,
				"labels": map[string]interface{}{
					"app.kubernetes.io/managed-by": "kipper",
				},
			},
			"spec": spec,
		},
	}

	_, getErr := dynClient.Resource(manifest.ProjectGVR).Get(ctx, project, metav1.GetOptions{})
	if getErr != nil && !errors.IsNotFound(getErr) {
		return fmt.Errorf("checking project %s: %w", project, getErr)
	}
	if errors.IsNotFound(getErr) {
		_, err := dynClient.Resource(manifest.ProjectGVR).Create(ctx, projectObj, metav1.CreateOptions{})
		if err == nil {
			fmt.Printf("  ✔  Project %s created (environments: %v)\n", project, m.Environments)
			return nil
		}
		if !errors.IsAlreadyExists(err) {
			return fmt.Errorf("creating project %s: %w", project, err)
		}
		// Lost a create race — another apply created it first. Fall through and
		// union environments into the winner's Project.
	}

	// A Project is updated in place, not replaced: it holds fields the manifest
	// never carries (members, tier, quota, shared storage) that other tools own,
	// and mutating the live object preserves them. Environments are unioned,
	// never pruned — dropping one deletes its namespace and all data, which stays
	// an explicit, confirmed action (`kip project remove-env` / `kip project
	// delete`). The union runs under RetryOnConflict so a concurrent environment
	// add can't be lost to a stale read: a conflicting write forces a re-read and
	// re-union.
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		live, err := dynClient.Resource(manifest.ProjectGVR).Get(ctx, project, metav1.GetOptions{})
		if err != nil {
			return err
		}
		existingEnvs, _, _ := unstructured.NestedSlice(live.Object, "spec", "environments")
		if err := unstructured.SetNestedSlice(live.Object, unionEnvironments(existingEnvs, m.Environments), "spec", "environments"); err != nil {
			return err
		}
		if m.DisplayName != "" {
			if err := unstructured.SetNestedField(live.Object, m.DisplayName, "spec", "displayName"); err != nil {
				return err
			}
		}
		_, err = dynClient.Resource(manifest.ProjectGVR).Update(ctx, live, metav1.UpdateOptions{})
		return err
	}); err != nil {
		return fmt.Errorf("updating project %s: %w", project, err)
	}
	fmt.Printf("  ✔  Project %s updated\n", project)

	return nil
}

// workloadKindOf names the workload kind a GVR stores, or "" when the resource
// is not one of the kinds that compete for a name.
func workloadKindOf(gvr schema.GroupVersionResource) string {
	switch gvr {
	case manifest.AppGVR:
		return "app"
	case manifest.FunctionGVR:
		return "function"
	case manifest.JobGVR:
		return "job"
	default:
		return ""
	}
}
