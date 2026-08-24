package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/controller/pkg/servicecatalog"
	"github.com/getkipper/kipper/kip/internal/manifest"

	"github.com/getkipper/kipper/kip/internal/service"
)

// The remedy the reconciler writes onto a refused service is the whole point of
// writing it. A table of phases says "failed" and nothing an operator can act
// on, so the message goes underneath in full.
func TestServiceListReportsWhyAServiceIsBlocked(t *testing.T) {
	var out bytes.Buffer

	writeServiceList(&out, []service.Status{
		{Name: "cache", Type: "redis", Status: "running", Ready: "1/1", Storage: "1Gi"},
		{
			Name: "db", Type: "postgres", Status: "failed", Ready: "0/1", Storage: "5Gi",
			BlockedReason:  "DataWithoutCredentials",
			BlockedMessage: "service db has data in data-db-0 and no PASSWORD in db-credentials; restore db-credentials from a backup",
		},
	})

	printed := out.String()
	assert.Contains(t, printed, "db", "the table still lists every service")
	assert.Contains(t, printed, "DataWithoutCredentials", "the reason is what an operator searches for")
	assert.Contains(t, printed, "restore db-credentials from a backup", "the remedy is the part they act on")
}

// Two of them each get their own line, so a namespace that lost its Secrets in
// one restore reads as a list rather than a sentence about the first one.
func TestServiceListReportsEveryBlockedService(t *testing.T) {
	var out bytes.Buffer

	writeServiceList(&out, []service.Status{
		{Name: "db", Type: "postgres", Status: "failed", BlockedReason: "DataWithoutCredentials", BlockedMessage: "db has data and no password"},
		{Name: "queue", Type: "rabbitmq", Status: "failed", BlockedReason: "SecretNotOwned", BlockedMessage: "queue-credentials belongs to something else"},
	})

	printed := out.String()
	assert.Contains(t, printed, "db has data and no password")
	assert.Contains(t, printed, "queue-credentials belongs to something else")
	assert.Equal(t, 2, strings.Count(printed, "  !   "), "each blocked service gets its own advisory line")
}

// Nothing blocked, nothing said. Every cluster older than the condition lists
// services that carry none, and a healthy one carries none either.
func TestServiceListSaysNothingWhenNothingIsBlocked(t *testing.T) {
	var out bytes.Buffer

	writeServiceList(&out, []service.Status{
		{Name: "cache", Type: "redis", Status: "running", Ready: "1/1", Storage: "1Gi"},
	})

	assert.NotContains(t, out.String(), "  !   ", "a healthy namespace was given a warning block")
}

// Info on a refused service leads with the refusal. The connection details under
// it are the ones that will not work, and reading them first sends an operator
// looking for a network problem.
func TestServiceInfoLeadsWithTheBlockage(t *testing.T) {
	var out bytes.Buffer

	writeBlockedNotice(&out, "db", "SecretNotOwned", "secret db-credentials is not owned by this service")

	printed := out.String()
	assert.Contains(t, printed, "SecretNotOwned")
	assert.Contains(t, printed, "not owned by this service")
	assert.Contains(t, printed, "  !   ", "the house mark for something an operator has to act on")
}

func TestServiceInfoSaysNothingWhenNothingIsBlocked(t *testing.T) {
	var out bytes.Buffer

	writeBlockedNotice(&out, "db", "", "")

	assert.Empty(t, out.String(), "a healthy service was given a warning")
}

func blockedServiceCR(name, reason, message string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
		"spec":       map[string]interface{}{"type": "postgres"},
		"status": map[string]interface{}{
			"phase": "Failed",
			"conditions": []interface{}{map[string]interface{}{
				"type":    servicecatalog.ConditionCredentialsReady,
				"status":  "False",
				"reason":  reason,
				"message": message,
			}},
		},
	}}
}

// The case this whole feature exists for: the credentials Secret is gone, so
// reading connection details out of it fails with "not found". Reporting the
// refusal has to come instead of that, or the one command an operator runs on a
// broken service buries the remedy under an error about a missing object.
func TestServiceInfoReportsTheRefusalWhenTheSecretIsGone(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"},
		blockedServiceCR("db", "DataWithoutCredentials",
			"service db has data in data-db-0 and no PASSWORD in db-credentials"),
	)
	// No Secret anywhere, which is exactly what DataWithoutCredentials means.
	mgr := &service.Manager{Client: k8sfake.NewSimpleClientset(), Dynamic: dyn} //nolint:staticcheck

	var out bytes.Buffer
	err := writeServiceInfo(context.Background(), &out, mgr, "default", "db")

	require.NoError(t, err, "the command failed on the missing Secret instead of explaining it")
	printed := out.String()
	assert.Contains(t, printed, "DataWithoutCredentials")
	assert.Contains(t, printed, "data-db-0")
	assert.NotContains(t, printed, "Host:", "connection details the engine will not accept must not be offered")
}

func healthyServiceCR(name, svcType string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]interface{}{
		"apiVersion": "kipper.run/v1alpha1",
		"kind":       "Service",
		"metadata":   map[string]interface{}{"name": name, "namespace": "default"},
		"spec":       map[string]interface{}{"type": svcType},
		"status":     map[string]interface{}{"phase": "Running"},
	}}
}

// A service with nothing wrong prints what it always printed, and the shape it
// prints comes from the type in the same read that found nothing wrong. MinIO is
// the type that proves it: an empty type would take the host-and-port branch
// instead of the S3 one.
func TestServiceInfoPrintsTheDetailsOfAHealthyService(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"},
		healthyServiceCR("obj", "minio"),
	)
	secrets := k8sfake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "obj-credentials", Namespace: "default"},
		Data: map[string][]byte{
			"ENDPOINT":   []byte("http://obj.default.svc.cluster.local:9000"),
			"ACCESS_KEY": []byte("kipper"),
			"SECRET_KEY": []byte("s3cret"),
		},
	})
	mgr := &service.Manager{Client: secrets, Dynamic: dyn}

	var out bytes.Buffer
	require.NoError(t, writeServiceInfo(context.Background(), &out, mgr, "default", "obj"))

	printed := out.String()
	assert.Contains(t, printed, "Endpoint:", "the S3 shape needs the type the snapshot carried")
	assert.Contains(t, printed, "obj.default.svc.cluster.local")
	assert.NotContains(t, printed, "  !   ", "a healthy service was given a warning")
}

// The one-read rule is the whole point of the snapshot, and it is invisible in
// the output: two reads print exactly what one prints, right up until the
// condition changes between them and the command describes a service the
// reconciler has already refused. Counting is the only way to see it.
func TestServiceInfoReadsTheServiceOnce(t *testing.T) {
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(
		runtime.NewScheme(),
		map[schema.GroupVersionResource]string{manifest.ServiceGVR: "ServiceList"},
		healthyServiceCR("db", "postgres"),
	)
	secrets := k8sfake.NewSimpleClientset(&corev1.Secret{ //nolint:staticcheck
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "default"},
		Data: map[string][]byte{
			"HOST": []byte("db.default.svc.cluster.local"), "PORT": []byte("5432"),
			"USERNAME": []byte("kipper"), "PASSWORD": []byte("s3cret"), "NAME": []byte("app"),
		},
	})
	mgr := &service.Manager{Client: secrets, Dynamic: dyn}

	var out bytes.Buffer
	require.NoError(t, writeServiceInfo(context.Background(), &out, mgr, "default", "db"))

	reads := 0
	for _, action := range dyn.Actions() {
		if action.Matches("get", "services") {
			reads++
		}
	}
	assert.Equal(t, 1, reads,
		"the blockage and the type must come from one look at the service, or they can disagree")
}
