package ai

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// AdminCreateOptions are the inputs to CreateAdmin.
type AdminCreateOptions struct {
	Email    string
	Name     string
	Username string
	Password string
}

// CreateAdmin runs LibreChat's `npm run create-user` script inside a
// running librechat pod via the apiserver's exec subresource. The
// script is the only way to seed an admin account when
// ALLOW_REGISTRATION is off, which is our default.
//
// Argument order is `email name username password` (verified against
// danny-avila/LibreChat config/create-user.js). The `--` separator is
// required so npm passes the rest through to the script.
func CreateAdmin(
	ctx context.Context,
	clientset kubernetes.Interface,
	restConfig *rest.Config,
	out io.Writer,
	opts AdminCreateOptions,
) error {
	if err := validateAdminOptions(&opts); err != nil {
		return err
	}

	pod, err := pickReadyLibreChatPod(ctx, clientset)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "  ...  Creating admin user via pod %s\n", pod)

	cmd := []string{
		"npm", "run", "create-user", "--",
		opts.Email, opts.Name, opts.Username, opts.Password,
		"--email-verified=true",
	}

	stdoutBuf := &bytes.Buffer{}
	stderrBuf := &bytes.Buffer{}
	if err := execInPod(ctx, clientset, restConfig, pod, cmd, stdoutBuf, stderrBuf); err != nil {
		// LibreChat's script writes errors (e.g. duplicate email) to
		// stderr; surface that to the user.
		stderr := strings.TrimSpace(stderrBuf.String())
		stdout := strings.TrimSpace(stdoutBuf.String())
		if stderr != "" {
			return fmt.Errorf("create-user failed: %w\nstderr: %s\nstdout: %s", err, stderr, stdout)
		}
		return fmt.Errorf("create-user failed: %w\noutput: %s", err, stdout)
	}

	_, _ = fmt.Fprintf(out, "  ✔   Admin user %q created (username %q)\n", opts.Email, opts.Username)
	return nil
}

// validateAdminOptions checks required fields and fills defaults. The
// username defaults to the local part of the email so users only need
// to pass three flags in the common case.
func validateAdminOptions(opts *AdminCreateOptions) error {
	if opts.Email == "" {
		return errors.New("--email is required")
	}
	if !strings.Contains(opts.Email, "@") {
		return fmt.Errorf("email %q does not look like an email address", opts.Email)
	}
	if opts.Password == "" {
		return errors.New("--password is required")
	}
	if len(opts.Password) < 8 {
		return errors.New("password must be at least 8 characters")
	}
	if opts.Name == "" {
		return errors.New("--name is required")
	}
	if opts.Username == "" {
		opts.Username = strings.SplitN(opts.Email, "@", 2)[0]
	}
	return nil
}

// pickReadyLibreChatPod returns the name of a running, Ready pod owned
// by the LibreChat Deployment. We read the Deployment's
// spec.selector.matchLabels rather than hardcoding label keys, because
// chart authors do not always follow the same conventions and a
// hardcoded selector silently misses the pod when they pick different
// labels.
func pickReadyLibreChatPod(ctx context.Context, clientset kubernetes.Interface) (string, error) {
	dep, err := clientset.AppsV1().Deployments(Namespace).Get(ctx, LibreChatDeploymentName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", fmt.Errorf("deployment %s/%s not found; is the bundle installed? Run 'kip ai status'", Namespace, LibreChatDeploymentName)
	}
	if err != nil {
		return "", fmt.Errorf("reading deployment %s/%s: %w", Namespace, LibreChatDeploymentName, err)
	}
	if dep.Spec.Selector == nil || len(dep.Spec.Selector.MatchLabels) == 0 {
		return "", fmt.Errorf("deployment %s/%s has no label selector; cannot find its pods", Namespace, LibreChatDeploymentName)
	}
	parts := make([]string, 0, len(dep.Spec.Selector.MatchLabels))
	for k, v := range dep.Spec.Selector.MatchLabels {
		parts = append(parts, fmt.Sprintf("%s=%s", k, v))
	}
	pods, err := clientset.CoreV1().Pods(Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: strings.Join(parts, ","),
	})
	if err != nil {
		return "", fmt.Errorf("listing librechat pods: %w", err)
	}
	for _, p := range pods.Items {
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		if !podReady(p) {
			continue
		}
		return p.Name, nil
	}
	return "", fmt.Errorf("no Ready librechat pod found in namespace %s; run 'kip ai status' to check bundle health", Namespace)
}

func podReady(p corev1.Pod) bool {
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// execInPod runs cmd inside the named pod's first container via the
// apiserver's exec subresource. SPDY upgrade streams the result back.
func execInPod(
	ctx context.Context,
	clientset kubernetes.Interface,
	restConfig *rest.Config,
	pod string,
	cmd []string,
	stdout, stderr io.Writer,
) error {
	req := clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: cmd,
			Stdout:  true,
			Stderr:  true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("building exec request: %w", err)
	}
	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: stdout,
		Stderr: stderr,
	}); err != nil {
		return fmt.Errorf("exec stream: %w", err)
	}
	return nil
}
