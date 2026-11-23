package v1alpha1

import (
	"os"
	"regexp"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestImageSchemaRefusesWhitespace checks image fields that project members can
// set, including service versions used as image tags. kip reads pods over SSH as
// tab-separated records, so a tab or newline in an image name fails the read.
func TestImageSchemaRefusesWhitespace(t *testing.T) {
	for file, field := range map[string]string{
		"kipper.run_apps.yaml":      "image",
		"kipper.run_jobs.yaml":      "image",
		"kipper.run_functions.yaml": "image",
		"kipper.run_services.yaml":  "version",
	} {
		t.Run(file, func(t *testing.T) {
			raw, err := os.ReadFile("../../../deploy/crds/" + file)
			if err != nil {
				t.Fatalf("reading the generated CRD: %v", err)
			}
			var crd struct {
				Spec struct {
					Versions []struct {
						Schema struct {
							OpenAPIV3Schema struct {
								Properties struct {
									Spec struct {
										Properties map[string]struct {
											Pattern string `json:"pattern"`
										} `json:"properties"`
									} `json:"spec"`
								} `json:"properties"`
							} `json:"openAPIV3Schema"`
						} `json:"schema"`
					} `json:"versions"`
				} `json:"spec"`
			}
			if err := yaml.Unmarshal(raw, &crd); err != nil {
				t.Fatalf("parsing the CRD: %v", err)
			}
			if len(crd.Spec.Versions) == 0 {
				t.Fatal("the CRD has no versions")
			}
			pattern := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties[field].Pattern
			if pattern == "" {
				t.Fatalf("spec.%s has no pattern", field)
			}
			re := regexp.MustCompile(pattern)
			for _, image := range []string{"", "nginx", "ghcr.io/getkipper/app:1.2.3", "registry.example.com:5000/shop/web@sha256:0123abcd"} {
				if !re.MatchString(image) {
					t.Errorf("the pattern refuses %q", image)
				}
			}
			for _, image := range []string{"nginx\tbroken", "nginx\nbroken", "nginx broken", "nginx\r"} {
				if re.MatchString(image) {
					t.Errorf("the pattern accepts %q", image)
				}
			}
		})
	}
}
