package installer

import "fmt"

const nfsClientConfPath = "/etc/modprobe.d/kipper-nfs.conf"

const nfsDirectoryDelegationsParam = "/sys/module/nfsv4/parameters/directory_delegations"

// nfsClientCommand persists the setting before applying it to the running kernel
// if the parameter exists. Both paths must be safe to use unquoted in a shell.
func nfsClientCommand(confPath, paramPath string) string {
	return fmt.Sprintf("umask 022 && printf 'options nfsv4 directory_delegations=0\\n' > %[1]s.tmp && mv -f %[1]s.tmp %[1]s && "+
		"{ ! test -e %[2]s || echo N > %[2]s; }", confPath, paramPath)
}

// DisableNFSDirectoryDelegations stops the host's NFS client from requesting
// directory delegations. Linux 7.0 can include these requests with directory
// GETATTR operations. Longhorn 1.7.3's share manager rejects them as illegal,
// failing the whole operation and causing "Remote I/O error" when apps and
// backups access subdirectories of shared volumes.
//
// It replaces the modprobe configuration for future module loads, including
// after reboots, and updates the live parameter when present, affecting existing
// mounts immediately. Persistence requires NFS to be built as a module. Kernels
// without the parameter ignore the option. Repeated calls are safe.
// The runner needs permission to write both settings. A command failure returns
// an error, and the persistent setting may already have been written.
func DisableNFSDirectoryDelegations(client CommandRunner) error {
	if _, err := client.Run(nfsClientCommand(nfsClientConfPath, nfsDirectoryDelegationsParam)); err != nil {
		return fmt.Errorf("configuring the NFS client for shared volumes: %w", err)
	}
	return nil
}
