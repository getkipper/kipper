// Package resourcebounds classifies CPU and memory values by ownership
// and resolves container resources within the user's bounds.
package resourcebounds

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/getkipper/kipper/controller/pkg/provenance"
)

// Source says who set a resource quantity; see provenance.Source.
type Source = provenance.Source

const (
	Unset     = provenance.Unset
	User      = provenance.User
	Automatic = provenance.Automatic
	Held      = provenance.Held
)

// HeldManager preserves held values during copy and migration.
const HeldManager = provenance.HeldManager

// ClassifyAppQuantity decides who set an App quantity; see provenance.ClassifyApp.
func ClassifyAppQuantity(value string, managedFields []metav1.ManagedFieldsEntry, field string) Source {
	return provenance.ClassifyApp(value, managedFields, field)
}

// ClassifyOwnedQuantity decides who set a Service or Function quantity; see
// provenance.ClassifyOwned.
func ClassifyOwnedQuantity(value string) Source {
	return provenance.ClassifyOwned(value)
}
