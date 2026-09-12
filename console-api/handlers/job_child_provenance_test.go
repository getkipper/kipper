package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// A Function's cron trigger creates a CronJob called <function>-cron, which is
// a name a Job CR may also have. Both carry Kipper's managed-by label, so it is
// the resource-type that separates them: the Job's verbs must not copy, read or
// rewrite a Function's child.
func functionCronJob(namespace, name string) *batchv1.CronJob {
	cj := collidingCronJob(namespace)
	cj.Name = name
	cj.Labels = map[string]string{
		"app":                      "report",
		kipperLabel:                kipperValue,
		"kipper.run/resource-type": "function",
	}
	return cj
}

const functionChildName = "report-cron"

func TestTriggerRefusesAChildAnotherWorkloadOwns(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	client := fake.NewClientset(functionCronJob(shopNS, functionChildName))
	h := &Jobs{
		Client:   client,
		CRClient: testCRClient(jobCR(shopNS, functionChildName, "0 3 * * *")),
	}

	rec := httptest.NewRecorder()
	h.Trigger(rec, jobRequest("POST", "/api/v1/jobs/"+functionChildName+"/trigger",
		"dev@test.com", functionChildName, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
	if got := triggeredJobNamespaces(t, client); len(got) != 0 {
		t.Errorf("another workload's template must not be run, ran in %v", got)
	}
}

func TestGetResourcesRefusesAChildAnotherWorkloadOwns(t *testing.T) {
	// Answering 200 with an empty body here shows a job that cannot reconcile
	// as a healthy one with nothing pinned.
	withCollisionResolver(t, "deployer", "")
	h := &Jobs{
		Client:   fake.NewClientset(functionCronJob(shopNS, functionChildName)),
		CRClient: testCRClient(jobCR(shopNS, functionChildName, "0 3 * * *")),
	}

	rec := httptest.NewRecorder()
	h.GetResources(rec, jobRequest("GET", "/api/v1/jobs/"+functionChildName+"/resources",
		"dev@test.com", functionChildName, ""))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

func TestUpdateResourcesRefusesAChildAnotherWorkloadOwns(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	h := &Jobs{
		Client:   fake.NewClientset(functionCronJob(shopNS, functionChildName)),
		CRClient: testCRClient(jobCR(shopNS, functionChildName, "0 3 * * *")),
	}

	rec := httptest.NewRecorder()
	h.UpdateResources(rec, jobRequest("PUT", "/api/v1/jobs/"+functionChildName+"/resources",
		"dev@test.com", functionChildName, `{"memory_limit":"512Mi","cpu_limit":"500m"}`))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body %s", rec.Code, rec.Body.String())
	}
}

// A child adopted by this job carries a controller reference to it, which is
// the strong half of the rule and must keep working when the labels do not.
func TestTriggerAcceptsAnAdoptedChild(t *testing.T) {
	withCollisionResolver(t, "deployer", "")
	job := jobCR(shopNS, collidingJob, "0 3 * * *")
	job.UID = "job-uid"
	cj := collidingCronJob(shopNS)
	cj.Labels = map[string]string{"app": "something-else"}
	yes := true
	cj.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "kipper.run/v1alpha1",
		Kind:       "Job",
		Name:       job.Name,
		UID:        job.UID,
		Controller: &yes,
	}}
	client := fake.NewClientset(cj)
	h := &Jobs{Client: client, CRClient: testCRClient(job)}

	rec := httptest.NewRecorder()
	h.Trigger(rec, jobRequest("POST", "/api/v1/jobs/"+collidingJob+"/trigger", "dev@test.com", collidingJob, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
}
