package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	authnv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/kip/internal/installer"
)

func nodeRecordsConfigMap(t *testing.T, records ...installer.NodeRecord) *corev1.ConfigMap {
	data := map[string]string{}
	for _, r := range records {
		body, err := json.Marshal(r)
		require.NoError(t, err)
		data[r.MachineID] = string(body)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Namespace: installer.NodeRecordsNamespace, Name: installer.NodeRecordsConfigMap},
		Data:       data,
	}
}

func recordFor(name, role, state string) installer.NodeRecord {
	sum := sha256.Sum256([]byte(name))
	id := fmt.Sprintf("%x", sum[:16])
	return installer.NodeRecord{Name: name, Host: "203.0.113.1" + name[len(name)-1:], MachineID: id, Role: role, State: state}
}

func k3sNode(name, version string, server bool) *corev1.Node {
	n := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{}},
		Status: corev1.NodeStatus{NodeInfo: corev1.NodeSystemInfo{
			KubeletVersion: version,
			MachineID:      recordFor(name, "", "").MachineID,
		}},
	}
	if server {
		n.Labels["node-role.kubernetes.io/control-plane"] = "true"
	}
	return n
}

func TestCheckK3sUpgradeReportsTheServerMove(t *testing.T) {
	pin := installer.PinnedK3sVersion()
	cs := fake.NewClientset(k3sNode("server-1", "v1.35.4+k3s1", true), k3sNode("worker-1", "v1.35.4+k3s1", false),
		nodeRecordsConfigMap(t, recordFor("server-1", installer.NodeRoleServer, installer.NodeStateReady),
			recordFor("worker-1", installer.NodeRoleWorker, installer.NodeStateReady)),
	)
	var out bytes.Buffer

	require.NoError(t, checkK3sUpgrade(context.Background(), cs, &out))

	assert.Contains(t, out.String(), "moves the server from v1.35.4+k3s1 to "+pin)
	assert.Contains(t, out.String(), "then 1 worker")
}

func TestCheckK3sUpgradeReportsAServerOnThePin(t *testing.T) {
	pin := installer.PinnedK3sVersion()
	cs := fake.NewClientset(k3sNode("server-1", pin, true), k3sNode("worker-1", "v1.35.4+k3s1", false),
		nodeRecordsConfigMap(t, recordFor("server-1", installer.NodeRoleServer, installer.NodeStateReady),
			recordFor("worker-1", installer.NodeRoleWorker, installer.NodeStateReady)),
	)
	var out bytes.Buffer

	require.NoError(t, checkK3sUpgrade(context.Background(), cs, &out))

	assert.Contains(t, out.String(), "the server already runs "+pin)
	assert.Contains(t, out.String(), "brings 1 worker")
}

func TestCheckK3sUpgradeFailsWhenTheServerCannotMove(t *testing.T) {
	cs := fake.NewClientset(k3sNode("server-1", "v1.34.9+k3s1", true))
	var out bytes.Buffer

	err := checkK3sUpgrade(context.Background(), cs, &out)

	require.Error(t, err)
	assert.Contains(t, out.String(), "more than one Kubernetes minor")
}

func TestCheckK3sUpgradeNeedsAServerNode(t *testing.T) {
	cs := fake.NewClientset(k3sNode("worker-1", "v1.35.4+k3s1", false))

	err := checkK3sUpgrade(context.Background(), cs, &bytes.Buffer{})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "server node")
}

func TestUpgradeCommandOffersK3s(t *testing.T) {
	flag := upgradeCmd.Flags().Lookup("k3s")
	require.NotNil(t, flag)
	assert.Equal(t, "false", flag.DefValue)
}

func TestCheckK3sUpgradeFailsOnAWorkerTheUpgradeWouldRefuse(t *testing.T) {
	cs := fake.NewClientset(
		k3sNode("server-1", "v1.35.4+k3s1", true), k3sNode("worker-1", "v1.32.9+k3s1", false),
		nodeRecordsConfigMap(t, recordFor("server-1", installer.NodeRoleServer, installer.NodeStateReady),
			recordFor("worker-1", installer.NodeRoleWorker, installer.NodeStateReady)),
	)
	var out bytes.Buffer

	err := checkK3sUpgrade(context.Background(), cs, &out)

	require.Error(t, err)
	assert.Contains(t, out.String(), "worker-1")
}

func TestCheckK3sUpgradeCountsOnlyWorkersTheUpgradeMoves(t *testing.T) {
	pin := installer.PinnedK3sVersion()
	cs := fake.NewClientset(
		k3sNode("server-1", pin, true), k3sNode("worker-1", "v1.35.4+k3s1", false), k3sNode("worker-2", "v1.35.4+k3s1", false),
		nodeRecordsConfigMap(t, recordFor("server-1", installer.NodeRoleServer, installer.NodeStateReady),
			recordFor("worker-1", installer.NodeRoleWorker, installer.NodeStateReady),
			recordFor("worker-2", installer.NodeRoleWorker, installer.NodeStateRemoving)),
	)
	var out bytes.Buffer

	require.NoError(t, checkK3sUpgrade(context.Background(), cs, &out))

	assert.Contains(t, out.String(), "brings 1 worker")
}

func TestCheckK3sUpgradeFailsOnAnUnrecordedWorker(t *testing.T) {
	pin := installer.PinnedK3sVersion()
	cs := fake.NewClientset(
		k3sNode("server-1", pin, true), k3sNode("worker-9", "v1.35.4+k3s1", false),
		nodeRecordsConfigMap(t, recordFor("server-1", installer.NodeRoleServer, installer.NodeStateReady)),
	)
	var out bytes.Buffer

	err := checkK3sUpgrade(context.Background(), cs, &out)

	require.Error(t, err)
	assert.Contains(t, out.String(), "worker-9")
	assert.Contains(t, out.String(), "kip node add")
}

func TestCheckK3sUpgradeFailsOnAWorkerOnAnotherMachine(t *testing.T) {
	pin := installer.PinnedK3sVersion()
	worker := k3sNode("worker-1", pin, false)
	worker.Status.NodeInfo.MachineID = strings.Repeat("f", 32)
	cs := fake.NewClientset(
		k3sNode("server-1", pin, true), worker,
		nodeRecordsConfigMap(t, recordFor("server-1", installer.NodeRoleServer, installer.NodeStateReady),
			recordFor("worker-1", installer.NodeRoleWorker, installer.NodeStateReady)),
	)
	var out bytes.Buffer

	err := checkK3sUpgrade(context.Background(), cs, &out)

	require.Error(t, err, "kip upgrade refuses a Node whose machine is not the recorded one")
	assert.Contains(t, out.String(), "worker-1")
}

func TestCheckK3sUpgradeFailsOnAServerOnAnotherMachine(t *testing.T) {
	pin := installer.PinnedK3sVersion()
	server := k3sNode("server-1", pin, true)
	server.Status.NodeInfo.MachineID = strings.Repeat("c", 32)
	cs := fake.NewClientset(server,
		nodeRecordsConfigMap(t, recordFor("server-1", installer.NodeRoleServer, installer.NodeStateReady)))
	var out bytes.Buffer

	err := checkK3sUpgrade(context.Background(), cs, &out)

	require.Error(t, err, "kip upgrade refuses a server whose machine is not the recorded one")
	assert.Contains(t, out.String(), "server-1")
}

func TestCheckK3sUpgradeFailsOnAServerAndWorkerSharingAMachine(t *testing.T) {
	pin := installer.PinnedK3sVersion()
	worker := k3sNode("worker-1", pin, false)
	worker.Status.NodeInfo.MachineID = recordFor("server-1", "", "").MachineID
	cs := fake.NewClientset(k3sNode("server-1", pin, true), worker,
		nodeRecordsConfigMap(t, recordFor("server-1", installer.NodeRoleServer, installer.NodeStateReady)))

	require.Error(t, checkK3sUpgrade(context.Background(), cs, &bytes.Buffer{}))
}

func TestCheckK3sUpgradeAdoptsAnUnrecordedServer(t *testing.T) {
	cs := fake.NewClientset(k3sNode("server-1", "v1.35.4+k3s1", true))
	var out bytes.Buffer

	require.NoError(t, checkK3sUpgrade(context.Background(), cs, &out))
	assert.Contains(t, out.String(), "moves the server from v1.35.4+k3s1")
}

func selfReviewsAnswering(errs ...error) (*fake.Clientset, *int) {
	cs := fake.NewClientset()
	calls := 0
	cs.PrependReactor("create", "selfsubjectreviews", func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls <= len(errs) {
			return true, nil, errs[calls-1]
		}
		return true, &authnv1.SelfSubjectReview{}, nil
	})
	return cs, &calls
}

func TestWaitForAPIAuthenticationRetriesUntilTheCredentialsAreAccepted(t *testing.T) {
	unauthorized := apierrors.NewUnauthorized("oidc: authenticator not initialized")
	cs, calls := selfReviewsAnswering(unauthorized, unauthorized)

	require.NoError(t, waitForAPIAuthentication(context.Background(), cs, time.Second, time.Millisecond))

	assert.Equal(t, 3, *calls)
}

func TestWaitForAPIAuthenticationNamesTheRerunWhenTheCredentialsStayRejected(t *testing.T) {
	unauthorized := apierrors.NewUnauthorized("oidc: authenticator not initialized")
	errs := make([]error, 1000)
	for i := range errs {
		errs[i] = unauthorized
	}
	cs, _ := selfReviewsAnswering(errs...)

	err := waitForAPIAuthentication(context.Background(), cs, 20*time.Millisecond, time.Millisecond)

	require.Error(t, err)
	assert.Contains(t, err.Error(), k3sUpgradeCommand)
}

func TestWaitForAPIAuthenticationStopsAtAnErrorThatIsNotARejection(t *testing.T) {
	cs, calls := selfReviewsAnswering(apierrors.NewForbidden(schema.GroupResource{Resource: "selfsubjectreviews"}, "", errors.New("denied")))

	err := waitForAPIAuthentication(context.Background(), cs, time.Second, time.Millisecond)

	require.Error(t, err)
	assert.Equal(t, 1, *calls)
}
