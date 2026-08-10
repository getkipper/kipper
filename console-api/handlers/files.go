package handlers

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

const maxFileSize = 1 << 20 // 1MB

// Paths that are blocked from browsing for safety.
//
// /var/run/secrets is where Kubernetes projects the pod's ServiceAccount token
// and the API server's CA. Handing those to a browser is not disclosing a file:
// it is handing over the pod's identity and everything that identity may do
// against the API server, to anyone the console lets browse the pod. The other
// three are kernel interfaces that leak host detail the console has no business
// showing.
//
// /run/secrets is the same directory by its other name. /var/run is a symlink
// to /run on essentially every image, and the check below cleans a path without
// resolving symlinks, so blocking one spelling and not the other blocks nothing.
var blockedPaths = []string{"/proc", "/sys", "/dev", "/var/run/secrets", "/run/secrets"}

// isBlockedPath reports whether a path may not be browsed: either it is not
// anchored, or it names something the console does not serve.
//
// A path without a leading slash is resolved against the container's working
// directory — execve carries no shell, and the cwd is the image's WORKDIR, which
// is / when unset. So
// `var/run/secrets/…/token` reaches exactly the file `/var/run/secrets/…/token`
// does while matching none of the prefixes above, and a denylist of absolute
// paths answers "not blocked" to the same file under another spelling. Nothing
// the console offers needs a relative path, so they are refused rather than
// resolved.
//
// This is lexical and cannot see aliasing inside the pod, so it is the first of
// two checks rather than the whole answer: refuseBlockedTarget resolves the path
// in the container and applies this again to what comes back.
func isBlockedPath(p string) bool {
	cleaned := path.Clean(p)
	if !path.IsAbs(cleaned) {
		return true
	}
	for _, blocked := range blockedPaths {
		if cleaned == blocked || strings.HasPrefix(cleaned, blocked+"/") {
			return true
		}
	}
	return false
}

// blockedTargetError says a path was refused because of what it resolves to.
var errBlockedTarget = errors.New("path resolves to a restricted location")

// errUnresolvedTarget says the container could not answer what a path resolves
// to, which is refused for the same reason but is not the same fact.
var errUnresolvedTarget = errors.New("path could not be resolved in the container")

// refusalMessage says why a target was refused. "restricted" is the wrong word
// for a pod that could not answer: an image carrying no readlink refuses every
// path including /, and an operator told their app directory is restricted has
// nothing to distinguish that from a policy refusal.
func refusalMessage(err error, p, restricted string) string {
	if errors.Is(err, errUnresolvedTarget) {
		return fmt.Sprintf("%s could not be resolved in the container", p)
	}
	return restricted
}

// refuseBlockedTarget resolves p inside the container and reports whether what
// it actually names may be read or written.
//
// The lexical check above authorises the spelling the caller sent. It cannot see
// a symlink, so `/app/logs/current -> /var/run/secrets/kubernetes.io/serviceaccount`
// passes it, and stat, cat and tee all follow the link. That matters because
// listing, reading and downloading are open to a project viewer while a shell is
// not: a viewer with no other route to the ServiceAccount token could read it
// through a link the image already contains.
//
// So the target is canonicalised in the pod and the denylist applied to the
// answer. On a real workload `/var/run/secrets/kubernetes.io/serviceaccount/token`
// comes back as `/run/secrets/kubernetes.io/serviceaccount/..2026_.../token`,
// which is why both spellings are on the list — resolution rewrites the /var one
// away.
//
// A path that will not resolve is refused rather than allowed through on the
// lexical answer. readlink -f needs every component but the last to exist, so a
// read of something absent fails here instead of failing at cat, and a write
// into a directory that is not there fails here instead of at tee. Letting an
// unresolvable path fall back to the lexical check would hand back the bypass
// for any image that lacks readlink.
//
// There is a race between resolving and reading: a link swapped in between the
// two would be followed. Closing it needs a single in-pod operation that opens
// beneath a root rather than two commands. Under the threat this exists for it
// does not help — a viewer cannot write to the pod, and anything that can swap
// the link is already running in the container with the credential mounted.
func (f *Files) refuseBlockedTarget(ctx context.Context, namespace, pod, container, p string) error {
	out, err := f.execInPod(ctx, namespace, pod, container, []string{"readlink", "-f", "--", p})
	if err != nil {
		return fmt.Errorf("%w: %s", errUnresolvedTarget, p)
	}
	resolved := strings.TrimSpace(out)
	if resolved == "" {
		return fmt.Errorf("%w: %s", errUnresolvedTarget, p)
	}
	if isBlockedPath(resolved) {
		return fmt.Errorf("%w: %s", errBlockedTarget, p)
	}
	return nil
}

// Files provides handlers for browsing the filesystem inside running pods.
type Files struct {
	Client kubernetes.Interface
	Config *rest.Config

	// runInPod answers an in-pod command in place of the cluster. Production
	// leaves it nil and execInPod streams for real.
	//
	// The blocked-target guard decides on one thing only: what `readlink -f`
	// says the path resolves to. Nothing reachable from a fake clientset can
	// produce that answer, so without this a test cannot drive the guard at
	// all, and deleting a refuseBlockedTarget call leaves the suite green.
	runInPod func(ctx context.Context, namespace, pod, container string, command []string) (string, error)

	// writeInPod answers a write in place of the cluster, for the same reason
	// as runInPod. Save resolves the target in every replica before writing to
	// any, and that invariant is about which pods were written to — which a
	// test cannot see without standing in for the write itself.
	writeInPod func(ctx context.Context, namespace, pod, container string, cmd []string, content string) error
}

type fileEntry struct {
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	Permissions string `json:"permissions"`
	Modified    string `json:"modified"`
	IsDir       bool   `json:"is_dir"`
}

type fileListResponse struct {
	Path     string      `json:"path"`
	Entries  []fileEntry `json:"entries"`
	Pod      string      `json:"pod"`
	PodCount int         `json:"pod_count"`
}

// List returns the contents of a directory inside a running pod.
// GET /api/v1/projects/{name}/apps/{app}/files?path=/
func (f *Files) List(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")
	dirPath := r.URL.Query().Get("path")
	if dirPath == "" {
		dirPath = "/"
	}
	dirPath = path.Clean(dirPath)

	if isBlockedPath(dirPath) {
		respondError(w, http.StatusForbidden, fmt.Sprintf("access to %s is restricted for safety", dirPath))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	pod, podCount, err := f.findRunningPod(ctx, namespace, app)
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("no running pod found for app %q", app))
		return
	}
	podName := pod.Name
	container := appContainerFromPod(pod)

	if err := f.refuseBlockedTarget(ctx, namespace, podName, container, dirPath); err != nil {
		respondError(w, http.StatusForbidden, refusalMessage(err, dirPath,
			fmt.Sprintf("access to %s is restricted for safety", dirPath)))
		return
	}

	// Try GNU ls first, fall back to BusyBox ls
	output, err := f.execInPod(ctx, namespace, podName, container, []string{"ls", "-la", "--time-style=long-iso", dirPath})
	if err != nil || strings.Contains(output, "unrecognized option") {
		// BusyBox fallback — no --time-style support
		output, err = f.execInPod(ctx, namespace, podName, container, []string{"ls", "-la", dirPath})
		if err != nil {
			// Return user-friendly error messages
			errMsg := err.Error()
			if strings.Contains(errMsg, "Permission denied") {
				respondError(w, http.StatusForbidden, fmt.Sprintf("permission denied: cannot access %s", dirPath))
				return
			}
			if strings.Contains(errMsg, "No such file") {
				respondError(w, http.StatusNotFound, fmt.Sprintf("directory not found: %s", dirPath))
				return
			}
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to list directory: %v", err))
			return
		}
	}

	entries := parseLsOutput(output)

	respondJSON(w, http.StatusOK, fileListResponse{
		Path:     dirPath,
		Entries:  entries,
		Pod:      podName,
		PodCount: podCount,
	})
}

// Content returns the contents of a file inside a running pod.
// GET /api/v1/projects/{name}/apps/{app}/files/content?path=/app/config.php
func (f *Files) Content(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		respondError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	filePath = path.Clean(filePath)
	if isBlockedPath(filePath) {
		respondError(w, http.StatusForbidden, fmt.Sprintf("access to %s is restricted", filePath))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	pod, _, err := f.findRunningPod(ctx, namespace, app)
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("no running pod found for app %q", app))
		return
	}
	podName := pod.Name
	container := appContainerFromPod(pod)

	if err := f.refuseBlockedTarget(ctx, namespace, podName, container, filePath); err != nil {
		respondError(w, http.StatusForbidden, refusalMessage(err, filePath,
			fmt.Sprintf("access to %s is restricted", filePath)))
		return
	}

	// Check file size before reading
	sizeOutput, err := f.execInPod(ctx, namespace, podName, container, []string{"stat", "-c", "%s", filePath})
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("file not found: %s", filePath))
		return
	}

	size, err := strconv.ParseInt(strings.TrimSpace(sizeOutput), 10, 64)
	if err == nil && size > maxFileSize {
		respondError(w, http.StatusRequestEntityTooLarge, "file exceeds 1MB limit — use download instead")
		return
	}

	output, err := f.execInPod(ctx, namespace, podName, container, []string{"cat", filePath})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to read file: %v", err))
		return
	}

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(output)) //nolint:gosec // Content-Type is text/plain
}

// Download returns a file with Content-Disposition: attachment for downloading.
// GET /api/v1/projects/{name}/apps/{app}/files/download?path=/app/config.php
func (f *Files) Download(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")
	filePath := r.URL.Query().Get("path")
	if filePath == "" {
		respondError(w, http.StatusBadRequest, "path query parameter is required")
		return
	}
	filePath = path.Clean(filePath)
	if isBlockedPath(filePath) {
		respondError(w, http.StatusForbidden, fmt.Sprintf("access to %s is restricted", filePath))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	pod, _, err := f.findRunningPod(ctx, namespace, app)
	if err != nil {
		respondError(w, http.StatusNotFound, fmt.Sprintf("no running pod found for app %q", app))
		return
	}
	podName := pod.Name
	container := appContainerFromPod(pod)

	if err := f.refuseBlockedTarget(ctx, namespace, podName, container, filePath); err != nil {
		respondError(w, http.StatusForbidden, refusalMessage(err, filePath,
			fmt.Sprintf("access to %s is restricted", filePath)))
		return
	}

	output, err := f.execInPod(ctx, namespace, podName, container, []string{"cat", filePath})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to read file: %v", err))
		return
	}

	filename := path.Base(filePath)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Length", strconv.Itoa(len(output)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(output)) //nolint:gosec // Content-Type is application/octet-stream with attachment disposition
}

// Save writes content to a file inside ALL running pods for this app.
// PUT /api/v1/projects/{name}/apps/{app}/files/content?path=/app/config.php
func (f *Files) Save(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")
	filePath := r.URL.Query().Get("path")

	if filePath == "" {
		respondError(w, http.StatusBadRequest, "path is required")
		return
	}
	filePath = path.Clean(filePath)
	if isBlockedPath(filePath) {
		respondError(w, http.StatusForbidden, fmt.Sprintf("writing to %s is restricted", filePath))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	pods, err := f.findAllRunningPods(ctx, namespace, app)
	if err != nil || len(pods) == 0 {
		respondError(w, http.StatusNotFound, fmt.Sprintf("no running pod for app %q", app))
		return
	}

	// Read the request body, rejecting anything over the size cap
	body, err := io.ReadAll(io.LimitReader(r.Body, maxFileSize+1))
	if err != nil {
		respondError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(body) > maxFileSize {
		respondError(w, http.StatusBadRequest, "file content exceeds 1MB limit")
		return
	}
	content := string(body)

	// tee writes stdin to the target path with no shell, so the path
	// cannot be interpreted as a command.
	cmd := []string{"tee", filePath}
	var failed []string

	// Every pod resolves the target before any of them is written to.
	// Interleaving the two lets a replica whose target is fine be written while
	// a later replica's refusal answers 403, reporting a write that happened as
	// one that did not.
	for _, pod := range pods {
		if err := f.refuseBlockedTarget(ctx, namespace, pod.Name, appContainerFromPod(pod), filePath); err != nil {
			respondError(w, http.StatusForbidden, refusalMessage(err, filePath,
				fmt.Sprintf("writing to %s is restricted", filePath)))
			return
		}
	}

	for _, pod := range pods {
		if err := f.writeToOnePod(ctx, namespace, pod.Name, appContainerFromPod(pod), cmd, content); err != nil {
			failed = append(failed, pod.Name)
		}
	}

	if len(failed) > 0 {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"status":  "partial",
			"message": fmt.Sprintf("saved to %d/%d pods (failed: %s)", len(pods)-len(failed), len(pods), strings.Join(failed, ", ")),
		})
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":    "saved",
		"pod_count": len(pods),
	})
}

func (f *Files) writeToOnePod(ctx context.Context, namespace, pod, container string, cmd []string, content string) error {
	if f.writeInPod != nil {
		return f.writeInPod(ctx, namespace, pod, container, cmd, content)
	}

	reqExec := f.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   cmd,
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(f.Config, "POST", reqExec.URL())
	if err != nil {
		return err
	}

	stdin := bytes.NewBufferString(content)
	var stdout, stderr bytes.Buffer

	return executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: &stdout,
		Stderr: &stderr,
	})
}

// Upload handles multipart file upload to a pod.
// POST /api/v1/projects/{name}/apps/{app}/files/upload?path=/app/uploads/
func (f *Files) Upload(w http.ResponseWriter, r *http.Request) {
	namespace := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")
	dirPath := r.URL.Query().Get("path")

	if dirPath == "" {
		dirPath = "/"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	pod, _, err := f.findRunningPod(ctx, namespace, app)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}

	// Parse multipart form (10MB max)
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	if err := r.ParseMultipartForm(10 << 20); err != nil { //nolint:gosec // body capped to 10 MiB by MaxBytesReader above
		respondError(w, http.StatusBadRequest, "failed to parse upload")
		return
	}

	file, header, err := r.FormFile("file")
	if err != nil {
		respondError(w, http.StatusBadRequest, "file is required")
		return
	}
	defer func() { _ = file.Close() }()

	content, err := io.ReadAll(file)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to read uploaded file")
		return
	}

	// tee writes stdin to the target path with no shell, so neither the
	// path nor the uploaded filename can be interpreted as a command.
	targetPath := path.Join(dirPath, path.Base(header.Filename))
	if isBlockedPath(targetPath) {
		respondError(w, http.StatusForbidden, "cannot write to this path")
		return
	}
	if err := f.refuseBlockedTarget(ctx, namespace, pod.Name, appContainerFromPod(pod), targetPath); err != nil {
		respondError(w, http.StatusForbidden, refusalMessage(err, targetPath, "cannot write to this path"))
		return
	}
	cmd := []string{"tee", targetPath}

	if err := f.writeToOnePod(ctx, namespace, pod.Name, appContainerFromPod(pod), cmd, string(content)); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("failed to upload: %v", err))
		return
	}

	respondJSON(w, http.StatusOK, map[string]string{"status": "uploaded", "path": targetPath})
}

func (f *Files) findRunningPod(ctx context.Context, namespace, app string) (pod *corev1.Pod, podCount int, err error) {
	pods, err := f.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", app),
	})
	if err != nil {
		return nil, 0, fmt.Errorf("listing pods: %w", err)
	}

	var runningCount int
	var first *corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			runningCount++
			if first == nil {
				first = &pods.Items[i]
			}
		}
	}

	if first == nil {
		return nil, 0, fmt.Errorf("no running pod for app %q", app)
	}

	return first, runningCount, nil
}

func (f *Files) findAllRunningPods(ctx context.Context, namespace, app string) ([]*corev1.Pod, error) {
	pods, err := f.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", app),
	})
	if err != nil {
		return nil, err
	}
	var running []*corev1.Pod
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			running = append(running, &pods.Items[i])
		}
	}
	return running, nil
}

func (f *Files) execInPod(ctx context.Context, namespace, pod, container string, command []string) (string, error) {
	if f.runInPod != nil {
		return f.runInPod(ctx, namespace, pod, container, command)
	}

	req := f.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(f.Config, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("creating executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("%s", strings.TrimSpace(stderr.String()))
		}
		return "", err
	}

	return stdout.String(), nil
}

// parseLsOutput parses the output of `ls -la` into file entries.
// Supports both GNU coreutils and BusyBox formats:
//
//	GNU:     drwxr-xr-x 2 root root 4096 2024-03-21 10:00 dirname
//	BusyBox: drwxr-xr-x 2 root root 4096 Mar 18 00:11 dirname
func parseLsOutput(output string) []fileEntry {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	entries := make([]fileEntry, 0, len(lines))

	for _, line := range lines {
		if strings.HasPrefix(line, "total ") || line == "" {
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}

		permissions := fields[0]
		isDir := strings.HasPrefix(permissions, "d")
		size, _ := strconv.ParseInt(fields[4], 10, 64)

		// Detect format: GNU has YYYY-MM-DD at field 5, BusyBox has Mon at field 5
		var modified, name string
		if len(fields[5]) == 10 && fields[5][4] == '-' {
			// GNU format: fields[5]=date fields[6]=time fields[7+]=name
			modified = fields[5] + " " + fields[6]
			name = strings.Join(fields[7:], " ")
		} else {
			// BusyBox format: fields[5]=Mon fields[6]=DD fields[7]=HH:MM fields[8+]=name
			if len(fields) < 9 {
				continue
			}
			modified = fields[5] + " " + fields[6] + " " + fields[7]
			name = strings.Join(fields[8:], " ")
		}

		if name == "." || name == ".." {
			continue
		}

		// Handle symlinks: "name -> target"
		if idx := strings.Index(name, " -> "); idx != -1 {
			name = name[:idx]
		}

		entries = append(entries, fileEntry{
			Name:        name,
			Size:        size,
			Permissions: permissions,
			Modified:    modified,
			IsDir:       isDir,
		})
	}

	return entries
}
