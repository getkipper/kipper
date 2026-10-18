package cmd

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/getkipper/kipper/kip/internal/manifest"
	"github.com/getkipper/kipper/kip/internal/service"
)

func TestServiceTLSOnCreate(t *testing.T) {
	cases := []struct {
		svcType, flag, want string
		wantErr             bool
	}{
		{"postgres", "", "on", false},
		{"postgres", "off", "off", false},
		{"postgres", "on", "on", false},
		{"postgres", "required", "", true},
		{"redis", "", "", false},
		{"mysql", "on", "", true},
	}
	for _, c := range cases {
		got, err := serviceTLSOnCreate(c.svcType, c.flag)
		if c.wantErr {
			assert.Error(t, err, "%+v", c)
			continue
		}
		assert.NoError(t, err, "%+v", c)
		assert.Equal(t, c.want, got, "%+v", c)
	}
}

func TestSetServiceTLSPatchesOnlyPostgres(t *testing.T) {
	dyn := serviceCRs(t, map[string]string{"db": "shop"})
	ctx := context.Background()
	db, err := dyn.Resource(manifest.ServiceGVR).Namespace("shop").Get(ctx, "db", metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, unstructured.SetNestedField(db.Object, "postgres", "spec", "type"))
	_, err = dyn.Resource(manifest.ServiceGVR).Namespace("shop").Update(ctx, db, metav1.UpdateOptions{})
	require.NoError(t, err)

	changed, err := setServiceTLS(ctx, dyn, "shop", "db", "on")
	require.NoError(t, err)
	assert.True(t, changed)
	changed, err = setServiceTLS(ctx, dyn, "shop", "db", "on")
	require.NoError(t, err)
	assert.False(t, changed, "setting the value it has restarts nothing, so kip must not say it does")

	db, err = dyn.Resource(manifest.ServiceGVR).Namespace("shop").Get(ctx, "db", metav1.GetOptions{})
	require.NoError(t, err)
	tls, _, _ := unstructured.NestedString(db.Object, "spec", "tls")
	assert.Equal(t, "on", tls)

	cache := serviceCRs(t, map[string]string{"cache": "shop"})
	_, err = setServiceTLS(ctx, cache, "shop", "cache", "on")
	assert.Error(t, err, "only Postgres serves TLS so far")
}

func TestDescribeServiceTLS(t *testing.T) {
	assert.Equal(t, "off", describeServiceTLS(service.Snapshot{}))
	assert.Equal(t, "on", describeServiceTLS(service.Snapshot{TLS: "on", TLSServing: true}))
	assert.Contains(t, describeServiceTLS(service.Snapshot{TLS: "on"}), "not served yet", "no report is not proof of TLS")
	assert.Equal(t, "on, not served yet: waiting for cert-manager",
		describeServiceTLS(service.Snapshot{TLS: "on", TLSWaiting: "waiting for cert-manager"}))
}
