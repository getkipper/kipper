package installer

import (
	"embed"
	"fmt"
)

// admissionPolicies holds Kipper's ValidatingAdmissionPolicies and their
// bindings, copied from deploy/policies.
//
//go:embed policies/*.yaml
var admissionPolicies embed.FS

// AdmissionPolicyManifests returns embedded policy YAML files in filename order.
// It returns an error if a directory or file cannot be read.
func AdmissionPolicyManifests() ([][]byte, error) {
	entries, err := admissionPolicies.ReadDir("policies")
	if err != nil {
		return nil, fmt.Errorf("reading embedded admission policies: %w", err)
	}
	manifests := make([][]byte, 0, len(entries))
	for _, e := range entries {
		data, err := admissionPolicies.ReadFile("policies/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("reading admission policy %s: %w", e.Name(), err)
		}
		manifests = append(manifests, data)
	}
	return manifests, nil
}

// InstallAdmissionPolicies applies embedded policies in filename order using kubectl.
// Call it after InstallCRDs because the policies use Kipper resources as parameters.
// It stops on the first read or apply error, leaving earlier policies applied.
func InstallAdmissionPolicies(client CommandRunner) error {
	manifests, err := AdmissionPolicyManifests()
	if err != nil {
		return err
	}
	for _, m := range manifests {
		if _, err := client.Run(fmt.Sprintf("cat <<'KIPEOF' | kubectl apply -f -\n%sKIPEOF", m)); err != nil {
			return fmt.Errorf("applying admission policies: %w", err)
		}
	}
	return nil
}
