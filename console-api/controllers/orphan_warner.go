package controllers

import (
	"context"
	"fmt"
	"log"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/labels"
)

const (
	orphanScanInterval = 5 * time.Minute
	orphanEventReason  = "OrphanWorkload"
)

// orphanSystemNamespace is skipped during orphan scanning. Kipper system
// components (zot, console, console-api) carry managed-by=kipper but
// legitimately have no owning Kipper CR.
const orphanSystemNamespace = "kipper-system"

// RunOrphanWarner starts a periodic loop that scans for workloads carrying
// managed-by=kipper without an owning Kipper CR (App, Service, Function,
// Volume) and surfaces each as a log warning and a Kubernetes Warning event
// on the offending workload.
//
// CRs are the source of truth: a Kipper-labelled workload that no CR claims
// is drift. It may have been added by a manual kubectl apply, left behind by
// a partial delete, or imported from somewhere else. The warner surfaces
// these so the operator can either delete the orphan or wrap it in a CR
// rather than letting it sit invisibly between the CLI and the console.
func RunOrphanWarner(ctx context.Context, c client.Client, recorder record.EventRecorder) {
	log.Printf("orphan warner started (interval: %s)", orphanScanInterval)
	ticker := time.NewTicker(orphanScanInterval)
	defer ticker.Stop()

	scanForOrphans(ctx, c, recorder)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			scanForOrphans(ctx, c, recorder)
		}
	}
}

func scanForOrphans(ctx context.Context, c client.Client, recorder record.EventRecorder) {
	scanOrphanDeployments(ctx, c, recorder)
	scanOrphanStatefulSets(ctx, c, recorder)
	scanOrphanPVCs(ctx, c, recorder)
}

func scanOrphanDeployments(ctx context.Context, c client.Client, recorder record.EventRecorder) {
	var deployments appsv1.DeploymentList
	if err := c.List(ctx, &deployments, client.MatchingLabels{labels.ManagedBy: labels.Kipper}); err != nil {
		log.Printf("orphan warner: list deployments: %v", err)
		return
	}

	for i := range deployments.Items {
		deploy := &deployments.Items[i]
		if deploy.Namespace == orphanSystemNamespace {
			continue
		}

		expectedKind := "App"
		var found bool
		var lookupErr error

		if deploy.Labels[labels.ResourceType] == labels.ResourceTypeFunction {
			expectedKind = "Function"
			var fn kipperv1.Function
			lookupErr = c.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: deploy.Namespace}, &fn)
		} else {
			var app kipperv1.App
			lookupErr = c.Get(ctx, types.NamespacedName{Name: deploy.Name, Namespace: deploy.Namespace}, &app)
		}

		if lookupErr == nil {
			found = true
		} else if !errors.IsNotFound(lookupErr) {
			log.Printf("orphan warner: lookup %s/%s: %v", deploy.Namespace, deploy.Name, lookupErr)
			continue
		}

		if !found {
			warnOrphan(recorder, deploy, expectedKind)
		}
	}
}

func scanOrphanStatefulSets(ctx context.Context, c client.Client, recorder record.EventRecorder) {
	var statefulsets appsv1.StatefulSetList
	if err := c.List(ctx, &statefulsets, client.MatchingLabels{labels.ManagedBy: labels.Kipper}); err != nil {
		log.Printf("orphan warner: list statefulsets: %v", err)
		return
	}

	for i := range statefulsets.Items {
		sts := &statefulsets.Items[i]
		if sts.Namespace == orphanSystemNamespace {
			continue
		}

		var svc kipperv1.Service
		err := c.Get(ctx, types.NamespacedName{Name: sts.Name, Namespace: sts.Namespace}, &svc)
		if err == nil {
			continue
		}
		if !errors.IsNotFound(err) {
			log.Printf("orphan warner: lookup service %s/%s: %v", sts.Namespace, sts.Name, err)
			continue
		}

		warnOrphan(recorder, sts, "Service")
	}
}

func scanOrphanPVCs(ctx context.Context, c client.Client, recorder record.EventRecorder) {
	var pvcs corev1.PersistentVolumeClaimList
	if err := c.List(ctx, &pvcs, client.MatchingLabels{labels.ResourceType: labels.ResourceTypeSharedVolume}); err != nil {
		log.Printf("orphan warner: list pvcs: %v", err)
		return
	}

	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if pvc.Namespace == orphanSystemNamespace {
			continue
		}

		// Volume CRs are named after the volume, which the PVC carries as a label.
		volumeName := pvc.Labels[labels.VolumeName]
		if volumeName == "" {
			volumeName = pvc.Name
		}

		var vol kipperv1.Volume
		err := c.Get(ctx, types.NamespacedName{Name: volumeName, Namespace: pvc.Namespace}, &vol)
		if err == nil {
			continue
		}
		if !errors.IsNotFound(err) {
			log.Printf("orphan warner: lookup volume %s/%s: %v", pvc.Namespace, volumeName, err)
			continue
		}

		warnOrphan(recorder, pvc, "Volume")
	}
}

func warnOrphan(recorder record.EventRecorder, obj client.Object, expectedKind string) {
	msg := fmt.Sprintf(
		"%s %q in namespace %q carries %s=%s but no owning %s CR exists — drift",
		obj.GetObjectKind().GroupVersionKind().Kind,
		obj.GetName(),
		obj.GetNamespace(),
		labels.ManagedBy,
		labels.Kipper,
		expectedKind,
	)
	log.Printf("orphan warner: %s", msg)
	if recorder != nil {
		recorder.Event(obj, corev1.EventTypeWarning, orphanEventReason, msg)
	}
}
