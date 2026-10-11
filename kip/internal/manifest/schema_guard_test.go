package manifest

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// manifestOmits records intentional gaps between CRD and manifest fields.
var manifestOmits = map[string]map[string]string{
	"services": {"bindings": "nothing reads or writes it; a binding is declared on the app as serviceBindings"},
}

// Check top-level field coverage so new CRD fields prompt a manifest review.
func TestManifestCarriesEveryCRDSpecField(t *testing.T) {
	for kind, target := range map[string]interface{}{
		"apps": AppSpec{}, "jobs": JobSpec{}, "services": SvcSpec{}, "volumes": VolSpec{}, "functions": FuncSpec{},
	} {
		raw, err := os.ReadFile("../../../deploy/crds/kipper.run_" + kind + ".yaml")
		if err != nil {
			t.Fatal(err)
		}
		var crd struct {
			Spec struct {
				Versions []struct {
					Schema struct {
						OpenAPIV3Schema struct {
							Properties map[string]struct {
								Properties map[string]interface{} `json:"properties"`
							} `json:"properties"`
						} `json:"openAPIV3Schema"`
					} `json:"schema"`
				} `json:"versions"`
			} `json:"spec"`
		}
		if err := yaml.Unmarshal(raw, &crd); err != nil {
			t.Fatal(err)
		}
		carried := map[string]bool{}
		typ := reflect.TypeOf(target)
		for i := 0; i < typ.NumField(); i++ {
			name := strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0]
			carried[name] = true
		}
		var missing []string
		for field := range crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties {
			if !carried[field] && manifestOmits[kind][field] == "" {
				missing = append(missing, field)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			t.Errorf("%s: the manifest carries no %v; add them, or list them in manifestOmits with the reason", kind, missing)
		}
	}
}
