package installer

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"k8s.io/client-go/tools/clientcmd"
)

// renderExecKubeconfig builds a kubeconfig that carries no credential at
// all: the user entry is an exec plugin invoking kip, which serves the
// operator's own short-lived OIDC token. The cluster CA and server address
// are the only material in the file, so losing the file loses nothing and
// revoking a person means their login, never a certificate rotation.
//
// The exec args pin the domain because kubectl tells a credential plugin
// nothing about which kubeconfig invoked it. Without the pin the plugin has
// only kip's current_cluster to go on, which is a global setting bearing no
// relation to the file kubectl is holding, so an operator working across two
// clusters gets one cluster's token sent to the other's API server.
func renderExecKubeconfig(domain, server string, caData []byte, execCommand string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
  - name: %[1]s
    cluster:
      server: %[2]s
      certificate-authority-data: %[3]s
contexts:
  - name: %[1]s
    context:
      cluster: %[1]s
      user: oidc@%[1]s
current-context: %[1]s
users:
  - name: oidc@%[1]s
    user:
      exec:
        apiVersion: client.authentication.k8s.io/v1
        command: %[4]s
        args:
          - auth
          - kubectl-token
          - --cluster-domain
          - %[1]s
        interactiveMode: Never
        installHint: |
          This kubeconfig authenticates through the kip CLI.
          Install kip and run: kip auth login
`, domain, server, base64.StdEncoding.EncodeToString(caData), execCommand)
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
	return renderExecKubeconfig(domain, cluster.Server, cluster.CertificateAuthorityData, execCommand), cluster.Server, cluster.CertificateAuthorityData, nil
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
	if !activeCredentialIsKipExec(existing) {
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

// activeCredentialIsKipExec reports whether the credential the current context
// uses is kip's own exec plugin, which is the only credential a re-pin may
// replace.
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
func activeCredentialIsKipExec(cfg *clientcmdapi.Config) bool {
	kubeContext, _ := ActiveContext(cfg)
	authInfo := cfg.AuthInfos[kubeContext.AuthInfo]
	if authInfo == nil || authInfo.Exec == nil {
		return false
	}
	if filepath.Base(authInfo.Exec.Command) != "kip" {
		return false
	}
	args := authInfo.Exec.Args
	return len(args) >= 2 && args[0] == "auth" && args[1] == "kubectl-token"
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
