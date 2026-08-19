package installer

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"k8s.io/client-go/tools/clientcmd"
)

// renderExecKubeconfig builds a kubeconfig that carries no credential at
// all: the user entry is an exec plugin invoking kip, which serves the
// operator's own short-lived OIDC token. The cluster CA, server address and
// the transport settings needed to reach it are the only material in the file,
// so losing the file loses nothing and revoking a person means their login,
// never a certificate rotation.
//
// It marshals a config object rather than filling in a text template. The
// values can come from a kubeconfig somebody else wrote, and a server address
// carrying newlines used to be written verbatim into the document: a crafted
// export could add an exec entry of its own, which client-go then ran. Nothing
// is escaped here because nothing is interpolated.
//
// The exec args pin the domain because kubectl tells a credential plugin
// nothing about which kubeconfig invoked it. Without the pin the plugin has
// only kip's current_cluster to go on, which is a global setting bearing no
// relation to the file kubectl is holding, so an operator working across two
// clusters gets one cluster's token sent to the other's API server.
func renderExecKubeconfig(domain string, source *clientcmdapi.Cluster, namespace, execCommand string) (string, error) {
	user := "oidc@" + domain
	cfg := clientcmdapi.NewConfig()
	cfg.Clusters[domain] = &clientcmdapi.Cluster{
		Server:                   source.Server,
		CertificateAuthorityData: source.CertificateAuthorityData,
		// Carried across because they decide whether the address is reachable
		// and verifiable at all, and neither runs anything: a cluster behind a
		// proxy, or served on an IP whose certificate names something else,
		// is unreachable without them.
		ProxyURL:           source.ProxyURL,
		TLSServerName:      source.TLSServerName,
		DisableCompression: source.DisableCompression,
	}
	cfg.AuthInfos[user] = &clientcmdapi.AuthInfo{
		Exec: &clientcmdapi.ExecConfig{
			APIVersion:      "client.authentication.k8s.io/v1",
			Command:         execCommand,
			Args:            []string{"auth", "kubectl-token", "--cluster-domain", domain},
			InteractiveMode: clientcmdapi.NeverExecInteractiveMode,
			InstallHint: `This kubeconfig authenticates through the kip CLI.
Install kip and run: kip auth login
`,
		},
	}
	cfg.Contexts[domain] = &clientcmdapi.Context{
		Cluster:  domain,
		AuthInfo: user,
		// Carried across: a default namespace is the operator's own setting,
		// and silently dropping it sends every later kubectl at the wrong
		// namespace without saying so.
		Namespace: namespace,
	}
	cfg.CurrentContext = domain

	out, err := clientcmd.Write(*cfg)
	if err != nil {
		return "", fmt.Errorf("rendering kubeconfig: %w", err)
	}
	return string(out), nil
}

// ActiveContext resolves which entry in a kubeconfig is live: the current
// context, or the only one when the file names no current context. Anything else yields a zero
// context, so a caller sees no cluster rather than a guess among entries. It is
// the single answer to "which entry is live", so the decision to rewrite and
// the rewrite itself cannot disagree about which one that is. The name comes
// back with it because an inferred context has one worth naming in an error.
func ActiveContext(cfg *clientcmdapi.Config) (clientcmdapi.Context, string) {
	name := cfg.CurrentContext
	if name == "" && len(cfg.Contexts) == 1 {
		for only := range cfg.Contexts {
			name = only
		}
	}
	if kubeContext, ok := cfg.Contexts[name]; ok && kubeContext != nil {
		return *kubeContext, name
	}
	return clientcmdapi.Context{}, name
}

// execFromAPIConfig extracts the active cluster's server and CA from a
// loaded kubeconfig and renders the credential-free exec kubeconfig for it.
// The active context decides which cluster survives — this output replaces a
// credential-bearing file, so guessing among entries could keep the wrong
// server and CA. Map iteration order must never choose.
func execFromAPIConfig(domain string, cfg *clientcmdapi.Config, execCommand string) (content, server string, caData []byte, err error) {
	kubeContext, contextName := ActiveContext(cfg)
	if kubeContext.Cluster == "" {
		return "", "", nil, fmt.Errorf("kubeconfig has no usable current context; set one before rewriting")
	}
	cluster, ok := cfg.Clusters[kubeContext.Cluster]
	if !ok {
		return "", "", nil, fmt.Errorf("kubeconfig context %q references missing cluster %q", contextName, kubeContext.Cluster)
	}
	if cluster.Server == "" || len(cluster.CertificateAuthorityData) == 0 {
		return "", "", nil, fmt.Errorf("kubeconfig carries no cluster server and CA to keep")
	}
	// A kubeconfig that skips verification is not re-rendered into one kip
	// wrote: the result would carry kip's name and a weaker guarantee than
	// every other file it writes.
	if cluster.InsecureSkipTLSVerify {
		return "", "", nil, fmt.Errorf("kubeconfig disables TLS verification for %q, which kip will not carry over", kubeContext.Cluster)
	}
	content, rerr := renderExecKubeconfig(domain, cluster, kubeContext.Namespace, execCommand)
	if rerr != nil {
		return "", "", nil, rerr
	}
	return content, cluster.Server, cluster.CertificateAuthorityData, nil
}

// RenderExecFromAdmin renders the credential-free exec kubeconfig from an
// in-memory admin kubeconfig string, touching no disk. Install uses it to
// convert the fetched k3s admin config before writing anything, so the admin
// certificate is held only in memory and never lands on the operator machine
// in the default path. Returns the rendered content plus the server and CA
// the login gate needs to build a bearer client.
func RenderExecFromAdmin(domain, adminKubeconfig, execCommand string) (content, server string, caData []byte, err error) {
	cfg, err := clientcmd.Load([]byte(adminKubeconfig))
	if err != nil {
		return "", "", nil, fmt.Errorf("parsing admin kubeconfig: %w", err)
	}
	return execFromAPIConfig(domain, cfg, execCommand)
}

// RewriteKubeconfigToExec replaces the kubeconfig at path with the exec-based
// one, keeping its server address and cluster CA. domain names the rendered
// objects and the auth-store identity; path is the cluster's actual
// kubeconfig (config.Cluster.Kubeconfig), which after a rename or import is
// not the domain-derived default. The file it replaces typically carries the
// shared k3s admin certificate; after this, that certificate exists only on
// the server, as break-glass.
//
// snapshot is the content the caller read and acted on, and the rewrite is
// rendered from it rather than from a second read. The caller spends up to a
// minute proving the operator's login against the server that content names,
// so re-reading here would let a file replaced during that minute be converted
// on the strength of a proof made against the cluster it used to name.
//
// The file is re-read to refuse when it no longer holds those bytes, which
// covers a replacement made while the proof was in flight. A replacement
// landing between that read and the rename is not covered: POSIX has no
// compare-and-swap on file content, so closing it needs a lock every writer of
// a cluster kubeconfig takes, which is a wider change than this one.
func RewriteKubeconfigToExec(domain, path string, snapshot []byte) (string, error) {
	existing, err := clientcmd.Load(snapshot)
	if err != nil {
		return "", fmt.Errorf("reading kubeconfig %s: %w", path, err)
	}

	content, _, _, err := execFromAPIConfig(domain, existing, execCommandForHost())
	if err != nil {
		return "", fmt.Errorf("kubeconfig %s: %w", path, err)
	}

	current, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("re-reading kubeconfig %s: %w", path, err)
	}
	if !bytes.Equal(current, snapshot) {
		return "", fmt.Errorf("kubeconfig %s changed while your login was being checked, so it was left as it is", path)
	}

	if err := WriteFileAtomic(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// RepinExecKubeconfig re-renders an exec kubeconfig for a new domain, so the
// cluster its credential plugin asks for stays the cluster it points at. A
// domain change is the only thing that moves that identity, and the pin is
// stale the moment it does: the plugin would ask for a session keyed on the
// old domain, which no longer exists.
//
// Any other kubeconfig is left untouched, and reported as such. Those files
// carry a credential this cannot reissue: the k3s admin certificate, when the
// operator asked for it with --admin-kubeconfig or an install fell back to it,
// or another tool's exec plugin. Converting one here would revoke a working
// credential as a side effect of renaming a domain. Whether to give up the
// admin certificate is 'kip auth kubeconfig' to ask, not this.
func RepinExecKubeconfig(domain, path string) (repinned bool, err error) {
	existing, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return false, fmt.Errorf("reading kubeconfig %s: %w", path, err)
	}
	if !ActiveCredentialIsKipExec(existing) {
		return false, nil
	}

	content, _, _, err := execFromAPIConfig(domain, existing, execCommandForHost())
	if err != nil {
		return false, fmt.Errorf("kubeconfig %s: %w", path, err)
	}
	if err := WriteFileAtomic(path, []byte(content), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

// ActiveCredentialIsKipExec reports whether the credential the current context
// uses is kip's own exec plugin, which is the only credential a re-pin may
// replace, and the only one an import may write over.
//
// Two narrowings, each of which is a credential someone loses otherwise. Only
// the active entry counts, because re-rendering keeps the active context and
// discards the rest: a file pairing an exec user with a certificate user, the
// certificate one live, would lose that certificate. And only kip's own plugin
// counts, because a cluster entry may point at a kubeconfig authenticating
// through somebody else's — aws eks get-token, gke-gcloud-auth-plugin,
// kubelogin — and rewriting one of those replaces a credential kip never
// issued and cannot reissue.
// The test is the exact shape renderExecKubeconfig writes, rather than the
// presence of a telling argument anywhere: another tool's plugin taking
// "kubectl-token" as a value would otherwise read as kip's own. Recognising
// too little only leaves a stale pin, which refuses and names the one command
// that fixes it; recognising too much destroys a credential.
func ActiveCredentialIsKipExec(cfg *clientcmdapi.Config) bool {
	kubeContext, _ := ActiveContext(cfg)
	return IsKipExecAuthInfo(cfg.AuthInfos[kubeContext.AuthInfo])
}

// isThisKipsCommand reports whether a kubeconfig's exec command is this kip.
//
// Three spellings are ours: the bare name, whatever this build would render
// today, and this binary's own path. The third matters because the second
// changes with the environment: a file written while kip was off PATH carries
// an absolute path, and once kip is on PATH the rendered command becomes "kip",
// which would leave kip refusing to recognise a file it wrote itself.
//
// Comparison folds case off Linux, where the filesystems are case-insensitive
// and the same binary is reachable by paths that differ only in spelling.
func isThisKipsCommand(command string) bool {
	// The bare name only, with no directory in it. A path whose last element is
	// "kip" is somebody's program called kip until it is shown to be this one,
	// which is what samePath below decides.
	if command == "kip" || command == "kip.exe" {
		return true
	}
	for _, ours := range []string{execCommandForHost(), currentExecutable()} {
		if ours == "" {
			continue
		}
		if samePath(command, ours) {
			return true
		}
	}
	return false
}

func currentExecutable() string {
	self, err := osExecutable()
	if err != nil {
		return ""
	}
	return self
}

// isKipBinaryName reports whether a command's last element names this program.
// Windows renders it with the extension, so a kubeconfig written there carries
// "kip.exe" wherever this one would carry "kip".
//
// The basename is enough for the loose predicate, whose worst outcome is a
// stale pin. Anything deciding whether a credential may be destroyed asks
// isThisKipsCommand instead, which does not accept a stranger's binary because
// of what it is called.
func isKipBinaryName(command string) bool {
	base := filepath.Base(command)
	return base == "kip" || base == "kip.exe"
}

// samePath reports whether two paths name one file.
//
// It asks the filesystem when both exist, because case sensitivity belongs to
// the filesystem rather than to the operating system: macOS mounts
// case-sensitive volumes, and treating two distinct executables there as one
// would let a stranger's binary pass for kip. When a path does not exist there
// is nothing to compare but the spelling, and the exact spelling is the only
// safe answer.
func samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	infoA, errA := os.Stat(a)
	infoB, errB := os.Stat(b)
	if errA != nil || errB != nil {
		return false
	}
	return os.SameFile(infoA, infoB)
}

// IsKipExecAuthInfo reports whether one entry is kip's own credential plugin.
// Callers that replace a whole file rather than an active context ask this of
// every entry: an inactive one is still somebody's way into a cluster.
//
// This is the loose test, and it answers "does this look like our plugin",
// which is the right question when the consequence is a stale pin. A caller
// about to destroy the entry asks IsExactlyKipExec instead.
func IsKipExecAuthInfo(authInfo *clientcmdapi.AuthInfo) bool {
	if authInfo == nil || authInfo.Exec == nil {
		return false
	}
	if !isKipBinaryName(authInfo.Exec.Command) {
		return false
	}
	args := authInfo.Exec.Args
	return len(args) >= 2 && args[0] == "auth" && args[1] == "kubectl-token"
}

// IsExactlyKipExec reports whether an entry is the plugin kip renders, in full,
// and nothing else. It is the question to ask before overwriting a file: kip
// can reissue exactly what it wrote, and nothing more.
//
// A program named kip somewhere else, a wrapper taking extra arguments, or the
// same plugin carrying environment the operator added, all authenticate someone
// and none of them can be reproduced from an export. The loose test above reads
// all three as ours.
func IsExactlyKipExec(authInfo *clientcmdapi.AuthInfo) bool {
	if authInfo == nil || authInfo.Exec == nil {
		return false
	}
	exec := authInfo.Exec
	// kip writes "kip" when it is on PATH and its own absolute path when it is
	// not, so both are ours. Any other command is somebody else's, whatever it
	// is called: destroying what it authenticates cannot be undone by
	// re-rendering ours. The basename is not consulted, because a binary named
	// something else is still this one when the path matches, and a stranger's
	// binary named kip is still a stranger's.
	if !isThisKipsCommand(exec.Command) {
		return false
	}
	if len(exec.Args) < 2 || exec.Args[0] != "auth" || exec.Args[1] != "kubectl-token" {
		return false
	}
	if len(exec.Args) != 4 || exec.Args[2] != "--cluster-domain" || exec.Args[3] == "" {
		return false
	}
	if len(exec.Env) != 0 || exec.ProvideClusterInfo {
		return false
	}
	return exec.APIVersion == "client.authentication.k8s.io/v1" &&
		exec.InteractiveMode == clientcmdapi.NeverExecInteractiveMode
}

// RenderImportedKubeconfig re-renders a kubeconfig that arrived from someone
// else, keeping the server address, the cluster certificate authority, and the
// transport settings needed to reach that address. Nothing else survives.
//
// An import is a file a colleague sent, and a kubeconfig is not only data: an
// exec stanza names a command that client-go runs the moment anything asks for
// credentials. Checking the incoming file for known-bad shapes leaves whatever
// nobody thought of, so nothing executable survives the crossing at all. What
// lands is a file kip wrote, pinned to the domain the bundle declares.
func RenderImportedKubeconfig(domain, kubeconfig string) (string, error) {
	content, _, _, err := RenderExecFromAdmin(domain, kubeconfig, execCommandForHost())
	if err != nil {
		return "", fmt.Errorf("reading the imported kubeconfig: %w", err)
	}
	return content, nil
}

// WriteFileAtomic writes content to path via a temp file and rename, so a
// concurrent reader never sees a partial write and a crash never truncates
// the target.
func WriteFileAtomic(path string, content []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// WriteExecKubeconfigTo renders the exec kubeconfig a cluster's snapshot
// describes and writes it somewhere new, leaving the original alone.
//
// Separate from RewriteKubeconfigToExec because that one re-reads its target
// and refuses when the bytes have moved, which is exactly right when replacing
// a file in place and meaningless for a path the snapshot never came from.
//
// An existing file is never overwritten. The whole point of writing elsewhere
// is that nothing is given up, and silently replacing whatever was already at
// the path would give up something the operator did not name.
func WriteExecKubeconfigTo(domain, path string, snapshot []byte) (string, error) {
	existing, err := clientcmd.Load(snapshot)
	if err != nil {
		return "", fmt.Errorf("reading the cluster's kubeconfig: %w", err)
	}
	content, _, _, err := execFromAPIConfig(domain, existing, execCommandForHost())
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(path); err == nil {
		return "", fmt.Errorf("%s already exists, and writing over it would give up whatever it holds. Choose a path that does not exist", path)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("checking %s: %w", path, err)
	}
	if err := WriteFileAtomic(path, []byte(content), 0o600); err != nil {
		return "", err
	}
	return path, nil
}
