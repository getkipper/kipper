package cmd

import (
	"testing"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
