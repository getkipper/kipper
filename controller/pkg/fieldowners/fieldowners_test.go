package fieldowners

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func managedEntry(manager, fields string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{
		Manager:    manager,
		FieldsType: "FieldsV1",
		FieldsV1:   &metav1.FieldsV1{Raw: []byte(fields)},
	}
}

func TestOwners(t *testing.T) {
	entries := []metav1.ManagedFieldsEntry{
		managedEntry("kip", `{"f:spec":{"f:image":{},"f:resources":{"f:memoryRequest":{},"f:memoryLimit":{}}}}`),
		managedEntry("console-api", `{"f:spec":{"f:resources":{"f:memoryRequest":{},"f:cpuRequest":{}}}}`),
		managedEntry("kipper-console", `{"f:metadata":{"f:labels":{}}}`),
		managedEntry("kip-self", `{"f:spec":{".":{},"f:resources":{".":{},"f:cpuLimit":{}},"f:ports":{"k:{\"port\":80}":{".":{}}}}}`),
	}

	cases := []struct {
		name string
		path []string
		want []string
	}{
		{"shared", []string{"spec", "resources", "memoryRequest"}, []string{"console-api", "kip"}},
		{"single owner", []string{"spec", "resources", "memoryLimit"}, []string{"kip"}},
		{"other owner", []string{"spec", "resources", "cpuRequest"}, []string{"console-api"}},
		{"through ancestors that own themselves", []string{"spec", "resources", "cpuLimit"}, []string{"kip-self"}},
		{"unowned", []string{"spec", "resources", "memoryLimitMissing"}, nil},
		{"parent path is not a leaf", []string{"spec", "resources"}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Owners(entries, tc.path...); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Owners(%v) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

func TestOwnersListsEachManagerOnce(t *testing.T) {
	update := managedEntry("kip", `{"f:spec":{"f:resources":{"f:memoryRequest":{}}}}`)
	update.Operation = metav1.ManagedFieldsOperationUpdate
	apply := managedEntry("kip", `{"f:spec":{"f:resources":{"f:memoryRequest":{}}}}`)
	apply.Operation = metav1.ManagedFieldsOperationApply
	got := Owners([]metav1.ManagedFieldsEntry{update, apply}, "spec", "resources", "memoryRequest")
	if !reflect.DeepEqual(got, []string{"kip"}) {
		t.Fatalf("fieldOwners = %v, want [kip]", got)
	}
}

func TestOwnersIgnoresUnreadableEntries(t *testing.T) {
	entries := []metav1.ManagedFieldsEntry{
		{Manager: "no-fields"},
		managedEntry("broken", `{not json`),
		managedEntry("kip", `{"f:spec":{"f:resources":{"f:memoryRequest":{}}}}`),
	}
	got := Owners(entries, "spec", "resources", "memoryRequest")
	if !reflect.DeepEqual(got, []string{"kip"}) {
		t.Fatalf("fieldOwners = %v, want [kip]", got)
	}
}
