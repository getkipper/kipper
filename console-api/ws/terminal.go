package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/getkipper/kipper/console-api/middleware"
)

const (
	// pongWait is how long the server waits for a pong before treating the
	// connection as dead. pingPeriod must be shorter so a ping goes out well
	// before the deadline.
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
	// writeWait bounds a single frame write so a stuck client (full TCP send
	// buffer) fails the write instead of blocking the handler forever.
	writeWait = 10 * time.Second
)

// keepalive sends a WebSocket ping on an interval until stop is closed.
// WriteControl is safe to call concurrently with the stream's writes.
func keepalive(conn *websocket.Conn, stop <-chan struct{}) {
	ticker := time.NewTicker(pingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return
			}
		case <-stop:
			return
		}
	}
}

// Terminal handles WebSocket connections for interactive pod shells.
type Terminal struct {
	Client   kubernetes.Interface
	Config   *rest.Config
	Issuer   string
	Audience string
	KeyFunc  jwt.Keyfunc
	Resolver *middleware.ProjectAccessResolver
}

type resizeMessage struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols"`
	Rows uint16 `json:"rows"`
}

// Handle upgrades the connection to WebSocket and proxies stdin/stdout
// between the browser and a pod exec stream. The Dex JWT travels in the
// Sec-WebSocket-Protocol header (see authSubprotocol), not the URL.
// URL: /api/v1/terminal/{namespace}/{app}?pod=optional
func (t *Terminal) Handle(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// Expected: api/v1/terminal/{namespace}/{app}
	if len(parts) < 5 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	namespace := parts[3]
	app := parts[4]

	email, ok := AuthenticatedEmail(r, t.Issuer, t.Audience, t.KeyFunc)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	// A shell is a mutating action, so it needs deployer or above on the
	// project that owns the namespace.
	if !authorizeProject(r.Context(), t.Resolver, email, namespace, middleware.ProjectRoleDeployer) {
		http.Error(w, "forbidden: you do not have deploy access to this project", http.StatusForbidden)
		return
	}

	podName := r.URL.Query().Get("pod")

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("terminal websocket upgrade failed: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()

	ctx := r.Context()

	pod, findErr := t.findPod(r, namespace, app, podName)
	if findErr != nil {
		writeError(conn, fmt.Sprintf("no running pod found: %v", findErr))
		return
	}
	podName = pod.Name
	// Pick the app container so exec targets it rather than the sidecar
	// proxy, which would otherwise make the exec request ambiguous.
	container := appContainerName(pod, app)

	// Send welcome banner
	writeBanner(conn, app, namespace, podName, containerImage(pod, container))

	// Prefer bash for readline support (arrow keys, history), fall back to sh
	shell := t.detectShell(r.Context(), namespace, podName, container)

	req := t.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   []string{shell},
			Stdin:     true,
			Stdout:    true,
			Stderr:    true,
			TTY:       true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(t.Config, "POST", req.URL())
	if err != nil {
		writeError(conn, fmt.Sprintf("failed to create executor: %v", err))
		return
	}

	// wsStream bridges the WebSocket connection and the exec stream
	stream := &wsStream{
		conn:    conn,
		sizeCh:  make(chan remotecommand.TerminalSize, 1),
		writeMu: sync.Mutex{},
	}

	// Keep the connection alive across idle periods. Without pings, proxies
	// (Traefik, load balancers) close a silent WebSocket after their idle
	// timeout, which shows up as the terminal randomly disconnecting.
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	stopPing := make(chan struct{})
	defer close(stopPing)
	go keepalive(conn, stopPing)

	err = executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             stream,
		Stdout:            stream,
		Stderr:            stream,
		Tty:               true,
		TerminalSizeQueue: stream,
	})
	if err != nil {
		// Connection likely closed by client — only log unexpected errors
		if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
			log.Printf("terminal exec stream ended: %v", err)
		}
	}
}

// detectShell checks whether bash is available in the pod, falling back to sh.
func (t *Terminal) detectShell(ctx context.Context, namespace, pod, container string) string {
	req := t.Client.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   []string{"test", "-x", "/bin/bash"},
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	executor, err := remotecommand.NewSPDYExecutor(t.Config, "POST", req.URL())
	if err != nil {
		return "/bin/sh"
	}

	if err := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: io.Discard,
		Stderr: io.Discard,
	}); err != nil {
		return "/bin/sh"
	}

	return "/bin/bash"
}

// findPod returns the pod to exec into. When podName is given it fetches
// that specific pod (verifying it belongs to the app); otherwise it picks
// the first running pod for the app.
func (t *Terminal) findPod(r *http.Request, namespace, app, podName string) (*corev1.Pod, error) {
	ctx := r.Context()

	if podName != "" {
		pod, err := t.Client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("getting pod %q: %w", podName, err)
		}
		if pod.Labels["app"] != app {
			return nil, fmt.Errorf("pod %q does not belong to app %q", podName, app)
		}
		if pod.Status.Phase != corev1.PodRunning {
			return nil, fmt.Errorf("pod %q is not running", podName)
		}
		return pod, nil
	}

	pods, err := t.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", app),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			return &pods.Items[i], nil
		}
	}
	return nil, fmt.Errorf("no running pod for app %q", app)
}

// containerImage returns the image of the named container, or the first
// container's image when the name is empty.
func containerImage(pod *corev1.Pod, container string) string {
	for _, c := range pod.Spec.Containers {
		if container == "" || c.Name == container {
			return c.Image
		}
	}
	if len(pod.Spec.Containers) > 0 {
		return pod.Spec.Containers[0].Image
	}
	return ""
}

func writeBanner(conn *websocket.Conn, app, namespace, pod, image string) {
	cyan := "\033[36m"
	green := "\033[32m"
	dim := "\033[2m"
	bold := "\033[1m"
	reset := "\033[0m"

	// Truncate long image names
	shortImage := image
	if len(shortImage) > 50 {
		shortImage = "..." + shortImage[len(shortImage)-47:]
	}

	lines := []string{
		"",
		cyan + bold + "   _  ___                       ",
		"  | |/ (_)_ __  _ __   ___ _ __ ",
		"  | ' /| | '_ \\| '_ \\ / _ \\ '__|",
		"  | . \\| | |_) | |_) |  __/ |   ",
		"  |_|\\_\\_| .__/| .__/ \\___|_|   ",
		"         |_|   |_|" + reset,
		"",
		fmt.Sprintf("  %sApp:%s        %s%s%s", dim, reset, bold, app, reset),
		fmt.Sprintf("  %sNamespace:%s  %s%s%s", dim, reset, bold, namespace, reset),
		fmt.Sprintf("  %sPod:%s        %s%s%s", dim, reset, bold, pod, reset),
		fmt.Sprintf("  %sImage:%s      %s%s%s", dim, reset, bold, shortImage, reset),
		"",
		fmt.Sprintf("  %sType 'exit' to disconnect%s", green, reset),
		"",
	}

	banner := ""
	for _, line := range lines {
		banner += line + "\r\n"
	}

	_ = conn.WriteMessage(websocket.TextMessage, []byte(banner))
}

// wsStream adapts a gorilla/websocket connection to the io.Reader/io.Writer
// interfaces needed by remotecommand.StreamOptions, and implements
// remotecommand.TerminalSizeQueue for resize events.
type wsStream struct {
	conn    *websocket.Conn
	sizeCh  chan remotecommand.TerminalSize
	buf     []byte
	writeMu sync.Mutex
}

// Read reads from the WebSocket connection. Resize messages are handled
// separately and forwarded to the TerminalSizeQueue channel.
func (s *wsStream) Read(p []byte) (int, error) {
	for {
		if len(s.buf) > 0 {
			n := copy(p, s.buf)
			s.buf = s.buf[n:]
			return n, nil
		}

		_, data, err := s.conn.ReadMessage()
		if err != nil {
			return 0, io.EOF
		}

		// Check if this is a resize message
		if len(data) > 0 && data[0] == '{' {
			var msg resizeMessage
			if json.Unmarshal(data, &msg) == nil && msg.Type == "resize" {
				select {
				case s.sizeCh <- remotecommand.TerminalSize{
					Width:  msg.Cols,
					Height: msg.Rows,
				}:
				default:
				}
				continue
			}
		}

		s.buf = data
	}
}

// Write sends data from the exec stream back to the browser via WebSocket.
func (s *wsStream) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	if err := s.conn.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Next blocks until a terminal resize event is received.
func (s *wsStream) Next() *remotecommand.TerminalSize {
	size, ok := <-s.sizeCh
	if !ok {
		return nil
	}
	return &size
}
