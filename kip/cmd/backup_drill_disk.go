package cmd

import (
	"context"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8slabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/getkipper/kipper/kip/internal/installer"
	"github.com/getkipper/kipper/kip/internal/service"
)

// drillVolume describes a claim's requested size in bytes and estimated copy
// count for the drill, including one copy for local storage.
type drillVolume struct {
	bytes  int64
	copies int
}

// drillClaims selects claims referenced by the namespace drill's selected pods
// or matching StatefulSet pod and claim templates, so stopped services count.
// It matches references by namespace and claim name, includes each input entry
// once even if several pods reference it, and sorts the results by claim name.
func drillClaims(pods []corev1.Pod, sets []appsv1.StatefulSet, claims []corev1.PersistentVolumeClaim) ([]corev1.PersistentVolumeClaim, error) {
	selectors, err := drillPodSelectors()
	if err != nil {
		return nil, err
	}
	wanted := map[string]bool{}
	want := func(ns string, labels map[string]string, volumes []corev1.Volume) bool {
		for _, s := range selectors {
			if !s.Matches(k8slabels.Set(labels)) {
				continue
			}
			for _, v := range volumes {
				if v.PersistentVolumeClaim != nil {
					wanted[ns+"/"+v.PersistentVolumeClaim.ClaimName] = true
				}
			}
			return true
		}
		return false
	}
	for _, pod := range pods {
		want(pod.Namespace, pod.Labels, pod.Spec.Volumes)
	}
	var selectedSets []appsv1.StatefulSet
	for _, set := range sets {
		if want(set.Namespace, set.Spec.Template.Labels, set.Spec.Template.Spec.Volumes) {
			selectedSets = append(selectedSets, set)
		}
	}
	var out []corev1.PersistentVolumeClaim
	for _, c := range claims {
		_, templated := installer.ClaimOwner(selectedSets, c.Namespace, c.Name)
		if wanted[c.Namespace+"/"+c.Name] || templated {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// drillPodSelectors returns the label selectors of a namespace drill's
// restore, so the estimate follows what the restore picks.
func drillPodSelectors() ([]k8slabels.Selector, error) {
	raw, _ := drillRestoreSpec("", "", "")["orLabelSelectors"].([]interface{})
	var out []k8slabels.Selector
	for _, r := range raw {
		m, _ := r.(map[string]interface{})
		var ls metav1.LabelSelector
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(m, &ls); err != nil {
			return nil, err
		}
		s, err := metav1.LabelSelectorAsSelector(&ls)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// drillDiskNote estimates Longhorn or local storage from live claim sizes and
// copy counts, which may differ from the backup. It returns "" for an empty input.
func drillDiskNote(volumes []drillVolume) string {
	if len(volumes) == 0 {
		return ""
	}
	var requested, onDisk int64
	for _, v := range volumes {
		requested += v.bytes
		onDisk += v.bytes * int64(v.copies)
	}
	return fmt.Sprintf("Based on the namespace's current volumes, the drill restores about %d %s totaling %s, which take up to %s of storage with their copies. "+
		"A backup taken before volumes were added, removed or resized restores different sizes.",
		len(volumes), plural(len(volumes), "volume", "volumes"), service.FormatSize(requested), service.FormatSize(onDisk))
}

// reportDrillDisk prints the drill's disk note for source, or why it could
// not be estimated.
func reportDrillDisk(ctx context.Context, mgr *service.Manager, source string) {
	note, err := estimateDrillDisk(ctx, mgr, source)
	switch {
	case err != nil:
		fmt.Printf("  ·  The drill's disk use was not estimated: %v\n", err)
	case note != "":
		fmt.Printf("  ·  %s\n", note)
	}
}

func estimateDrillDisk(ctx context.Context, mgr *service.Manager, source string) (string, error) {
	pods, err := mgr.Client.CoreV1().Pods(source).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing pods: %w", err)
	}
	sets, err := mgr.Client.AppsV1().StatefulSets(source).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing StatefulSets: %w", err)
	}
	claims, err := mgr.Client.CoreV1().PersistentVolumeClaims(source).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", fmt.Errorf("listing volume claims: %w", err)
	}
	restored, err := drillClaims(pods.Items, sets.Items, claims.Items)
	if err != nil {
		return "", err
	}
	copies := map[string]int{}
	var volumes []drillVolume
	for _, c := range restored {
		n := 1
		if class := c.Spec.StorageClassName; class != nil {
			known, ok := copies[*class]
			if !ok {
				if known, err = mgr.EffectiveCopies(ctx, *class); err != nil {
					return "", err
				}
				copies[*class] = known
			}
			n = known
		}
		size := c.Spec.Resources.Requests[corev1.ResourceStorage]
		volumes = append(volumes, drillVolume{bytes: size.Value(), copies: n})
	}
	return drillDiskNote(volumes), nil
}

// degradedDrillWarning reports current and configured copy counts when
// Longhorn reports degraded robustness. It returns "" for any other state.
func degradedDrillWarning(claim string, c service.ClaimCopies) string {
	if c.Robustness != "degraded" {
		return ""
	}
	return fmt.Sprintf("%s has %d of %d copies. Longhorn could not place every copy, which is harmless for the drill's scratch copy.", claim, c.Current, c.Wanted)
}

// reportDegradedDrillVolumes warns about each degraded volume in the drill
// namespace. A drill never fails on it, and read errors are skipped.
func reportDegradedDrillVolumes(ctx context.Context, mgr *service.Manager) {
	claims, err := mgr.Client.CoreV1().PersistentVolumeClaims(drillNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return
	}
	for i := range claims.Items {
		c, err := mgr.ClaimCopies(ctx, &claims.Items[i])
		if err != nil {
			continue
		}
		if w := degradedDrillWarning(claims.Items[i].Name, c); w != "" {
			fmt.Printf("  ⚠  %s\n", w)
		}
	}
}
