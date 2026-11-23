package v1alpha1

import (
	"os"
	"regexp"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestImageSchemaRefusesWhitespace checks that CRDs reject whitespace in image
// names project members can set. kip reads pods over SSH as tab-separated records,
// so a tab or newline in an image name would fail any read containing that image.
func TestImageSchemaRefusesWhitespace(t *testing.T) {
	for _, file := range []string{"kipper.run_apps.yaml", "kipper.run_jobs.yaml", "kipper.run_functions.yaml"} {
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
										Properties struct {
											Image struct {
												Pattern string `json:"pattern"`
											} `json:"image"`
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
			pattern := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties.Spec.Properties.Image.Pattern
			if pattern == "" {
				t.Fatal("spec.image has no pattern")
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
