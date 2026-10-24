package cmd

import (
	"context"
	"fmt"
	"io"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

// lowServerRoom is the default size of a volume from kip volume create. Less
// room than that on the server is worth a line in kip status.
const lowServerRoom = 5 << 30

// refuseWithoutStorageRoom returns an error for a definite storage shortage.
// If the check is unavailable or inconclusive, it prints a warning to out and
// returns nil.
func refuseWithoutStorageRoom(ctx context.Context, out io.Writer, mgr *service.Manager, volumes []service.NewVolume, sizeOption string) error {
	warning, err := mgr.CheckStorageRoom(ctx, volumes, sizeOption)
	if warning != "" {
		say(out, "  ⚠  %s\n", warning)
	}
	return err
}

// newVolumesInPlans lists the volumes a manifest would add: those of the
// services and shared volumes it creates, and dump volumes for existing
// Postgres services whose consistent backups it enables.
func newVolumesInPlans(ctx context.Context, mgr *service.Manager, plans []applyPlan) ([]service.NewVolume, error) {
	var volumes []service.NewVolume
	for _, p := range plans {
		for _, res := range p.resources {
			if res.GVR != manifest.ServiceGVR && res.GVR != manifest.VolumeGVR {
				continue
			}
			name := res.Object.GetName()
			storage, _, _ := unstructured.NestedString(res.Object.Object, "spec", "storage")
			dump, _, _ := unstructured.NestedBool(res.Object.Object, "spec", "consistentBackups")
			_, err := mgr.Dynamic.Resource(res.GVR).Namespace(p.namespace).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				if res.GVR == manifest.ServiceGVR && dump {
					// Apply replaces the whole spec, so a manifest without storage
					// resets the service, and its new dump volume, to the default.
					if storage == "" {
						storage = service.DefaultStorage
					}
					added, err := mgr.DumpVolume(ctx, p.namespace, name, storage)
					if err != nil {
						return nil, err
					}
					volumes = append(volumes, named(name, added)...)
				}
				continue
			}
			if !errors.IsNotFound(err) {
				return nil, fmt.Errorf("reading %s/%s: %w", res.Object.GetKind(), name, err)
			}
			if res.GVR == manifest.VolumeGVR {
				size, _, _ := unstructured.NestedString(res.Object.Object, "spec", "size")
				q, err := resource.ParseQuantity(size)
				if err != nil {
					return nil, fmt.Errorf("volume %s: size %q: %w", name, size, err)
				}
				volumes = append(volumes, service.SharedVolume(p.namespace, name, q.Value()))
				continue
			}
			serviceType, _, _ := unstructured.NestedString(res.Object.Object, "spec", "type")
			created, err := service.ServiceVolumes(p.namespace, name, serviceType, storage, dump)
			if err != nil {
				return nil, fmt.Errorf("service %s: %w", name, err)
			}
			copies, _, _ := unstructured.NestedInt64(res.Object.Object, "spec", "copies")
			created[0].Copies = int32(copies) //nolint:gosec // G115: Validate bounds copies to 1 or 2.
			volumes = append(volumes, named(name, created)...)
		}
	}
	return volumes, nil
}

// named prefixes each volume's name with its service name, modifying the input slice.
func named(owner string, volumes []service.NewVolume) []service.NewVolume {
	for i := range volumes {
		volumes[i].Name = owner + " " + volumes[i].Name
	}
	return volumes
}

// reportServerStorageRoom warns when the roomiest eligible Longhorn disk has
// less capacity for a new single-copy volume than lowServerRoom. It stays silent
// when there is enough room or the check fails.
func reportServerStorageRoom(ctx context.Context, out io.Writer, mgr *service.Manager) {
	room, err := mgr.ServerStorageRoom(ctx)
	if err != nil || room.Bytes >= lowServerRoom {
		return
	}
	say(out, "  Storage:\n")
	say(out, "    ⚠  %s has %s left for new volumes scheduled by Longhorn.\n", room.Node, service.FormatSize(room.Bytes))
	say(out, "       kip service add requires enough space for the service. Free space with\n")
	say(out, "       kip service delete <name> --delete-data, or grow the server's disk.\n\n")
}
