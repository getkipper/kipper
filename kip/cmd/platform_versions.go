package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

// platformVersionNamespaces are the namespaces Kipper installs its own
// components into, matching the set the console counts as platform workloads.
var platformVersionNamespaces = []string{
	"cert-manager", "dex", "keda", "kipper-system", "kube-system",
	"longhorn-system", "monitoring", "traefik", "velero", "zot",
}

var platformVersionsCmd = &cobra.Command{
	Use:   "versions",
	Short: "Show platform component versions",
	Long: `Show the kip version recorded on Kipper's App CRD, the console-api build
recorded at startup, each node's Kubernetes version, and the images in the pod
template of every platform Deployment, StatefulSet and DaemonSet, grouped by namespace.
The Kubernetes version is shown once when all nodes agree.
Source builds preserve an existing CRD release stamp. A rollback to a console-api
build that predates build recording can leave the newer build recorded.

A workload marked "updating" may still have pods from an earlier pod template,
so some of them may run the previous image. READY counts pods that pass their
readiness checks.`,
	Args: cobra.NoArgs,
	RunE: runPlatformVersions,
}

type platformWorkload struct {
	Namespace string
	Kind      string
	Name      string
	Ready     int32
	Desired   int32
	Updating  bool
	Images    []string
}

type platformVersionReport struct {
	CRDs       string
	ConsoleAPI string
	Kubernetes string
	CLI        string
	Workloads  []platformWorkload
}

func runPlatformVersions(_ *cobra.Command, _ []string) error {
	_, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	crds, err := kipperVersionOnCluster(ctx, k8sClient.Dynamic())
	if err != nil {
		return err
	}
	consoleAPI, err := consoleAPIBuild(ctx, k8sClient.Clientset())
	if err != nil {
		return err
	}
	nodes, err := k8sClient.Clientset().CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("listing nodes: %w", err)
	}
	workloads, err := collectPlatformWorkloads(ctx, k8sClient.Clientset(), platformVersionNamespaces)
	if err != nil {
		return err
	}
	printPlatformVersions(os.Stdout, platformVersionReport{
		CRDs:       crds,
		ConsoleAPI: consoleAPI,
		Kubernetes: kubernetesVersions(nodes.Items),
		CLI:        rootCmd.Version,
		Workloads:  workloads,
	})
	return nil
}

// kipperVersionOnCluster returns the kip version stamped on the App CRD, or ""
// when the CRD or stamp is absent. A source build preserves an existing release
// stamp, so the recorded version may predate the last CRD write.
func kipperVersionOnCluster(ctx context.Context, dyn dynamic.Interface) (string, error) {
	crd, err := dyn.Resource(crdGVR).Get(ctx, "apps.kipper.run", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the Kipper version from the App CRD: %w", err)
	}
	return crdWriterVersion(crd), nil
}

// consoleAPIBuild returns the build console-api stamps on its namespace when it
// starts, or "" when the namespace or stamp is absent. The stamp identifies the
// code behind the moving image tag. A rollback to a build that predates stamping
// leaves the newer value in place unless the stamp is cleared.
func consoleAPIBuild(ctx context.Context, cs kubernetes.Interface) (string, error) {
	ns, err := cs.CoreV1().Namespaces().Get(ctx, "kipper-system", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("reading the console-api build: %w", err)
	}
	return ns.Annotations[labels.AnnoConsoleAPIBuild], nil
}

// kubernetesVersions returns the kubelet version all nodes share, or each
// node's name and version in name order when they differ, as during a node upgrade.
// It returns "" for an empty node list.
func kubernetesVersions(nodes []corev1.Node) string {
	sorted := append([]corev1.Node(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	distinct := map[string]bool{}
	var perNode []string
	for _, n := range sorted {
		v := n.Status.NodeInfo.KubeletVersion
		distinct[v] = true
		perNode = append(perNode, n.Name+" "+v)
	}
	if len(distinct) == 1 {
		return sorted[0].Status.NodeInfo.KubeletVersion
	}
	return strings.Join(perNode, ", ")
}

// collectPlatformWorkloads lists the Deployments, StatefulSets and DaemonSets in
// the given namespaces with their pod template images. Results are sorted by
// namespace, then kind (Deployment, StatefulSet, DaemonSet), then name.
func collectPlatformWorkloads(ctx context.Context, cs kubernetes.Interface, namespaces []string) ([]platformWorkload, error) {
	sortedNamespaces := append([]string(nil), namespaces...)
	sort.Strings(sortedNamespaces)
	var workloads []platformWorkload
	for _, ns := range sortedNamespaces {
		var inNamespace []platformWorkload

		deployments, err := cs.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing deployments in %s: %w", ns, err)
		}
		for _, d := range deployments.Items {
			desired := replicasOrOne(d.Spec.Replicas)
			updating := d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedReplicas < desired || d.Status.Replicas > d.Status.UpdatedReplicas
			inNamespace = append(inNamespace, platformWorkload{
				Namespace: ns, Kind: "deployment", Name: d.Name,
				Ready: d.Status.ReadyReplicas, Desired: desired,
				Updating: updating,
				Images:   templateImages(d.Spec.Template),
			})
		}

		statefulSets, err := cs.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing statefulsets in %s: %w", ns, err)
		}
		for _, s := range statefulSets.Items {
			desired := replicasOrOne(s.Spec.Replicas)
			updating := s.Status.ObservedGeneration < s.Generation || s.Status.UpdatedReplicas < desired || s.Status.Replicas > s.Status.UpdatedReplicas
			inNamespace = append(inNamespace, platformWorkload{
				Namespace: ns, Kind: "statefulset", Name: s.Name,
				Ready: s.Status.ReadyReplicas, Desired: desired,
				Updating: updating,
				Images:   templateImages(s.Spec.Template),
			})
		}

		daemonSets, err := cs.AppsV1().DaemonSets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing daemonsets in %s: %w", ns, err)
		}
		for _, d := range daemonSets.Items {
			inNamespace = append(inNamespace, platformWorkload{
				Namespace: ns, Kind: "daemonset", Name: d.Name,
				Ready: d.Status.NumberReady, Desired: d.Status.DesiredNumberScheduled,
				Updating: d.Status.ObservedGeneration < d.Generation || d.Status.UpdatedNumberScheduled < d.Status.DesiredNumberScheduled,
				Images:   templateImages(d.Spec.Template),
			})
		}

		sort.SliceStable(inNamespace, func(i, j int) bool {
			if inNamespace[i].Kind != inNamespace[j].Kind {
				return kindOrder(inNamespace[i].Kind) < kindOrder(inNamespace[j].Kind)
			}
			return inNamespace[i].Name < inNamespace[j].Name
		})
		workloads = append(workloads, inNamespace...)
	}
	return workloads, nil
}

func replicasOrOne(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

func kindOrder(kind string) int {
	switch kind {
	case "deployment":
		return 0
	case "statefulset":
		return 1
	default:
		return 2
	}
}

// templateImages lists distinct pod template images in container order, followed
// by init container images marked "(init)". Images are deduplicated separately
// within each group. Velero loads its plugins through init containers, so their
// versions matter as much as the main image's.
func templateImages(template corev1.PodTemplateSpec) []string {
	seen := map[string]bool{}
	var images []string
	add := func(image string) {
		if !seen[image] {
			seen[image] = true
			images = append(images, image)
		}
	}
	for _, c := range template.Spec.Containers {
		add(c.Image)
	}
	for _, c := range template.Spec.InitContainers {
		add(c.Image + " (init)")
	}
	return images
}

func printPlatformVersions(w io.Writer, report platformVersionReport) {
	crds := "written by kip " + report.CRDs
	if report.CRDs == "" {
		crds = "no version recorded"
	}
	consoleAPI := "build " + report.ConsoleAPI + " (recorded at its last start)"
	if report.ConsoleAPI == "" {
		consoleAPI = "no build recorded"
	}
	_, _ = fmt.Fprintf(w, "\n  Kipper CRDs:  %s\n", crds)
	_, _ = fmt.Fprintf(w, "  Console API:  %s\n", consoleAPI)
	_, _ = fmt.Fprintf(w, "  Kubernetes:   %s\n", report.Kubernetes)
	_, _ = fmt.Fprintf(w, "  kip CLI:      %s\n\n", report.CLI)

	if len(report.Workloads) == 0 {
		_, _ = fmt.Fprintf(w, "  No platform workloads found.\n\n")
		return
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "  NAMESPACE\tWORKLOAD\tREADY\tIMAGES")
	previousNamespace := ""
	for _, wl := range report.Workloads {
		namespace := wl.Namespace
		if namespace == previousNamespace {
			namespace = ""
		}
		previousNamespace = wl.Namespace

		ready := fmt.Sprintf("%d/%d", wl.Ready, wl.Desired)
		if wl.Updating {
			ready += " updating"
		}
		first := ""
		if len(wl.Images) > 0 {
			first = wl.Images[0]
		}
		_, _ = fmt.Fprintf(tw, "  %s\t%s/%s\t%s\t%s\n", namespace, wl.Kind, wl.Name, ready, first)
		for _, image := range wl.Images[min(1, len(wl.Images)):] {
			_, _ = fmt.Fprintf(tw, "  \t\t\t%s\n", image)
		}
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w)
}
