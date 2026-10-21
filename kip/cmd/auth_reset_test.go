package cmd

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/getkipper/kipper/kip/internal/auth"
	"github.com/getkipper/kipper/kip/internal/config"
)

func TestResetOverSSH(t *testing.T) {
	assert.True(t, resetOverSSH(true, true, true), "--ssh-key selects SSH recovery")
	assert.True(t, resetOverSSH(false, true, false), "kip's credential plugin requires SSH recovery when no session is stored")
	assert.False(t, resetOverSSH(false, true, true), "a stored session selects local authentication")
	assert.False(t, resetOverSSH(false, false, false), "other kubeconfig authentication uses the local client")
}

func TestKubeconfigNeedsSession(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
		return p
	}
	kipExec := kipRenderedKubeconfig(t, "https://203.0.113.10:6443")

	assert.True(t, kubeconfigNeedsSession(write("exec.yaml", kipExec)))
	assert.False(t, kubeconfigNeedsSession(write("admin.yaml", adminCertKubeconfig)))
	assert.False(t, kubeconfigNeedsSession(write("foreign.yaml", foreignExecKubeconfig)), "another tool's plugin handles its own authentication")
	assert.True(t, kubeconfigNeedsSession(filepath.Join(dir, "missing.yaml")), "a missing kubeconfig allows SSH recovery")

	parsed, err := clientcmd.Load([]byte(kipExec))
	require.NoError(t, err)
	parsed.AuthInfos["old"] = &clientcmdapi.AuthInfo{ClientCertificateData: []byte("cert"), ClientKeyData: []byte("key")}
	inactiveCert, err := clientcmd.Write(*parsed)
	require.NoError(t, err)
	assert.True(t, kubeconfigNeedsSession(write("inactive.yaml", string(inactiveCert))), "only credentials in the current context affect session requirements")
}

func TestHasLocalSession(t *testing.T) {
	store := &auth.Store{Clusters: map[string]*auth.Credentials{
		"live.example":    {IDToken: "t", ExpiresAt: time.Now().Add(time.Hour)},
		"refresh.example": {IDToken: "t", ExpiresAt: time.Now().Add(-time.Hour), RefreshToken: "r"},
		"dead.example":    {IDToken: "t", ExpiresAt: time.Now().Add(-time.Hour)},
	}}

	assert.True(t, hasLocalSession(store, "live.example"))
	assert.True(t, hasLocalSession(store, "refresh.example"))
	assert.False(t, hasLocalSession(store, "dead.example"))
	assert.False(t, hasLocalSession(store, "absent.example"))
	assert.False(t, hasLocalSession(nil, "live.example"))
}

func TestClientsetFromKubeconfig(t *testing.T) {
	_, err := clientsetFromKubeconfig(realAdminKubeconfig(t), &config.Cluster{Host: "203.0.113.10"})
	assert.NoError(t, err)

	// Use a sensitive-looking kind to check that parse errors conceal input.
	_, err = clientsetFromKubeconfig("apiVersion: v1\nkind: c2VjcmV0LWtleQ==\n", &config.Cluster{Host: "203.0.113.10"})
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "c2VjcmV0LWtleQ==", "parse errors must omit kubeconfig contents")
}

// tamperedAdminKubeconfig includes routing and TLS overrides that recovery must ignore.
const tamperedAdminKubeconfig = `apiVersion: v1
kind: Config
clusters:
- name: default
  cluster:
    server: http://198.51.100.9:8080
    certificate-authority-data: Y2EtcGVt
    proxy-url: http://198.51.100.66:3128
    tls-server-name: other.example
    insecure-skip-tls-verify: true
users:
- name: default
  user: {client-certificate-data: Y2VydA==, client-key-data: a2V5}
contexts:
- name: default
  context: {cluster: default, user: default}
current-context: default
`

func TestAdminRESTConfigTakesOnlyTheCredentialFromTheFile(t *testing.T) {
	cfg, err := adminRESTConfig(tamperedAdminKubeconfig, "203.0.113.10")
	require.NoError(t, err)

	assert.Equal(t, "https://203.0.113.10:6443", cfg.Host)
	assert.Equal(t, []byte("ca-pem"), cfg.CAData)
	assert.Equal(t, []byte("cert"), cfg.CertData)
	assert.Equal(t, []byte("key"), cfg.KeyData)
	assert.False(t, cfg.Insecure)
	assert.Empty(t, cfg.ServerName)
	require.NotNil(t, cfg.Proxy, "a nil Proxy would use environment proxy settings")
	t.Setenv("HTTPS_PROXY", "http://198.51.100.66:3128")
	proxy, err := cfg.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "203.0.113.10:6443"}})
	require.NoError(t, err)
	assert.Nil(t, proxy, "connect directly to the configured host")

	cfg, err = adminRESTConfig(tamperedAdminKubeconfig, "2001:db8::10")
	require.NoError(t, err)
	assert.Equal(t, "https://[2001:db8::10]:6443", cfg.Host)
}

func TestAdminRESTConfigNeedsTheWholeCredential(t *testing.T) {
	noCA := strings.Replace(tamperedAdminKubeconfig, "    certificate-authority-data: Y2EtcGVt\n", "", 1)
	_, err := adminRESTConfig(noCA, "203.0.113.10")
	assert.Error(t, err, "the embedded CA is required for server verification")

	noKey := strings.Replace(tamperedAdminKubeconfig, ", client-key-data: a2V5", "", 1)
	_, err = adminRESTConfig(noKey, "203.0.113.10")
	assert.Error(t, err)
}

func TestResetPasswordTakesAnSSHKey(t *testing.T) {
	assert.NotNil(t, authResetPasswordCmd.Flags().Lookup("ssh-key"))
}

// realAdminKubeconfig uses a self-signed certificate as both CA and client
// certificate so client construction can validate the PEM data.
func realAdminKubeconfig(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "system:admin"},
		NotBefore: time.Now(), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	cert := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	keyPEM := base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	return "apiVersion: v1\nkind: Config\nclusters:\n- name: default\n  cluster: {server: \"https://127.0.0.1:6443\", certificate-authority-data: " + cert +
		"}\nusers:\n- name: default\n  user: {client-certificate-data: " + cert + ", client-key-data: " + keyPEM +
		"}\ncontexts:\n- name: default\n  context: {cluster: default, user: default}\ncurrent-context: default\n"
}
