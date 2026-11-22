package installer

import "errors"

// k3sNetworkConfigPath is separate from the generated config.yaml so its
// settings survive installation reruns.
const k3sNetworkConfigPath = "/etc/rancher/k3s/config.yaml.d/kipper-network.yaml"

// withoutOutput removes the runner's error wrapper, which may contain secrets
// from command output. The underlying error must be safe to display. It preserves
// node lease refusals because their text is fixed, and returns a generic error
// when no underlying cause is available.
func withoutOutput(err error) error {
	var refusal leaseRefusal
	if errors.As(err, &refusal) {
		return refusal
	}
	if cause := errors.Unwrap(err); cause != nil {
		return cause
	}
	return errors.New("the command failed")
}
