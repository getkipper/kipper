package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sfake "k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func serviceWithCondition(name, reason, message string) *kipperv1.Service {
	return &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop-prod"},
		Spec:       kipperv1.ServiceSpec{Type: "postgres", Storage: "5Gi"},
		Status: kipperv1.ServiceStatus{
			Phase: "Failed",
			Conditions: []metav1.Condition{{
				Type: kipperv1.ConditionCredentialsReady, Status: metav1.ConditionFalse,
				Reason: reason, Message: message, LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

// The console shows a phase and nothing else, so a service the reconciler
// refused reads as "failed" with no way to find out why. The remedy is written
// onto the object for exactly this.
func TestServicesList_CarriesWhyAServiceIsBlocked(t *testing.T) {
	handler := &Services{
		Client: k8sfake.NewClientset(),
		CRClient: testCRClient(
			serviceWithCondition("db", "DataWithoutCredentials", "service db has data in data-db-0 and no PASSWORD in db-credentials"),
		),
	}

	rec := httptest.NewRecorder()
	handler.List(rec, httptest.NewRequest(http.MethodGet, "/services?namespace=shop-prod", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "DataWithoutCredentials", got[0]["blockedReason"])
	assert.Contains(t, got[0]["blockedMessage"], "data-db-0")
}

// A healthy service carries no condition, and neither does any service on a
// cluster older than it. The fields stay out of the response entirely.
func TestServicesList_SaysNothingAboutAHealthyService(t *testing.T) {
	healthy := &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "cache", Namespace: "shop-prod"},
		Spec:       kipperv1.ServiceSpec{Type: "redis"},
		Status:     kipperv1.ServiceStatus{Phase: "Running"},
	}
	handler := &Services{Client: k8sfake.NewClientset(), CRClient: testCRClient(healthy)}

	rec := httptest.NewRecorder()
	handler.List(rec, httptest.NewRequest(http.MethodGet, "/services?namespace=shop-prod", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.NotContains(t, got[0], "blockedReason")
	assert.NotContains(t, got[0], "blockedMessage")
}

// A condition the reconciler has retracted, or one somebody else set true, is
// not a blockage.
func TestServicesList_IgnoresACredentialsConditionThatIsTrue(t *testing.T) {
	cleared := serviceWithCondition("db", "DataWithoutCredentials", "cleared since")
	cleared.Status.Conditions[0].Status = metav1.ConditionTrue
	handler := &Services{Client: k8sfake.NewClientset(), CRClient: testCRClient(cleared)}

	rec := httptest.NewRecorder()
	handler.List(rec, httptest.NewRequest(http.MethodGet, "/services?namespace=shop-prod", nil))

	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.NotContains(t, got[0], "blockedReason")
}

// The CRD puts no uniqueness on a condition's type, so two can sit on one
// object. Stopping at the first would let a stale true one hide a live refusal.
func TestServicesList_FindsABlockageBehindAStaleCondition(t *testing.T) {
	svc := serviceWithCondition("db", "DataWithoutCredentials", "db has data in data-db-0 and no PASSWORD")
	svc.Status.Conditions = append([]metav1.Condition{{
		Type: kipperv1.ConditionCredentialsReady, Status: metav1.ConditionTrue,
		Reason: "Cleared", Message: "left behind by an edit", LastTransitionTime: metav1.Now(),
	}}, svc.Status.Conditions...)
	handler := &Services{Client: k8sfake.NewClientset(), CRClient: testCRClient(svc)}

	rec := httptest.NewRecorder()
	handler.List(rec, httptest.NewRequest(http.MethodGet, "/services?namespace=shop-prod", nil))

	var got []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got, 1)
	assert.Equal(t, "DataWithoutCredentials", got[0]["blockedReason"],
		"a stale condition ahead of the live one hid the refusal")
}
