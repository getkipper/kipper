package cmd

import (
	"context"
	"io"
	"sort"
	"strconv"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/getkipper/kipper/controller/pkg/platform"
	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/manifest"
)

// reportLocalVolumes lists what local storage leaves behind on a cluster
// that moved to Longhorn: moves that did not finish, service volumes still
// on local-path, and local copies kept after a move. It prints nothing when
// none are left. A failed service or claim lookup suppresses all output.
// A failed volume lookup suppresses only the retained-copy count.
func reportLocalVolumes(ctx context.Context, out io.Writer, client kubernetes.Interface, dyn dynamic.Interface) {
	services, err := dyn.Resource(manifest.ServiceGVR).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	claims, err := client.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	local := map[string]bool{}
	for _, c := range claims.Items {
		if c.Spec.StorageClassName != nil && *c.Spec.StorageClassName == platform.LocalPathClass {
			local[c.Namespace+"/"+c.Name] = true
		}
	}
	var unfinished, onLocal []string
	for _, s := range services.Items {
		name := s.GetNamespace() + "/" + s.GetName()
		switch {
		case s.GetAnnotations()[platform.StorageMoveAnnotation] != "":
			unfinished = append(unfinished, name)
		case local[s.GetNamespace()+"/"+installer.ServiceDataClaim(s.GetName())]:
			onLocal = append(onLocal, name)
		}
	}
	sort.Strings(unfinished)
	sort.Strings(onLocal)

	var kept int
	var keptBytes int64
	pvs, err := client.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{LabelSelector: platform.RetainedLocalCopyLabel + "=true"})
	if err == nil {
		for _, pv := range pvs.Items {
			if pv.Annotations[platform.RetainedLocalCopyStateAnnotation] != platform.RetainedLocalCopyMoved {
				continue
			}
			kept++
			used, _ := strconv.ParseInt(pv.Annotations[platform.RetainedLocalCopyUsedBytesAnnotation], 10, 64)
			keptBytes += used
		}
	}

	if len(unfinished) > 0 {
		say(out, "  Moves to Longhorn not finished:\n")
		say(out, "    ⚠  %s\n", strings.Join(unfinished, "\n    ⚠  "))
		say(out, "       These services stay stopped. Finish each move with 'kip service storage longhorn <name>', or undo it with --rollback.\n\n")
	}
	if len(onLocal) > 0 {
		say(out, "  Service volumes still on local storage:\n")
		say(out, "    ·  %s\n", strings.Join(onLocal, "\n    ·  "))
		say(out, "       Move each one with 'kip service storage longhorn <name>'. The service is stopped while its data is copied.\n\n")
	}
	if kept > 0 {
		say(out, "  Local copies kept after a move: %d (%s)\n", kept, humanGiB(keptBytes))
		say(out, "       'kip cluster storage prune' deletes them once the services run well on Longhorn.\n\n")
	}
}
