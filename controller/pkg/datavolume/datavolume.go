// Package datavolume decides which PersistentVolumeClaims hold a service's
// data, because more than one module deletes them.
//
// The CLI destroys them on `kip service delete --delete-data`, and the Service
// finalizer destroys them when the console asks for it. Deleting a volume
// cannot be undone, so the two must not drift on which ones they mean.
//
// Two conditions have to hold. The label narrows the set to the service: the
// StatefulSet controller stamps its own selector, app=<service>, onto every
// claim it creates from the template, so a service's claims carry it whether or
// not the template does. The name then has to be one a StatefulSet would have
// built from a claim template called "data", which is what both writers of a
// Kipper service use. A volume that merely carries the label belongs to whoever
// made it, and an App of the same name carries it too.
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
