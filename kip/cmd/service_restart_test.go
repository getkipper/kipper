package cmd

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/getkipper/kipper/kip/internal/manifest"
)

// serviceCRs builds a fake cluster holding one Service CR per entry. A key may
// carry an "@" suffix so the same service name can appear in two namespaces.
func serviceCRs(t *testing.T, services map[string]string) *dynamicfake.FakeDynamicClient {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "Service"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "kipper.run", Version: "v1alpha1", Kind: "ServiceList"},
		&unstructured.UnstructuredList{},
	)
	dyn := dynamicfake.NewSimpleDynamicClient(scheme)

	for key, namespace := range services {
		name, _, _ := strings.Cut(key, "@")
		cr := &unstructured.Unstructured{Object: map[string]interface{}{
			"apiVersion": "kipper.run/v1alpha1",
			"kind":       "Service",
			"metadata":   map[string]interface{}{"name": name, "namespace": namespace},
		}}
		_, err := dyn.Resource(manifest.ServiceGVR).Namespace(namespace).
			Create(context.Background(), cr, metav1.CreateOptions{})
		require.NoError(t, err)
	}
	return dyn
}

// An alert names a namespace, and the recovery command has to be able to reach
// it. Without these flags the command works only against whatever project
// happens to be persisted, which for a fresh cluster is none at all: the
// namespace resolves empty and the Service CR is never found.
func TestServiceRestartTakesTheSameTargetingFlagsAsTheRestOfServiceCommands(t *testing.T) {
	for _, name := range []string{"project", "environment"} {
		f := serviceRestartCmd.Flags().Lookup(name)
		require.NotNil(t, f, "kip service restart must accept --%s, like every other service command", name)
	}

	assert.Equal(t, "default", serviceRestartCmd.Flags().Lookup("project").DefValue,
		"a cluster with no persisted project must still restart a service in default")
}

// An alert names a namespace, and the operator running the command it prints
// should not have to work out which project that namespace belongs to. `kip app
// restart` already finds its workload by name across the cluster; the recovery
// command for a service does the same when no project was named.
func TestServiceRestartFindsTheServiceWhenNoProjectWasNamed(t *testing.T) {
	dyn := serviceCRs(t, map[string]string{"db": "shop-test"})

	ns, err := findServiceNamespace(context.Background(), dyn, "db")
	require.NoError(t, err)
	assert.Equal(t, "shop-test", ns)
}

// Two projects can each run a service called db. Guessing between them would
// restart the wrong database, so the command has to say so and stop.
func TestServiceRestartRefusesAnAmbiguousName(t *testing.T) {
	dyn := serviceCRs(t, map[string]string{"db": "shop-test", "db@2": "shop-prod"})

	_, err := findServiceNamespace(context.Background(), dyn, "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shop-prod")
	assert.Contains(t, err.Error(), "shop-test")
	assert.Contains(t, err.Error(), "--project", "the error has to name the way out")
}

func TestServiceRestartSaysWhenThereIsNoSuchService(t *testing.T) {
	dyn := serviceCRs(t, nil)

	_, err := findServiceNamespace(context.Background(), dyn, "db")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "db")
}

// The Service controller only learned to project the restart stamp onto the
// StatefulSet's pod template in this branch. Against a cluster running a
// released console-api the annotation lands on the CR, the write succeeds, and
// nothing whatsoever happens — while the command says the pod was recreated.
//
// That is a false green at the moment of recovery, in work whose entire purpose
// is ending a false green. The alerts and the service listing both point an
// operator here, so it has to say plainly when the cluster did not act.

// stampReader is a cluster that reports whatever stamp the test says.
func stampReader(stamp string) templateStamp {
	return func(context.Context) (string, error) { return stamp, nil }
}

func TestConfirmRestartAcceptsAClusterThatActed(t *testing.T) {
	require.NoError(t, confirmRestart(context.Background(),
		stampReader("2026-09-06T09:00:00Z"), "db", "2026-09-06T09:00:00Z", time.Second))
}

func TestConfirmRestartRefusesToClaimSuccessOnAClusterThatDidNot(t *testing.T) {
	// The stamp reached the CR; the reconciler never copied it to the template.
	err := confirmRestart(context.Background(), stampReader(""), "db", "2026-09-06T09:00:00Z", 300*time.Millisecond)

	require.Error(t, err, "the pod was not recreated, so the command must not say it was")
	assert.Contains(t, err.Error(), "kip upgrade",
		"a console-api predating this is the likely cause, so name the fix")
	assert.Contains(t, strings.ToLower(err.Error()), "did not reach",
		"say what was observed rather than asserting why")
	assert.Contains(t, err.Error(), "busy controller looks the same",
		"a busy reconciler produces this same timeout, so the cause must not be asserted")
}

// A stamp from an earlier restart is not this restart.
func TestConfirmRestartIgnoresAnOlderStamp(t *testing.T) {
	err := confirmRestart(context.Background(),
		stampReader("2026-09-05T08:00:00Z"), "db", "2026-09-06T09:00:00Z", 300*time.Millisecond)
	require.Error(t, err)
}

// A cluster that acts a moment later still counts: the reconciler is not
// instantaneous, and failing on the first read would cry wolf on every restart.
func TestConfirmRestartWaitsForTheReconciler(t *testing.T) {
	const stamp = "2026-09-06T09:00:00Z"

	var mu sync.Mutex
	current := ""
	go func() {
		time.Sleep(150 * time.Millisecond)
		mu.Lock()
		current = stamp
		mu.Unlock()
	}()

	read := func(context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	}

	require.NoError(t, confirmRestart(context.Background(), read, "db", stamp, 5*time.Second))
}

// A read that fails throughout the window is a different problem from a cluster
// that did not act, and must not be reported as one.
func TestConfirmRestartSaysWhenItCouldNotLook(t *testing.T) {
	read := func(context.Context) (string, error) {
		return "", errors.New("statefulsets.apps \"db\" is forbidden")
	}

	err := confirmRestart(context.Background(), read, "db", "2026-09-06T09:00:00Z", 300*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not read")
	assert.NotContains(t, err.Error(), "kip upgrade",
		"an unreadable workload is not evidence of an old console-api")
}

// The wait bounded the sleep between polls and not the polls themselves, so an
// API server that accepts the connection and then stops answering held the
// recovery command open indefinitely — the same unbounded-network-call shape
// this branch removed from the mail transport, on the path an operator reaches
// for during an incident.
func TestConfirmRestartGivesUpOnAnApiServerThatStopsAnswering(t *testing.T) {
	// A read that returns only when its own context does.
	read := func(ctx context.Context) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}

	done := make(chan error, 1)
	go func() {
		done <- confirmRestart(context.Background(), read, "db", "stamp", 300*time.Millisecond)
	}()

	select {
	case err := <-done:
		require.Error(t, err, "the read never answered, so the restart is unconfirmed")
	case <-time.After(10 * time.Second):
		t.Fatal("the confirmation is not bounded: it waited well past its own deadline")
	}
}

// The production reader looks in the namespace and at the name it was given.
func TestStatefulSetStampReadsTheRightWorkload(t *testing.T) {
	client := fake.NewSimpleClientset( //nolint:staticcheck
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-test"},
			Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"kipper.run/restartedAt": "mine"}},
			}},
		},
		&appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-prod"},
			Spec: appsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"kipper.run/restartedAt": "another project's"}},
			}},
		},
	)

	got, err := statefulSetStamp(client, "shop-test", "db")(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "mine", got)

	_, err = statefulSetStamp(client, "shop-test", "cache")(context.Background())
	require.Error(t, err, "a service with no workload behind it cannot be confirmed")
}
