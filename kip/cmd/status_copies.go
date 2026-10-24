package cmd

import (
	"context"
	"fmt"
	"io"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

// copiesEntry is one object in the storage copies section of kip status.
type copiesEntry struct {
	namespace, name, kind string
	copies                service.Copies
}

// label names the object and adds detail in parentheses for services and volumes.
func (e copiesEntry) label(detail string) string {
	if e.kind == "project" {
		return "shared storage of project " + e.name
	}
	if detail != "" {
		return fmt.Sprintf("%s/%s (%s, %s)", e.namespace, e.name, e.kind, detail)
	}
	return fmt.Sprintf("%s/%s (%s)", e.namespace, e.name, e.kind)
}

// reportStorageCopies lists the services, volumes and project shared storage
// whose copies are unset, those being converted, and those whose
// StorageCopies condition is otherwise False (degraded, held back at
// creation, or a conversion that cannot go on) or whose copies Kipper could
// not check. A kind that cannot be listed is skipped.
func reportStorageCopies(ctx context.Context, out io.Writer, dyn dynamic.Interface) {
	var unset, converting, attention []copiesEntry
	for _, kind := range []struct {
		gvr  schema.GroupVersionResource
		name string
	}{{manifest.ServiceGVR, "service"}, {manifest.VolumeGVR, "volume"}, {manifest.ProjectGVR, "project"}} {
		list, err := dyn.Resource(kind.gvr).List(ctx, metav1.ListOptions{})
		if err != nil {
			continue
		}
		for _, obj := range list.Items {
			e := copiesEntry{namespace: obj.GetNamespace(), name: obj.GetName(), kind: kind.name, copies: service.CopiesOf(obj.Object)}
			switch {
			case e.copies.Reason == "NotSet":
				unset = append(unset, e)
			case e.copies.Converting():
				converting = append(converting, e)
			case e.copies.Status == "False", e.copies.Reason == "Unknown":
				attention = append(attention, e)
			}
		}
	}
	byLabel := func(entries []copiesEntry) {
		sort.Slice(entries, func(i, j int) bool { return entries[i].label("") < entries[j].label("") })
	}
	byLabel(unset)
	byLabel(converting)
	byLabel(attention)

	if len(unset) > 0 {
		say(out, "  Storage copies not set, so Kipper leaves them as they are:\n")
		for _, e := range unset {
			switch {
			case e.kind == "project":
				// No kip command sets project storage copies, so the
				// controller's message carries the command.
				say(out, "    ·  %s\n       %s\n", e.label(""), e.copies.Message)
			case e.copies.LiveKnown:
				say(out, "    ·  %s\n", e.label(fmt.Sprintf("%d %s", e.copies.Live, pluralCopies(e.copies.Live))))
			default:
				say(out, "    ·  %s\n", e.label(""))
			}
		}
		say(out, "       Set copies on a service or volume with 'kip service update <name> --copies <n>' or 'kip volume update <name> --copies <n>'. Kipper converts the volume when it has a different number.\n\n")
	}
	if len(converting) > 0 {
		say(out, "  Storage copies being converted:\n")
		for _, e := range converting {
			say(out, "    …  %s\n       %s\n", e.label(""), e.copies.Message)
		}
		say(out, "\n")
	}
	if len(attention) > 0 {
		say(out, "  Storage copies needing attention:\n")
		for _, e := range attention {
			say(out, "    ✗  %s\n       %s\n", e.label(""), e.copies.Message)
		}
		say(out, "\n")
	}
}
