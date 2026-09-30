package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/usergrants"
)

func grantsCM(monitoring string) *corev1.ConfigMap {
	data := map[string]string{"users": kipUsers}
	if monitoring != "" {
		data["monitoring"] = monitoring
	}
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "kipper-users", Namespace: "kipper-system"}, Data: data}
}

func storedMonitoringKey(t *testing.T, cs *k8sfake.Clientset) string {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps("kipper-system").Get(context.Background(), "kipper-users", metav1.GetOptions{})
	require.NoError(t, err)
	return cm.Data["monitoring"]
}

const kipUsers = `{"admin@test.com":"admin","dev@test.com":"deployer","viewer@test.com":"viewer"}`

func TestSetMonitoring_GrantAndRevoke(t *testing.T) {
	cs := k8sfake.NewClientset(grantsCM(""))
	ctx := context.Background()

	require.NoError(t, setMonitoring(ctx, cs, "dev@test.com", true))
	assert.Equal(t, `["dev@test.com"]`, storedMonitoringKey(t, cs))

	require.NoError(t, setMonitoring(ctx, cs, "dev@test.com", false))
	assert.Equal(t, `[]`, storedMonitoringKey(t, cs))
}

func TestSetMonitoring_UnknownUserRefused(t *testing.T) {
	cs := k8sfake.NewClientset(grantsCM(""))

	err := setMonitoring(context.Background(), cs, "stranger@test.com", true)
	assert.True(t, errors.Is(err, usergrants.ErrUnknownUser), "err = %v", err)
}

func TestRemoveRole_DropsMonitoringGrant(t *testing.T) {
	cs := k8sfake.NewClientset(grantsCM(`["dev@test.com","viewer@test.com"]`))

	require.NoError(t, removeRole(context.Background(), cs, "dev@test.com"))
	assert.Equal(t, `["viewer@test.com"]`, storedMonitoringKey(t, cs))
}

func TestSetRole_NewUserStartsWithoutStaleGrant(t *testing.T) {
	cs := k8sfake.NewClientset(grantsCM(`["old@test.com","dev@test.com"]`))
	ctx := context.Background()

	require.NoError(t, setRole(ctx, cs, "old@test.com", "viewer"))
	require.NoError(t, setRole(ctx, cs, "dev@test.com", "viewer"))
	assert.Equal(t, `["dev@test.com"]`, storedMonitoringKey(t, cs), "a role change keeps the grant, a new user starts without one")
}

func TestMonitoringHolders(t *testing.T) {
	cs := k8sfake.NewClientset(grantsCM(`["dev@test.com","gone@test.com"]`))

	holders := monitoringHolders(context.Background(), cs)
	assert.True(t, holders["admin@test.com"])
	assert.True(t, holders["dev@test.com"])
	assert.False(t, holders["viewer@test.com"])
	assert.False(t, holders["gone@test.com"], "a grant without a role is not access")
}
