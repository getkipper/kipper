package cmd

import (
	"io"
	"os"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/ssh"
)

// agentServer supports reading node records and holding the node lifecycle lease.
type agentServer interface {
	installer.LeaseServer
	installer.FirewallRunner
}

// upgradeAgentsAfterInstall upgrades workers after the server install, holding
// the same node lifecycle lease as kip node add and remove. It takes the lease
// only when a worker is recorded. dial receives each worker's host and recorded
// SSH user, with an empty user selecting the default login. It returns the
// connection and cleanup function.
func upgradeAgentsAfterInstall(out io.Writer, server agentServer, dial func(host, user string) (installer.FirewallRunner, func(), error)) error {
	records, err := installer.ReadNodeRecords(server)
	if err != nil {
		return err
	}
	logins := map[string]string{}
	for _, r := range records {
		logins[r.Host] = r.SSHUser
	}
	if !installer.HasWorkers(records) {
		return nil
	}
	say(out, "\n  Bringing the workers to the server's k3s version...\n")
	lease, err := installer.TakeNodeLease(server, "kip install")
	if err != nil {
		return err
	}
	defer lease.Release()
	onWorker := func(host string, use func(installer.CommandRunner) error) error {
		c, done, err := dial(host, logins[host])
		if err != nil {
			return err
		}
		defer done()
		return use(lease.Host(c))
	}
	return installer.UpgradeAgents(out, lease.Server(server), onWorker)
}

// sshDialer returns a dial function that uses base's keys and options.
// An empty user selects base's user.
func sshDialer(base ssh.Config) func(host, user string) (installer.FirewallRunner, func(), error) {
	return func(host, user string) (installer.FirewallRunner, func(), error) {
		c, err := ssh.Dial(agentSSH(base, host, user))
		if err != nil {
			return nil, nil, err
		}
		return c, func() { _ = c.Close() }, nil
	}
}

// agentSSH returns a copy of base for host, using base's user when user is empty.
func agentSSH(base ssh.Config, host, user string) ssh.Config {
	cfg := base
	cfg.Host = host
	if user != "" {
		cfg.User = user
	}
	return cfg
}

// upgradeAgentsDirect connects to the server and upgrades workers with the
// same SSH keys. Workers use their recorded SSH user, falling back to user.
func upgradeAgentsDirect(host, user, keyPath, fallbackKeyPath string) error {
	cfg := ssh.Config{Host: host, User: user, KeyPath: keyPath, FallbackKeyPath: fallbackKeyPath}
	server, err := ssh.Dial(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = server.Close() }()
	return upgradeAgentsAfterInstall(os.Stdout, server, sshDialer(cfg))
}
