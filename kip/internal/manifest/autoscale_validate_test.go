package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func i32(v int32) *int32 { return &v }

func parseApp(t *testing.T, app string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kipper.yaml")
	body := "project: shop\nenvironment: test\napps:\n  web:\n    image: nginx:1.27\n    port: 80\n" + app
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ParseFile(path)
	return err
}

func TestValidate_AutoscaleBlock(t *testing.T) {
	tests := []struct {
		name, app, want string
	}{
		{name: "cpu only, min omitted", app: "    autoscale:\n      enabled: true\n      maxReplicas: 5\n      cpuTarget: 70\n"},
		{name: "opt-out without max", app: "    replicas: 4\n    autoscale:\n      enabled: false\n"},
		{name: "disabled with an unused negative target", app: "    replicas: 3\n    autoscale:\n      enabled: false\n      maxReplicas: 5\n      cpuTarget: -1\n"},
		{name: "replicas omitted with bounds", app: "    autoscale:\n      enabled: true\n      minReplicas: 2\n      maxReplicas: 5\n      cpuTarget: 70\n"},
		{name: "replicas within bounds", app: "    replicas: 3\n    autoscale:\n      enabled: true\n      minReplicas: 2\n      maxReplicas: 5\n      cpuTarget: 70\n"},
		{name: "memory target 0 next to a cpu target", app: "    autoscale:\n      enabled: true\n      maxReplicas: 5\n      cpuTarget: 70\n      memoryTarget: 0\n"},
		{name: "explicit min 0", app: "    autoscale:\n      enabled: true\n      minReplicas: 0\n      maxReplicas: 5\n      cpuTarget: 70\n", want: "minReplicas must be at least 1"},
		{name: "enabled without max", app: "    autoscale:\n      enabled: true\n      cpuTarget: 70\n", want: "maxReplicas is required"},
		{name: "disabled with min and no max", app: "    autoscale:\n      enabled: false\n      minReplicas: 3\n", want: "set maxReplicas with minReplicas, or leave both out to remove the bounds"},
		{name: "min above max", app: "    autoscale:\n      enabled: false\n      minReplicas: 6\n      maxReplicas: 5\n", want: "minReplicas (6) must not exceed maxReplicas (5)"},
		{name: "enabled without a target", app: "    autoscale:\n      enabled: true\n      maxReplicas: 5\n", want: "set cpuTarget or memoryTarget"},
		{name: "negative target", app: "    autoscale:\n      enabled: true\n      maxReplicas: 5\n      cpuTarget: -1\n", want: "cpuTarget must not be negative"},
		{name: "replicas above max", app: "    replicas: 8\n    autoscale:\n      enabled: false\n      maxReplicas: 5\n", want: "replicas (8) must be between 1 and 5"},
		{name: "explicit replicas 0 with bounds", app: "    replicas: 0\n    autoscale:\n      enabled: true\n      maxReplicas: 5\n      cpuTarget: 70\n", want: "replicas (0) must be between 1 and 5"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := parseApp(t, tt.app)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("ParseFile() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("ParseFile() = %v, want an error containing %q", err, tt.want)
			}
			if !strings.Contains(err.Error(), `app "web": autoscale:`) {
				t.Errorf("the error should name the app and the block, got %v", err)
			}
		})
	}
}
