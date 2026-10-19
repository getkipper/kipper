package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

func TestConsistentBackupsOnCreate(t *testing.T) {
	cases := []struct {
		svcType    string
		set, value bool
		want       bool
		wantErr    bool
	}{
		{"postgres", false, true, true, false},
		{"postgres", true, false, false, false},
		{"postgres", true, true, true, false},
		{"redis", false, true, false, false},
		{"mysql", true, true, false, true},
		{"mysql", true, false, false, false},
	}
	for _, c := range cases {
		got, err := consistentBackupsOnCreate(c.svcType, c.set, c.value)
		if c.wantErr {
			assert.Error(t, err, "%+v", c)
			continue
		}
		require.NoError(t, err, "%+v", c)
		assert.Equal(t, c.want, got, "%+v", c)
	}
}

func TestSetConsistentBackupsPatchesOnlyPostgres(t *testing.T) {
	ctx := context.Background()
	dyn := serviceCRs(t, map[string]string{"db": "shop"})
	db, err := dyn.Resource(manifest.ServiceGVR).Namespace("shop").Get(ctx, "db", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(db.Object, "postgres", "spec", "type"))
	_, err = dyn.Resource(manifest.ServiceGVR).Namespace("shop").Update(ctx, db, metav1.UpdateOptions{})
	require.NoError(t, err)

	changed, err := setConsistentBackups(ctx, dyn, "shop", "db", true)
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = setConsistentBackups(ctx, dyn, "shop", "db", true)
	require.NoError(t, err)
	assert.False(t, changed, "kip must not announce a restart that does not happen")

	cache := serviceCRs(t, map[string]string{"cache": "shop"})
	_, err = setConsistentBackups(ctx, cache, "shop", "cache", true)
	assert.Error(t, err)
}
