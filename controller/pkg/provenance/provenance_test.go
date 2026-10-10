package provenance

import (
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

func ownedBy(managers ...string) []metav1.ManagedFieldsEntry {
	entries := make([]metav1.ManagedFieldsEntry, 0, len(managers))
	for _, m := range managers {
		entries = append(entries, managedEntry(m, `{"f:spec":{"f:resources":{"f:memoryRequest":{}}}}`))
	}
	return entries
}

func TestClassifyApp(t *testing.T) {
	cases := []struct {
		name   string
		value  string
		owners []metav1.ManagedFieldsEntry
		want   Source
	}{
		{"no value", "", ownedBy("kip"), Unset},
		{"set with kip", "512Mi", ownedBy("kip"), User},
		{"set in the new console", "512Mi", ownedBy("kipper-console"), User},
		{"set with another tool", "512Mi", ownedBy("kubectl-edit"), User},
		{"written by the old auto-sizer or old console", "256Mi", ownedBy("console-api"), Automatic},
		{"same-value save shares it", "256Mi", ownedBy("console-api", "kipper-console"), User},
		{"no owner at all", "256Mi", nil, Held},
		{"assigned on a first apply", "256Mi", ownedBy("before-first-apply"), Held},
		{"created by a restore", "256Mi", ownedBy("velero"), Held},
		{"carried unconfirmed by a copy", "256Mi", ownedBy("kipper-held"), Held},
		{"automatic and held together stays held", "256Mi", ownedBy("console-api", "kipper-held"), Held},
		{"a user owner outweighs held ones", "256Mi", ownedBy("before-first-apply", "kip"), User},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyApp(tc.value, tc.owners, "memoryRequest")
			if got != tc.want {
				t.Fatalf("ClassifyApp(%q) = %v, want %v", tc.value, got, tc.want)
			}
		})
	}
}

func TestClassifyOwnedForServicesAndFunctions(t *testing.T) {
	if got := ClassifyOwned(""); got != Unset {
		t.Fatalf("empty value = %v, want Unset", got)
	}
	// Service and Function quantities were never written by the auto-sizer,
	// so any value is the user's whoever wrote it.
	if got := ClassifyOwned("2Gi"); got != User {
		t.Fatalf("set value = %v, want User", got)
	}
}

func TestModeOf(t *testing.T) {
	cases := []struct {
		name              string
		request, limit    Source
		requestBelowLimit bool
		want              Mode
	}{
		{"nothing set", Unset, Unset, false, ModeAutomatic},
		{"automatic values", Automatic, Automatic, true, ModeAutomatic},
		{"user range", User, User, true, ModeBounded},
		{"user equal pair", User, User, false, ModeFixed},
		{"user request only", User, Unset, false, ModeFixed},
		{"user limit only", Unset, User, false, ModeFixed},
		{"held request", Held, User, true, ModeHeld},
		{"held limit", User, Held, true, ModeHeld},
	}
	for _, c := range cases {
		if got := ModeOf(c.request, c.limit, c.requestBelowLimit); got != c.want {
			t.Errorf("%s: ModeOf = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestModeNames(t *testing.T) {
	for mode, want := range map[Mode]string{ModeAutomatic: "automatic", ModeBounded: "bounded", ModeFixed: "fixed", ModeHeld: "held"} {
		if got := mode.String(); got != want {
			t.Errorf("Mode(%d).String() = %q, want %q", mode, got, want)
		}
	}
}
