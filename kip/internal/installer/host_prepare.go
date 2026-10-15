package installer

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
)

const k3sSELinuxConfigPath = "/etc/rancher/k3s/config.yaml.d/kipper-selinux.yaml"

// kernelReleasePattern limits uname -r output to characters that are safe in
// an unquoted package name.
var kernelReleasePattern = regexp.MustCompile(`^[0-9A-Za-z._+-]+$`)

// prepareHostForK3s prepares kernel modules and family-specific configuration
// before starting a k3s server or worker.
func prepareHostForK3s(client CommandRunner, wireGuardOptional bool) error {
	host, err := DetectHostOS(client)
	if err != nil {
		return fmt.Errorf("preparing the host for k3s: %w", err)
	}
	if err := ensureKernelModules(client, host, wireGuardOptional); err != nil {
		return err
	}
	return writeK3sHostConfig(client, host)
}

// ensureKernelModules installs missing modules for the running kernel where
// a package is configured, and reports any required modules still unavailable.
// wireGuardOptional lets existing VXLAN servers continue without WireGuard.
func ensureKernelModules(client CommandRunner, host HostOS, wireGuardOptional bool) error {
	modules := host.KernelModules()
	if len(modules) == 0 {
		return nil
	}
	missing, err := missingKernelModules(client, modules)
	if err != nil || len(missing) == 0 {
		return err
	}

	release, err := client.Run("uname -r")
	if err != nil {
		return fmt.Errorf("reading the running kernel: %w", err)
	}
	release = strings.TrimSpace(release)
	if !kernelReleasePattern.MatchString(release) {
		return fmt.Errorf("host reported an invalid kernel release: %q", release)
	}

	if pkgs := host.KernelModulePackages(); len(pkgs) > 0 {
		// Pin modules to the running kernel so they are usable before a reboot.
		var versioned []string
		for _, pkg := range pkgs {
			versioned = append(versioned, pkg+"-"+release)
		}
		fmt.Print(packageInstallNotice(versioned))
		if _, err := client.Run(host.InstallPackagesCommand(false, versioned...)); err != nil && len(withoutOptional(missing, wireGuardOptional)) > 0 {
			return fmt.Errorf("installing %s for missing kernel modules (%s): %w. If packages for the running kernel are unavailable, run 'dnf update', reboot, and retry",
				strings.Join(versioned, " "), strings.Join(missing, ", "), err)
		}
		if missing, err = missingKernelModules(client, modules); err != nil {
			return err
		}
	}
	if missing = withoutOptional(missing, wireGuardOptional); len(missing) > 0 {
		return fmt.Errorf("kernel %s could not load required modules (%s). %s, reboot, and retry",
			release, strings.Join(missing, ", "), kernelUpdateHint(host))
	}
	return nil
}

func withoutOptional(missing []string, wireGuardOptional bool) []string {
	if !wireGuardOptional {
		return missing
	}
	return slices.DeleteFunc(slices.Clone(missing), func(m string) bool { return m == "wireguard" })
}

func kernelUpdateHint(host HostOS) string {
	if host.Family == FamilyRHEL {
		return "Run 'dnf update'"
	}
	return "Run 'apt-get update && apt-get upgrade'"
}

// missingKernelModules attempts to load each module and reports failures.
// Loading verifies compatibility with the running kernel before installation.
func missingKernelModules(client CommandRunner, modules []string) ([]string, error) {
	out, err := client.Run(fmt.Sprintf(`for m in %s; do modprobe -q "$m" 2>/dev/null || echo "MISSING:$m"; done`,
		strings.Join(modules, " ")))
	if err != nil {
		return nil, fmt.Errorf("checking kernel modules: %w", err)
	}
	var missing []string
	for _, line := range strings.Split(out, "\n") {
		if m, ok := strings.CutPrefix(strings.TrimSpace(line), "MISSING:"); ok {
			missing = append(missing, m)
		}
	}
	return missing, nil
}

// writeK3sHostConfig enables SELinux in a k3s drop-in for RHEL-family hosts.
func writeK3sHostConfig(client CommandRunner, host HostOS) error {
	if !host.K3sSELinux() {
		return nil
	}
	if _, err := client.Run("mkdir -p /etc/rancher/k3s/config.yaml.d"); err != nil {
		return fmt.Errorf("creating k3s config.yaml.d directory: %w", err)
	}
	cmd := fmt.Sprintf("cat > %s << 'KIPEOF'\nselinux: true\nKIPEOF", k3sSELinuxConfigPath)
	if _, err := client.Run(cmd); err != nil {
		return fmt.Errorf("writing k3s selinux config: %w", err)
	}
	return nil
}
