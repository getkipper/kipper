package cmd

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"

	"github.com/getkipper/kipper/kip/internal/config"
)

func TestSSHConfigReachesEachNodeThroughTheTunnel(t *testing.T) {
	cluster := &config.Cluster{Name: "prod", SSHUser: "ubuntu", SSHKey: "/home/op/.ssh/id_ed25519", Ops: &config.Ops{Device: "laptop", Nodes: []config.OpsNode{
		{MachineID: accessServerID, Name: "server", Host: "192.0.2.10", SSHPorts: []int{2222}},
		{MachineID: accessWorkerID, Name: "worker-1", Host: "192.0.2.11"},
	}}}

	out, err := sshConfigBlocks(cluster, "kip")

	require.NoError(t, err)
	assert.Contains(t, out, "Host prod-server\n  HostName 192.0.2.10\n  User ubuntu\n  Port 2222\n  IdentityFile /home/op/.ssh/id_ed25519\n")
	assert.Contains(t, out, "Host prod-worker-1\n  HostName 192.0.2.11\n  User ubuntu\n  Port 22\n")
	assert.Contains(t, out, "--node' '"+accessWorkerID+"' '--device' 'laptop' %p\n")
	assert.Contains(t, out, "ProxyCommand 'kip' '--cluster' 'prod'", "kip comes from PATH, so an upgrade keeps it working")

	cluster.Nodes = []config.NodeRecord{{Name: "worker-1", Host: "192.0.2.11", Role: "worker", SSHUser: "ec2-user"}}
	out, err = sshConfigBlocks(cluster, "kip")
	require.NoError(t, err)
	assert.Contains(t, out, "Host prod-worker-1\n  HostName 192.0.2.11\n  User ec2-user\n", "a node keeps its own login")
	assert.Contains(t, out, "Host prod-server\n  HostName 192.0.2.10\n  User ubuntu\n")
}

func testKubeconfig() *clientcmdapi.Config {
	kc := clientcmdapi.NewConfig()
	kc.Clusters["prod"] = &clientcmdapi.Cluster{Server: "https://api.example.com:6443", CertificateAuthorityData: []byte("ca"), ProxyURL: "http://proxy.example.com:3128"}
	kc.AuthInfos["admin"] = &clientcmdapi.AuthInfo{Token: "t"}
	kc.Contexts["prod"] = &clientcmdapi.Context{Cluster: "prod", AuthInfo: "admin"}
	kc.CurrentContext = "prod"
	return kc
}

func TestTunnelContextKeepsTheCertificateCheck(t *testing.T) {
	kc := testKubeconfig()

	name, err := addTunnelContext(kc, 16443)

	require.NoError(t, err)
	assert.Equal(t, "prod-tunnel", name)
	entry := kc.Clusters[kc.Contexts[name].Cluster]
	assert.Equal(t, "https://127.0.0.1:16443", entry.Server)
	assert.Equal(t, "api.example.com", entry.TLSServerName)
	assert.Equal(t, []byte("ca"), entry.CertificateAuthorityData)
	assert.Empty(t, entry.ProxyURL)
	assert.Equal(t, "admin", kc.Contexts[name].AuthInfo)
	assert.Equal(t, "https://api.example.com:6443", kc.Clusters["prod"].Server, "the original entry stays")
	assert.Equal(t, "prod", kc.CurrentContext)

	name, err = addTunnelContext(kc, 26443)
	require.NoError(t, err, "a rerun replaces what kip wrote")
	assert.Equal(t, "https://127.0.0.1:26443", kc.Clusters[kc.Contexts[name].Cluster].Server)

	kc = testKubeconfig()
	kc.Clusters["prod"].TLSServerName = "kubernetes.internal"
	name, err = addTunnelContext(kc, 16443)
	require.NoError(t, err)
	assert.Equal(t, "kubernetes.internal", kc.Clusters[kc.Contexts[name].Cluster].TLSServerName)
}

func TestKubeconfigLeavesEntriesItDidNotWriteAlone(t *testing.T) {
	for name, setup := range map[string]func(*clientcmdapi.Config){
		"context": func(kc *clientcmdapi.Config) {
			kc.Contexts["prod-tunnel"] = &clientcmdapi.Context{Cluster: "staging", AuthInfo: "admin"}
		},
		"cluster": func(kc *clientcmdapi.Config) {
			kc.Clusters["prod-tunnel"] = &clientcmdapi.Cluster{Server: "https://staging.example.com:6443"}
		},
		"local forward of another cluster": func(kc *clientcmdapi.Config) {
			kc.Clusters["prod-tunnel"] = &clientcmdapi.Cluster{Server: "https://127.0.0.1:26443", CertificateAuthorityData: []byte("other ca")}
		},
	} {
		t.Run(name, func(t *testing.T) {
			kc := testKubeconfig()
			setup(kc)
			before := kc.DeepCopy()

			_, err := addTunnelContext(kc, 16443)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "prod-tunnel")
			assert.Equal(t, before, kc)
		})
	}
}

func TestForwardCarriesEachConnection(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serveForward(ctx, l, func(context.Context) (net.Conn, error) {
			server, client := net.Pipe()
			go func() {
				defer func() { _ = server.Close() }()
				buf := make([]byte, 4)
				if _, err := io.ReadFull(server, buf); err == nil {
					_, _ = server.Write(append([]byte("re:"), buf...))
				}
			}()
			return client, nil
		})
	}()

	c, err := net.Dial("tcp", l.Addr().String())
	require.NoError(t, err)
	_, err = c.Write([]byte("ping"))
	require.NoError(t, err)
	got := make([]byte, 7)
	_, err = io.ReadFull(c, got)
	require.NoError(t, err)
	assert.Equal(t, "re:ping", string(got))
	_ = c.Close()

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("forward kept running after Ctrl+C")
	}
}
