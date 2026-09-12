package handlers

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k8s.io/client-go/kubernetes/fake"

	"github.com/getkipper/kipper/console-api/middleware"
)

// An AILogs whose provider is unconfigured, so an admitted caller stops at the
// handler's own 400 rather than reaching a provider.
func unconfiguredAILogs() *AILogs {
	return &AILogs{Settings: &AISettings{Client: fake.NewClientset()}}
}

func analyseRequest(email, role, namespace string) *http.Request {
	body := `{"logs":"panic: nil map","app_name":"api","namespace":"` + namespace + `"}`
	req := httptest.NewRequest("POST", "/api/v1/ai/analyse-logs", strings.NewReader(body))
	ctx := context.WithValue(req.Context(), middleware.UserContextKey, &middleware.Claims{Email: email})
	ctx = context.WithValue(ctx, middleware.RoleContextKey, role)
	return req.WithContext(ctx)
}

// The Analyse button is promised to anyone who can read a workload's logs
// (docs/en/observability.md), and the console's project invite makes every
// project member a cluster viewer, so the cluster role refused the people the
// feature is for. Project standing is what admits them.
func TestAnalyseLogsAdmitsAProjectMemberWhoseClusterRoleIsViewer(t *testing.T) {
	withCollisionResolver(t, "viewer", "")
	h := unconfiguredAILogs()

	rec := httptest.NewRecorder()
	h.AnalyseLogs(rec, analyseRequest("dev@test.com", middleware.RoleViewer, shopNS))

	if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Fatalf("status = %d, want the caller admitted past the gate; body %s", rec.Code, rec.Body.String())
	}
}

// An account with standing nowhere is not admitted to an endpoint that spends
// the operator's AI provider budget. The diagnose routes established that a
// project viewer may spend it; they established nothing about an outsider.
func TestAnalyseLogsRefusesAnAccountWithNoStanding(t *testing.T) {
	withCollisionResolver(t, "", "")
	h := unconfiguredAILogs()

	rec := httptest.NewRecorder()
	h.AnalyseLogs(rec, analyseRequest("dev@test.com", middleware.RoleViewer, shopNS))

	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403; body %s", rec.Code, rec.Body.String())
	}
}

// Nobody admitted today loses anything: a cluster deployer keeps the route
// whether or not they belong to a project.
func TestAnalyseLogsKeepsAdmittingAClusterDeployer(t *testing.T) {
	withCollisionResolver(t, "", "")
	h := unconfiguredAILogs()

	rec := httptest.NewRecorder()
	h.AnalyseLogs(rec, analyseRequest("dev@test.com", middleware.RoleDeployer, shopNS))

	if rec.Code == http.StatusForbidden || rec.Code == http.StatusUnauthorized {
		t.Fatalf("status = %d, want a cluster deployer still admitted; body %s", rec.Code, rec.Body.String())
	}
}
