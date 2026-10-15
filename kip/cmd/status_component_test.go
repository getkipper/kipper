package cmd

import (
	"testing"

	"github.com/getkipper/kipper/kip/internal/k8s"
)

func TestComponentMarkTellsStartingFromFailed(t *testing.T) {
	cases := []struct {
		status k8s.ComponentStatus
		want   string
	}{
		{k8s.ComponentStatus{Healthy: true}, "✔"},
		{k8s.ComponentStatus{Starting: true}, "…"},
		{k8s.ComponentStatus{}, "✗"},
	}
	for _, c := range cases {
		if got := componentMark(c.status); got != c.want {
			t.Errorf("componentMark(%+v) = %q, want %q", c.status, got, c.want)
		}
	}
}
