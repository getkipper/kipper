package handlers

import (
	"bytes"
	"context"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
)

func filesRouter(h *Files) *chi.Mux {
	r := chi.NewRouter()
	r.Route("/projects/{name}/apps/{app}", func(r chi.Router) {
		r.Get("/files", h.List)
		r.Get("/files/content", h.Content)
		r.Get("/files/download", h.Download)
		// Save and Upload belong here as much as the read routes: a guard with
		// no route in this suite can be deleted without a test noticing.
		r.Put("/files/content", h.Save)
		r.Post("/files/upload", h.Upload)
	})
	return r
}

func TestFiles_List_DefaultsToRoot(t *testing.T) {
	// With no pods, should return 404
	client := fake.NewClientset()
	h := &Files{Client: client}
	r := filesRouter(h)

	req := httptest.NewRequest("GET", "/projects/staging/apps/myapp/files", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "no running pod found")
}

func TestFiles_Content_MissingPath(t *testing.T) {
	// Even without pods, missing path should return 400
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-abc123",
			Namespace: "staging",
			Labels:    map[string]string{"app": "myapp"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewClientset(pod)
	h := &Files{Client: client}
	r := filesRouter(h)

	req := httptest.NewRequest("GET", "/projects/staging/apps/myapp/files/content", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "path query parameter is required")
}

func TestFiles_Download_MissingPath(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-abc123",
			Namespace: "staging",
			Labels:    map[string]string{"app": "myapp"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewClientset(pod)
	h := &Files{Client: client}
	r := filesRouter(h)

	req := httptest.NewRequest("GET", "/projects/staging/apps/myapp/files/download", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusBadRequest, rec.Code)
	assert.Contains(t, rec.Body.String(), "path query parameter is required")
}

func TestFiles_AppNotFound(t *testing.T) {
	client := fake.NewClientset()
	h := &Files{Client: client}
	r := filesRouter(h)

	tests := []struct {
		name string
		path string
	}{
		{"list files for nonexistent app", "/projects/staging/apps/ghost/files?path=/"},
		{"content for nonexistent app", "/projects/staging/apps/ghost/files/content?path=/etc/hosts"},
		{"download for nonexistent app", "/projects/staging/apps/ghost/files/download?path=/etc/hosts"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusNotFound, rec.Code)
			assert.Contains(t, rec.Body.String(), "no running pod found")
		})
	}
}

// Save and Upload write into the pod, so a target the console must not write to
// has to be refused before tee runs. These are refused on the spelling alone.
//
// The pod resolves every path to somewhere ordinary here, so the lexical check
// is the only thing that can refuse: deleting it from either handler fails its
// cases rather than being covered by the resolved-target guard.
func TestFiles_BlockedPath_RefusesWrites(t *testing.T) {
	targets := []string{
		"/var/run/secrets/kubernetes.io/serviceaccount/token",
		"/run/secrets/kubernetes.io/serviceaccount/token",
		"var/run/secrets/kubernetes.io/serviceaccount/token",
		"/proc/self/environ",
	}

	for _, target := range targets {
		t.Run("save "+target, func(t *testing.T) {
			rec := httptest.NewRecorder()
			filesWithPod(&podAnswers{resolved: "/app/data/ordinary"}).ServeHTTP(rec,
				httptest.NewRequest("PUT", "/projects/staging/apps/myapp/files/content?path="+target,
					strings.NewReader("x")))
			assert.Equal(t, http.StatusForbidden, rec.Code, "writing to %s must be refused", target)
		})

		// Upload names its target from the directory it was sent to plus the
		// uploaded filename, so the blocked path arrives split across the two.
		t.Run("upload "+target, func(t *testing.T) {
			dir, name := path.Split(target)
			rec := httptest.NewRecorder()
			filesWithPod(&podAnswers{resolved: "/app/data/ordinary"}).ServeHTTP(rec,
				uploadOf(t, dir, name))
			assert.Equal(t, http.StatusForbidden, rec.Code, "uploading to %s must be refused", target)
		})
	}
}

// podAnswers stands in for the container, so a test can decide what a path
// resolves to. That answer is the blocked-target guard's entire input, and
// nothing reachable from a fake clientset can produce it.
type podAnswers struct {
	resolved string
	ran      [][]string
}

func (p *podAnswers) run(_ context.Context, _, _, _ string, command []string) (string, error) {
	p.ran = append(p.ran, command)
	switch command[0] {
	case "readlink":
		return p.resolved + "\n", nil
	case "ls":
		return "total 4\n-rw-r--r-- 1 root root 7 2024-03-21 10:00 current\n", nil
	case "stat":
		return "7\n", nil
	case "cat":
		return "hunter2", nil
	}
	return "", nil
}

func (p *podAnswers) ranCommand(name string) bool {
	for _, c := range p.ran {
		if len(c) > 0 && c[0] == name {
			return true
		}
	}
	return false
}

func filesWithPod(answers *podAnswers) *chi.Mux {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "myapp-abc123", Namespace: "staging",
			Labels: map[string]string{"app": "myapp"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	return filesRouter(&Files{
		Client:   fake.NewClientset(pod),
		Config:   &rest.Config{Host: "https://127.0.0.1:6443"},
		runInPod: answers.run,
	})
}

func uploadOf(t *testing.T, dir, filename string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", filename)
	assert.NoError(t, err)
	_, err = part.Write([]byte("x"))
	assert.NoError(t, err)
	assert.NoError(t, form.Close())

	req := httptest.NewRequest("POST",
		"/projects/staging/apps/myapp/files/upload?path="+dir, &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	return req
}

// An image can carry a symlink from an ordinary path into a blocked one, and
// stat, cat and tee all follow it. Listing, reading and downloading are open to
// a project viewer while a shell needs deployer, so this is a viewer's route to
// the ServiceAccount token — the reason the guard resolves the target rather
// than trusting the spelling.
//
// Every case sends a path the lexical check allows and has the pod resolve it
// to the projected token, in the form a live workload returns. Deleting the
// refuseBlockedTarget call from the handler under test fails its case.
func TestFiles_RefusesAPathResolvingIntoABlockedLocation(t *testing.T) {
	const resolved = "/run/secrets/kubernetes.io/serviceaccount/..2026_08_05_13_00_55.293925486/token"

	tests := []struct {
		name    string
		request func(t *testing.T) *http.Request
		// The command the handler would have run on the resolved target. It
		// must not reach the pod once the guard has refused.
		reads string
	}{
		{
			name: "list",
			request: func(*testing.T) *http.Request {
				return httptest.NewRequest("GET", "/projects/staging/apps/myapp/files?path=/app/logs", nil)
			},
			reads: "ls",
		},
		{
			name: "content",
			request: func(*testing.T) *http.Request {
				return httptest.NewRequest("GET", "/projects/staging/apps/myapp/files/content?path=/app/logs/current", nil)
			},
			reads: "cat",
		},
		{
			name: "download",
			request: func(*testing.T) *http.Request {
				return httptest.NewRequest("GET", "/projects/staging/apps/myapp/files/download?path=/app/logs/current", nil)
			},
			reads: "cat",
		},
		{
			name: "save",
			request: func(*testing.T) *http.Request {
				return httptest.NewRequest("PUT", "/projects/staging/apps/myapp/files/content?path=/app/logs/current",
					strings.NewReader("x"))
			},
			reads: "tee",
		},
		{
			name: "upload",
			request: func(t *testing.T) *http.Request {
				return uploadOf(t, "/app/uploads", "current")
			},
			reads: "tee",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answers := &podAnswers{resolved: resolved}
			rec := httptest.NewRecorder()
			filesWithPod(answers).ServeHTTP(rec, tt.request(t))

			assert.Equal(t, http.StatusForbidden, rec.Code,
				"a path resolving to %s must be refused", resolved)
			assert.True(t, answers.ranCommand("readlink"),
				"the guard must resolve the target in the pod")
			assert.False(t, answers.ranCommand(tt.reads),
				"%s must not run on a target the guard refused", tt.reads)
		})
	}
}

// The mirror of the case above: the same handlers on the same paths, resolving
// somewhere ordinary, must go through. Without this, refusing everything would
// pass the test above.
func TestFiles_AllowsAPathResolvingSomewhereOrdinary(t *testing.T) {
	tests := []struct {
		name    string
		request func(t *testing.T) *http.Request
		reads   string
	}{
		{
			name: "list",
			request: func(*testing.T) *http.Request {
				return httptest.NewRequest("GET", "/projects/staging/apps/myapp/files?path=/app/logs", nil)
			},
			reads: "ls",
		},
		{
			name: "content",
			request: func(*testing.T) *http.Request {
				return httptest.NewRequest("GET", "/projects/staging/apps/myapp/files/content?path=/app/logs/current", nil)
			},
			reads: "cat",
		},
		{
			name: "download",
			request: func(*testing.T) *http.Request {
				return httptest.NewRequest("GET", "/projects/staging/apps/myapp/files/download?path=/app/logs/current", nil)
			},
			reads: "cat",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			answers := &podAnswers{resolved: "/app/logs/2026-08-05.log"}
			rec := httptest.NewRecorder()
			filesWithPod(answers).ServeHTTP(rec, tt.request(t))

			assert.Equal(t, http.StatusOK, rec.Code)
			assert.True(t, answers.ranCommand(tt.reads), "%s should have run", tt.reads)
		})
	}
}

// A path that will not resolve is refused rather than falling back to the
// lexical answer, which would hand the bypass back to any image that cannot
// resolve paths at all. Both ways of failing are covered: readlink absent from
// the image, which fails the exec, and readlink answering nothing.
func TestFiles_RefusesWhenThePodCannotResolveThePath(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, string, string, string, []string) (string, error)
	}{
		{
			name: "no readlink in the image",
			run: func(_ context.Context, _, _, _ string, command []string) (string, error) {
				if command[0] == "readlink" {
					return "", errors.New("executable file not found in $PATH")
				}
				return "hunter2", nil
			},
		},
		{
			name: "readlink answers nothing",
			run: func(_ context.Context, _, _, _ string, command []string) (string, error) {
				if command[0] == "readlink" {
					return "\n", nil
				}
				return "hunter2", nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name: "myapp-abc123", Namespace: "staging",
					Labels: map[string]string{"app": "myapp"},
				},
				Status: corev1.PodStatus{Phase: corev1.PodRunning},
			}
			r := filesRouter(&Files{Client: fake.NewClientset(pod), runInPod: tt.run})

			rec := httptest.NewRecorder()
			r.ServeHTTP(rec,
				httptest.NewRequest("GET", "/projects/staging/apps/myapp/files/content?path=/app/logs/current", nil))

			assert.Equal(t, http.StatusForbidden, rec.Code)
			// "restricted" would be wrong here: the path is ordinary and the
			// pod simply could not answer, which on a readlink-less image is
			// every path including /. An operator told their app directory is
			// restricted has nothing to tell the two apart.
			assert.Contains(t, rec.Body.String(), "could not be resolved")
			assert.NotContains(t, rec.Body.String(), "is restricted")
		})
	}
}

func TestIsBlockedPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		blocked bool
	}{
		{"proc root", "/proc", true},
		{"sys root", "/sys", true},
		{"dev root", "/dev", true},
		{"proc subpath", "/proc/cpuinfo", true},
		{"sys subpath", "/sys/class/net", true},
		{"dev subpath", "/dev/null", true},
		{"app root", "/app", false},
		{"etc", "/etc", false},
		{"root slash", "/", false},
		{"tmp", "/tmp", false},
		{"proc-like name", "/processing", false},
		{"sys-like name", "/system", false},
		{"dev-like name", "/developer", false},
		{"nested safe path", "/app/proc/data", false},

		// Kubernetes projects the pod's ServiceAccount token here. Reading it is
		// not reading a file — it is taking the pod's identity and everything
		// that identity may do against the API server, which for a browser open
		// to a project member is an escalation out of the console's own model.
		{"service account projection root", "/var/run/secrets", true},
		{"service account token", "/var/run/secrets/kubernetes.io/serviceaccount/token", true},
		{"service account ca", "/var/run/secrets/kubernetes.io/serviceaccount/ca.crt", true},
		// /var/run is a symlink to /run on essentially every image, and
		// path.Clean does not resolve symlinks — so blocking only the /var
		// spelling leaves the same token readable one character shorter.
		{"service account via run symlink", "/run/secrets", true},
		{"service account token via run symlink", "/run/secrets/kubernetes.io/serviceaccount/token", true},
		// A mount whose name merely starts the same way is an ordinary directory.
		{"secrets-like name", "/var/run/secretstuff", false},
		{"secrets-like name via run", "/run/secretstuff", false},
		{"unrelated var run path", "/var/run/app.sock", false},

		// A path the console did not anchor resolves against the container's
		// working directory — execve carries no shell, and the cwd is the image's
		// WORKDIR, which is / when unset. So the relative spelling reaches the
		// same file while matching none of the absolute prefixes above.
		{"relative secrets path", "var/run/secrets/kubernetes.io/serviceaccount/token", true},
		{"dot-relative secrets path", "./var/run/secrets/kubernetes.io/serviceaccount/token", true},
		{"relative walk up", "../../var/run/secrets", true},
		{"relative ordinary path", "app/config.json", true},
		{"empty path", "", true},
		{"bare dot", ".", true},

		// What the pod actually returns for the token once the path is resolved.
		// Taken from `readlink -f` on a live workload: resolution rewrites the
		// /var spelling away and lands under /run, and the projected volume adds
		// a timestamped directory. Blocking only /var/run/secrets would let the
		// canonical form through.
		{"resolved projected token", "/run/secrets/kubernetes.io/serviceaccount/..2026_08_05_13_00_55.293925486/token", true},
		{"resolved projected dir", "/run/secrets/kubernetes.io/serviceaccount/..data", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isBlockedPath(tt.path)
			assert.Equal(t, tt.blocked, result)
		})
	}
}

func TestFiles_BlockedPath_Proc(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-abc123",
			Namespace: "staging",
			Labels:    map[string]string{"app": "myapp"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewClientset(pod)
	h := &Files{Client: client}
	r := filesRouter(h)

	req := httptest.NewRequest("GET", "/projects/staging/apps/myapp/files?path=/proc", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "restricted")
}

func TestFiles_BlockedPath_Sys(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-abc123",
			Namespace: "staging",
			Labels:    map[string]string{"app": "myapp"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewClientset(pod)
	h := &Files{Client: client}
	r := filesRouter(h)

	req := httptest.NewRequest("GET", "/projects/staging/apps/myapp/files?path=/sys", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "restricted")
}

func TestFiles_BlockedPath_Dev(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-abc123",
			Namespace: "staging",
			Labels:    map[string]string{"app": "myapp"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewClientset(pod)
	h := &Files{Client: client}
	r := filesRouter(h)

	req := httptest.NewRequest("GET", "/projects/staging/apps/myapp/files?path=/dev", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "restricted")
}

func TestFiles_BlockedPath_SubPath(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-abc123",
			Namespace: "staging",
			Labels:    map[string]string{"app": "myapp"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewClientset(pod)
	h := &Files{Client: client}
	r := filesRouter(h)

	tests := []struct {
		name string
		path string
	}{
		{"proc cpuinfo", "/projects/staging/apps/myapp/files?path=/proc/cpuinfo"},
		{"sys class", "/projects/staging/apps/myapp/files?path=/sys/class/net"},
		{"dev null", "/projects/staging/apps/myapp/files?path=/dev/null"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", tt.path, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			assert.Equal(t, http.StatusForbidden, rec.Code)
			assert.Contains(t, rec.Body.String(), "restricted")
		})
	}
}

func TestFiles_BlockedPath_Content(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-abc123",
			Namespace: "staging",
			Labels:    map[string]string{"app": "myapp"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewClientset(pod)
	h := &Files{Client: client}
	r := filesRouter(h)

	req := httptest.NewRequest("GET", "/projects/staging/apps/myapp/files/content?path=/proc/cpuinfo", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "restricted")
}

func TestFiles_BlockedPath_Download(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "myapp-abc123",
			Namespace: "staging",
			Labels:    map[string]string{"app": "myapp"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	client := fake.NewClientset(pod)
	h := &Files{Client: client}
	r := filesRouter(h)

	req := httptest.NewRequest("GET", "/projects/staging/apps/myapp/files/download?path=/sys/firmware", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "restricted")
}

func TestFiles_AllowedPath_ReturnsNoPodError(t *testing.T) {
	// /app is allowed but no pod has exec capability in fake client,
	// so we verify the path check passes and the request reaches pod lookup.
	client := fake.NewClientset()
	h := &Files{Client: client}
	r := filesRouter(h)

	req := httptest.NewRequest("GET", "/projects/staging/apps/myapp/files?path=/app", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Should get 404 (no pod found), not 403 (blocked path)
	assert.Equal(t, http.StatusNotFound, rec.Code)
	assert.Contains(t, rec.Body.String(), "no running pod found")
}

func TestFiles_ParseLsOutput(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected []fileEntry
	}{
		{
			name: "parses standard ls output",
			input: `total 20
drwxr-xr-x 3 root root 4096 2024-03-21 10:00 uploads
-rw-r--r-- 1 root root 1234 2024-03-21 10:00 config.php
lrwxrwxrwx 1 root root   11 2024-03-21 10:00 link -> /etc/hosts`,
			expected: []fileEntry{
				{Name: "uploads", Size: 4096, Permissions: "drwxr-xr-x", Modified: "2024-03-21 10:00", IsDir: true},
				{Name: "config.php", Size: 1234, Permissions: "-rw-r--r--", Modified: "2024-03-21 10:00", IsDir: false},
				{Name: "link", Size: 11, Permissions: "lrwxrwxrwx", Modified: "2024-03-21 10:00", IsDir: false},
			},
		},
		{
			name:     "skips dot entries",
			input:    "total 8\ndrwxr-xr-x 2 root root 4096 2024-03-21 10:00 .\ndrwxr-xr-x 3 root root 4096 2024-03-21 10:00 ..",
			expected: []fileEntry{},
		},
		{
			name:     "handles empty output",
			input:    "",
			expected: []fileEntry{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseLsOutput(tt.input)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Save writes the same file into every replica, so it resolves the target in
// all of them before writing to any. Interleaving the two lets a replica whose
// target is fine be written while a later replica's refusal answers 403, which
// tells the operator nothing was written when something was.
//
// The second pod is the one that refuses, so an implementation that wrote as it
// went would already have modified the first.
func TestFiles_SaveWritesNoPodWhenAnyReplicaRefusesTheTarget(t *testing.T) {
	tests := []struct {
		name   string
		second func(command []string) (string, error)
	}{
		{
			name: "the second replica resolves the target into a blocked location",
			second: func(command []string) (string, error) {
				if command[0] == "readlink" {
					return "/run/secrets/kubernetes.io/serviceaccount/token\n", nil
				}
				return "", nil
			},
		},
		{
			name: "the second replica cannot resolve the target at all",
			second: func(command []string) (string, error) {
				if command[0] == "readlink" {
					return "", errors.New("executable file not found in $PATH")
				}
				return "", nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pods := []runtime.Object{}
			for _, name := range []string{"myapp-1", "myapp-2"} {
				pods = append(pods, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name: name, Namespace: "staging",
						Labels: map[string]string{"app": "myapp"},
					},
					Status: corev1.PodStatus{Phase: corev1.PodRunning},
				})
			}

			var written []string
			h := &Files{
				Client: fake.NewClientset(pods...),
				runInPod: func(_ context.Context, _, pod, _ string, command []string) (string, error) {
					if pod == "myapp-2" {
						return tt.second(command)
					}
					if command[0] == "readlink" {
						return "/app/config.json\n", nil
					}
					return "", nil
				},
				writeInPod: func(_ context.Context, _, pod, _ string, _ []string, _ string) error {
					written = append(written, pod)
					return nil
				},
			}

			rec := httptest.NewRecorder()
			filesRouter(h).ServeHTTP(rec,
				httptest.NewRequest("PUT", "/projects/staging/apps/myapp/files/content?path=/app/config.json",
					strings.NewReader("x")))

			assert.Equal(t, http.StatusForbidden, rec.Code)
			assert.Empty(t, written,
				"no replica may be written to when another one's target is refused")
		})
	}
}
