package cmd

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/controller/pkg/labels"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// discoverSystemNamespace is skipped during the orphan scan. Kipper system
// components (zot, console, console-api) carry managed-by=kipper but
// legitimately have no owning CR.
const discoverSystemNamespace = "kipper-system"

var discoverCmd = &cobra.Command{
	Use:   "discover",
	Short: "Find Kipper-labelled workloads that have no owning Kipper CR",
	Long: `Scans the current cluster for Deployments, StatefulSets, and PVCs
that carry app.kubernetes.io/managed-by=kipper but are not owned by an
App, Service, Function, or Volume CR.

For each orphan found, kip discover prints a suggested kip command that
will faithfully recreate it as a proper CR, preserving image, port,
resources, storage, env vars, and so on. Running the suggested command
creates the CR; the controller adopts the existing workload to match,
no deletion required. Edit the suggested command first if you want to
change anything.`,
	RunE: runDiscover,
}

func init() {
	rootCmd.AddCommand(discoverCmd)
}

type orphanWorkload struct {
	Namespace    string
	Kind         string
	Name         string
	ExpectedKind string
	Suggest      string
}

func runDiscover(_ *cobra.Command, _ []string) error {
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}

	ctx := context.Background()
	clientset := k8sClient.Clientset()
	dynClient := k8sClient.Dynamic()

	// Pre-fetch Project CRs so we can map a namespace back to its
	// (project, environment) pair when suggesting kip commands.
	var projectList []unstructured.Unstructured
	if pl, err := dynClient.Resource(manifest.ProjectGVR).List(ctx, metav1.ListOptions{}); err == nil {
		projectList = pl.Items
	}

	var orphans []orphanWorkload

	deployments, err := clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{
		LabelSelector: labels.KipperManagedSelector,
	})
	if err != nil {
		return fmt.Errorf("listing deployments: %w", err)
	}
	for i := range deployments.Items {
		d := &deployments.Items[i]
		if d.Namespace == discoverSystemNamespace {
			continue
		}

		expectedKind := "App"
		gvr := manifest.AppGVR
		isFunction := d.Labels[labels.ResourceType] == labels.ResourceTypeFunction
		if isFunction {
			expectedKind = "Function"
			gvr = manifest.FunctionGVR
		}

		if _, err := dynClient.Resource(gvr).Namespace(d.Namespace).Get(ctx, d.Name, metav1.GetOptions{}); errors.IsNotFound(err) {
			project, env := resolveNamespaceToProject(cluster, projectList, d.Namespace)
			suggest := suggestForApp(d, project, env)
			if isFunction {
				suggest = "# Function recreation requires the original source code; recreate via 'kip function create'."
			}
			orphans = append(orphans, orphanWorkload{
				Namespace: d.Namespace, Kind: "Deployment", Name: d.Name,
				ExpectedKind: expectedKind, Suggest: suggest,
			})
		}
	}

	statefulsets, err := clientset.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{
		LabelSelector: labels.KipperManagedSelector,
	})
	if err != nil {
		return fmt.Errorf("listing statefulsets: %w", err)
	}
	for i := range statefulsets.Items {
		s := &statefulsets.Items[i]
		if s.Namespace == discoverSystemNamespace {
			continue
		}
		if _, err := dynClient.Resource(manifest.ServiceGVR).Namespace(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{}); errors.IsNotFound(err) {
			project, env := resolveNamespaceToProject(cluster, projectList, s.Namespace)
			orphans = append(orphans, orphanWorkload{
				Namespace: s.Namespace, Kind: "StatefulSet", Name: s.Name,
				ExpectedKind: "Service", Suggest: suggestForService(s, project, env),
			})
		}
	}

	pvcs, err := clientset.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", labels.ResourceType, labels.ResourceTypeSharedVolume),
	})
	if err != nil {
		return fmt.Errorf("listing pvcs: %w", err)
	}
	for i := range pvcs.Items {
		p := &pvcs.Items[i]
		if p.Namespace == discoverSystemNamespace {
			continue
		}
		volumeName := p.Labels[labels.VolumeName]
		if volumeName == "" {
			volumeName = p.Name
		}
		if _, err := dynClient.Resource(manifest.VolumeGVR).Namespace(p.Namespace).Get(ctx, volumeName, metav1.GetOptions{}); errors.IsNotFound(err) {
			project, env := resolveNamespaceToProject(cluster, projectList, p.Namespace)
			orphans = append(orphans, orphanWorkload{
				Namespace: p.Namespace, Kind: "PVC", Name: p.Name,
				ExpectedKind: "Volume", Suggest: suggestForVolume(p, volumeName, project, env),
			})
		}
	}

	if len(orphans) == 0 {
		fmt.Printf("\n  No orphans found on cluster %q\n\n", cluster.Name)
		return nil
	}

	sort.Slice(orphans, func(i, j int) bool {
		if orphans[i].Namespace != orphans[j].Namespace {
			return orphans[i].Namespace < orphans[j].Namespace
		}
		return orphans[i].Name < orphans[j].Name
	})

	fmt.Printf("\n  Drift detected on cluster %q:\n\n", cluster.Name)
	fmt.Printf("  %-25s %-15s %-30s %s\n", "NAMESPACE", "KIND", "NAME", "EXPECTED CR")
	for _, o := range orphans {
		fmt.Printf("  %-25s %-15s %-30s %s\n", o.Namespace, o.Kind, o.Name, o.ExpectedKind)
	}
	fmt.Printf("\n  Suggested commands to bring each orphan under management:\n")
	for _, o := range orphans {
		fmt.Printf("\n  %s (%s in %s → %s):\n    %s\n", o.Name, o.Kind, o.Namespace, o.ExpectedKind, o.Suggest)
	}
	fmt.Printf("\n  Run the suggested command to create the CR; the controller will\n")
	fmt.Printf("  adopt the existing workload to match. Edit the command first if you\n")
	fmt.Printf("  want to change settings.\n\n")
	return nil
}

// resolveNamespaceToProject reverses cluster.ResolveNamespace to find the
// (project, environment) pair that produced the given namespace. Returns
// empty strings if no Project CR matches — in which case the suggested
// command will lack --project / --environment flags and the operator will
// need to add them by hand.
func resolveNamespaceToProject(cluster *config.Cluster, projects []unstructured.Unstructured, namespace string) (string, string) {
	for _, p := range projects {
		projectName := p.GetName()
		if cluster.Org != "" {
			projectName = strings.TrimPrefix(projectName, cluster.Org+"-")
		}

		envs, _, _ := unstructured.NestedSlice(p.Object, "spec", "environments")
		if len(envs) == 0 {
			if cluster.ResolveNamespace(projectName, "") == namespace {
				return projectName, ""
			}
			continue
		}
		for _, e := range envs {
			envMap, ok := e.(map[string]interface{})
			if !ok {
				continue
			}
			envName, _ := envMap["name"].(string)
			if cluster.ResolveNamespace(projectName, envName) == namespace {
				return projectName, envName
			}
		}
	}
	return "", ""
}

// suggestForApp builds a `kip app deploy` command line that recreates an
// orphan Deployment. Only fields the App CR can express are included;
// callers should review the suggestion before running it.
func suggestForApp(d *appsv1.Deployment, project, env string) string {
	parts := []string{"kip app deploy", "--name", d.Name}
	if project != "" {
		parts = append(parts, "--project", project)
	}
	if env != "" {
		parts = append(parts, "--environment", env)
	}

	if len(d.Spec.Template.Spec.Containers) > 0 {
		c := &d.Spec.Template.Spec.Containers[0]
		parts = append(parts, "--image", c.Image)
		if len(c.Ports) > 0 {
			parts = append(parts, "--port", fmt.Sprintf("%d", c.Ports[0].ContainerPort))
		}
		if mem, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			parts = append(parts, "--memory", mem.String())
		}
		if cpu, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			parts = append(parts, "--cpu", cpu.String())
		}
		for _, e := range c.Env {
			if e.ValueFrom == nil {
				parts = append(parts, "--env", fmt.Sprintf("%s=%s", e.Name, e.Value))
			}
		}
	}

	if d.Spec.Replicas != nil && *d.Spec.Replicas != 1 {
		parts = append(parts, "--replicas", fmt.Sprintf("%d", *d.Spec.Replicas))
	}

	return strings.Join(parts, " ")
}

// suggestForService builds a `kip service add` command line that recreates
// an orphan StatefulSet. Credential rotation is a known caveat: the new
// CR will trigger fresh credentials via the controller, breaking apps
// already bound to the old Secret unless they are re-bound.
func suggestForService(s *appsv1.StatefulSet, project, env string) string {
	svcType := s.Labels[labels.ServiceType]
	if svcType == "" {
		svcType = "<TYPE>"
	}
	parts := []string{"kip service add", svcType, "--name", s.Name}
	if project != "" {
		parts = append(parts, "--project", project)
	}
	if env != "" {
		parts = append(parts, "--environment", env)
	}

	if len(s.Spec.VolumeClaimTemplates) > 0 {
		if storage, ok := s.Spec.VolumeClaimTemplates[0].Spec.Resources.Requests[corev1.ResourceStorage]; ok {
			parts = append(parts, "--storage", storage.String())
		}
	}
	if len(s.Spec.Template.Spec.Containers) > 0 {
		c := &s.Spec.Template.Spec.Containers[0]
		if mem, ok := c.Resources.Limits[corev1.ResourceMemory]; ok {
			parts = append(parts, "--memory", mem.String())
		}
		if cpu, ok := c.Resources.Limits[corev1.ResourceCPU]; ok {
			parts = append(parts, "--cpu", cpu.String())
		}
	}
	return strings.Join(parts, " ")
}

// suggestForVolume builds a `kip volume create` command line that
// recreates an orphan PVC labelled as a shared volume.
func suggestForVolume(p *corev1.PersistentVolumeClaim, name, project, env string) string {
	parts := []string{"kip volume create", name}
	if project != "" {
		parts = append(parts, "--project", project)
	}
	if env != "" {
		parts = append(parts, "--environment", env)
	}
	if storage, ok := p.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		parts = append(parts, "--size", storage.String())
	}
	return strings.Join(parts, " ")
}
