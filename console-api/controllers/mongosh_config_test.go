package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"
)

func mongoReconciler(t *testing.T) (*ServiceReconciler, func() appsv1.StatefulSet) {
	t.Helper()
	svc := bareService("mongodb")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	get := func() appsv1.StatefulSet {
		var sts appsv1.StatefulSet
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
		return sts
	}
	return r, get
}

func TestReconcileStatefulSet_MongoDBSilencesMongosh(t *testing.T) {
	r, get := mongoReconciler(t)
	svc := bareService("mongodb")
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	var cm corev1.ConfigMap
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-mongosh"}, &cm))
	var conf struct {
		Mongosh map[string]interface{} `json:"mongosh"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(cm.Data["mongosh.conf"]), &conf))
	assert.Equal(t, true, conf.Mongosh["forceDisableTelemetry"])
	assert.Equal(t, "", conf.Mongosh["snippetIndexSourceURLs"])
	require.Len(t, cm.OwnerReferences, 1, "the config goes when the service does")

	sts := get()
	assert.Contains(t, sts.Spec.Template.Spec.Containers[0].VolumeMounts,
		corev1.VolumeMount{Name: "mongosh-config", MountPath: "/etc/mongosh.conf", SubPath: "mongosh.conf", ReadOnly: true})
	require.Len(t, sts.Spec.Template.Spec.Volumes, 1)
	assert.Equal(t, "db-mongosh", sts.Spec.Template.Spec.Volumes[0].ConfigMap.Name)
}

// Reconciliation adds the config volume while preserving unrelated volumes.
func TestReconcileStatefulSet_MongoDBConfigReachesAnExistingService(t *testing.T) {
	r, get := mongoReconciler(t)
	svc := bareService("mongodb")
	ctx := context.Background()
	require.NoError(t, r.reconcileStatefulSet(ctx, svc))

	sts := get()
	sts.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: "extra", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
	require.NoError(t, r.Update(ctx, &sts))

	require.NoError(t, r.reconcileStatefulSet(ctx, svc))
	names := []string{}
	for _, v := range get().Spec.Template.Spec.Volumes {
		names = append(names, v.Name)
	}
	assert.ElementsMatch(t, []string{"extra", "mongosh-config"}, names)
}

func TestReconcileStatefulSet_OtherServicesGetNoMongoshConfig(t *testing.T) {
	svc := bareService("postgres")
	r := &ServiceReconciler{
		Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc).Build(),
		Scheme: testScheme(),
	}
	require.NoError(t, r.reconcileStatefulSet(context.Background(), svc))

	var sts appsv1.StatefulSet
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db"}, &sts))
	assert.Empty(t, sts.Spec.Template.Spec.Volumes)
}

func TestReconcileMongoshConfig_RefusesAConfigMapTheServiceDoesNotControl(t *testing.T) {
	controller := true
	for name, meta := range map[string]metav1.ObjectMeta{
		"unlabelled":         {},
		"labelled ownerless": {Labels: map[string]string{kipperLabel: kipperValue}},
		"another controller": {OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Service", Name: "other", UID: "uid-other", Controller: &controller}}},
	} {
		t.Run(name, func(t *testing.T) {
			svc := bareService("mongodb")
			meta.Name, meta.Namespace = "db-mongosh", "ns"
			foreign := &corev1.ConfigMap{ObjectMeta: meta, Data: map[string]string{"settings": "theirs"}}
			r := &ServiceReconciler{
				Client: crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(svc, foreign).Build(),
				Scheme: testScheme(),
			}

			assert.Error(t, r.reconcileMongoshConfig(context.Background(), svc))
			var cm corev1.ConfigMap
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "db-mongosh"}, &cm))
			assert.Equal(t, map[string]string{"settings": "theirs"}, cm.Data)
		})
	}
}
