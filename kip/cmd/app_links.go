package cmd

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/getkipper/kipper/controller/pkg/applink"
	"github.com/getkipper/kipper/kip/internal/k8s"
)

var appLinksCmd = &cobra.Command{
	Use:   "links [app]",
	Short: "Show what an app links to and whether the traffic gets through",
	Long: `Lists the apps this one declares it reaches, and checks each one.

A link has to be several things at once: the other project has to have agreed,
the target has to exist and serve a port, the allowance has to be in place, and
the address has to have reached the running pods. This reports each of those,
then tries the connection itself from inside the calling pod. The only place
the allowance applies, and so the only place worth testing from.`,
	Args: cobra.RangeArgs(0, 1),
	RunE: runAppLinks,
}

func init() {
	appLinksCmd.Flags().String("project", "", "project name")
	appLinksCmd.Flags().String("environment", "", "environment")
	appCmd.AddCommand(appLinksCmd)
}

// linkReport is what could be established about one declared link.
type linkReport struct {
	target, targetNS, envKey, url string
	consented                     bool
	consentNote                   string
	targetPort                    int64
	allowancePort                 int64
	addressInPod                  bool
	probe                         string
}

func runAppLinks(cmd *cobra.Command, args []string) error {
	app := ""
	if len(args) == 1 {
		app = args[0]
	}
	ns, k8sClient, err := resolveAppNamespace(cmd, app)
	if err != nil {
		return err
	}
	if app == "" {
		return fmt.Errorf("no app given")
	}

	ctx := context.Background()
	dyn := k8sClient.Dynamic()

	appObj, err := dyn.Resource(appGVR).Namespace(ns).Get(ctx, app, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("app %q not found in %s", app, ns)
	}
	links, _, _ := unstructured.NestedSlice(appObj.Object, "spec", "links")
	if len(links) == 0 {
		fmt.Printf("\n  %s declares no links.\n\n", app)
		return nil
	}

	callerProject, _ := resolveProjectName(cmd)
	pod := runningPodFor(ctx, k8sClient, ns, app)

	fmt.Printf("\n  Links for %s in %s\n\n", app, ns)
	for _, raw := range links {
		entry, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		target, _ := entry["app"].(string)
		targetNS, _ := entry["namespace"].(string)
		printLinkReport(inspectLink(ctx, k8sClient, ns, app, callerProject, target, targetNS, pod))
	}

	if pod == "" {
		fmt.Printf("     No running pod for %s, so nothing could be tried from where the\n", app)
		fmt.Printf("     allowance applies. The checks above are what the platform can see.\n")
	}
	fmt.Println()
	return nil
}

// inspectLink establishes what it can about one link, in the order the traffic
// depends on it: consent, then the target, then the allowance, then the address,
// then the connection itself.
func inspectLink(ctx context.Context, k8sClient *k8s.Client, ns, app, callerProject, target, targetNS, pod string) linkReport {
	r := linkReport{target: target, targetNS: targetNS, envKey: applink.EnvKey(target)}
	dyn := k8sClient.Dynamic()

	targetProject := projectOwning(ctx, k8sClient, targetNS)
	switch {
	case targetNS == ns:
		r.consented, r.consentNote = true, "same project, so nobody to ask"
	case targetProject == "":
		r.consentNote = fmt.Sprintf("no project owns %s", targetNS)
	case targetProject == callerProject:
		r.consented, r.consentNote = true, "same project, so nobody to ask"
	default:
		allowed := allowLinksFrom(ctx, k8sClient, targetProject)
		for _, name := range allowed {
			if name == callerProject {
				r.consented = true
				break
			}
		}
		if r.consented {
			r.consentNote = fmt.Sprintf("%s allows %s", targetProject, callerProject)
		} else {
			r.consentNote = fmt.Sprintf("%s has not agreed to be linked to by %s", targetProject, callerProject)
		}
	}

	if obj, err := dyn.Resource(appGVR).Namespace(targetNS).Get(ctx, target, metav1.GetOptions{}); err == nil {
		r.targetPort, _, _ = unstructured.NestedInt64(obj.Object, "spec", "port")
	}
	if r.targetPort > 0 {
		r.url = fmt.Sprintf("http://%s.%s.svc.cluster.local:%d", target, targetNS, r.targetPort)
	}

	if np, err := k8sClient.Clientset().NetworkingV1().NetworkPolicies(ns).
		Get(ctx, "kipper-link-"+app, metav1.GetOptions{}); err == nil {
		for _, rule := range np.Spec.Egress {
			for _, peer := range rule.To {
				if peer.NamespaceSelector == nil || peer.PodSelector == nil {
					continue
				}
				if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == targetNS &&
					peer.PodSelector.MatchLabels["app"] == target && len(rule.Ports) > 0 {
					r.allowancePort = int64(rule.Ports[0].Port.IntValue())
				}
			}
		}
	}

	if pod != "" {
		if p, err := k8sClient.Clientset().CoreV1().Pods(ns).Get(ctx, pod, metav1.GetOptions{}); err == nil {
			for _, c := range p.Spec.Containers {
				for _, ev := range c.Env {
					if ev.Name == r.envKey && ev.Value != "" {
						r.addressInPod = true
					}
				}
			}
		}
		if r.targetPort > 0 {
			r.probe = probeFromPod(ctx, k8sClient, ns, pod, app,
				fmt.Sprintf("%s.%s.svc.cluster.local", target, targetNS), r.targetPort)
		}
	}
	return r
}

func printLinkReport(r linkReport) {
	ok := r.consented && r.targetPort > 0 && r.allowancePort > 0 && strings.HasPrefix(r.probe, "reachable")
	mark := "⚠"
	if ok {
		mark = "✔"
	}
	fmt.Printf("  %s  %s in %s\n", mark, r.target, r.targetNS)
	if r.url != "" {
		fmt.Printf("       %s=%s\n", r.envKey, r.url)
	}

	fmt.Printf("       consent      %s\n", r.consentNote)
	if r.targetPort > 0 {
		fmt.Printf("       target       serving on port %d\n", r.targetPort)
	} else {
		fmt.Printf("       target       not found, or serving no port\n")
	}
	switch {
	case r.allowancePort > 0 && r.allowancePort != r.targetPort:
		// The two differing is correct: the allowance names the port the target's
		// pods listen on, which is 10000 above its own when it runs the
		// instance-id sidecar, while the address names the port its Service
		// publishes. Said plainly, because it looks wrong until it is explained.
		fmt.Printf("       allowance    egress to %s on port %d, which is where its Service sends %d\n",
			r.target, r.allowancePort, r.targetPort)
	case r.allowancePort > 0:
		fmt.Printf("       allowance    egress to %s on port %d\n", r.target, r.allowancePort)
	case r.targetNS == "":
		fmt.Printf("       allowance    none\n")
	default:
		fmt.Printf("       allowance    none: nothing opens a path to %s\n", r.targetNS)
	}
	fmt.Printf("       address      %s\n",
		either(r.addressInPod, "in the running pod", "not in the running pod yet"))
	if r.probe != "" {
		fmt.Printf("       connection   %s\n", r.probe)
	}
	fmt.Println()
}

// either picks the wording for an outcome. Two arguments rather than one,
// because a single note has to describe success and failure at once and ends up
// describing whichever the author had in mind.
func either(ok bool, whenTrue, whenFalse string) string {
	if ok {
		return whenTrue
	}
	return whenFalse
}

// probeFromPod opens a TCP connection to the target from inside the calling
// pod, which is the only place the egress allowance applies.
//
// The tool has to come from the app's own image, and an app image promises
// nothing: no shell in a distroless one, busybox in an alpine one, occasionally
// curl. Each candidate is tried and the first that runs decides. When none of
// them is there the answer is that it could not be tried, which is not the same
// as the link being shut and must not be reported as though it were.
func probeFromPod(ctx context.Context, k8sClient *k8s.Client, ns, pod, container, host string, port int64) string {
	candidates := []struct {
		name string
		argv []string
	}{
		{"nc", []string{"nc", "-z", "-w", "5", host, fmt.Sprintf("%d", port)}},
		{"curl", []string{"curl", "-s", "-o", "/dev/null", "--connect-timeout", "5", fmt.Sprintf("http://%s:%d", host, port)}},
		{"wget", []string{"wget", "-q", "-T", "5", "-O", "/dev/null", fmt.Sprintf("http://%s:%d", host, port)}},
	}
	for _, c := range candidates {
		out, errText := execInPod(ctx, k8sClient, ns, pod, container, c.argv)
		if toolMissing(out + errText) {
			continue
		}
		return probeResult(c.name, errText == "", host, port, pod)
	}
	return fmt.Sprintf("could not be tried: %s has no tool to open a connection with, so this says nothing either way", container)
}

// execInPod runs a command in a pod and returns its output and any error text.
func execInPod(ctx context.Context, k8sClient *k8s.Client, ns, pod, container string, argv []string) (string, string) {
	cluster, err := loadCurrentClusterConfig()
	if err != nil {
		return "", err.Error()
	}
	restCfg, err := clientcmd.BuildConfigFromFlags("", cluster.Kubeconfig)
	if err != nil {
		return "", err.Error()
	}
	req := k8sClient.Clientset().CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container, Command: argv, Stdout: true, Stderr: true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restCfg, "POST", req.URL())
	if err != nil {
		return "", err.Error()
	}
	var stdout, stderr bytes.Buffer
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout, Stderr: &stderr,
	}); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), msg
	}
	return stdout.String(), ""
}

// runningPodFor returns one running pod of the app, or "" when it has none.
func runningPodFor(ctx context.Context, k8sClient *k8s.Client, ns, app string) string {
	pods, err := k8sClient.Clientset().CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "app=" + app,
	})
	if err != nil {
		return ""
	}
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			return pods.Items[i].Name
		}
	}
	return ""
}

// projectOwning returns the project a namespace belongs to, from the label the
// project reconciler writes.
func projectOwning(ctx context.Context, k8sClient *k8s.Client, ns string) string {
	obj, err := k8sClient.Clientset().CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	return obj.Labels["kipper.run/project"]
}

// allowLinksFrom returns the projects a project has agreed to be linked to by.
func allowLinksFrom(ctx context.Context, k8sClient *k8s.Client, project string) []string {
	obj, err := k8sClient.Dynamic().Resource(projectGVR).Get(ctx, project, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	allowed, _, _ := unstructured.NestedStringSlice(obj.Object, "spec", "allowLinksFrom")
	return allowed
}

// toolMissing reports whether output is the container saying it has no such
// program, rather than the program saying it could not connect. Confusing the
// two turns "nothing here can test this" into "the link is shut", which is the
// wrong answer to act on.
func toolMissing(output string) bool {
	for _, sign := range []string{"not found", "no such file", "executable file not found"} {
		if strings.Contains(output, sign) {
			return true
		}
	}
	return false
}

// probeResult is what one attempt proved.
func probeResult(tool string, connected bool, host string, port int64, pod string) string {
	if connected {
		return fmt.Sprintf("reachable: %s connected to %s:%d from %s", tool, host, port, pod)
	}
	return fmt.Sprintf("refused or blocked: %s could not reach %s:%d from %s", tool, host, port, pod)
}
