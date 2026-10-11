package handlers

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func unreadableCredentials() *GitCredentials {
	client := fake.NewClientset()
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("the server is currently unable to handle the request")
	})
	return &GitCredentials{Client: client, CRClient: testCRClient()}
}

// A read failure must be distinguishable from an empty credential list.
func TestGitCredentialsReportAReadTheyCouldNotMake(t *testing.T) {
	handler := unreadableCredentials()

	for name, serve := range map[string]http.HandlerFunc{"list": handler.List, "health": handler.Health} {
		rec := httptest.NewRecorder()
		serve(rec, httptest.NewRequest(http.MethodGet, "/settings/git-credentials", nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s: a credential list that could not be read must answer 500, got %d: %s", name, rec.Code, rec.Body)
		}
	}
}
