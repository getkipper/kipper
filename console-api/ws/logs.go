package ws

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/console-api/middleware"
)

// The client sends the auth sentinel as a subprotocol; echoing it back is
// what makes the browser complete the handshake. The token, offered as a
// second subprotocol, is never selected, so it stays out of the response.
var upgrader = websocket.Upgrader{
	CheckOrigin:  middleware.CheckWebSocketOrigin,
	Subprotocols: []string{authSubprotocol},
}

// LogStreamer handles WebSocket connections for streaming pod logs.
type LogStreamer struct {
	Client   kubernetes.Interface
	Issuer   string
	Audience string
	KeyFunc  jwt.Keyfunc
	Resolver *middleware.ProjectAccessResolver
}

// Handle upgrades the HTTP connection to WebSocket and streams logs
// from the first running pod of the named deployment.
// WS /api/v1/projects/{name}/apps/{app}/logs
// HandleRaw handles WebSocket connections without Chi URL params.
// Parses /api/v1/projects/{name}/apps/{app}/logs from the raw path.
func (l *LogStreamer) HandleRaw(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	// Expected: api/v1/projects/{name}/apps/{app}/logs
	if len(parts) < 7 {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	project := parts[3]
	app := parts[5]
	l.streamLogs(w, r, project, app)
}

// Handle handles WebSocket connections using Chi URL params.
func (l *LogStreamer) Handle(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	app := chi.URLParam(r, "app")
	l.streamLogs(w, r, project, app)
}

func (l *LogStreamer) streamLogs(w http.ResponseWriter, r *http.Request, project, app string) {
	// This handler runs on the raw mux, bypassing the Chi auth chain, so it
	// authenticates and authorizes here before upgrading. Reading logs needs
	// membership of the project (viewer or above).
	email, ok := AuthenticatedEmail(r, l.Issuer, l.Audience, l.KeyFunc)
	if !ok {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !authorizeProject(r.Context(), l.Resolver, email, project, middleware.ProjectRoleViewer) {
		http.Error(w, "forbidden: you do not have access to this project", http.StatusForbidden)
		return
	}

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("websocket upgrade failed: %v", err)
		return
	}
	defer func() { _ = conn.Close() }()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	// Keep the connection alive and detect dead clients: without pings a
	// silently disconnected browser would leave this handler and its log
	// stream running until the pod stopped writing.
	_ = conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongWait))
	})
	stopPing := make(chan struct{})
	defer close(stopPing)
	go keepalive(conn, stopPing)

	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				cancel()
				return
			}
		}
	}()

	var pod *corev1.Pod
	if podName := r.URL.Query().Get("pod"); podName != "" {
		pod, err = l.findSpecificPod(ctx, project, app, podName)
	} else {
		pod, err = l.findPod(ctx, project, app)
	}
	if err != nil {
		writeError(conn, err.Error())
		return
	}

	// Use the app container name for multi-container pods (sidecar support)
	container := appContainerName(pod, app)

	// First try to get previous container logs (from crashed containers)
	if pod.Status.Phase != corev1.PodRunning {
		l.streamPreviousLogs(ctx, conn, project, pod.Name, container)
	}

	// Then stream current logs
	tailLines := parseTailLines(r.URL.Query().Get("tail"))
	follow := pod.Status.Phase == corev1.PodRunning
	req := l.Client.CoreV1().Pods(project).GetLogs(pod.Name, &corev1.PodLogOptions{
		Container: container,
		Follow:    follow,
		TailLines: &tailLines,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		// If current logs fail, try previous logs as fallback
		l.streamPreviousLogs(ctx, conn, project, pod.Name, container)
		return
	}
	defer func() { _ = stream.Close() }()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := conn.WriteMessage(websocket.TextMessage, scanner.Bytes()); err != nil {
			return
		}
	}
}

func (l *LogStreamer) streamPreviousLogs(ctx context.Context, conn *websocket.Conn, namespace, podName, container string) {
	writeError(conn, "--- Previous container logs (crashed) ---")

	tailLines := int64(200)
	previous := true
	req := l.Client.CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: container,
		Previous:  previous,
		TailLines: &tailLines,
	})

	stream, err := req.Stream(ctx)
	if err != nil {
		writeError(conn, fmt.Sprintf("no previous logs available: %v", err))
		return
	}
	defer func() { _ = stream.Close() }()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
		_ = conn.WriteMessage(websocket.TextMessage, scanner.Bytes())
	}

	writeError(conn, "--- End of previous container logs ---")
}

func (l *LogStreamer) findSpecificPod(ctx context.Context, namespace, app, podName string) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pod, err := l.Client.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("pod %q not found: %w", podName, err)
	}

	if pod.Labels["app"] != app {
		return nil, fmt.Errorf("pod %q does not belong to app %q", podName, app)
	}

	return pod, nil
}

func (l *LogStreamer) findPod(ctx context.Context, namespace, app string) (*corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	pods, err := l.Client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("app=%s", app),
	})
	if err != nil {
		return nil, fmt.Errorf("failed to list pods: %w", err)
	}

	// Prefer running pods
	for i := range pods.Items {
		if pods.Items[i].Status.Phase == corev1.PodRunning {
			return &pods.Items[i], nil
		}
	}

	// Return any pod (crashed, pending, etc.)
	if len(pods.Items) > 0 {
		return &pods.Items[0], nil
	}

	return nil, fmt.Errorf("no pods found for app %q", app)
}

// appContainerName returns the container name to stream logs from.
// For single-container pods this returns empty (Kubernetes defaults to
// the only container). For multi-container pods (e.g. with the sidecar
// proxy) it returns the app container name to avoid streaming sidecar logs.
func appContainerName(pod *corev1.Pod, app string) string {
	if len(pod.Spec.Containers) <= 1 {
		return ""
	}
	for _, c := range pod.Spec.Containers {
		if c.Name == app {
			return app
		}
	}
	return pod.Spec.Containers[0].Name
}

func parseTailLines(s string) int64 {
	if s == "" {
		return 100
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 {
		return 100
	}
	if n > 1000 {
		return 1000
	}
	return n
}

func writeError(conn *websocket.Conn, msg string) {
	_ = conn.SetWriteDeadline(time.Now().Add(writeWait))
	_ = conn.WriteMessage(websocket.TextMessage, []byte(msg))
}
