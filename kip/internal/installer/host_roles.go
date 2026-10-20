package installer

import "fmt"

// HostRole identifies a server or worker for host preparation.
// The zero value uses server settings.
type HostRole string

const (
	RoleServer HostRole = NodeRoleServer
	RoleWorker HostRole = NodeRoleWorker
)

func (r HostRole) k3sUnit() string {
	if r == RoleWorker {
		return "k3s-agent"
	}
	return "k3s"
}

// requiredPorts returns the ports checked for conflicting listeners during preflight.
func (r HostRole) requiredPorts() []int {
	if r == RoleWorker {
		return []int{kubeletPortNumber}
	}
	return []int{80, 443, 6443}
}

const kubeletPortNumber = 10250

// raiseInotifyLimit sets the inotify instance limit for Promtail and adds a
// boot-time setting if the configuration has none.
func raiseInotifyLimit(client CommandRunner) error {
	if _, err := client.Run("sysctl -w fs.inotify.max_user_instances=512 >/dev/null && " +
		"{ grep -q max_user_instances /etc/sysctl.d/99-kipper.conf 2>/dev/null || echo 'fs.inotify.max_user_instances=512' >> /etc/sysctl.d/99-kipper.conf; }"); err != nil {
		return fmt.Errorf("raising the inotify limit: %w", err)
	}
	return nil
}

// prepareNodeOS configures the host prerequisites shared by servers and workers.
// Setting longhorn requires the iSCSI module on RHEL hosts.
func prepareNodeOS(client CommandRunner, wireGuardOptional, longhorn bool) error {
	if err := prepareHostForK3s(client, wireGuardOptional, longhorn); err != nil {
		return err
	}
	// protect-kernel-defaults makes the kubelet refuse to start unless these
	// sysctls are already set.
	if err := writeKubeletSysctls(client); err != nil {
		return err
	}
	if longhorn {
		if err := DisableNFSDirectoryDelegations(client); err != nil {
			return err
		}
	}
	return raiseInotifyLimit(client)
}

// prepareWorkerHost configures DNS, kubelet settings, and storage dependencies
// before the k3s agent starts.
func prepareWorkerHost(workerClient CommandRunner, resolvers []string) error {
	// Probe from the worker because its egress rules may differ from the server's.
	warnUnreachableResolvers(probeDNSResolvers(workerClient, resolvers), resolvers)
	if err := writeResolvConf(workerClient, renderResolvConf(resolvers)); err != nil {
		return err
	}
	if err := writeAgentDNSConfig(workerClient); err != nil {
		return err
	}
	// Workers need the storage modules to mount Longhorn volumes.
	if err := prepareNodeOS(workerClient, false, true); err != nil {
		return err
	}
	if err := writeAgentKubeletConfig(workerClient); err != nil {
		return err
	}
	host, err := DetectHostOS(workerClient)
	if err != nil {
		return fmt.Errorf("preparing the worker for storage: %w", err)
	}
	// nfs-common can bring rpcbind back, so hardening runs again after the packages.
	return installStorageDependencies(workerClient, host, func() error { return HardenHost(workerClient) })
}

// PreflightWorker checks OS support, memory, disk space, and kubelet port
// conflicts. Existing k3s listeners are allowed for retries.
func PreflightWorker(client CommandRunner) (SystemInfo, *PreflightResult, error) {
	sys, err := gatherSystemInfo(client, RoleWorker)
	if err != nil {
		return sys, nil, fmt.Errorf("gathering system info: %w", err)
	}
	result, err := runPreflightChecks(sys, PlatformState{}, false, false, RoleWorker)
	return sys, result, err
}
