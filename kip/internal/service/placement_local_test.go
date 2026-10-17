package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// localPathPV builds a Local or HostPath volume with affinity to node.
func localPathPV(name, node string, local bool) *corev1.PersistentVolume {
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: corev1.PersistentVolumeSpec{
			NodeAffinity: &corev1.VolumeNodeAffinity{Required: &corev1.NodeSelector{
				NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
					{Key: "kubernetes.io/hostname", Operator: corev1.NodeSelectorOpIn, Values: []string{node}},
				}}},
			}},
		},
	}
	if local {
		pv.Spec.Local = &corev1.LocalVolumeSource{Path: "/var/lib/rancher/k3s/storage/" + name}
	} else {
		pv.Spec.HostPath = &corev1.HostPathVolumeSource{Path: "/var/lib/rancher/k3s/storage/" + name}
	}
	return pv
}

func TestPlacementReadsTheNodeOfALocalVolume(t *testing.T) {
	for name, local := range map[string]bool{"local volume": true, "hostPath volume": false} {
		t.Run(name, func(t *testing.T) {
			client := fake.NewSimpleClientset( //nolint:staticcheck
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-0", Namespace: "shop"}, Spec: corev1.PodSpec{NodeName: "server-1"}},
				&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-db-0", Namespace: "shop"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-local"}},
				localPathPV("pvc-local", "server-1", local),
			)
			p, err := (&Manager{Client: client}).Placement(context.Background(), "shop", "db")
			require.NoError(t, err)
			assert.Equal(t, []string{"server-1"}, p.DataNodes)
		})
	}
}
