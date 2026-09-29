// Package fieldowners reads which field managers own a field, from an
// object's managedFields.
package fieldowners

import (
	"encoding/json"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Owners returns sorted, distinct field managers for a scalar field path.
// It follows object fields (f: entries); list selectors are unsupported.
// Unreadable field sets are skipped.
func Owners(entries []metav1.ManagedFieldsEntry, path ...string) []string {
	var owners []string
	for _, entry := range entries {
		if entry.FieldsV1 == nil {
			continue
		}
		var fields map[string]any
		if err := json.Unmarshal(entry.FieldsV1.Raw, &fields); err != nil {
			continue
		}
		if ownsLeaf(fields, path) {
			owners = append(owners, entry.Manager)
		}
	}
	slices.Sort(owners)
	return slices.Compact(owners)
}

// ownsLeaf reports whether a FieldsV1 set owns path as a scalar leaf, which
// FieldsV1 records as an empty object. An ancestor's "." entry marks the
// ancestor itself as a member and does not stop the walk to its children.
func ownsLeaf(fields map[string]any, path []string) bool {
	node := fields
	for _, name := range path {
		child, ok := node["f:"+name].(map[string]any)
		if !ok {
			return false
		}
		node = child
	}
	return len(node) == 0
}
