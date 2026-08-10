package handlers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
)

// testRunResponse is what the UI uses to tail the resulting pod's logs.
type testRunResponse struct {
	JobName   string `json:"job_name"`
	Namespace string `json:"namespace"`
}

// TestRun launches a one-off run of a cron-triggered function with the
// same image, env, bindings, and volumes the scheduled run would use.
// The CronJob and its schedule are not modified — this creates an
// independent batch/v1.Job that runs immediately.
//
// KIPPER_TRIGGER is set to "test" instead of "cron" so function code
// can branch on the env var (skip outbound notifications, log
// differently, return early, etc).
//
// POST /api/v1/projects/{name}/functions/{fn}/test
func (f *Functions) TestRun(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")
	fnName := chi.URLParam(r, "fn")

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	var fn kipperv1.Function
	if err := f.CRClient.Get(ctx, crclient.ObjectKey{Namespace: project, Name: fnName}, &fn); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("function %q not found", fnName))
			return
		}
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("getting function: %v", err))
		return
	}

	// Test runs only make sense for cron-triggered functions. HTTP and
	// event-driven functions are easier to invoke directly (curl, etc).
	if !hasCronTrigger(&fn) {
		respondError(w, http.StatusBadRequest, "test runs are only available for cron-triggered functions")
		return
	}

	// Stage the pull Secret before referencing it in the Job's pod spec — a
	// test run right after Function creation must not race the first
	// controller reconcile. The spec references exactly what this call
	// staged, from a single credential read.
	pullSecrets, err := controllers.StageFunctionPullSecret(ctx, f.CRClient, &fn)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("staging image pull secret: %v", err))
		return
	}

	podSpec, err := controllers.BuildBatchPodSpec(ctx, f.CRClient, &fn, "test", pullSecrets)
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("building pod spec: %v", err))
		return
	}

	jobName := fmt.Sprintf("%s-test-%s", fn.Name, randomHex(4))
	labels := map[string]string{
		"app":                      fn.Name,
		kipperLabel:                kipperValue,
		"kipper.run/resource-type": "function",
		"kipper.run/trigger":       "test",
		"kipper.run/test-run":      "true",
	}

	backoff := int32(0)
	ttl := int32(600) // self-clean after 10 minutes

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: fn.Namespace,
			Labels:    labels,
			// Owner reference so the test Job is garbage-collected if
			// the parent Function is deleted while a test is in flight.
			OwnerReferences: []metav1.OwnerReference{ownerRefForFunction(&fn)},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				// Test pods run to completion; the cron pod template uses
				// OnFailure but for a single test run Never is cleaner —
				// failures show up as a failed Job, not a restart loop.
				Spec: withRestartPolicy(podSpec, corev1.RestartPolicyNever),
			},
		},
	}

	if _, err := f.Client.BatchV1().Jobs(fn.Namespace).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("creating test job: %v", err))
		return
	}

	respondJSON(w, http.StatusOK, testRunResponse{
		JobName:   jobName,
		Namespace: fn.Namespace,
	})
}

func hasCronTrigger(fn *kipperv1.Function) bool {
	for _, t := range fn.Spec.Triggers {
		if t.Type == "cron" {
			return true
		}
	}
	return false
}

func withRestartPolicy(spec corev1.PodSpec, policy corev1.RestartPolicy) corev1.PodSpec {
	spec.RestartPolicy = policy
	return spec
}

// ownerRefForFunction builds an OwnerReference pointing at the Function CR
// without needing a runtime.Scheme. We hard-code the apiVersion and kind
// because the handler doesn't import the scheme builder; the values are
// stable contract for the v1alpha1 API.
func ownerRefForFunction(fn *kipperv1.Function) metav1.OwnerReference {
	controller := true
	return metav1.OwnerReference{
		APIVersion: "kipper.run/v1alpha1",
		Kind:       "Function",
		Name:       fn.Name,
		UID:        fn.UID,
		Controller: &controller,
	}
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
