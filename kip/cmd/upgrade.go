package cmd

import (
	"bufio"
	"context"
	"embed"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/yaml"

	"github.com/getkipper/kipper/controller/pkg/pubip"
	"github.com/getkipper/kipper/controller/pkg/rollout"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/infra"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

//go:embed crds/*.yaml
var upgradeCRDs embed.FS

var crdGVR = schema.GroupVersionResource{
	Group:    "apiextensions.k8s.io",
	Version:  "v1",
	Resource: "customresourcedefinitions",
}

var upgradeCmd = &cobra.Command{
	Use:   "upgrade",
	Short: "Upgrade Kipper components on the current cluster",
	Long: `Updates CRD schemas, restarts console and console-api with the latest
images, and reconciles cluster system components (KEDA, Longhorn, Traefik,
Loki, Prometheus, Grafana, Velero, Zot) to the versions pinned in this
kip build.

By default, system component upgrades require explicit confirmation
because a chart upgrade can fail mid-rollout and disrupt running
workloads. Pass --yes to skip the prompt (e.g. for automation), or
--skip-system to upgrade only the Kipper console layer.

On a cluster old enough to hold shared git credentials with no
allow-list, the upgrade prints which apps reference each one and asks
before granting them. Pass --seed-credential-grants to skip that
prompt (e.g. for automation); a scripted upgrade without it completes
and prints the flag rather than granting on inference.

The cluster's own certificate authority and the trust anchor the API
server verifies logins against are reconciled over SSH on every run,
including with --skip-system, because they are this cluster's identity
rather than a component version.

Examples:
  kip upgrade
  kip upgrade --skip-system
  kip upgrade --yes
  kip upgrade --seed-credential-grants`,
	SilenceUsage: true,
	RunE:         runUpgrade,
}

func init() {
	upgradeCmd.Flags().Bool("skip-system", false, "skip cluster system components (Traefik, Longhorn, KEDA, etc.). Upgrade only Kipper CRDs and console")
	upgradeCmd.Flags().Bool("yes", false, "skip the confirmation prompt before upgrading system components")
	upgradeCmd.Flags().Bool("seed-credential-grants", false, "grant each shared git credential the projects whose apps already reference it, without asking. Skips the pre-rollout prompt on a legacy cluster; a no-op everywhere else")
	upgradeCmd.Flags().String("ssh-key", "", "path to SSH private key for the host; needed by every upgrade, including --skip-system, because the cluster's trust material is reconciled over SSH (overrides cluster.ssh_key in config and KIP_SSH_KEY env)")
	rootCmd.AddCommand(upgradeCmd)
}

func runUpgrade(cmd *cobra.Command, _ []string) error {
	skipSystem, _ := cmd.Flags().GetBool("skip-system")
	autoYes, _ := cmd.Flags().GetBool("yes")
	seedGrants, _ := cmd.Flags().GetBool("seed-credential-grants")
	sshKey, _ := cmd.Flags().GetString("ssh-key")

	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	clientset := k8sClient.Clientset()

	fmt.Printf("\n  Upgrading Kipper on %s...\n\n", cluster.Domain)

	// Update CRD schemas via K8s API. Later steps (the system-component
	// upgrade applies an ApiKey canary) need the new CRDs registered, so a
	// failure here has to stop the upgrade at its real cause rather than let a
	// downstream step die with a bare "no matches for kind".
	if err := reconcileClusterAPIState(ctx, k8sClient.Dynamic(), rootCmd.Version, cluster.Domain, os.Stdout); err != nil {
		return err
	}

	// Record what the gateway heartbeat needs on the ClusterIdentity before any
	// workload restarts. The recovery source for a cluster whose CR predates the
	// gateway block is the live console-api env, and a new pod reconciling the old
	// CR would rewrite that env first — on an opted-out cluster it would clear it
	// outright, losing the only copy. This has to run after the CRD update, or the
	// new clusterHost field is pruned from the write.
	if err := reconcileGatewayIdentity(ctx, k8sClient.Dynamic(), clientset, cluster.Host); err != nil {
		return fmt.Errorf("recording gateway identity: %w", err)
	}

	// Ask before granting: reference is not proof, and shared credential names
	// are predictable enough that an app pointed at the wrong one used to eat
	// one failed build and get the permission on the next upgrade. Deciding
	// this once, before either pass, freezes the snapshot both passes fill
	// from: an app pointed at an undecided credential during the rollout
	// window is a reference, not a grant, and turning it into one would be the
	// exact defect this exists to prevent.
	grants, err := credentialSeedConsent(
		ctx, clientset, k8sClient.Dynamic(), os.Stdout,
		seedGrants,
		term.IsTerminal(int(os.Stdin.Fd())),
		func() (bool, error) {
			return confirmInteractiveNonFatal("  Grant these credentials on their apps' projects? [y/N] ")
		},
	)
	if err != nil {
		return err
	}

	// An upgrade that gives up during the rollout still has the record, and the
	// lists the old console-api erased are erased whether or not it finishes.
	// Writing them back on the way out is the difference between an operator
	// retrying a failed upgrade and a curated grant arriving at the next one as
	// an inference to approve, with whatever else came to reference it in the
	// meantime.
	//
	// It runs on every exit, successful or not, rather than on a flag saying
	// which. A repair with nothing to repair writes nothing: Restore only fills
	// a list that is absent, and by the end of a run that got as far as pass two
	// none are. That leaves no ordering to get wrong, and it catches the one
	// case a flag would miss — a late write from the pod being replaced, landing
	// after the closing pass had already checked. What it writes back is
	// everything this run stands behind, the grants it made as well as the ones
	// it found, or the promise would hold for one kind and not the other.
	defer repairErasedAllowLists(ctx, clientset, os.Stdout, grants.repairRecord())

	// Before the new console-api serves builds: a shared credential written
	// before allow-lists existed allows nobody, so an upgrade that restarted the
	// builder first would refuse builds that were working until the seeding
	// caught up. It runs again after the rollout, and only then is the cluster
	// recorded as migrated.
	if err := seedSharedCredentialGrants(ctx, clientset, os.Stdout, grants); err != nil {
		return err
	}

	// Taken off before the restart so that what is read after it was written by
	// the console-api this upgrade started, rather than by one that served here
	// at some point and has since been rolled back.
	if err := clearConsoleAPIStamp(ctx, clientset); err != nil {
		return err
	}

	// Restart console components to pull latest images. kipper-authz is
	// pinned to :latest and released in lockstep with console-api. A bare
	// kubectl apply of its unchanged manifest never rolls the pods to the
	// new image, so bump its template annotation here to force the pull.
	components := []struct {
		name      string
		namespace string
	}{
		{consoleAPIName, kipperSystemNS},
		{"console", kipperSystemNS},
		{"kipper-authz", kipperSystemNS},
	}

	for _, comp := range components {
		fmt.Printf("  ...  %s\n", comp.name)

		deploy, getErr := clientset.AppsV1().Deployments(comp.namespace).Get(ctx, comp.name, metav1.GetOptions{})
		if apierrors.IsNotFound(getErr) {
			fmt.Printf("  ✗  %s (not found)\n", comp.name)
			continue
		}
		// Anything else is unknown rather than absent, and the two lead opposite
		// ways: a component that is not there is one this cluster does not run,
		// while one that cannot be read is one this upgrade has not moved, and
		// carrying on would report an upgrade that did not happen.
		if getErr != nil {
			return fmt.Errorf("reading %s: %w", comp.name, getErr)
		}
		var moved string
		hardened := false
		var imagesMoved []string
		mutate := func(dep *appsv1.Deployment) error {
			if dep.Spec.Template.Annotations == nil {
				dep.Spec.Template.Annotations = make(map[string]string)
			}
			dep.Spec.Template.Annotations["kipper.run/upgradedAt"] = time.Now().Format(time.RFC3339)

			// Reconcile the image onto the one this kip build pins. Bumping the
			// annotation alone only re-pulls whatever tag the deployment already
			// carries, so a cluster moved onto a hand-built or sideloaded tag
			// during an incident would stay on it through every upgrade. The pull
			// policy goes with it: a :latest tag that kept an inherited
			// IfNotPresent would serve a stale cached layer instead of the
			// released image.
			if pinned := installer.PinnedImage(comp.name); pinned != "" && len(dep.Spec.Template.Spec.Containers) > 0 {
				if current := dep.Spec.Template.Spec.Containers[0].Image; current != pinned {
					moved = fmt.Sprintf(" (%s → %s)", current, pinned)
				}
				dep.Spec.Template.Spec.Containers[0].Image = pinned
				dep.Spec.Template.Spec.Containers[0].ImagePullPolicy = corev1.PullAlways
			}

			// Deliver the pod-spec hardening the install manifest carries.
			// Restarting a cluster-powerful component onto a new image while
			// leaving it with the privileges of whenever the cluster was
			// installed is the gap that let every existing cluster run the
			// current console-api unhardened.
			if comp.name == consoleAPIName {
				applied, hardenErr := applyConsoleAPIHardening(dep)
				if hardenErr != nil {
					return fmt.Errorf("rendering console-api pod spec: %w", hardenErr)
				}
				hardened = applied

				repointed, imageErr := applyPlatformImageEnv(dep)
				if imageErr != nil {
					return fmt.Errorf("rendering console-api platform images: %w", imageErr)
				}
				imagesMoved = repointed
			}
			return nil
		}

		// The reconciler watches this Deployment and writes to it under its own
		// retry, so losing a race here is routine. Re-read and re-apply rather
		// than failing an upgrade the operator would only have to run again.
		if err := updateDeploymentWithRetry(ctx, clientset, comp.namespace, comp.name, deploy, mutate); err != nil {
			return fmt.Errorf("upgrading %s: %w", comp.name, err)
		}

		if hardened {
			moved += " (pod hardening applied)"
		}
		for _, m := range imagesMoved {
			moved += " (" + m + ")"
		}

		// The API accepting a template is not the same as the cluster running it.
		// With a surge-only strategy the previous pod keeps serving, so a new pod
		// that cannot start leaves everything looking healthy while none of this
		// release is live — the pod hardening and the new heartbeat code are
		// exactly the kind of change that meets a long-lived cluster for the
		// first time here.
		if err := waitForRollout(ctx, clientset, comp.namespace, comp.name, rolloutTimeout); err != nil {
			fmt.Printf("  ✗  %s%s\n", comp.name, moved)
			return err
		}

		fmt.Printf("  ✔  %s%s\n", comp.name, moved)

		if comp.name == consoleAPIName {
			// The revision this upgrade just rolled, taken here because this is
			// the only moment anything knows which one that is. The build stamp
			// it will record is an annotation and outlives the pod that wrote
			// it, so reading the stamp later says a console-api recorded itself
			// and not which one is serving now. The components after this take
			// their own time to roll, and a rollback landing in that stretch
			// would otherwise be adopted as the rollout to wait out.
			grants.rolledConsoleAPI = true
			pinned, pinErr := pinConsoleAPIRollout(ctx, clientset)
			if pinErr != nil {
				// Said out loud, and the migration stays open for it. Silently
				// carrying on would leave the closing pass to look the rollout
				// up for itself at the end, by which point a rollback could
				// have landed and would be what it found.
				fmt.Printf("  !   Could not record which console-api this upgrade rolled: %v\n"+
					"      The shared credential migration stays open, and the next upgrade finishes it.\n", pinErr)
			}
			grants.rolled = pinned
		}
	}

	// Again, now that the writer which erases an allow-list is gone, and only
	// now is the cluster recorded as migrated: a grant the old pod replaced
	// while it was still serving is written back here, where marking the
	// migration before this ran would have left the build refused for good.
	if err := closeSharedCredentialGrants(ctx, clientset, os.Stdout, grants); err != nil {
		return err
	}

	// Which namespaces their projects have not recorded taking. The console
	// publishes those records as it reconciles and the next release resolves
	// ownership from them, so this is the operator's chance to see a namespace
	// that is about to stop answering to anybody while it is still harmless.
	// It reads and prints; it changes nothing.
	reportNamespacesWithoutAClaim(ctx, clientset, k8sClient.Dynamic(), os.Stdout, claimsSettleWait)

	explicitKey, fallbackKey := resolveSSHKey(sshKey, cluster)

	// The cluster's trust material is not a system component and is not skipped
	// with them. A cluster installed before the API server was anchored on the
	// cluster's own authority has no anchor file at all, and console-api minting
	// that authority cannot put one there — nothing inside the cluster writes to
	// the host. Left missing, 'kip cluster auth sync' and 'kip cluster domain'
	// both refuse a gateway-fronted issuer they have nothing to verify, and the
	// certificate status reports failing logins and sends the operator into a
	// full authority replacement over a file that was never written.
	//
	// It is announced before it runs because it reaches the host over SSH, which
	// --skip-system otherwise never does, and an operator who passed that flag
	// has said they do not want the host touched today.
	fmt.Printf("\n  The cluster's certificate authority and the trust anchor the API server\n")
	fmt.Printf("  verifies logins against are reconciled over SSH. This runs on every upgrade,\n")
	fmt.Printf("  including with --skip-system, because it is this cluster's own identity\n")
	fmt.Printf("  rather than a component version.\n")
	if err := ensureTrustMaterial(cluster, explicitKey, fallbackKey); err != nil {
		return err
	}

	if skipSystem {
		fmt.Printf("\n  System components skipped (--skip-system).\n")
		fmt.Printf("  Kipper CRDs, console and trust material are up to date. Run 'kip upgrade'\n")
		fmt.Printf("  without --skip-system to reconcile cluster components.\n\n")
		return nil
	}

	state, err := loadPlatformState(ctx, k8sClient.Dynamic())
	if err != nil {
		return fmt.Errorf("reading platform state: %w", err)
	}

	// Carry the persisted backup-storage mode into the upgrade state.
	// Credentials are not persisted in ~/.kip/config.yaml; for external
	// mode they stay on the cluster as a Kubernetes Secret and the
	// upgrade preserves them rather than rewriting.
	if cluster.BackupStorage != nil && cluster.BackupStorage.Mode == "external" {
		state.BackupStorage = &installer.BackupStorageConfig{
			Bucket:   cluster.BackupStorage.Bucket,
			Region:   cluster.BackupStorage.Region,
			Endpoint: cluster.BackupStorage.Endpoint,
		}
	}

	return runSystemUpgrade(cluster.Host, explicitKey, fallbackKey, cluster.Domain, cluster.DNSResolvers, cluster.TrustedProxies, state, autoYes)
}

// ensureTrustMaterial brings the host's copy of the cluster's certificate
// authority up to date: the authority itself, the certificate it signs, and the
// trust anchor the API server verifies logins against.
//
// It is idempotent and does nothing on a cluster that already has all three,
// which is why it runs on every upgrade rather than being something an operator
// has to know to ask for. A cluster with no host recorded is left alone: that
// configuration cannot be reached over SSH at all, and failing the upgrade over
// it would be worse than the gap.
// The host-side steps of an upgrade, as function values so a test can drive the
// sequence production runs. What matters is the order: the API server gets its
// arguments before anything asks it to load an authentication config, because
// on an old cluster there is no flag for it to load one with.
var (
	ensureHopMaterial     = func(c *ssh.Client) error { return installer.EnsureHopMaterial(c) }
	ensureAPIServerConfig = func(c *ssh.Client, notify func(string)) (bool, error) {
		return installer.EnsureAPIServerConfig(c, notify)
	}
	syncOperatorAuth   = installer.SyncOperatorAuthIfConfigured
	ensureOperatorAuth = installer.EnsureOperatorAuth
)

func ensureTrustMaterial(cluster *config.Cluster, explicitKey, fallbackKey string) error {
	host := cluster.Host
	if host == "" {
		return nil
	}
	provider := &infra.BareMetalProvider{
		Host:           host,
		SSHKey:         explicitKey,
		FallbackSSHKey: fallbackKey,
	}
	if err := provider.Connect(); err != nil {
		return fmt.Errorf("connecting to %s: %w", host, err)
	}
	defer func() { _ = provider.Close() }()

	return reconcileHostTrust(provider.Client(), cluster.DexHost())
}

// reconcileHostTrust is everything an upgrade does on the server itself.
func reconcileHostTrust(client *ssh.Client, dexHost string) error {
	fmt.Printf("\n  ...  Certificate authority and trust anchor\n")
	if err := ensureHopMaterial(client); err != nil {
		return fmt.Errorf("ensuring the cluster certificate authority: %w", err)
	}

	// A cluster installed before kip configured the API server has no
	// authenticator at all, so operator login cannot work there however often
	// the anchor is reconciled. The arguments live on the host, which is why
	// only something reaching over SSH can add them.
	apiserverChanged, err := ensureAPIServerConfig(client, func(m string) {
		fmt.Printf("  ...  %s\n", m)
	})
	if err != nil {
		return fmt.Errorf("bringing the API server arguments up to date: %w", err)
	}
	if apiserverChanged {
		fmt.Printf("  ✔  API server arguments, and k3s restarted on them\n")
	}

	// Writing the anchor is half the job. The API server reads that file only
	// when its authentication config is re-rendered, so an upgrade that wrote
	// the anchor and stopped left the exact cluster this exists for — one
	// migrating onto a per-cluster authority — still unable to verify a login,
	// while reporting success. Install has always synced afterwards; this path
	// did not.
	//
	// A cluster with no issuer configured has nothing to render and is left
	// alone rather than failed: OIDC is not set up there, so there is no trust
	// to repair.
	synced, err := syncOperatorAuth(client)
	if err != nil {
		return fmt.Errorf("loading the trust anchor into the API server: %w", err)
	}
	if synced {
		fmt.Printf("  ✔  Certificate authority, trust anchor, and the API server has loaded it\n")
		return nil
	}

	// No issuer configured. On a cluster old enough to have needed the
	// arguments above, that is the rest of the same repair: the API server is
	// running the zero-authenticator stub, and this is what gives it Dex.
	//
	// A failure here is reported rather than raised. The cluster is then where
	// it already was, with no authenticator and certificate authentication
	// untouched, so failing the whole upgrade over it would turn a cluster that
	// cannot reach its own Dex into a cluster that cannot be upgraded either.
	if dexHost == "" {
		fmt.Printf("  ✔  Certificate authority and trust anchor (no domain recorded, so no login issuer to configure)\n")
		return nil
	}
	fmt.Printf("  ...  Operator login against %s\n", dexHost)
	if err := ensureOperatorAuth(client, dexHost); err != nil {
		fmt.Printf("  ⚠  Operator login is still not configured: %v\n", err)
		fmt.Printf("     The API server holds no authenticator, which is where this cluster already was,\n")
		fmt.Printf("     and certificate authentication is unaffected. Run 'kip upgrade' again once\n")
		fmt.Printf("     %s serves its discovery document.\n", dexHost)
		return nil
	}
	fmt.Printf("  ✔  Operator login configured. Run 'kip auth login', then 'kip auth kubeconfig'\n")
	return nil
}

// Names and timings for the console-layer upgrade.
const (
	consoleAPIName    = "console-api"
	kipperSystemNS    = "kipper-system"
	rolloutTimeout    = 5 * time.Minute
	rolloutPollPeriod = 2 * time.Second
)

// updateDeploymentWithRetry applies mutate to the Deployment and writes it back,
// re-reading and re-applying if another writer got there first. The first attempt
// reuses the object the caller already read, so the common case costs no extra
// API call.
func updateDeploymentWithRetry(ctx context.Context, clientset kubernetes.Interface,
	namespace, name string, first *appsv1.Deployment, mutate func(*appsv1.Deployment) error) error {
	deploy := first
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if deploy == nil {
			fresh, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			deploy = fresh
		}
		if err := mutate(deploy); err != nil {
			return err
		}
		_, err := clientset.AppsV1().Deployments(namespace).Update(ctx, deploy, metav1.UpdateOptions{})
		if err != nil {
			// Force a re-read on the next attempt; the local copy is stale.
			deploy = nil
		}
		return err
	})
}

// waitForRollout blocks until the Deployment has actually rolled the spec it was
// just given, and says what is wrong when it does not. Without this the upgrade
// reports success on an accepted template while the previous pod keeps serving,
// which is indistinguishable from a working upgrade until something downstream —
// a missing heartbeat, an unproven registration — surfaces days later.
func waitForRollout(ctx context.Context, clientset kubernetes.Interface, namespace, name string,
	budget time.Duration) error {
	deadline := time.Now().Add(budget)
	for {
		dep, err := clientset.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("reading %s while waiting for its rollout: %w", name, err)
		}
		if rollout.Ready(dep) {
			return nil
		}
		if rollout.Failed(dep) {
			return fmt.Errorf("%s did not roll out: %s (inspect it with `kubectl -n %s describe deploy %s`)",
				name, rollout.Message(dep), namespace, name)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%s was still rolling out after %s: %d/%d replicas updated, %d available (inspect it with `kubectl -n %s describe deploy %s`)",
				name, budget, dep.Status.UpdatedReplicas, wantReplicas(dep), dep.Status.AvailableReplicas,
				namespace, name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(rolloutPollPeriod):
		}
	}
}

// wantReplicas is the Deployment's desired replica count, defaulting as the API does.
func wantReplicas(dep *appsv1.Deployment) int32 {
	if dep.Spec.Replicas != nil {
		return *dep.Spec.Replicas
	}
	return 1
}

// applyConsoleAPIHardening brings a live console-api Deployment up to the pod
// spec a fresh install renders: the pod and container security contexts, and the
// writable /tmp the read-only root filesystem needs. Volumes and mounts are
// merged by name rather than replaced, so anything an operator added to the
// Deployment survives. Reports whether it changed anything.
// platformImageEnv names the console-api env vars that carry a first-party image
// reference this kip build owns. They are not cluster state: they name images
// OTHER pods run — the instance-proxy sidecar attached to every app, and the
// migration data mover — so a cluster installed before one of those images moved
// keeps handing out a reference nothing can pull, while console-api's own image
// is perfectly current. Nothing else is reconciled from the manifest's env,
// which is why this is an explicit list and not a name pattern: cluster identity
// and operator controls live in the same block, and a future manifest variable
// must be added here deliberately rather than captured by a suffix.
//
// An operator who pins one of these by hand has it reset on the next upgrade.
// The upgrade names every value it moves so that is visible rather than silent.
var platformImageEnv = []string{"SIDECAR_IMAGE", "DATAMOVER_IMAGE"}

// applyPlatformImageEnv reconciles the platformImageEnv vars onto the references
// this build ships and returns one line per value it moved.
func applyPlatformImageEnv(deploy *appsv1.Deployment) ([]string, error) {
	desired, err := installer.DesiredConsoleAPIDeployment()
	if err != nil {
		return nil, err
	}
	wantIdx := consoleAPIContainerIndex(desired.Spec.Template.Spec.Containers)
	if wantIdx < 0 {
		return nil, fmt.Errorf("console-api container not found in the rendered install manifest")
	}
	liveIdx := consoleAPIContainerIndex(deploy.Spec.Template.Spec.Containers)
	if liveIdx < 0 {
		return nil, fmt.Errorf("console-api container not found in its deployment")
	}

	shipped := map[string]string{}
	for _, e := range desired.Spec.Template.Spec.Containers[wantIdx].Env {
		if e.Value != "" {
			shipped[e.Name] = e.Value
		}
	}

	live := &deploy.Spec.Template.Spec.Containers[liveIdx]
	var moved []string
	for _, name := range platformImageEnv {
		want, ok := shipped[name]
		if !ok {
			continue
		}
		found := false
		// Every entry with this name is written, not just the first: a Deployment
		// carrying the name twice is served the last one, so stopping early would
		// report a move that the pod never sees.
		for i := range live.Env {
			if live.Env[i].Name != name {
				continue
			}
			found = true
			if live.Env[i].Value == want && live.Env[i].ValueFrom == nil {
				continue
			}
			from := live.Env[i].Value
			if live.Env[i].ValueFrom != nil {
				from = "a reference"
				// value and valueFrom are mutually exclusive; leaving both set
				// makes the Deployment invalid and fails the whole upgrade.
				live.Env[i].ValueFrom = nil
			}
			if from == "" {
				from = "unset"
			}
			live.Env[i].Value = want
			moved = append(moved, fmt.Sprintf("%s: %s → %s", name, from, want))
		}
		if !found {
			live.Env = append(live.Env, corev1.EnvVar{Name: name, Value: want})
			moved = append(moved, fmt.Sprintf("%s: unset → %s", name, want))
		}
	}
	return moved, nil
}

func applyConsoleAPIHardening(deploy *appsv1.Deployment) (bool, error) {
	desired, err := installer.DesiredConsoleAPIDeployment()
	if err != nil {
		return false, err
	}
	want := desired.Spec.Template.Spec
	live := &deploy.Spec.Template.Spec
	changed := false

	if !reflect.DeepEqual(live.SecurityContext, want.SecurityContext) {
		live.SecurityContext = want.SecurityContext
		changed = true
	}
	idx := consoleAPIContainerIndex(live.Containers)
	if idx < 0 {
		return false, fmt.Errorf("console-api container not found in its deployment")
	}
	wantIdx := consoleAPIContainerIndex(want.Containers)
	if wantIdx < 0 {
		return false, fmt.Errorf("console-api container not found in the rendered install manifest")
	}
	if !reflect.DeepEqual(live.Containers[idx].SecurityContext, want.Containers[wantIdx].SecurityContext) {
		live.Containers[idx].SecurityContext = want.Containers[wantIdx].SecurityContext
		changed = true
	}
	// The installer owns the names it ships: a live volume or mount that shares a
	// name but not a definition is reconciled onto the desired one, because
	// turning on a read-only root filesystem next to a "tmp" that is not a
	// writable /tmp gives a pod that cannot run. Anything named differently is
	// left exactly as it is.
	for _, v := range want.Volumes {
		switch i := volumeIndex(live.Volumes, v.Name); {
		case i < 0:
			live.Volumes = append(live.Volumes, v)
			changed = true
		case !reflect.DeepEqual(live.Volumes[i], v):
			live.Volumes[i] = v
			changed = true
		}
	}
	for _, m := range want.Containers[wantIdx].VolumeMounts {
		ctr := &live.Containers[idx]
		switch i := mountIndex(ctr.VolumeMounts, m.Name); {
		case i < 0:
			ctr.VolumeMounts = append(ctr.VolumeMounts, m)
			changed = true
		case !reflect.DeepEqual(ctr.VolumeMounts[i], m):
			ctr.VolumeMounts[i] = m
			changed = true
		}
	}
	return changed, nil
}

func consoleAPIContainerIndex(containers []corev1.Container) int {
	for i := range containers {
		if containers[i].Name == consoleAPIName {
			return i
		}
	}
	return -1
}

func volumeIndex(volumes []corev1.Volume, name string) int {
	for i := range volumes {
		if volumes[i].Name == name {
			return i
		}
	}
	return -1
}

func mountIndex(mounts []corev1.VolumeMount, name string) int {
	for i := range mounts {
		if mounts[i].Name == name {
			return i
		}
	}
	return -1
}

// kipperRunSuffix is what makes a domain a gateway registration rather than a
// cluster's own name.
const kipperRunSuffix = ".kipper.run"

// clusterIdentityName is the singleton every cluster carries.
const clusterIdentityName = "cluster"

var clusterIdentityGVR = schema.GroupVersionResource{
	Group:    "kipper.run",
	Version:  "v1alpha1",
	Resource: "clusteridentities",
}

// reconcileGatewayIdentity records on the ClusterIdentity what the gateway
// heartbeat needs: the kipper.run domain and the public host. The serving
// reconciler renders the console-api env from the CR, so anything the CR cannot
// express is at the mercy of whatever the last host transition left behind — and
// a transition that did not know the host used to blank CLUSTER_HOST, silently
// stopping the heartbeat, the hop pin, and the registration proof.
//
// The kipper.run domain is adopted from the live Deployment when the CR carries
// no gateway block, because the running cluster is the only place it survives on
// a cluster installed before the block existed. Nothing is invented: a cluster
// with no gateway registration anywhere is left alone and reported, since only
// the operator knows whether it should still hold a *.kipper.run name.
func reconcileGatewayIdentity(ctx context.Context, dyn dynamic.Interface, clientset kubernetes.Interface, host string) error {
	ci, err := dyn.Resource(clusterIdentityGVR).Get(ctx, clusterIdentityName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	// An opt-out is a decision about registering, not a reason to forget what the
	// cluster is. Record the facts either way and never touch register: the env is
	// cleared while it is false, so if the values only lived there, an operator
	// turning registration back on would otherwise find nothing to turn on.
	register, hasRegister, _ := unstructured.NestedBool(ci.Object, "spec", "gateway", "register")
	optedOut := hasRegister && !register

	currentDomain, _, _ := unstructured.NestedString(ci.Object, "spec", "gateway", "kipperRunDomain")
	kipperRunDomain := currentDomain
	if kipperRunDomain == "" {
		// Only a *.kipper.run name is a gateway registration. An older install
		// could leave the cluster's own custom domain in this variable, and
		// adopting that would record a name the gateway can never serve.
		if live := liveEnvValue(ctx, clientset, "KIPPER_RUN_DOMAIN"); strings.HasSuffix(live, kipperRunSuffix) {
			kipperRunDomain = live
		}
	}
	if kipperRunDomain == "" {
		if optedOut {
			fmt.Printf("  ·   Gateway registration is off for this cluster (gateway.register=false).\n")
			return nil
		}
		fmt.Printf("  !   No *.kipper.run registration recorded for this cluster; its gateway heartbeat stays off.\n")
		// A cluster that has moved to a custom domain still has its old identity
		// on the CR. Naming it turns "something is missing" into one command,
		// without deciding for the operator: reviving a name the cluster gave up
		// is theirs to choose.
		if previous := formerKipperRunDomain(ci); previous != "" {
			fmt.Printf("      This cluster used to serve %s. To keep that name, set spec.gateway.kipperRunDomain\n", previous)
			fmt.Printf("      to it and spec.gateway.clusterHost to the cluster's public IP on the ClusterIdentity.\n")
			return nil
		}
		fmt.Printf("      If it should hold a kipper.run name, set spec.gateway on the ClusterIdentity.\n")
		return nil
	}

	currentHost, _, _ := unstructured.NestedString(ci.Object, "spec", "gateway", "clusterHost")
	liveHost := liveEnvValue(ctx, clientset, "CLUSTER_HOST")
	// The gateway registers an address, not a name: it refuses anything that is
	// not a public IP. Record nothing rather than something unusable — and prefer
	// the address the cluster is already registering with over the one in the
	// local config, which can be stale in a way the cluster-side values cannot:
	// replacing a working address would break renewal, pinning, and proof at once.
	recordHost := firstRoutableIP(currentHost, liveHost, host)
	if recordHost == "" {
		fmt.Printf("  !   %s has no usable public IP recorded; its gateway heartbeat cannot register.\n", kipperRunDomain)
		fmt.Printf("      Set spec.gateway.clusterHost on the ClusterIdentity to the cluster's public IP.\n")
		return nil
	}
	// A disagreement here is not something an upgrade can settle: the gateway ties
	// a registration to one address, so a server that genuinely moved needs a new
	// registration rather than a quiet edit. Keep what is recorded and report every
	// other address in play — after a real move the Deployment can still carry the
	// recorded one (steady reconciliation puts it back), which leaves the host kip
	// itself reached as the only signal that anything changed.
	if others := otherRoutableIPs(recordHost, liveHost, host); len(others) > 0 {
		fmt.Printf("  !   %s is recorded at %s, but this cluster also answers at %s.\n",
			kipperRunDomain, recordHost, strings.Join(others, ", "))
		fmt.Printf("      Keeping %s. A cluster that has really moved needs a fresh registration, not an edited record.\n", recordHost)
	}
	if currentHost == recordHost && currentDomain == kipperRunDomain {
		return nil
	}
	recorded, err := recordGatewayIdentity(ctx, dyn, kipperRunDomain, liveHost, host)
	if err != nil {
		return err
	}
	if recorded == "" {
		return nil
	}
	if optedOut {
		fmt.Printf("  ·   Gateway identity recorded (%s at %s); registration stays off (gateway.register=false)\n", kipperRunDomain, recorded)
		return nil
	}
	fmt.Printf("  ✔  Gateway identity recorded (%s at %s)\n", kipperRunDomain, recorded)
	return nil
}

// recordGatewayIdentity writes the domain and address onto the ClusterIdentity,
// re-reading on conflict. The reconciler writes CR status on nearly every pass,
// so losing that race is routine and must not end an upgrade — and because the
// address is re-decided against the freshly read object on every attempt, a value
// another writer recorded in the meantime still wins over the local config.
// Returns the address recorded, or "" when the CR already said the same thing.
func recordGatewayIdentity(ctx context.Context, dyn dynamic.Interface,
	domain, liveHost, cliHost string) (string, error) {
	var recorded string
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ci, err := dyn.Resource(clusterIdentityGVR).Get(ctx, clusterIdentityName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		currentDomain, _, _ := unstructured.NestedString(ci.Object, "spec", "gateway", "kipperRunDomain")
		currentHost, _, _ := unstructured.NestedString(ci.Object, "spec", "gateway", "clusterHost")
		// A domain recorded on the CR outranks the one this run recovered, for the
		// same reason a recorded address does: it is the cluster's own statement of
		// its identity. Standing down also keeps the pair intact — combining a
		// freshly read address with the domain captured before the conflict would
		// record a registration that never existed.
		if currentDomain != "" && currentDomain != domain {
			recorded = ""
			return nil
		}
		host := firstRoutableIP(currentHost, liveHost, cliHost)
		if host == "" {
			recorded = ""
			return nil
		}
		if currentDomain == domain && currentHost == host {
			recorded = ""
			return nil
		}
		if err := unstructured.SetNestedField(ci.Object, domain, "spec", "gateway", "kipperRunDomain"); err != nil {
			return err
		}
		if err := unstructured.SetNestedField(ci.Object, host, "spec", "gateway", "clusterHost"); err != nil {
			return err
		}
		if _, err := dyn.Resource(clusterIdentityGVR).Update(ctx, ci, metav1.UpdateOptions{}); err != nil {
			return err
		}
		recorded = host
		return nil
	})
	return recorded, err
}

// formerKipperRunDomain returns the *.kipper.run identity this cluster served
// before it moved, read from the identity history the reconciler keeps on the
// CR. Empty when the cluster never had one.
func formerKipperRunDomain(ci *unstructured.Unstructured) string {
	for _, path := range [][]string{
		{"status", "lastSteady", "domain"},
		{"status", "steady", "domain"},
	} {
		if d, _, _ := unstructured.NestedString(ci.Object, path...); strings.HasSuffix(d, ".kipper.run") {
			return d
		}
	}
	return ""
}

// otherRoutableIPs returns the distinct routable addresses among the candidates
// that are not the one being kept.
func otherRoutableIPs(kept string, candidates ...string) []string {
	var others []string
	seen := []string{kept}
	for _, c := range candidates {
		if !pubip.IsPublic(c) || containsAddress(seen, c) {
			continue
		}
		seen = append(seen, c)
		others = append(others, c)
	}
	return others
}

// containsAddress reports whether any entry denotes the same address as c,
// comparing what they parse to rather than how they are spelled.
func containsAddress(addresses []string, c string) bool {
	for _, a := range addresses {
		if pubip.SameAddress(a, c) {
			return true
		}
	}
	return false
}

// firstRoutableIP returns the first candidate the gateway would accept: a
// routable public IP, checked against the same policy the gateway applies when
// it registers one.
func firstRoutableIP(candidates ...string) string {
	for _, c := range candidates {
		if pubip.IsPublic(c) {
			return c
		}
	}
	return ""
}

// liveEnvValue reads one env var off the running console-api container, which is
// where a value the CR never carried still survives.
func liveEnvValue(ctx context.Context, clientset kubernetes.Interface, name string) string {
	deploy, err := clientset.AppsV1().Deployments(kipperSystemNS).Get(ctx, consoleAPIName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	idx := consoleAPIContainerIndex(deploy.Spec.Template.Spec.Containers)
	if idx < 0 {
		return ""
	}
	for _, e := range deploy.Spec.Template.Spec.Containers[idx].Env {
		if e.Name == name {
			return e.Value
		}
	}
	return ""
}

var platformConfigGVR = schema.GroupVersionResource{
	Group:    "kipper.run",
	Version:  "v1alpha1",
	Resource: "platformconfigs",
}

// loadPlatformState reads the active sizing profile plus per-component
// overrides from the PlatformConfig CR that `kip install` writes. NotFound
// here means the CR is absent (never installed, deletion race, RBAC change,
// manual kubectl delete). Treat that as fatal rather than synthesizing a
// small profile that could downsize a real cluster.
//
// Any other error (RBAC, discovery lag, transient API failure) is also
// surfaced so the upgrade aborts rather than silently downsizing
// Prometheus and Loki.
func loadPlatformState(ctx context.Context, dyn dynamic.Interface) (installer.PlatformState, error) {
	obj, err := dyn.Resource(platformConfigGVR).Get(ctx, "platform", metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return installer.PlatformState{}, fmt.Errorf(
				"PlatformConfig %q not found: refusing to proceed with assumed defaults that could downsize Prometheus or Loki; run `kip install` to create it",
				"platform")
		}
		return installer.PlatformState{}, err
	}
	profile, _, err := unstructured.NestedString(obj.Object, "spec", "profile")
	if err != nil {
		return installer.PlatformState{}, fmt.Errorf("reading spec.profile: %w", err)
	}
	if profile == "" {
		profile = installer.ProfileSmall
	}

	state := installer.PlatformState{
		Profile:          profile,
		MemoryOverrides:  map[string]string{},
		EnabledOverrides: map[string]bool{},
	}

	raw, _, _ := unstructured.NestedSlice(obj.Object, "spec", "components")
	for _, item := range raw {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := m["name"].(string)
		if name == "" {
			continue
		}
		if v, _ := m["memoryLimit"].(string); v != "" {
			state.MemoryOverrides[name] = v
		}
		if v, ok := m["enabled"].(bool); ok {
			state.EnabledOverrides[name] = v
		}
	}

	return state, nil
}

// runSystemUpgrade re-applies cluster system components (Traefik, Longhorn,
// KEDA, etc.) at the versions pinned in this kip build. Prompts for
// confirmation unless autoYes is set or stdin is not a terminal.
func runSystemUpgrade(host, explicitKey, fallbackKey, domain string, dnsResolvers, trustedProxies []string, state installer.PlatformState, autoYes bool) error {
	components := installer.SystemComponents(domain, dnsResolvers, trustedProxies, state)

	fmt.Printf("\n  System components to reconcile on %s:\n", host)
	for _, c := range components {
		fmt.Printf("    - %s\n", c.Name)
	}
	fmt.Printf("\n  Each component is re-applied at the version pinned in this kip build.\n")
	fmt.Printf("  Helm-controller upgrades charts in place; a failed chart can disrupt running workloads.\n\n")

	if !autoYes {
		ok, err := confirmInteractive("  Continue with system upgrade? [y/N] ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Printf("\n  System upgrade cancelled. Kipper CRDs and console were already restarted.\n\n")
			return nil
		}
	}

	provider := &infra.BareMetalProvider{
		Host:           host,
		SSHKey:         explicitKey,
		FallbackSSHKey: fallbackKey,
	}
	if err := provider.Connect(); err != nil {
		return fmt.Errorf("connecting to %s: %w", host, err)
	}
	defer func() { _ = provider.Close() }()

	fmt.Println()
	if err := installer.RunSystemUpgrade(provider.Client(), components); err != nil {
		return err
	}

	fmt.Printf("\n  Upgrade complete. Cluster components reconciled.\n\n")
	return nil
}

// confirmInteractive returns true when the user types y/yes at the prompt.
// In non-interactive contexts (no TTY on stdin) it returns an error pointing
// the user at --yes — prompting in a script would silently hang.
func confirmInteractive(prompt string) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, fmt.Errorf("stdin is not a terminal. Pass --yes to confirm system upgrade non-interactively, or --skip-system")
	}
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	return parseYesNo(line), nil
}

// confirmInteractiveNonFatal is confirmInteractive's sibling for a prompt the
// upgrade must survive without a TTY. A scripted run declines rather than
// aborts, so the upgrade completes and the caller prints how to answer the
// prompt next time. Interactive runs read y/N through the same path.
func confirmInteractiveNonFatal(prompt string) (bool, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return false, nil
	}
	fmt.Print(prompt)
	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("reading confirmation: %w", err)
	}
	return parseYesNo(line), nil
}

// parseYesNo treats only "y" / "yes" (case-insensitive, surrounding
// whitespace ignored) as a positive answer. Anything else — including
// empty input, "yep", "yeah", or "ok" — counts as no, so an unsure
// user pressing Enter does not unintentionally trigger the upgrade.
func parseYesNo(line string) bool {
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes"
}

// storedVersionsDroppedBy reports the API versions the live CRD still has
// objects stored under that the incoming CRD no longer declares.
//
// The apiserver already refuses that update, so this is an earlier and clearer
// refusal rather than the only thing standing between an older CLI and data
// loss. It earns its place by naming the cause before any CRD is written, and
// by covering the case where storedVersions is not reported at all.
//
// storedVersions is the exact question, and comparing served versions was the
// wrong one: it blocked the deprecation path the versioning plan prescribes,
// because a release that legitimately retires an old served version looks
// identical to an older CLI carrying older CRDs. Kubernetes already draws this
// line — a version may only leave spec.versions once nothing is stored under it
// — so honouring storedVersions permits a real retirement and still refuses the
// change that strands data.
//
// Comparison is against every declared version, served or not, because the
// schema has to remain present for Kubernetes to decode what is stored.
//
// This answers only whether a version *name* disappears, which leaves two ways
// an older kip could still overwrite a newer cluster: a field or validation
// rule added to an existing version, where no name changes at all; and a served
// version that has not yet become the storage version, which never appears in
// storedVersions and so looks like nothing was dropped.
//
// Both are the same question — is this binary older than what it is about to
// overwrite — and neither is answerable from the schema. They are covered by
// the version stamp instead, which is checked alongside this one in applyCRDs.
// See crd_version_stamp.go.
//
// Comparing openAPIV3Schema deeply enough to tell a regression from a
// legitimate field removal was considered and rejected: it would block real
// upgrades more often than it caught this, and the stamp answers directly.
func storedVersionsDroppedBy(existing, incoming *unstructured.Unstructured) []string {
	declared := map[string]bool{}
	for _, v := range crdDeclaredVersions(incoming) {
		declared[v] = true
	}
	stored, found, err := unstructured.NestedStringSlice(existing.Object, "status", "storedVersions")
	if err != nil || !found {
		// A CRD with no reported storedVersions tells us nothing, so fall back
		// to the declared set: dropping a version we cannot prove is empty is
		// the case worth refusing.
		stored = crdDeclaredVersions(existing)
	}
	var dropped []string
	for _, v := range stored {
		if !declared[v] {
			dropped = append(dropped, v)
		}
	}
	return dropped
}

// crdDeclaredVersions lists every version a CRD declares, served or not.
func crdDeclaredVersions(crd *unstructured.Unstructured) []string {
	return crdVersionNames(crd, false)
}

// crdServedVersions lists only the versions a CRD serves.
func crdServedVersions(crd *unstructured.Unstructured) []string {
	return crdVersionNames(crd, true)
}

// crdVersionNames lists a CRD's version names, optionally restricted to those
// it serves.
func crdVersionNames(crd *unstructured.Unstructured, servedOnly bool) []string {
	versions, found, err := unstructured.NestedSlice(crd.Object, "spec", "versions")
	if err != nil || !found {
		return nil
	}
	var names []string
	for _, raw := range versions {
		entry, ok := raw.(map[string]interface{})
		if !ok {
			continue
		}
		if servedOnly {
			// Kubernetes requires served to be a boolean. Absent, or any other
			// type, is not a served version, and reading it as one let a
			// malformed embedded CRD past the broken-build guard below.
			served, ok := entry["served"].(bool)
			if !ok || !served {
				continue
			}
		}
		if name, ok := entry["name"].(string); ok && name != "" {
			names = append(names, name)
		}
	}
	return names
}

func applyCRDs(ctx context.Context, dynClient dynamic.Interface, kipVersion string, out io.Writer) error {
	entries, err := upgradeCRDs.ReadDir("crds")
	if err != nil {
		return fmt.Errorf("reading embedded CRDs: %w", err)
	}

	// Two passes. Every CRD is read and checked before any is written, because
	// this loop mutates cluster state irreversibly: a single pass that refuses
	// on the fifth CRD has already replaced the first four, and Kubernetes has
	// already pruned stored objects under them. Refusing has to mean nothing
	// happened.
	type plannedCRD struct {
		name   string
		obj    *unstructured.Unstructured
		exists bool
	}
	var plan []plannedCRD

	for _, entry := range entries {
		data, readErr := upgradeCRDs.ReadFile("crds/" + entry.Name())
		if readErr != nil {
			return fmt.Errorf("reading CRD %s: %w", entry.Name(), readErr)
		}

		var obj unstructured.Unstructured
		if err := yaml.Unmarshal(data, &obj.Object); err != nil {
			return fmt.Errorf("parsing CRD %s: %w", entry.Name(), err)
		}
		name := obj.GetName()

		// A CRD declaring no served version is a broken embedded file rather
		// than a downgrade, and blaming the cluster for it would send the
		// operator looking in the wrong place.
		if len(crdServedVersions(&obj)) == 0 {
			return fmt.Errorf("embedded CRD %s declares no served version, so this kip build cannot be trusted to apply it", name)
		}

		existing, getErr := dynClient.Resource(crdGVR).Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			if !apierrors.IsNotFound(getErr) {
				return fmt.Errorf("reading CRD %s: %w", name, getErr)
			}
			plan = append(plan, plannedCRD{name: name, obj: obj.DeepCopy()})
			continue
		}

		// This Update is a full replace, so an older kip rewrites a newer
		// cluster's schema with its own. Two things are checked before that can
		// happen, and both refuse before anything is written.
		//
		// First, whether this binary is older than the one that wrote the live
		// CRD. That covers the cases a version-name comparison cannot see: a
		// field or validation rule added to an existing version, and a served
		// version that has not yet become the storage version.
		if live, mine, newer := installer.ClusterIsNewerThan(crdWriterVersion(existing), kipVersion); newer {
			return fmt.Errorf("refusing to apply CRD %s: the cluster's schema was written by kip %s and this is kip %s, "+
				"so applying it would replace a newer schema with an older one and prune anything the newer one added. Upgrade kip first",
				name, live, mine)
		} else if live != "" && mine == "" {
			// The stamp is orderable and this build is not, so the check above
			// could not run. Saying so beats letting a source build look like it
			// passed a guard it never reached.
			progressf(out, "  !   %s was written by kip %s; this build reports %q, which cannot be ordered against it, so the schema-age check was skipped.\n",
				name, live, kipVersion)
		}

		// Second, whether a version objects are stored under disappears. The
		// apiserver would also refuse that, later and less clearly.
		if dropped := storedVersionsDroppedBy(existing, &obj); len(dropped) > 0 {
			return fmt.Errorf("refusing to apply CRD %s: this kip does not declare API version(s) %s that the cluster still has objects stored under, "+
				"so applying it would strand them. Either the cluster is newer than this CLI (upgrade kip first), "+
				"or that version needs a storage migration before it can be retired",
				name, strings.Join(dropped, ", "))
		}

		obj.SetResourceVersion(existing.GetResourceVersion())
		// The Update is a full replace, so anything the cluster added to the
		// CRD's metadata goes with it unless it is carried across: an ArgoCD
		// sync option, a Helm ownership label, an operator's own note. The
		// embedded manifest still wins on any key it sets, because that is the
		// schema this build ships.
		carryOverMetadata(existing, &obj)
		plan = append(plan, plannedCRD{name: name, obj: obj.DeepCopy(), exists: true})
	}

	for _, p := range plan {
		// Stamped on the way out, on both paths, so the next run can tell
		// whether it is older than whatever wrote this. A build whose version
		// cannot be ordered still stamps what it has: the annotation records
		// who wrote the schema, and deciding what that is worth belongs to the
		// reader.
		stampCRDWriter(p.obj, kipVersion)

		if !p.exists {
			if _, createErr := dynClient.Resource(crdGVR).Create(ctx, p.obj, metav1.CreateOptions{}); createErr != nil {
				return fmt.Errorf("creating CRD %s: %w", p.name, createErr)
			}
			continue
		}
		if _, updateErr := dynClient.Resource(crdGVR).Update(ctx, p.obj, metav1.UpdateOptions{}); updateErr != nil {
			return fmt.Errorf("updating CRD %s: %w", p.name, updateErr)
		}
	}

	return nil
}

// initialAdminBindingName is the bootstrap cluster-admin grant a fresh install
// creates. Named here as well as in the installer because an upgrade has to
// look for it without re-applying the manifest that declares it.
const initialAdminBindingName = "kipper-initial-admin"

// consoleRBACGVRs maps the kinds in installer.ConsoleRBACManifest to their
// dynamic-client resources.
var consoleRBACGVRs = map[string]schema.GroupVersionResource{
	"ServiceAccount":     {Group: "", Version: "v1", Resource: "serviceaccounts"},
	"ClusterRole":        {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
	"ClusterRoleBinding": {Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"},
}

// progressf writes a progress line. A failed write to the operator's terminal
// is not a reason to abandon an upgrade, so the error is dropped deliberately
// rather than by omission.
func progressf(out io.Writer, format string, args ...interface{}) {
	_, _ = fmt.Fprintf(out, format, args...)
}

// reconcileClusterAPIState applies everything an upgrade delivers through the
// Kubernetes API before any workload restarts: the CRD schemas first, because
// later steps address the kinds they register, then the RBAC that has to reach
// existing clusters.
//
// It exists as a seam. Asserting on the appliers, or on the step table, proved
// nothing three times over: each test kept passing with the production call
// removed, because the test and runUpgrade were separate consumers of the same
// helper. A test drives this function, so deleting a step from it fails. The
// residual is the same one buildRouter has, that nothing can prove runUpgrade
// still calls this, and that deletion is at least a visible one.
func reconcileClusterAPIState(ctx context.Context, dynClient dynamic.Interface, kipVersion, domain string, out io.Writer) error {
	// A failure here has to stop the upgrade at its real cause: later steps
	// apply objects of the kinds these CRDs register, and would otherwise die
	// with a bare "no matches for kind".
	progressf(out, "  ...  Updating CRD schemas\n")
	if err := applyCRDs(ctx, dynClient, kipVersion, out); err != nil {
		progressf(out, "  ✗  CRD update failed: %v\n", err)
		return fmt.Errorf("updating CRD schemas: %w", err)
	}
	progressf(out, "  ✔  CRD schemas updated\n")

	// A permission added for a new feature only lands on fresh installs
	// otherwise, so an existing cluster serves errors from every handler and
	// every non-admin operator behind it.
	for _, step := range upgradeRBACSteps() {
		progressf(out, "  ...  %s\n", step.Name)
		if err := step.Apply(ctx, dynClient); err != nil {
			progressf(out, "  ✗  %s failed: %v\n", step.Name, err)
			return fmt.Errorf("applying %s: %w", strings.ToLower(step.Name), err)
		}
		progressf(out, "  ✔  %s\n", step.Name)
	}

	// A cluster installed before this binding existed has no other route to it.
	// Create-only: an existing one carries admins added since the install, and
	// re-applying the install-time copy would revoke every one of them.
	created, err := createInitialAdminBindingIfMissing(ctx, dynClient, domain)
	if err != nil {
		progressf(out, "  ✗  Operator admin binding failed: %v\n", err)
		return fmt.Errorf("creating the operator admin binding: %w", err)
	}
	if created {
		progressf(out, "  ✔  Operator admin binding created for admin@%s\n", domain)
	}
	return nil
}

// rbacStep is one RBAC manifest kip upgrade reconciles onto an existing cluster.
type rbacStep struct {
	Name  string
	Apply func(ctx context.Context, dynClient dynamic.Interface) error
}

// upgradeRBACSteps lists what kip upgrade re-applies. It is a table rather than
// two inline calls so a test can prove the upgrade reconciles both, and keep
// proving it: asserting on the appliers alone stays green when a call is
// dropped from runUpgrade, which is the failure this shape exists to prevent.
//
// The initial admin ClusterRoleBinding is deliberately absent. Its subjects are
// live state maintained by EnsureAdminBindingSubjects, so re-applying the
// install-time copy would reset the cluster to one admin and revoke everyone
// added since.
func upgradeRBACSteps() []rbacStep {
	return []rbacStep{
		{Name: "Console RBAC", Apply: applyConsoleRBAC},
		{Name: "Operator roles", Apply: applyOperatorClusterRoles},
	}
}

// applyConsoleRBAC creates or updates the console-api ServiceAccount,
// ClusterRole, and ClusterRoleBinding from the same manifest kip install
// applies, so permission changes ship on upgrade too.
func applyConsoleRBAC(ctx context.Context, dynClient dynamic.Interface) error {
	return applyRBACManifest(ctx, dynClient, installer.ConsoleRBACManifest, "console rbac")
}

// applyOperatorClusterRoles re-applies the three project ClusterRoles the
// membership reconciler binds per namespace.
//
// Without this an upgrade that adds a CRD kind, or widens what a deployer may
// do, leaves every non-admin operator on the old roles and they get 403s on the
// new kind. The initial admin ClusterRoleBinding is deliberately not included:
// its subjects are live state that renderAdminSubjectPatch maintains, so
// re-applying the install-time copy would revoke every admin added since.
func applyOperatorClusterRoles(ctx context.Context, dynClient dynamic.Interface) error {
	return applyRBACManifest(ctx, dynClient, installer.OperatorClusterRolesManifest, "operator cluster roles")
}

// applyRBACManifest creates or updates every document in an RBAC manifest.
func applyRBACManifest(ctx context.Context, dynClient dynamic.Interface, manifest, what string) error {
	for _, doc := range strings.Split(manifest, "\n---\n") {
		// A separator at either end, or a stray blank document, yields an empty
		// segment that unmarshals to an object with no kind. Failing the whole
		// upgrade on it would make a formatting slip in an embedded manifest an
		// outage.
		if strings.TrimSpace(doc) == "" {
			continue
		}
		var obj unstructured.Unstructured
		if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
			return fmt.Errorf("parsing %s document: %w", what, err)
		}
		gvr, ok := consoleRBACGVRs[obj.GetKind()]
		if !ok {
			return fmt.Errorf("unexpected kind %q in %s manifest", obj.GetKind(), what)
		}
		res := dynClient.Resource(gvr)
		var iface dynamic.ResourceInterface = res
		if ns := obj.GetNamespace(); ns != "" {
			iface = res.Namespace(ns)
		}
		existing, getErr := iface.Get(ctx, obj.GetName(), metav1.GetOptions{})
		if getErr != nil {
			if !apierrors.IsNotFound(getErr) {
				return fmt.Errorf("reading %s %s: %w", obj.GetKind(), obj.GetName(), getErr)
			}
			if _, createErr := iface.Create(ctx, &obj, metav1.CreateOptions{}); createErr != nil {
				return fmt.Errorf("creating %s %s: %w", obj.GetKind(), obj.GetName(), createErr)
			}
			continue
		}
		obj.SetResourceVersion(existing.GetResourceVersion())
		if _, updateErr := iface.Update(ctx, &obj, metav1.UpdateOptions{}); updateErr != nil {
			return fmt.Errorf("updating %s %s: %w", obj.GetKind(), obj.GetName(), updateErr)
		}
	}
	return nil
}

// createInitialAdminBindingIfMissing gives a cluster installed before the
// bootstrap grant existed a way to get one.
//
// The binding is deliberately absent from upgradeRBACSteps, because its
// subjects are live state that EnsureAdminBindingSubjects maintains and
// re-applying the install-time copy would revoke every admin added since. That
// reasoning holds for a cluster that has the binding and leaves one that never
// got it with no route to it at all: no OIDC identity can do anything, and
// neither install nor upgrade will fix it.
//
// So this creates and never updates. A binding that exists is left exactly as
// it is, including one whose subjects have been edited; AlreadyExists from a
// concurrent upgrade is the goal state reached by someone else, not a failure.
func createInitialAdminBindingIfMissing(ctx context.Context, dynClient dynamic.Interface, domain string) (bool, error) {
	gvr := consoleRBACGVRs["ClusterRoleBinding"]
	iface := dynClient.Resource(gvr)

	if _, err := iface.Get(ctx, initialAdminBindingName, metav1.GetOptions{}); err == nil {
		return false, nil
	} else if !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("reading %s: %w", initialAdminBindingName, err)
	}

	var obj unstructured.Unstructured
	doc := strings.TrimPrefix(strings.TrimSpace(installer.InitialAdminBindingManifest(domain)), "---\n")
	if err := yaml.Unmarshal([]byte(doc), &obj.Object); err != nil {
		return false, fmt.Errorf("parsing the initial admin binding: %w", err)
	}
	if _, err := iface.Create(ctx, &obj, metav1.CreateOptions{}); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return false, nil
		}
		return false, fmt.Errorf("creating %s: %w", initialAdminBindingName, err)
	}
	return true, nil
}
