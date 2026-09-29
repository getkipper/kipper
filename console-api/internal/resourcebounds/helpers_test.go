package resourcebounds

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func managedEntry(manager, fields string) metav1.ManagedFieldsEntry {
	return metav1.ManagedFieldsEntry{
		Manager:    manager,
		FieldsType: "FieldsV1",
		FieldsV1:   &metav1.FieldsV1{Raw: []byte(fields)},
	}
}
