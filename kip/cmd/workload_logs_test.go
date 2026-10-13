package cmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func logsTestPod(name string, containers ...string) corev1.Pod {
	pod := corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}}
	for _, c := range containers {
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: c})
	}
	return pod
}

func TestLogsCommandsDoNotFollowByDefault(t *testing.T) {
	for _, c := range []*cobra.Command{appLogsCmd, functionLogsCmd} {
		f := c.Flags().Lookup("follow")
		if f == nil {
			t.Fatalf("%s has no --follow flag", c.CommandPath())
		}
		if f.DefValue != "false" {
			t.Errorf("%s --follow defaults to %s, want false", c.CommandPath(), f.DefValue)
		}
		if f.Shorthand != "f" {
			t.Errorf("%s --follow shorthand = %q, want f", c.CommandPath(), f.Shorthand)
		}
	}
}

func TestLogsCommandsKeepTheirTailDefaults(t *testing.T) {
	for c, want := range map[*cobra.Command]string{appLogsCmd: "500", functionLogsCmd: "100"} {
		f := c.Flags().Lookup("tail")
		if f == nil {
			t.Fatalf("%s has no --tail flag", c.CommandPath())
		}
		if f.DefValue != want {
			t.Errorf("%s --tail defaults to %s, want %s", c.CommandPath(), f.DefValue, want)
		}
	}
}

func TestLogFlagsReachThePodLogOptions(t *testing.T) {
	cases := []struct {
		args       []string
		wantFollow bool
		wantTail   int64
	}{
		{nil, false, 500},
		{[]string{"--follow"}, true, 500},
		{[]string{"-f", "--tail", "20"}, true, 20},
		{[]string{"--tail", "7"}, false, 7},
	}
	for _, tc := range cases {
		c := &cobra.Command{Use: "logs"}
		addLogFlags(c, 500)
		if err := c.ParseFlags(tc.args); err != nil {
			t.Fatalf("%v: %v", tc.args, err)
		}
		pod := logsTestPod("shop-1", "shop", kipperSidecarContainer)
		opts := podLogOptions(&pod, "shop", readLogFlags(c))
		if opts.Follow != tc.wantFollow {
			t.Errorf("%v: Follow = %v, want %v", tc.args, opts.Follow, tc.wantFollow)
		}
		if opts.TailLines == nil || *opts.TailLines != tc.wantTail {
			t.Errorf("%v: TailLines = %v, want %d", tc.args, opts.TailLines, tc.wantTail)
		}
		if opts.Container != "shop" {
			t.Errorf("%v: Container = %q, want the workload container shop", tc.args, opts.Container)
		}
	}
}

// fakeLogs supplies distinct streams and records options to verify per-pod requests.
type fakeLogs struct {
	mu      sync.Mutex
	bodies  map[string]io.ReadCloser
	fail    map[string]error
	options map[string]*corev1.PodLogOptions
}

func newFakeLogs(bodies map[string]string) *fakeLogs {
	f := &fakeLogs{bodies: map[string]io.ReadCloser{}, fail: map[string]error{}, options: map[string]*corev1.PodLogOptions{}}
	for pod, body := range bodies {
		f.bodies[pod] = io.NopCloser(strings.NewReader(body))
	}
	return f
}

func (f *fakeLogs) open(_ context.Context, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.options[pod] = opts
	if err := f.fail[pod]; err != nil {
		return nil, err
	}
	body, ok := f.bodies[pod]
	if !ok {
		return nil, fmt.Errorf("no such pod %s", pod)
	}
	return body, nil
}

// lockedBuffer lets the test read output while followers are still writing.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func agedPod(name string, age time.Duration) corev1.Pod {
	pod := logsTestPod(name, "shop", kipperSidecarContainer)
	pod.CreationTimestamp = metav1.NewTime(time.Now().Add(-age))
	return pod
}

func TestPodLogsReadsEveryPodOldestFirst(t *testing.T) {
	logs := newFakeLogs(map[string]string{
		"shop-new": "n1\nn2\n",
		"shop-old": "o1\no2\n",
	})
	pods := []corev1.Pod{agedPod("shop-new", time.Minute), agedPod("shop-old", time.Hour)}
	var out, errOut bytes.Buffer

	err := printPodLogs(context.Background(), logs.open, pods, "shop", logRequest{Tail: 10}, &out, &errOut)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "[pod/shop-old/shop] o1\n[pod/shop-old/shop] o2\n[pod/shop-new/shop] n1\n[pod/shop-new/shop] n2\n"
	if out.String() != want {
		t.Errorf("output =\n%s\nwant\n%s", out.String(), want)
	}
	for _, pod := range []string{"shop-old", "shop-new"} {
		opts := logs.options[pod]
		if opts == nil {
			t.Fatalf("pod %s was never read", pod)
		}
		if opts.Follow || opts.TailLines == nil || *opts.TailLines != 10 || opts.Container != "shop" {
			t.Errorf("pod %s options = follow %v tail %v container %q", pod, opts.Follow, opts.TailLines, opts.Container)
		}
	}
	if errOut.Len() != 0 {
		t.Errorf("unexpected stderr: %q", errOut.String())
	}
}

// Preserve single-pod output for scripts, including a final partial line.
func TestPodLogsLeavesASinglePodUnprefixed(t *testing.T) {
	logs := newFakeLogs(map[string]string{"shop-1": "line one\nline two"})
	var out, errOut bytes.Buffer

	err := printPodLogs(context.Background(), logs.open, []corev1.Pod{agedPod("shop-1", time.Hour)}, "shop", logRequest{Tail: 10}, &out, &errOut)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if out.String() != "line one\nline two" {
		t.Errorf("output = %q", out.String())
	}
}

func TestPodLogsEndsAPrefixedPartialLine(t *testing.T) {
	logs := newFakeLogs(map[string]string{"shop-a": "a1", "shop-b": "b1\n"})
	pods := []corev1.Pod{agedPod("shop-a", time.Hour), agedPod("shop-b", time.Minute)}
	var out, errOut bytes.Buffer

	if err := printPodLogs(context.Background(), logs.open, pods, "shop", logRequest{Tail: 10}, &out, &errOut); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "[pod/shop-a/shop] a1\n[pod/shop-b/shop] b1\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestPodLogsReportsAPodItCannotReadAndPrintsTheRest(t *testing.T) {
	logs := newFakeLogs(map[string]string{"shop-ok": "fine\n"})
	logs.fail["shop-crashing"] = errors.New(`container "shop" in pod "shop-crashing" is waiting to start: CrashLoopBackOff`)
	pods := []corev1.Pod{agedPod("shop-crashing", time.Hour), agedPod("shop-ok", time.Minute)}
	var out, errOut bytes.Buffer

	err := printPodLogs(context.Background(), logs.open, pods, "shop", logRequest{Tail: 10}, &out, &errOut)
	if err != nil {
		t.Fatalf("one readable pod should be enough, got error: %v", err)
	}
	if out.String() != "[pod/shop-ok/shop] fine\n" {
		t.Errorf("output = %q", out.String())
	}
	if !strings.Contains(errOut.String(), "shop-crashing") || !strings.Contains(errOut.String(), "CrashLoopBackOff") {
		t.Errorf("stderr does not name the failed pod and why: %q", errOut.String())
	}
}

func TestPodLogsFailsWhenNoPodCanBeRead(t *testing.T) {
	logs := newFakeLogs(nil)
	logs.fail["shop-a"] = errors.New("waiting to start")
	logs.fail["shop-b"] = errors.New("waiting to start")
	pods := []corev1.Pod{agedPod("shop-a", time.Hour), agedPod("shop-b", time.Minute)}
	var out, errOut bytes.Buffer

	err := printPodLogs(context.Background(), logs.open, pods, "shop", logRequest{Tail: 10}, &out, &errOut)
	if err == nil {
		t.Fatal("expected an error when no pod could be read")
	}
	for _, pod := range []string{"shop-a", "shop-b"} {
		if !strings.Contains(err.Error(), pod) {
			t.Errorf("error does not name %s: %v", pod, err)
		}
	}
}

// A quiet pod must allow other pods' logs through while keeping the command open.
func TestPodLogsFollowsEveryPodConcurrently(t *testing.T) {
	quietReader, quietWriter := io.Pipe()
	logs := newFakeLogs(map[string]string{"shop-busy": "b1\nb2\n"})
	logs.bodies["shop-quiet"] = quietReader
	pods := []corev1.Pod{agedPod("shop-quiet", time.Hour), agedPod("shop-busy", time.Minute)}
	var out lockedBuffer
	var errOut bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- printPodLogs(context.Background(), logs.open, pods, "shop", logRequest{Follow: true, Tail: 5}, &out, &errOut)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "[pod/shop-busy/shop] b2\n") {
		if time.Now().After(deadline) {
			t.Fatalf("the busy pod's lines never arrived while the quiet pod was open; output %q", out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		t.Fatalf("returned before the quiet pod's stream ended: %v", err)
	default:
	}

	_, _ = quietWriter.Write([]byte("q1\n"))
	_ = quietWriter.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not return after every stream ended")
	}

	if !strings.Contains(out.String(), "[pod/shop-quiet/shop] q1\n") {
		t.Errorf("quiet pod's line missing: %q", out.String())
	}
	if got := linesOf(out.String(), "[pod/shop-busy/shop] "); strings.Join(got, ",") != "b1,b2" {
		t.Errorf("busy pod lines out of order: %v", got)
	}
	for _, pod := range []string{"shop-quiet", "shop-busy"} {
		if !logs.options[pod].Follow {
			t.Errorf("pod %s was not followed", pod)
		}
	}
}

func linesOf(output, prefix string) []string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if rest, ok := strings.CutPrefix(line, prefix); ok {
			lines = append(lines, rest)
		}
	}
	return lines
}

func TestAppLogsReadsEveryPodOfTheApp(t *testing.T) {
	client := fake.NewSimpleClientset(
		runningPod("shop-1", "acme-prod", "shop"),
		runningPod("shop-2", "acme-prod", "shop"),
		runningPod("blog-1", "acme-prod", "blog"),
	)
	var out, errOut bytes.Buffer

	if err := appLogs(context.Background(), client, "acme-prod", "shop", logRequest{Tail: 10}, &out, &errOut); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"[pod/shop-1/shop] fake logs\n", "[pod/shop-2/shop] fake logs\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q: %q", want, out.String())
		}
	}
	if strings.Contains(out.String(), "blog") {
		t.Errorf("output includes another app's pod: %q", out.String())
	}
}

func TestAppLogsKeepsItsErrorForAnAppWithNoPods(t *testing.T) {
	var out, errOut bytes.Buffer
	err := appLogs(context.Background(), fake.NewSimpleClientset(), "acme-prod", "shop", logRequest{Tail: 10}, &out, &errOut)
	if err == nil || err.Error() != `no pods found for app "shop"` {
		t.Errorf("error = %v, want no pods found for app \"shop\"", err)
	}
}

func TestFunctionLogsKeepsItsErrorForAFunctionWithNoPods(t *testing.T) {
	var out, errOut bytes.Buffer
	err := functionLogs(context.Background(), fake.NewSimpleClientset(), "acme-prod", "resize", logRequest{Tail: 10}, &out, &errOut)
	if err == nil || err.Error() != `no running pods for function "resize": it may be scaled to zero` {
		t.Errorf("error = %v", err)
	}
}

func TestFunctionLogsReadsEveryPod(t *testing.T) {
	client := fake.NewSimpleClientset(
		runningPod("resize-1", "acme-prod", "resize"),
		runningPod("resize-2", "acme-prod", "resize"),
	)
	var out, errOut bytes.Buffer
	if err := functionLogs(context.Background(), client, "acme-prod", "resize", logRequest{Tail: 10}, &out, &errOut); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{"[pod/resize-1/resize] fake logs\n", "[pod/resize-2/resize] fake logs\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q: %q", want, out.String())
		}
	}
}

// An output failure must cancel even a silent stream and preserve the write error.
func TestPodLogsFollowStopsEveryStreamWhenOutputFails(t *testing.T) {
	quietReader, quietWriter := io.Pipe()
	logs := newFakeLogs(map[string]string{"shop-busy": "b1\n"})
	open := func(ctx context.Context, pod string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
		if pod == "shop-quiet" {
			go func() {
				<-ctx.Done()
				_ = quietWriter.CloseWithError(ctx.Err())
			}()
			return quietReader, nil
		}
		return logs.open(ctx, pod, opts)
	}
	pods := []corev1.Pod{agedPod("shop-quiet", time.Hour), agedPod("shop-busy", time.Minute)}
	var errOut lockedBuffer

	done := make(chan error, 1)
	go func() {
		done <- printPodLogs(context.Background(), open, pods, "shop", logRequest{Follow: true, Tail: 5}, failingWriter{}, &errOut)
	}()

	select {
	case err := <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Errorf("error = %v, want the write error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still running after stdout failed; the quiet pod's stream was never cancelled")
	}
	if errOut.String() != "" {
		t.Errorf("a cancelled stream was reported as a pod failure: %q", errOut.String())
	}
}
