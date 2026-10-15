package installer

import "errors"

// k3sNetworkConfigPath is separate from the generated config.yaml so its
// settings survive installation reruns.
const k3sNetworkConfigPath = "/etc/rancher/k3s/config.yaml.d/kipper-network.yaml"

// withoutOutput removes the runner's error wrapper, which may contain secrets
// from command output. The underlying error must be safe to display.
func withoutOutput(err error) error {
	if cause := errors.Unwrap(err); cause != nil {
		return cause
	}
	return errors.New("the command failed")
}
