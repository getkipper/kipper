package migration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// MySQL data_length can underestimate the dump size, allowing an oversized
// dump past the initial size check. The sent byte count identifies transfers
// that exceeded the target's import limit.
func TestDumpOutgrewTargetOnlyAfterTheCapWasPassed(t *testing.T) {
	failed := errors.New("target returned 500: import failed")
	assert.True(t, dumpOutgrewTarget(maxDBImportBytes+1, failed))
	assert.False(t, dumpOutgrewTarget(maxDBImportBytes, failed), "a dump of exactly the cap fits")
	assert.False(t, dumpOutgrewTarget(10<<20, failed), "a small dump failed for some other reason")
	assert.True(t, dumpOutgrewTarget(maxDBImportBytes+1, nil), "a target that answered success still cut the dump at its cap")
	assert.True(t, dumpOutgrewTarget(10<<20, fmt.Errorf("target returned 413: %w", errDumpTooLarge)), "the target said so")
}

// TestReceiveDBImportRefusesADumpCutAtTheCap covers an import that exits
// successfully after a body read error closes stdin. The target must report
// HTTP 413 for the partial import.
func TestReceiveDBImportRefusesADumpCutAtTheCap(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "db-0", Namespace: "shop-prod", Labels: map[string]string{"app": "db"}}}
	var imported int64
	h := &Handler{
		Sessions: NewSessionStore(),
		Client:   fake.NewSimpleClientset(projectNamespace("shop-prod", "shop"), pod),
		CRClient: migrationOwners(t),
		execStream: func(_ context.Context, _, _, _ string, _ []string, stdin io.Reader, _ io.Writer) error {
			if stdin != nil {
				imported, _ = io.Copy(io.Discard, stdin)
			}
			return nil
		},
	}
	h.Sessions.Put(&Session{ID: "s1", Projects: []string{"shop"}})
	router := chi.NewRouter()
	router.Post("/{session}/db-import", h.ReceiveDBImportHandler)

	post := func(size int64) *httptest.ResponseRecorder {
		body := io.LimitReader(zeros{}, size)
		req := httptest.NewRequest(http.MethodPost, "/s1/db-import?service=db&namespace=shop-prod&type=mysql", body)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := post(maxDBImportBytes + 1)
	assert.Equal(t, http.StatusRequestEntityTooLarge, rec.Code, rec.Body.String())
	assert.Equal(t, int64(maxDBImportBytes), imported, "the import saw only the capped prefix")

	rec = post(1 << 20)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

func TestLeaveForManualStepsHandsTheDatabaseToTheManualSteps(t *testing.T) {
	h := &Handler{Client: fake.NewSimpleClientset()}
	svc := &kipperv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "orders-db", Namespace: "shop-prod"}, Spec: kipperv1.ServiceSpec{Type: "mysql"}}
	session := &Session{ID: "s1"}
	session.AddStep(Step{Name: "export", Status: StepRunning})
	session.AddStep(Step{Name: "import", Status: StepRunning})

	h.leaveForManualSteps(context.Background(), session, "shop-prod", svc, outgrewTargetDetail, "export", "import")

	assert.True(t, session.DataLeftBehind("shop-prod", "orders-db"), "bound apps must arrive stopped")
	steps := session.StepsSnapshot()
	require.Len(t, steps, 2)
	assert.Equal(t, StepSkipped, steps[0].Status)
	assert.Contains(t, steps[0].Detail, "768 MB")
	require.NotEmpty(t, steps[0].ManualSteps)
	assert.Contains(t, strings.Join(steps[0].ManualSteps, "\n"), "kip service import", "the steps import the dump")
	assert.Contains(t, strings.Join(steps[0].ManualSteps, "\n"), "--drop", "the manual import replaces the partial copy")
	assert.Equal(t, StepSkipped, steps[1].Status)
	assert.Empty(t, steps[1].ManualSteps, "the commands show once")
}
