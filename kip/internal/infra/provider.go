package infra

import "context"

// Machine represents a provisioned server.
type Machine struct {
	ID        string
	PublicIP  string
	PrivateIP string
	Region    string
}

// MachineSpec describes the desired server resources.
type MachineSpec struct {
	CPUs   int
	RAMGB  int
	DiskGB int
	Region string
}

// LBSpec describes a load balancer configuration.
type LBSpec struct {
	Ports  []int
	Region string
}

// LoadBalancer represents a provisioned load balancer.
type LoadBalancer struct {
	IP       string
	Hostname string
}

// InfraProvider abstracts infrastructure provisioning across bare metal
// and cloud providers. The MVP implements BareMetalProvider only.
type InfraProvider interface {
	// Provision creates one or more machines and returns their details.
	Provision(ctx context.Context, spec MachineSpec) ([]Machine, error)

	// Destroy tears down machines created by this provider.
	Destroy(ctx context.Context, machineIDs []string) error

	// GetLoadBalancer provisions a cloud load balancer if supported.
	// Bare metal returns nil — Traefik handles routing directly.
	GetLoadBalancer(ctx context.Context, spec LBSpec) (*LoadBalancer, error)

	// StorageClass returns the recommended default StorageClass for this provider.
	StorageClass() string

	// Name returns the provider identifier used in ~/.kip/config.yaml.
	Name() string
}
