// Package datavolume shares service-data PVC selection between the CLI and
// Service finalizer. Callers combine the app=<service> selector with Belongs,
// which requires a data-<service>-<ordinal> name. A label alone is insufficient
// for destructive cleanup.
package datavolume

import (
	"fmt"
	"strings"
)

// DeleteAnnotation on a Service asks the cluster to destroy its data when the
// service is deleted. The console sets it because a browser cannot hold a
// request open while a database stops, the CLI sets it because a project's own
// operators may delete their services but not the volumes underneath them, and
// anyone deleting a service with kubectl can set it too. Without it the volume
// stays, which is what an ordinary delete has always done.
const DeleteAnnotation = "kipper.run/delete-data"

// LabelKey is the label whose value is the service a claim belongs to. The
// StatefulSet controller copies it from the workload's selector.
const LabelKey = "app"

// Selector lists the claims that carry a service's label.
func Selector(service string) string {
	return fmt.Sprintf("%s=%s", LabelKey, service)
}

// Belongs reports whether a claim of this name is one the service's StatefulSet
// created for it.
//
// The tail has to be an ordinal and nothing else, so a copy somebody took of
// data-db-0 is not mistaken for the volume it was copied from.
func Belongs(service, claim string) bool {
	ordinal, ok := strings.CutPrefix(claim, "data-"+service+"-")
	if !ok || ordinal == "" {
		return false
	}
	for _, digit := range ordinal {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}
