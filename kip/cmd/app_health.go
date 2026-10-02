package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/getkipper/kipper/controller/pkg/healthcheck"
	"github.com/getkipper/kipper/kip/internal/deployer"
)

func addHealthFlags(cmd *cobra.Command) {
	cmd.Flags().String("health", "", `readiness check: http, tcp, none (disable the app check), or auto to let Kipper decide`)
	cmd.Flags().String("health-path", "", "path for an http check, e.g. /actuator/health/readiness; implies --health http")
	cmd.Flags().Int32("health-port", 0, "port to check, when it is not the app port (e.g. a management port)")
	cmd.Flags().Int32("health-startup-timeout", 0, "seconds a new pod may take to pass its check before the rollout is reported as stuck (default 300)")
	cmd.Flags().Int32("health-timeout", 0, "timeout for each check in seconds (default 2)")
}

// readHealthFlags preserves which settings were supplied so partial edits
// can be merged into the existing check.
func readHealthFlags(cmd *cobra.Command) (deployer.HealthEdit, error) {
	var edit deployer.HealthEdit
	edit.Type, _ = cmd.Flags().GetString("health")
	switch edit.Type {
	case "", "http", "tcp", "none", healthcheck.Auto:
	default:
		return edit, fmt.Errorf("--health takes http, tcp, none or auto, not %q", edit.Type)
	}
	if cmd.Flags().Changed("health-path") {
		path, _ := cmd.Flags().GetString("health-path")
		edit.Path = &path
	}
	for flag, to := range map[string]**int32{
		"health-port": &edit.Port, "health-startup-timeout": &edit.StartupTimeoutSeconds, "health-timeout": &edit.TimeoutSeconds,
	} {
		if cmd.Flags().Changed(flag) {
			v, _ := cmd.Flags().GetInt32(flag)
			*to = &v
		}
	}
	if edit.Type == healthcheck.Auto && (edit.Path != nil || edit.Port != nil || edit.StartupTimeoutSeconds != nil || edit.TimeoutSeconds != nil) {
		return edit, fmt.Errorf("--health auto removes the declared check, so it takes no other health settings")
	}
	return edit, nil
}

func describeHealthEdit(edit deployer.HealthEdit) string {
	if edit.Type == healthcheck.Auto {
		return "Health check set to automatic: Kipper uses connection checks on running pods"
	}
	return "Health check settings updated"
}
