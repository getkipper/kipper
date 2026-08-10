package installer

// KubeconfigMode decides what credential kip install writes to the operator
// machine.
type KubeconfigMode int

const (
	// KubeconfigExecInteractive: write the credential-free exec kubeconfig
	// and run the inline OIDC login + proof gate. The default at a terminal.
	KubeconfigExecInteractive KubeconfigMode = iota
	// KubeconfigExecDeferred: write the credential-free exec kubeconfig and
	// skip the inline login (headless/CI or --no-login). The first operator
	// runs `kip auth login && kip auth verify` later.
	KubeconfigExecDeferred
	// KubeconfigAdminCert: write the shared k3s admin certificate — the
	// explicit --admin-kubeconfig escape hatch for automation that must run
	// kubectl immediately with no login.
	KubeconfigAdminCert
)

// ResolveKubeconfigMode is a pure decision over the two flags and whether a
// terminal is attached. Headless (no TTY) or --no-login defers; the admin
// escape hatch wins over both.
func ResolveKubeconfigMode(adminKubeconfig, noLogin, isTTY bool) KubeconfigMode {
	switch {
	case adminKubeconfig:
		return KubeconfigAdminCert
	case noLogin || !isTTY:
		return KubeconfigExecDeferred
	default:
		return KubeconfigExecInteractive
	}
}
