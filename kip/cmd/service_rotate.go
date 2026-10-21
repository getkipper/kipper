package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"

	"github.com/getkipper/kipper/controller/pkg/rollout"
	"github.com/getkipper/kipper/controller/pkg/secretname"
	"github.com/getkipper/kipper/kip/internal/ai"
	"github.com/getkipper/kipper/kip/internal/deployer"
	"github.com/getkipper/kipper/kip/internal/k8s"
	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

var serviceRotatePasswordCmd = &cobra.Command{
	Use:   "rotate-password <name>",
	Short: "Give a service a new password and move its bound apps and functions onto it",
	Long: `Changes the password inside the database or broker, stores it in the service's
credentials, recreates the service's pod so every open session ends, and waits
for the apps and functions bound to the service to restart on the new password.

The old password stops working as soon as the engine has the new one. MinIO
switches when its pod restarts. Bound apps and functions may fail to connect to the service
until they restart. Jobs that already
started, values pasted by hand and external clients keep the old password until
you update them.

If the command is interrupted before the password is stored, run it again to
resume the rotation. After it is stored, follow the restart or status-check
instructions in the output.`,
	Args: cobra.ExactArgs(1),
	RunE: runServiceRotatePassword,
}

func init() {
	serviceRotatePasswordCmd.Flags().String("project", "default", "project namespace")
	serviceRotatePasswordCmd.Flags().String("environment", "", "target environment")
	serviceRotatePasswordCmd.Flags().Bool("yes", false, "rotate without asking for confirmation")
	serviceCmd.AddCommand(serviceRotatePasswordCmd)
}

const (
	rotationEngineWait   = 2 * time.Minute
	rotationCommitWait   = 30 * time.Second
	rotationServiceWait  = 5 * time.Minute
	rotationWorkloadWait = 15 * time.Minute
	rotationPoll         = 2 * time.Second
)

var veleroBackupGVR = schema.GroupVersionResource{Group: "velero.io", Version: "v1", Resource: "backups"}

func runServiceRotatePassword(cmd *cobra.Command, args []string) error {
	name := args[0]
	cluster, k8sClient, err := loadCurrentCluster()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	namespace := resolveServiceNamespace(cmd)
	if !cmd.Flags().Changed("project") && !cmd.Flags().Changed("environment") {
		if namespace, err = findServiceNamespace(ctx, k8sClient.Dynamic(), name); err != nil {
			return err
		}
	}
	restConfig, err := k8s.RESTConfig(cluster)
	if err != nil {
		return err
	}
	clientset, err := kubernetes.NewForConfig(k8s.WithoutPodSecurityWarnings(restConfig))
	if err != nil {
		return fmt.Errorf("creating the Kubernetes client: %w", err)
	}
	yes, _ := cmd.Flags().GetBool("yes")
	d := &deployer.Deployer{Client: clientset, Dynamic: k8sClient.Dynamic()}

	r := &passwordRotation{
		clientset: clientset,
		dynamic:   k8sClient.Dynamic(),
		namespace: namespace,
		service:   name,
		out:       os.Stdout,
		execIn: func(pod, container string) podExecFunc {
			return podExec(clientset, restConfig, namespace, pod, container)
		},
		forward: forwardToPod(clientset, restConfig, namespace),
		restart: func(ctx context.Context) (string, error) {
			return d.RestartWorkload(ctx, manifest.ServiceGVR, "service", namespace, name)
		},
		pauseTuning: func(ctx context.Context) (func(), error) {
			return pauseTuning(ctx, clientset, namespace, name)
		},
		confirm: func(question string) bool {
			return yes || confirm(question)
		},
		now:          time.Now,
		stampWait:    confirmRestartWithin,
		serviceWait:  rotationServiceWait,
		workloadWait: rotationWorkloadWait,
		poll:         rotationPoll,
	}
	return r.run(ctx)
}

// passwordRotation rotates one service's password. Injected cluster operations
// let tests exercise the workflow with fakes.
type passwordRotation struct {
	clientset    kubernetes.Interface
	dynamic      dynamic.Interface
	namespace    string
	service      string
	serviceUID   types.UID
	out          io.Writer
	execIn       func(pod, container string) podExecFunc
	forward      func(ctx context.Context, pod string, port int) (baseURL string, stop func(), err error)
	restart      func(ctx context.Context) (string, error)
	pauseTuning  func(ctx context.Context) (func(), error)
	confirm      func(question string) bool
	now          func() time.Time
	stampWait    time.Duration
	serviceWait  time.Duration
	workloadWait time.Duration
	poll         time.Duration
}

// boundWorkload is an App or Function whose credentials come from the
// service, with the templates of the children that carry them. A nil template
// means the workload has no such child.
type boundWorkload struct {
	kind       string
	name       string
	deployment *corev1.PodTemplateSpec
	cronJob    *batchv1.JobTemplateSpec
}

func (r *passwordRotation) run(ctx context.Context) error {
	svc, err := r.dynamic.Resource(manifest.ServiceGVR).Namespace(r.namespace).Get(ctx, r.service, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("no service %q in %s", r.service, r.namespace)
		}
		return fmt.Errorf("reading service %s: %w", r.service, err)
	}
	r.serviceUID = svc.GetUID()
	svcType, _, _ := unstructured.NestedString(svc.Object, "spec", "type")
	secretName := secretname.ServiceCredentials(r.service)
	secret, err := r.clientset.CoreV1().Secrets(r.namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("reading %s: %w", secretName, err)
	}
	if owner := metav1.GetControllerOf(secret); owner == nil || owner.UID != svc.GetUID() {
		return fmt.Errorf("%s does not belong to service %s. Run kip service credentials --repair first", secretName, r.service)
	}
	username, database := string(secret.Data["USERNAME"]), string(secret.Data["NAME"])
	key := service.RotatedKey(svcType)
	stored := string(secret.Data[key])
	if svcType == "minio" && (len(secret.Data["ACCESS_KEY"]) == 0 || stored == "") {
		return fmt.Errorf("%s has no ACCESS_KEY or SECRET_KEY. Run kip service credentials --repair first", secretName)
	}
	// Validate engine inputs before making changes, using zeros as a placeholder
	// for the new password.
	engineStep, err := service.EngineRotation(svcType, username, database, stored, strings.Repeat("0", 48))
	if err != nil {
		return err
	}
	var pod, container string
	changesEngine := engineStep != nil || svcType == "rabbitmq"
	if changesEngine {
		if pod, container, err = r.runningPod(ctx); err != nil {
			return err
		}
	}
	bound, err := r.boundWorkloads(ctx)
	if err != nil {
		return err
	}
	if err := r.refuseDuringBackup(ctx); err != nil {
		return err
	}

	r.printPlan(svcType, bound)
	if !r.confirm(fmt.Sprintf("Rotate the password of %s now?", r.service)) {
		say(r.out, "\n  Nothing was changed.\n\n")
		return nil
	}
	// A backup may have started while waiting for confirmation.
	if err := r.refuseDuringBackup(ctx); err != nil {
		return err
	}

	resume, err := r.pauseTuning(ctx)
	if err != nil {
		return fmt.Errorf("pausing resource tuning for %s: %w", r.service, err)
	}
	defer resume()

	owner := metav1.OwnerReference{APIVersion: svc.GetAPIVersion(), Kind: svc.GetKind(), Name: svc.GetName(), UID: svc.GetUID()}
	rotation, err := service.ClaimRotation(ctx, r.clientset, r.namespace, owner, r.now)
	if err != nil {
		return err
	}
	if rotation.Resumed {
		say(r.out, "\n  Finishing the rotation an earlier run started.\n")
	}
	// From here on the engine may hold the new password, so a failure keeps the
	// record and only gives up the lease. The next run finishes the rotation.
	release := func() {
		releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		_ = rotation.Release(releaseCtx)
	}

	if changesEngine {
		if err := r.changeEngine(ctx, svcType, username, database, stored, rotation, pod, container); err != nil {
			release()
			return err
		}
	}

	// The baseline the bound workloads must move away from, taken now so a
	// change made before the commit does not count as the restart.
	if bound, err = r.boundWorkloads(ctx); err != nil {
		release()
		return fmt.Errorf("%w. Run kip service rotate-password %s again to finish", err, r.service)
	}
	// The engine may already have the new password, so allow the commit to
	// finish even if the operator presses Ctrl-C.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rotationCommitWait)
	err = rotation.Extend(commitCtx)
	if err == nil {
		err = rotation.Commit(commitCtx, secretName, key)
	}
	if err == nil {
		if finishErr := rotation.Finish(commitCtx); finishErr != nil {
			say(r.out, "  !  The new password is stored, but kip could not clear the rotation record (%v). kip service credentials shows whether it is still there.\n", finishErr)
		}
	}
	cancel()
	if err != nil {
		release()
		return fmt.Errorf("%w. The engine may already have the new password. Run kip service rotate-password %s again to finish", err, r.service)
	}
	if changesEngine {
		say(r.out, "\n  ✔  %s (%s): password changed in the engine and in %s\n", r.service, svcType, secretName)
	} else {
		say(r.out, "\n  ✔  %s (%s): new secret key stored in %s. MinIO takes it when its pod restarts\n", r.service, svcType, secretName)
	}

	say(r.out, "  ...  Recreating the service's pod. If this is interrupted, run kip service restart %s\n", r.service)
	if err := r.restartService(ctx, svcType); err != nil {
		return err
	}
	say(r.out, "  ✔  Service pod recreated, which closed every open session\n")

	if rotation.Resumed && rotation.Password == stored {
		// An earlier run stored this password, so the bound workloads may have
		// re-rendered already and there is no change left to wait for.
		say(r.out, "  !  An earlier run had already stored this password, so kip cannot tell which bound apps and functions restarted on it. Check them with kip app info or kip function info\n")
	} else if err := r.waitForWorkloads(ctx, bound); err != nil {
		return interrupted(ctx, err)
	}
	r.printReport()
	return nil
}

// changeEngine moves the engine to the new password. A resumed run cannot know
// whether the engine took it before the earlier run stopped, so engines that
// sign in with the current password try the new one first and then the stored
// one. Postgres signs in over the local socket and needs neither.
func (r *passwordRotation) changeEngine(ctx context.Context, svcType, username, database, stored string, rotation *service.Rotation, pod, container string) error {
	ctx, cancel := context.WithTimeout(ctx, rotationEngineWait)
	defer cancel()
	candidates := []string{stored}
	if rotation.Resumed && svcType != "postgres" && rotation.Password != stored {
		candidates = []string{rotation.Password, stored}
	}

	change := func(current string) error {
		step, err := service.EngineRotation(svcType, username, database, current, rotation.Password)
		if err != nil {
			return err
		}
		var stderr bytes.Buffer
		if err := r.execIn(pod, container)(ctx, step.Argv, strings.NewReader(step.Stdin), io.Discard, &stderr); err != nil {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(redact(stderr.String(), current, rotation.Password)))
		}
		return nil
	}
	if svcType == "rabbitmq" {
		baseURL, stop, err := r.forward(ctx, pod, service.RabbitMQManagementPort)
		if err != nil {
			return fmt.Errorf("reaching the RabbitMQ management API of %s on port %d: %w. kip changes RabbitMQ passwords through the management plugin, which Kipper's default RabbitMQ image includes. The stored password stays in place. Run the command again once the management API answers",
				r.service, service.RabbitMQManagementPort, err)
		}
		defer stop()
		client := &http.Client{Timeout: 30 * time.Second}
		change = func(current string) error {
			return service.ChangeRabbitMQPassword(ctx, client, baseURL, username, current, rotation.Password)
		}
	}

	var failures []string
	for _, current := range candidates {
		err := change(current)
		if err == nil {
			return nil
		}
		failures = append(failures, err.Error())
	}
	return fmt.Errorf("kip could not confirm that %s took the new password (%s). The stored password stays in place, so run kip service rotate-password %s again to finish this rotation",
		r.service, strings.Join(failures, ". "), r.service)
}

// forwardToPod returns a function that opens a port-forward from a free
// loopback port to a pod in namespace. On success it returns an HTTP base URL
// and a stop function the caller must call once. The context cancels setup only.
func forwardToPod(clientset kubernetes.Interface, restConfig *rest.Config, namespace string) func(ctx context.Context, pod string, port int) (string, func(), error) {
	return func(ctx context.Context, pod string, port int) (string, func(), error) {
		transport, upgrader, err := k8s.SPDYRoundTripperFor(restConfig)
		if err != nil {
			return "", nil, err
		}
		url := clientset.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward").URL()
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, url)
		stop, ready := make(chan struct{}), make(chan struct{})
		fw, err := portforward.NewOnAddresses(dialer, []string{"127.0.0.1"}, []string{fmt.Sprintf("0:%d", port)}, stop, ready, io.Discard, io.Discard)
		if err != nil {
			return "", nil, err
		}
		failed := make(chan error, 1)
		go func() { failed <- fw.ForwardPorts() }()
		select {
		case <-ready:
		case err := <-failed:
			return "", nil, err
		case <-ctx.Done():
			close(stop)
			return "", nil, ctx.Err()
		}
		ports, err := fw.GetPorts()
		if err != nil || len(ports) == 0 {
			close(stop)
			return "", nil, fmt.Errorf("the port-forward reported no local port: %w", err)
		}
		return fmt.Sprintf("http://127.0.0.1:%d", ports[0].Local), func() { close(stop) }, nil
	}
}

// interrupted reports cancellation during a wait after the password is stored.
// Use it only for waits so failed requests retain their specific errors.
func interrupted(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.Canceled) {
		return fmt.Errorf("interrupted after the new password was stored. Check the service with kip service list and the bound apps with kip app info")
	}
	return err
}

// redact replaces the passwords in engine output before kip prints it.
func redact(output string, passwords ...string) string {
	for _, p := range passwords {
		if p != "" {
			output = strings.ReplaceAll(output, p, "[password]")
		}
	}
	return output
}

func (r *passwordRotation) runningPod(ctx context.Context) (string, string, error) {
	pods, err := r.servicePods(ctx)
	if err != nil {
		return "", "", err
	}
	for i := range pods {
		if podReady(&pods[i]) && pods[i].DeletionTimestamp == nil {
			return pods[i].Name, workloadContainerName(&pods[i], r.service), nil
		}
	}
	return "", "", fmt.Errorf("%s has no running pod, so its password cannot be changed now", r.service)
}

// servicePods lists pods with the service's app label that are controlled by
// its StatefulSet. It verifies that the Service controls the StatefulSet because
// labels alone could send passwords to another workload.
func (r *passwordRotation) servicePods(ctx context.Context) ([]corev1.Pod, error) {
	sts, err := r.clientset.AppsV1().StatefulSets(r.namespace).Get(ctx, r.service, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading the StatefulSet of %s: %w", r.service, err)
	}
	if owner := metav1.GetControllerOf(sts); owner == nil || owner.UID != r.serviceUID {
		return nil, fmt.Errorf("the StatefulSet %s is not controlled by service %s, so kip will not send it the password. If the service was just restored, retry once its controller has adopted the StatefulSet", r.service, r.service)
	}
	pods, err := r.clientset.CoreV1().Pods(r.namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=" + r.service})
	if err != nil {
		return nil, fmt.Errorf("listing the pods of %s: %w", r.service, err)
	}
	var owned []corev1.Pod
	for i := range pods.Items {
		if metav1.IsControlledBy(&pods.Items[i], sts) {
			owned = append(owned, pods.Items[i])
		}
	}
	return owned, nil
}

// refuseDuringBackup refuses while a Velero backup that covers the namespace is
// unfinished, because it could save the volume with one password and the
// credentials with the other. A cluster without Velero has nothing to wait for.
func (r *passwordRotation) refuseDuringBackup(ctx context.Context) error {
	list, err := r.dynamic.Resource(veleroBackupGVR).Namespace(ai.VeleroNamespace).List(ctx, metav1.ListOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if apierrors.IsForbidden(err) {
		return fmt.Errorf("kip cannot check for running backups, because you do not have permission to list Velero backups. Ask a cluster admin to run the rotation")
	}
	if err != nil {
		return fmt.Errorf("checking for running backups: %w", err)
	}
	var running []string
	for _, b := range list.Items {
		phase, _, _ := unstructured.NestedString(b.Object, "status", "phase")
		switch phase {
		case "Completed", "PartiallyFailed", "Failed", "FailedValidation", "Deleting":
			continue
		}
		included, _, _ := unstructured.NestedStringSlice(b.Object, "spec", "includedNamespaces")
		if len(included) == 0 || slices.Contains(included, "*") || slices.Contains(included, r.namespace) {
			running = append(running, b.GetName())
		}
	}
	if len(running) > 0 {
		sort.Strings(running)
		return fmt.Errorf("backup %s is still running and covers %s. Rotate once it has finished, so the backup holds a single password", strings.Join(running, ", "), r.namespace)
	}
	return nil
}

// boundWorkloads lists the Apps and Functions whose credentials come from the
// service, through a binding or, for a Function, as its event source.
func (r *passwordRotation) boundWorkloads(ctx context.Context) ([]boundWorkload, error) {
	var bound []boundWorkload
	for _, src := range []struct {
		kind string
		gvr  schema.GroupVersionResource
	}{{"app", manifest.AppGVR}, {"function", manifest.FunctionGVR}} {
		list, err := r.dynamic.Resource(src.gvr).Namespace(r.namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("listing the %ss bound to %s: %w", src.kind, r.service, err)
		}
		for _, item := range list.Items {
			if !usesService(item, r.service) {
				continue
			}
			w := boundWorkload{kind: src.kind, name: item.GetName()}
			dep, err := r.clientset.AppsV1().Deployments(r.namespace).Get(ctx, w.name, metav1.GetOptions{})
			switch {
			case err == nil:
				w.deployment = dep.Spec.Template.DeepCopy()
			case !apierrors.IsNotFound(err):
				return nil, fmt.Errorf("reading the deployment of %s: %w", w.name, err)
			}
			if src.kind == "function" {
				cj, err := r.clientset.BatchV1().CronJobs(r.namespace).Get(ctx, w.name+"-cron", metav1.GetOptions{})
				switch {
				case err == nil:
					w.cronJob = cj.Spec.JobTemplate.DeepCopy()
				case !apierrors.IsNotFound(err):
					return nil, fmt.Errorf("reading the cron job of %s: %w", w.name, err)
				}
			}
			bound = append(bound, w)
		}
	}
	sort.Slice(bound, func(i, j int) bool {
		if bound[i].kind != bound[j].kind {
			return bound[i].kind < bound[j].kind
		}
		return bound[i].name < bound[j].name
	})
	return bound, nil
}

// usesService reports whether an App or Function takes credentials from the
// named service: a service binding, or a Function trigger polling it.
func usesService(item unstructured.Unstructured, name string) bool {
	bindings, _, _ := unstructured.NestedSlice(item.Object, "spec", "serviceBindings")
	for _, b := range bindings {
		if m, ok := b.(map[string]any); ok && m["name"] == name {
			return true
		}
	}
	triggers, _, _ := unstructured.NestedSlice(item.Object, "spec", "triggers")
	for _, t := range triggers {
		m, ok := t.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := m["type"].(string); typ == "" || typ == "http" || typ == "cron" {
			continue
		}
		if config, ok := m["config"].(map[string]any); ok && config["source"] == name {
			return true
		}
	}
	return false
}

func (r *passwordRotation) printPlan(svcType string, bound []boundWorkload) {
	say(r.out, "\n  Rotating the password of %s (%s) in %s:\n", r.service, svcType, r.namespace)
	if svcType == "minio" {
		say(r.out, "    - the old secret key stops working when MinIO's pod restarts\n")
	} else {
		say(r.out, "    - the old password stops working as soon as the engine has the new one\n")
	}
	say(r.out, "    - the service's pod is recreated, which ends every open session\n")
	if len(bound) == 0 {
		say(r.out, "    - no app or function is bound to it\n")
	}
	for _, w := range bound {
		if w.deployment != nil {
			say(r.out, "    - %s %s restarts on the new password\n", w.kind, w.name)
		}
		if w.cronJob != nil {
			say(r.out, "    - %s %s uses the new password from its next scheduled run\n", w.kind, w.name)
		}
	}
}

// restartService recreates the service's pod and waits until a ready pod
// carries the restart stamp. The restart request itself survives Ctrl-C,
// because the new password is already stored and MinIO only takes it on a
// restart. The waits that follow remain cancelable.
func (r *passwordRotation) restartService(ctx context.Context, svcType string) error {
	restartCtx, cancelRestart := context.WithTimeout(context.WithoutCancel(ctx), rotationCommitWait)
	stamp, err := r.restart(restartCtx)
	cancelRestart()
	if err != nil {
		if svcType == "minio" {
			return fmt.Errorf("restarting %s: %w. The new secret key is stored, but MinIO keeps accepting the old one until it restarts, so run kip service restart %s", r.service, err, r.service)
		}
		return fmt.Errorf("restarting %s: %w. The password is rotated, but open sessions stay signed in until the service restarts. Run kip service restart %s", r.service, err, r.service)
	}
	if err := confirmRestart(ctx, statefulSetStamp(r.clientset, r.namespace, r.service), r.service, stamp, r.stampWait); err != nil {
		return interrupted(ctx, fmt.Errorf("%w. The password is rotated", err))
	}
	ctx, cancel := context.WithTimeout(ctx, r.serviceWait)
	defer cancel()
	for {
		pods, err := r.servicePods(ctx)
		if err == nil && restartedPodReady(pods, stamp) {
			return nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return interrupted(ctx, fmt.Errorf("%s did not come back within %s of the restart: %w. The password is rotated", r.service, r.serviceWait, err))
			}
			return interrupted(ctx, fmt.Errorf("%s did not come back within %s of the restart. The password is rotated. Check it with kip service list", r.service, r.serviceWait))
		case <-time.After(r.poll):
		}
	}
}

// restartedPodReady reports whether every pod carries the restart stamp and at
// least one of them is ready.
func restartedPodReady(pods []corev1.Pod, stamp string) bool {
	ready := false
	for i := range pods {
		if pods[i].Annotations["kipper.run/restartedAt"] != stamp {
			return false
		}
		ready = ready || podReady(&pods[i])
	}
	return ready
}

// waitForWorkloads waits for each remaining bound child's credential fingerprint
// to change from the baseline and for each Deployment to finish rolling out.
func (r *passwordRotation) waitForWorkloads(ctx context.Context, bound []boundWorkload) error {
	ctx, cancel := context.WithTimeout(ctx, r.workloadWait)
	defer cancel()
	waiting := map[string]boundWorkload{}
	for _, w := range bound {
		if w.deployment != nil || w.cronJob != nil {
			waiting[w.kind+" "+w.name] = w
		}
	}
	for len(waiting) > 0 {
		for label, w := range waiting {
			done, err := r.workloadMoved(ctx, w)
			if err != nil {
				return err
			}
			if done {
				say(r.out, "  ✔  %s picked up the new password\n", label)
				delete(waiting, label)
			}
		}
		if len(waiting) == 0 {
			break
		}
		select {
		case <-ctx.Done():
			labels := make([]string, 0, len(waiting))
			for label := range waiting {
				labels = append(labels, label)
			}
			sort.Strings(labels)
			return fmt.Errorf("after %s these have not picked up the new password yet: %s. The password is rotated. Check them with kip app info or kip function info", r.workloadWait, strings.Join(labels, ", "))
		case <-time.After(r.poll):
		}
	}
	return nil
}

// workloadMoved reports whether each remaining child's credential fingerprint
// has changed from the baseline and any Deployment rollout has finished.
// Deleted children count as complete. Read failures are retried on the next poll.
func (r *passwordRotation) workloadMoved(ctx context.Context, w boundWorkload) (bool, error) {
	if w.deployment != nil {
		dep, err := r.clientset.AppsV1().Deployments(r.namespace).Get(ctx, w.name, metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return false, nil //nolint:nilerr // a failed read is retried on the next poll
		case credentialFingerprint(&dep.Spec.Template) == credentialFingerprint(w.deployment), dep.Status.ObservedGeneration < dep.Generation:
			return false, nil
		case rollout.Failed(dep):
			return false, fmt.Errorf("%s %s stalled restarting on the new password: %s. The password is rotated", w.kind, w.name, rollout.Message(dep))
		case !rollout.Settled(dep):
			return false, nil
		}
	}
	if w.cronJob != nil {
		cj, err := r.clientset.BatchV1().CronJobs(r.namespace).Get(ctx, w.name+"-cron", metav1.GetOptions{})
		switch {
		case apierrors.IsNotFound(err):
		case err != nil:
			return false, nil //nolint:nilerr // a failed read is retried on the next poll
		case credentialFingerprint(&cj.Spec.JobTemplate.Spec.Template) == credentialFingerprint(&w.cronJob.Spec.Template):
			return false, nil
		}
	}
	return true, nil
}

// credentialFingerprint combines the binding-credentials annotation with sorted
// Secret names from envFrom and secretKeyRef in regular and init containers.
// Restart annotations and image changes alone leave it unchanged. It detects
// reference or hash changes, not whether the credentials themselves are valid.
func credentialFingerprint(t *corev1.PodTemplateSpec) string {
	var secrets []string
	for _, c := range append(append([]corev1.Container{}, t.Spec.InitContainers...), t.Spec.Containers...) {
		for _, from := range c.EnvFrom {
			if from.SecretRef != nil {
				secrets = append(secrets, from.SecretRef.Name)
			}
		}
		for _, env := range c.Env {
			if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil {
				secrets = append(secrets, env.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	sort.Strings(secrets)
	return t.Annotations["kipper.run/binding-credentials"] + "|" + strings.Join(secrets, ",")
}

func (r *passwordRotation) printReport() {
	say(r.out, "\n  Still holding the old password until you update them:\n")
	say(r.out, "     - runs of jobs and cron functions that started before the rotation\n")
	say(r.out, "     - values pasted by hand into an environment variable, a job or an external client\n")
	say(r.out, "     - backups and dumps taken before now, which may restore the old password\n")
	say(r.out, "\n  See the new password with: kip service info %s --show-secrets\n\n", r.service)
}
