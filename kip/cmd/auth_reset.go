package cmd

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/getkipper/kipper/kip/internal/auth"
	"github.com/getkipper/kipper/kip/internal/config"
	"github.com/getkipper/kipper/kip/internal/infra"
	"github.com/getkipper/kipper/kip/internal/installer"
)

func resetOverSSH(sshKeyGiven, needsSession, hasSession bool) bool {
	return sshKeyGiven || (needsSession && !hasSession)
}

// kubeconfigNeedsSession reports whether the current context uses kip's generated
// credential plugin without other credentials. Unreadable kubeconfigs and missing
// context or user entries also return true to allow SSH recovery.
func kubeconfigNeedsSession(path string) bool {
	parsed, err := clientcmd.LoadFromFile(path)
	if err != nil {
		return true
	}
	ctx := parsed.Contexts[parsed.CurrentContext]
	if ctx == nil {
		return true
	}
	user := parsed.AuthInfos[ctx.AuthInfo]
	if user == nil {
		return true
	}
	return installer.IsExactlyKipExec(user) && !carriesCredential(user)
}

// hasLocalSession checks for an unexpired session or a stored refresh token.
// Keep this check local so it works while Dex is unavailable. A stored
// refresh token may still be rejected when used.
func hasLocalSession(store *auth.Store, domain string) bool {
	if store == nil {
		return false
	}
	creds := store.Clusters[domain]
	return creds != nil && (time.Now().Before(creds.ExpiresAt) || creds.RefreshToken != "")
}

// adminClientsetOverSSH reads the server's k3s admin kubeconfig over SSH and
// returns a client built from it. The kubeconfig stays in memory.
func adminClientsetOverSSH(cluster *config.Cluster, sshKeyFlag string) (kubernetes.Interface, error) {
	explicit, fallback := resolveSSHKey(sshKeyFlag, cluster)
	provider := &infra.BareMetalProvider{Host: cluster.Host, User: sshUser(cluster), SSHKey: explicit, FallbackSSHKey: fallback}
	if err := provider.Connect(); err != nil {
		return nil, fmt.Errorf("connecting to %s: %w", cluster.Host, err)
	}
	defer func() { _ = provider.Close() }()
	kubeconfig, err := installer.FetchKubeconfig(provider.Client(), cluster.Host)
	if err != nil {
		return nil, err
	}
	return clientsetFromKubeconfig(kubeconfig, cluster.Host)
}

// clientsetFromKubeconfig builds a client in memory for the cluster at host.
// Generic errors keep the server's admin credentials out of error messages.
func clientsetFromKubeconfig(kubeconfig, host string) (kubernetes.Interface, error) {
	restConfig, err := adminRESTConfig(kubeconfig, host)
	if err != nil {
		return nil, err
	}
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, errors.New("the server's admin kubeconfig could not be used")
	}
	return clientset, nil
}

// adminRESTConfig uses the admin kubeconfig's embedded CA, client certificate
// and key to connect directly to host with TLS verification. Routing and TLS
// overrides in the file are ignored to protect the admin credentials.
func adminRESTConfig(kubeconfig, host string) (*rest.Config, error) {
	parsed, err := clientcmd.Load([]byte(kubeconfig))
	if err != nil {
		return nil, errors.New("the server's admin kubeconfig could not be parsed")
	}
	ctx := parsed.Contexts[parsed.CurrentContext]
	if ctx == nil || parsed.Clusters[ctx.Cluster] == nil || parsed.AuthInfos[ctx.AuthInfo] == nil {
		return nil, errors.New("the server's admin kubeconfig must have a current context with a valid cluster and user")
	}
	cluster, user := parsed.Clusters[ctx.Cluster], parsed.AuthInfos[ctx.AuthInfo]
	if len(cluster.CertificateAuthorityData) == 0 || len(user.ClientCertificateData) == 0 || len(user.ClientKeyData) == 0 {
		return nil, errors.New("the server's admin kubeconfig must include a CA certificate, client certificate and private key")
	}
	return &rest.Config{
		Host: "https://" + net.JoinHostPort(host, "6443"),
		TLSClientConfig: rest.TLSClientConfig{
			CAData:   cluster.CertificateAuthorityData,
			CertData: user.ClientCertificateData,
			KeyData:  user.ClientKeyData,
		},
		// A nil Proxy falls back to the environment's proxy settings.
		Proxy: func(*http.Request) (*url.URL, error) { return nil, nil },
	}, nil
}
