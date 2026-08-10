package infra

import (
	"context"
	"fmt"

	"github.com/getkipper/kipper/kip/internal/ssh"
)

// BareMetalProvider implements InfraProvider for existing Linux servers
// accessed via SSH. It is the default provider for Kipper installations.
//
// SSHKey is the user's explicit key choice (--ssh-key flag,
// cluster.SSHKey from config, or KIP_SSH_KEY env var). When set, ssh
// is invoked with `-i <path> -o IdentitiesOnly=yes` to force this key.
//
// FallbackSSHKey is a hint at a default key path (typically
// `~/.ssh/id_ed25519`). If SSHKey is empty and the fallback file
// exists, ssh tries it; otherwise ssh falls through to its normal
// lookup (ssh-agent, ~/.ssh/config, default identity files). The
// fallback path never sets IdentitiesOnly.
type BareMetalProvider struct {
	Host           string
	SSHKey         string
	FallbackSSHKey string
	client         *ssh.Client
}

// Connect establishes an SSH connection to the target server.
func (p *BareMetalProvider) Connect() error {
	client, err := ssh.Dial(ssh.Config{
		Host:            p.Host,
		User:            "root",
		KeyPath:         p.SSHKey,
		FallbackKeyPath: p.FallbackSSHKey,
	})
	if err != nil {
		return err
	}
	p.client = client
	return nil
}

// Client returns the underlying SSH client for use by installer steps.
func (p *BareMetalProvider) Client() *ssh.Client {
	return p.client
}

// Close terminates the SSH connection.
func (p *BareMetalProvider) Close() error {
	if p.client != nil {
		return p.client.Close()
	}
	return nil
}

// Provision is a no-op for bare metal — the machine already exists.
// It returns the host as a single Machine.
func (p *BareMetalProvider) Provision(_ context.Context, _ MachineSpec) ([]Machine, error) {
	return []Machine{
		{
			ID:       p.Host,
			PublicIP: p.Host,
		},
	}, nil
}

// Destroy is not supported for bare metal servers.
func (p *BareMetalProvider) Destroy(_ context.Context, _ []string) error {
	return fmt.Errorf("destroy is not supported for bare metal servers")
}

// GetLoadBalancer returns nil for bare metal — Traefik handles routing directly.
func (p *BareMetalProvider) GetLoadBalancer(_ context.Context, _ LBSpec) (*LoadBalancer, error) {
	return nil, nil
}

// StorageClass returns "longhorn" as the default storage backend for bare metal.
func (p *BareMetalProvider) StorageClass() string {
	return "longhorn"
}

// Name returns the provider identifier for config files.
func (p *BareMetalProvider) Name() string {
	return "baremetal"
}
