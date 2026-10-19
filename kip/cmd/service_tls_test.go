package cmd

import (
	"context"
	"errors"
	"io"
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
		{"postgres", "", "required", false},
		{"postgres", "off", "off", false},
		{"postgres", "on", "on", false},
		{"postgres", "required", "required", false},
		{"postgres", "strict", "", true},
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
	assert.Equal(t, "on (connections without TLS are still accepted)", describeServiceTLS(service.Snapshot{TLS: "on", TLSServing: true}))
	assert.Equal(t, "required (connections without TLS are refused)", describeServiceTLS(service.Snapshot{TLS: "required", TLSServing: true}))
	assert.Contains(t, describeServiceTLS(service.Snapshot{TLS: "required"}), "required, not served yet")
	assert.Contains(t, describeServiceTLS(service.Snapshot{TLS: "on"}), "not served yet", "no report is not proof of TLS")
	assert.Equal(t, "on, not served yet: waiting for cert-manager",
		describeServiceTLS(service.Snapshot{TLS: "on", TLSWaiting: "waiting for cert-manager"}))
}

func fakePsql(out string, err error) func() (podExecFunc, error) {
	return func() (podExecFunc, error) {
		return func(_ context.Context, command []string, _ io.Reader, stdout, stderr io.Writer) error {
			if command[len(command)-1] != plaintextClientsQuery {
				return errors.New("unexpected command")
			}
			_, _ = io.WriteString(stdout, out)
			if err != nil {
				_, _ = io.WriteString(stderr, "psql: could not connect")
			}
			return err
		}, nil
	}
}

func TestRequiringTLSNeedsTLSToBeServedFirst(t *testing.T) {
	ctx := context.Background()
	for _, s := range []service.Snapshot{{}, {TLS: "off"}, {TLS: "on"}} {
		err := requireTLSGate(ctx, s, "db", fakePsql("", nil), true)
		assert.ErrorContains(t, err, "--tls on", "--force must not skip the TLS readiness check: %+v", s)
	}
}

func TestRequiringTLSRefusesWhilePlaintextClientsAreConnected(t *testing.T) {
	ctx := context.Background()
	serving := service.Snapshot{TLS: "on", TLSServing: true}

	err := requireTLSGate(ctx, serving, "db", fakePsql("10.42.0.7 api kipper\n", nil), false)
	assert.ErrorContains(t, err, "10.42.0.7 api kipper")

	assert.NoError(t, requireTLSGate(ctx, serving, "db", fakePsql("10.42.0.7 api kipper\n", nil), true), "--force switches anyway")
	assert.NoError(t, requireTLSGate(ctx, serving, "db", fakePsql("", nil), false))
}

func TestAFailedClientCheckNeverPassesTheGate(t *testing.T) {
	serving := service.Snapshot{TLS: "on", TLSServing: true}
	err := requireTLSGate(context.Background(), serving, "db", fakePsql("", errors.New("exit 2")), true)
	assert.ErrorContains(t, err, "could not connect")
}

func TestStatusListsPostgresThatAcceptsPlaintext(t *testing.T) {
	svc := func(ns, name, typ, tls string) unstructured.Unstructured {
		u := unstructured.Unstructured{Object: map[string]interface{}{"spec": map[string]interface{}{"type": typ, "tls": tls}}}
		u.SetNamespace(ns)
		u.SetName(name)
		return u
	}
	confirmed := svc("shop", "orders", "postgres", "required")
	confirmed.SetGeneration(3)
	confirmed.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
		"type": "TLSReady", "status": "True", "reason": "Serving", "observedGeneration": int64(3)}}}
	switched := svc("shop", "billing", "postgres", "required")
	switched.SetGeneration(4)
	switched.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
		"type": "TLSReady", "status": "True", "reason": "Serving", "observedGeneration": int64(3)}}}
	open := plaintextPostgres([]unstructured.Unstructured{
		confirmed,
		switched,
		svc("shop", "legacy", "postgres", ""),
		svc("blog", "db", "postgres", "on"),
		svc("blog", "cache", "redis", ""),
	})
	assert.Equal(t, []string{"blog/db (tls on)", "shop/billing (tls required, not confirmed yet)", "shop/legacy (tls off)"}, open,
		"a Serving condition from before the switch says nothing about the new mode")
}
