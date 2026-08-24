package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

var (
	statefulSetGVR = appsv1.SchemeGroupVersion.WithResource("statefulsets")
	claimGVR       = corev1.SchemeGroupVersion.WithResource("persistentvolumeclaims")
)

func dataClaim(service string, ordinal int) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      fmt.Sprintf("data-%s-%d", service, ordinal),
		Namespace: "shop-prod",
		Labels:    map[string]string{"app": service},
	}}
}

// The service under test is "db" in "shop-prod" throughout, so the workload is
// the one the claim helper's fixtures belong to.
func statefulSet() *appsv1.StatefulSet {
	return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "db", Namespace: "shop-prod",
		Labels: map[string]string{labels.ManagedBy: labels.Kipper},
	}}
}

// destroy reads the volumes and then destroys them, which is the order every
// caller uses: identity is fixed before anything is deleted.
func destroy(m *Manager, timeout, interval time.Duration) error {
	ctx := context.Background()
	volumes, err := m.dataVolumes(ctx, "shop-prod", "db", timeout)
	if err != nil {
		return err
	}
	return m.destroyVolumes(ctx, "shop-prod", "db", volumes, timeout, interval)
}

func claimExists(t *testing.T, m *Manager, name string) bool {
	t.Helper()
	_, err := m.Client.CoreV1().PersistentVolumeClaims("shop-prod").Get(context.Background(), name, metav1.GetOptions{})
	return err == nil
}

// writeClaimsBackWhileTheWorkloadRuns gives the fake client the one behaviour of
// the StatefulSet controller this has to be safe against: while the StatefulSet
// is there, a deleted claim is back from the volumeClaimTemplate before anyone
// looks, so a destroy that does not wait leaves the volume exactly where it was.
func writeClaimsBackWhileTheWorkloadRuns(c *fake.Clientset) {
	tracker := c.Tracker()
	c.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		if _, err := tracker.Get(statefulSetGVR, "shop-prod", "db"); err == nil {
			return true, nil, nil
		}
		return false, nil, nil
	})
}

// The StatefulSet controller writes a claim back from its template the moment
// one goes, so deleting the volume while the workload is still there leaves the
// operator with a fresh empty one and the data exactly where it was. The
// workload goes first, through the owner reference on the CR that was deleted.
func TestDestroyVolumes_WaitsForTheWorkloadToGo(t *testing.T) {
	ctx := context.Background()
	c := fake.NewSimpleClientset( //nolint:staticcheck
		statefulSet(),
		dataClaim("db", 0),
	)
	writeClaimsBackWhileTheWorkloadRuns(c)
	m := &Manager{Client: c}

	// The StatefulSet goes while the wait is running, as garbage collection
	// would take it once the CR was deleted.
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = m.Client.AppsV1().StatefulSets("shop-prod").Delete(ctx, "db", metav1.DeleteOptions{})
	}()

	if err := destroy(m, 2*time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	if claimExists(t, m, "data-db-0") {
		t.Error("the volume survived a destroy that reported success")
	}
}

// A workload that will not go is a volume that cannot be destroyed, and saying
// so beats deleting a claim the StatefulSet writes straight back.
func TestDestroyVolumes_ReportsAWorkloadThatWillNotGo(t *testing.T) {
	m := &Manager{Client: fake.NewSimpleClientset( //nolint:staticcheck
		statefulSet(),
		dataClaim("db", 0),
	)}

	err := destroy(m, 40*time.Millisecond, 10*time.Millisecond)

	if err == nil {
		t.Fatal("a volume that could not be destroyed was reported as destroyed")
	}
	if !strings.Contains(err.Error(), "db") {
		t.Errorf("the message names no service: %v", err)
	}
	if !claimExists(t, m, "data-db-0") {
		t.Error("the claim was deleted anyway, so the StatefulSet writes an empty one back")
	}
}

// Destroying data is irreversible, so the label narrows the set and the name has
// to be one the StatefulSet would have created. A volume that merely carries the
// service's label belongs to whoever made it.
func TestDestroyVolumes_LeavesAVolumeThatIsNotTheWorkloadsClaim(t *testing.T) {
	mine := dataClaim("db", 0)
	borrowed := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "db-uploads", Namespace: "shop-prod", Labels: map[string]string{"app": "db"},
	}}
	other := dataClaim("cache", 0)
	m := &Manager{Client: fake.NewSimpleClientset(mine, borrowed, other)} //nolint:staticcheck

	if err := destroy(m, time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	if claimExists(t, m, "data-db-0") {
		t.Error("the service's own volume survived")
	}
	if !claimExists(t, m, "db-uploads") {
		t.Error("a volume that only shares the label was destroyed")
	}
	if !claimExists(t, m, "data-cache-0") {
		t.Error("another service's volume was destroyed")
	}
}

// Nothing to wait for is not an error: the workload has already gone, which is
// the state after a CR delete has been collected, or after the service was
// removed some other way and only the volume is left.
func TestDestroyVolumes_DestroysWhenTheWorkloadHasAlreadyGone(t *testing.T) {
	m := &Manager{Client: fake.NewSimpleClientset(dataClaim("db", 0))} //nolint:staticcheck

	if err := destroy(m, time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	if claimExists(t, m, "data-db-0") {
		t.Error("the leftover volume survived")
	}
}

// The fallback path destroys through the same rule as the CR path, so the two
// agree on which volumes belong to a service and neither races the StatefulSet.
func TestDelete_DestroysThroughTheSameRule(t *testing.T) {
	ctx := context.Background()
	borrowed := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "db-uploads", Namespace: "shop-prod", Labels: map[string]string{"app": "db"},
	}}
	m := &Manager{Client: fake.NewSimpleClientset( //nolint:staticcheck
		statefulSet(),
		dataClaim("db", 0),
		borrowed,
	)}

	if err := m.Delete(ctx, "shop-prod", "db", true); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if claimExists(t, m, "data-db-0") {
		t.Error("the service's volume survived a delete that asked for it to go")
	}
	if !claimExists(t, m, "db-uploads") {
		t.Error("a volume that only shares the label was destroyed")
	}
}

// The name on its own is not proof. A claim called data-db-0 that does not carry
// the service's label was made by something else, and both conditions have to
// hold before anything irreversible happens to it.
//
// The cost is stated rather than hidden: a claim whose labels were stripped by
// hand survives --delete-data and has to be removed the same way. Leaving a
// volume is the safe direction to be wrong in.
func TestDestroyVolumes_LeavesAClaimThatDoesNotCarryTheServiceLabel(t *testing.T) {
	unlabelled := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-db-0", Namespace: "shop-prod",
	}}
	m := &Manager{Client: fake.NewSimpleClientset(unlabelled)} //nolint:staticcheck

	if err := destroy(m, time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	if !claimExists(t, m, "data-db-0") {
		t.Error("a claim with no label tying it to this service was destroyed on its name alone")
	}
}

// A service runs one replica today, so its claim is always data-<name>-0. The
// rule matches any ordinal because the claim naming is the StatefulSet's, not
// this command's, and a service that ever ran two would otherwise leave its
// second volume behind with the data still on it.
func TestDestroyVolumes_DestroysEveryOrdinal(t *testing.T) {
	m := &Manager{Client: fake.NewSimpleClientset( //nolint:staticcheck
		dataClaim("db", 0),
		dataClaim("db", 1),
	)}

	if err := destroy(m, time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	for _, name := range []string{"data-db-0", "data-db-1"} {
		if claimExists(t, m, name) {
			t.Errorf("%s survived, so a scaled service keeps a volume with its data on it", name)
		}
	}
}

// swapTheClaimAsTheWorkloadGoes replaces the service's claim with a same-named
// one at the moment the wait sees the workload disappear, which is what a
// service someone creates under the same name during a delete leaves behind.
func swapTheClaimAsTheWorkloadGoes(c *fake.Clientset, replacement *corev1.PersistentVolumeClaim) {
	tracker := c.Tracker()
	seen := 0
	c.PrependReactor("get", "statefulsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		seen++
		if seen == 1 {
			return true, statefulSet(), nil
		}
		_ = tracker.Delete(claimGVR, "shop-prod", replacement.Name)
		_ = tracker.Add(replacement)
		return true, nil, errors.NewNotFound(corev1.Resource("statefulsets"), "db")
	})
}

// A name belongs to nobody once its service is gone, so the claim standing under
// it by the time the workload has been collected can be a new service's, with
// the operator's data already on it. What goes is decided before the wait and
// pinned by UID, so a volume that arrived since is left alone.
func TestDestroyVolumes_LeavesTheVolumeOfAServiceMadeSince(t *testing.T) {
	mine := dataClaim("db", 0)
	mine.UID = "the-service-being-deleted"
	replacement := dataClaim("db", 0)
	replacement.UID = "a-service-made-since"

	c := fake.NewSimpleClientset(mine) //nolint:staticcheck
	swapTheClaimAsTheWorkloadGoes(c, replacement)
	m := &Manager{Client: c}

	if err := destroy(m, time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	if !claimExists(t, m, "data-db-0") {
		t.Error("the volume of a service created during the delete was destroyed with the old one's data")
	}
}

// apiServerThatNeverAnswers accepts requests for one resource and leaves them
// open, the way an API server that has stopped answering does. Anything else
// gets an empty list, so a test can choose which step of the destroy hangs.
//
// The handler is released by the test rather than only by the client going away,
// because Close waits for a request still in flight and a step that outlives its
// timeout would then deadlock the test instead of failing it.
func apiServerThatNeverAnswers(t *testing.T, resource string) *kubernetes.Clientset {
	t.Helper()
	release := make(chan struct{})
	silent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, resource) {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"apiVersion":"v1","kind":"List","metadata":{},"items":[]}`))
	}))
	t.Cleanup(func() {
		close(release)
		silent.Close()
	})

	client, err := kubernetes.NewForConfig(&rest.Config{Host: silent.URL})
	if err != nil {
		t.Fatalf("client: %v", err)
	}
	return client
}

// The timeout has to bound the calls themselves, not just the gaps between
// polls. An API server that accepts a request and never answers would otherwise
// hold the command open for as long as it liked. Covered here are the two steps
// that run before anything has been destroyed; the deletes that follow carry the
// same deadline.
func TestDestroyVolumes_GivesUpWhenTheApiServerNeverAnswers(t *testing.T) {
	for _, step := range []struct {
		name     string
		resource string
	}{
		{"reading the volumes", "persistentvolumeclaims"},
		{"waiting for the workload", "statefulsets"},
	} {
		t.Run(step.name, func(t *testing.T) {
			m := &Manager{Client: apiServerThatNeverAnswers(t, step.resource)}

			done := make(chan error, 1)
			go func() {
				done <- destroy(m, 100*time.Millisecond, 10*time.Millisecond)
			}()

			select {
			case err := <-done:
				if err == nil {
					t.Fatal("a destroy that was never answered reported the volume gone")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("the destroy outlived its timeout, so the command hangs on an API server that does not answer")
			}
		})
	}
}

// A refused delete is the safeguard working when the volume it was aimed at has
// left: the name now belongs to something made since, and there is nothing of
// the operator's to destroy.
func TestDestroyVolumes_AcceptsARefusedDeleteWhenTheVolumeHasLeft(t *testing.T) {
	mine := dataClaim("db", 0)
	mine.UID = "the-service-being-deleted"
	replacement := dataClaim("db", 0)
	replacement.UID = "a-service-made-since"

	c := fake.NewSimpleClientset(mine) //nolint:staticcheck
	tracker := c.Tracker()
	c.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		_ = tracker.Delete(claimGVR, "shop-prod", replacement.Name)
		_ = tracker.Add(replacement)
		return true, nil, errors.NewConflict(claimGVR.GroupResource(), "data-db-0", fmt.Errorf("the UID does not match"))
	})
	m := &Manager{Client: c}

	if err := destroy(m, time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("a volume that had already left was reported as a failure: %v", err)
	}
}

// A refused delete with the volume still standing there is a delete that did not
// happen. Reporting the data destroyed would be a lie about the one thing this
// command cannot undo.
func TestDestroyVolumes_ReportsARefusedDeleteThatLeftTheVolume(t *testing.T) {
	mine := dataClaim("db", 0)
	mine.UID = "the-service-being-deleted"

	c := fake.NewSimpleClientset(mine) //nolint:staticcheck
	c.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewConflict(claimGVR.GroupResource(), "data-db-0", fmt.Errorf("an admission webhook said no"))
	})
	m := &Manager{Client: c}

	err := destroy(m, time.Second, 5*time.Millisecond)

	if err == nil {
		t.Fatal("a volume that was never deleted was reported as destroyed")
	}
	if !strings.Contains(err.Error(), "data-db-0") {
		t.Errorf("the message names no volume: %v", err)
	}
}

// The delete pins the volume's UID, so an API server that has since seen the
// name taken refuses the call rather than destroying whatever stands there now.
func TestDestroyVolumes_PinsTheVolumeItReadOnTheDelete(t *testing.T) {
	mine := dataClaim("db", 0)
	mine.UID = "the-service-being-deleted"
	c := fake.NewSimpleClientset(mine) //nolint:staticcheck
	var pinned types.UID
	c.PrependReactor("delete", "persistentvolumeclaims", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options := action.(k8stesting.DeleteActionImpl).DeleteOptions
		if options.Preconditions != nil && options.Preconditions.UID != nil {
			pinned = *options.Preconditions.UID
		}
		return false, nil, nil
	})
	m := &Manager{Client: c}

	if err := destroy(m, time.Second, 5*time.Millisecond); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	if pinned != "the-service-being-deleted" {
		t.Errorf("the delete carried %q instead of the volume that was read, so the API server had nothing to check", pinned)
	}
}

// The name of a service that has gone is free, so the volumes have to be read
// before the workload is deleted rather than after. A service made in between
// keeps its own data.
func TestDelete_ReadsTheVolumesBeforeItFreesTheName(t *testing.T) {
	mine := dataClaim("db", 0)
	mine.UID = "the-service-being-deleted"
	replacement := dataClaim("db", 0)
	replacement.UID = "a-service-made-since"

	c := fake.NewSimpleClientset(statefulSet(), mine) //nolint:staticcheck
	tracker := c.Tracker()
	c.PrependReactor("delete", "statefulsets", func(k8stesting.Action) (bool, runtime.Object, error) {
		_ = tracker.Delete(claimGVR, "shop-prod", replacement.Name)
		_ = tracker.Add(replacement)
		return false, nil, nil
	})
	m := &Manager{Client: c}

	if err := m.Delete(context.Background(), "shop-prod", "db", true); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if !claimExists(t, m, "data-db-0") {
		t.Error("the volume of a service created during the delete was destroyed with the old one's data")
	}
}

// The fallback path deletes the workload, the cluster address and the
// credentials, and a failure on any of them is the operator's to know about. It
// used to report the service gone whatever the API server said.
func TestDelete_ReportsWhatItCouldNotRemove(t *testing.T) {
	credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "db-credentials", Namespace: "shop-prod",
		Labels: map[string]string{labels.ManagedBy: labels.Kipper},
	}}
	c := fake.NewSimpleClientset(statefulSet(), dataClaim("db", 0), credentials) //nolint:staticcheck
	c.PrependReactor("delete", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("the API server said no")
	})
	m := &Manager{Client: c}

	err := m.Delete(context.Background(), "shop-prod", "db", true)

	if err == nil {
		t.Fatal("a secret that was not deleted was reported as a service fully removed")
	}
	if !strings.Contains(err.Error(), "db-credentials") {
		t.Errorf("the message names nothing an operator could go and look at: %v", err)
	}
}

// An accepted delete is a request, not an act. A finalizer that never clears
// leaves the claim standing with the data on it, and the command has to say so
// rather than report the volume gone because the API server took the call.
func TestDestroyVolumes_ReportsAVolumeThatNeverWent(t *testing.T) {
	mine := dataClaim("db", 0)
	mine.UID = "the-service-being-deleted"

	c := fake.NewSimpleClientset(mine) //nolint:staticcheck
	c.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, nil
	})
	m := &Manager{Client: c}

	err := destroy(m, 100*time.Millisecond, 5*time.Millisecond)

	if err == nil {
		t.Fatal("a volume held by a finalizer was reported as destroyed")
	}
	if !strings.Contains(err.Error(), "data-db-0") {
		t.Errorf("the message names no volume: %v", err)
	}
	if !claimExists(t, m, "data-db-0") {
		t.Error("the fixture did not hold the claim, so the test proves nothing")
	}
}

// The CR-less path has no owner reference to go on, so the management label is
// the only thing saying a workload is Kipper's. A StatefulSet called db with a
// claim called data-db-0 is what anybody's database looks like.
func TestDelete_LeavesAWorkloadKipperNeverMade(t *testing.T) {
	foreign := statefulSet()
	foreign.Labels = nil
	m := &Manager{Client: fake.NewSimpleClientset(foreign, dataClaim("db", 0))} //nolint:staticcheck

	err := m.Delete(context.Background(), "shop-prod", "db", true)

	if err == nil {
		t.Fatal("a workload Kipper never made was deleted on the strength of its name")
	}
	if !claimExists(t, m, "data-db-0") {
		t.Error("somebody else's volume was destroyed")
	}
	if _, err := m.Client.AppsV1().StatefulSets("shop-prod").
		Get(context.Background(), "db", metav1.GetOptions{}); err != nil {
		t.Error("somebody else's workload was deleted")
	}
}

// This path exists for a service whose records have already gone, so there may
// be no workload left to take anyone's word from. The address and the
// credentials both carry the management label, and one under this name that does
// not belongs to something else.
func TestDelete_LeavesAnAddressAndASecretThatAreNotKippers(t *testing.T) {
	theirs := map[string]string{"app": "db"}
	address := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "db", Namespace: "shop-prod", Labels: theirs,
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "db-credentials", Namespace: "shop-prod", Labels: theirs,
	}}
	m := &Manager{Client: fake.NewSimpleClientset(address, secret)} //nolint:staticcheck

	err := m.Delete(context.Background(), "shop-prod", "db", true)

	if err == nil {
		t.Fatal("objects that are not Kipper's were deleted on the strength of their names")
	}
	if _, err := m.Client.CoreV1().Services("shop-prod").
		Get(context.Background(), "db", metav1.GetOptions{}); err != nil {
		t.Error("somebody else's address was deleted")
	}
	if _, err := m.Client.CoreV1().Secrets("shop-prod").
		Get(context.Background(), "db-credentials", metav1.GetOptions{}); err != nil {
		t.Error("somebody else's secret was deleted")
	}
}

// A refused delete that the cluster then does not carry out leaves a volume that
// looks as though it simply never went. The refusal is the reason, so it belongs
// in the message rather than in the operator's head.
func TestDestroyVolumes_NamesTheRefusalWhenNothingElseRemovedTheVolume(t *testing.T) {
	mine := dataClaim("db", 0)
	mine.UID = "the-service-being-deleted"
	c := fake.NewSimpleClientset(mine) //nolint:staticcheck
	c.PrependReactor("delete", "persistentvolumeclaims", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.NewForbidden(claimGVR.GroupResource(), "data-db-0", fmt.Errorf("not allowed"))
	})
	m := &Manager{Client: c}

	err := destroy(m, 100*time.Millisecond, 5*time.Millisecond)

	if err == nil {
		t.Fatal("a volume nothing removed was reported as destroyed")
	}
	if !strings.Contains(err.Error(), "may not delete the volume") {
		t.Errorf("the refusal that explains it is missing: %v", err)
	}
}

// Checking as it goes leaves the service half removed the moment one object
// turns out to be somebody else's, and every rerun stops in the same place, so
// the volume that made the operator run this can never be cleared.
func TestDelete_ChecksEverythingBeforeItDeletesAnything(t *testing.T) {
	theirs := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "db-credentials", Namespace: "shop-prod", Labels: map[string]string{"app": "db"},
	}}
	m := &Manager{Client: fake.NewSimpleClientset( //nolint:staticcheck
		statefulSet(), dataClaim("db", 0), theirs,
	)}

	if err := m.Delete(context.Background(), "shop-prod", "db", true); err == nil {
		t.Fatal("a secret that is not Kipper's was taken with the service")
	}

	if _, err := m.Client.AppsV1().StatefulSets("shop-prod").
		Get(context.Background(), "db", metav1.GetOptions{}); err != nil {
		t.Error("the workload went before the refusal, so the service is half removed and every rerun stops here")
	}
	if !claimExists(t, m, "data-db-0") {
		t.Error("the volume went before the refusal")
	}
}

// The workload's provenance is read and then it is deleted, and the name is free
// the moment it goes. The delete carries the UID that was read, so what stands
// there by then is nobody's business of this command's.
func TestDelete_PinsTheWorkloadItRead(t *testing.T) {
	workload := statefulSet()
	workload.UID = "the-workload-that-was-read"
	c := fake.NewSimpleClientset(workload, dataClaim("db", 0)) //nolint:staticcheck
	var pinned types.UID
	c.PrependReactor("delete", "statefulsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		options := action.(k8stesting.DeleteActionImpl).DeleteOptions
		if options.Preconditions != nil && options.Preconditions.UID != nil {
			pinned = *options.Preconditions.UID
		}
		return false, nil, nil
	})
	m := &Manager{Client: c}

	if err := m.Delete(context.Background(), "shop-prod", "db", true); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if pinned != "the-workload-that-was-read" {
		t.Errorf("the workload was deleted by name alone, so a replacement under that name goes with it (pinned %q)", pinned)
	}
}

// This path is only reached when there was no service record, so a workload with
// a controller means one appeared while the command was reading: what stands
// here is a service somebody has just made, not the leftovers of one that went.
func TestDelete_LeavesAWorkloadThatGainedAServiceSinceTheCheck(t *testing.T) {
	controller := true
	adopted := statefulSet()
	adopted.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: "kipper.run/v1alpha1", Kind: "Service",
		Name: "db", UID: types.UID("a-service-made-since"), Controller: &controller,
	}}
	m := &Manager{Client: fake.NewSimpleClientset(adopted, dataClaim("db", 0))} //nolint:staticcheck

	err := m.Delete(context.Background(), "shop-prod", "db", true)

	if err == nil {
		t.Fatal("a workload belonging to a service created since was deleted with its data")
	}
	if !claimExists(t, m, "data-db-0") {
		t.Error("the new service's volume was destroyed")
	}
}

// The address and the credentials are read on a path that is only reached when
// there was no service record. An owner on either means one appeared while this
// was reading, and what stands there is a new service's, not leftovers.
func TestDelete_LeavesTheObjectsOfAServiceMadeSinceTheCheck(t *testing.T) {
	controller := true
	owned := []metav1.OwnerReference{{
		APIVersion: "kipper.run/v1alpha1", Kind: "Service",
		Name: "db", UID: types.UID("a-service-made-since"), Controller: &controller,
	}}
	kippers := map[string]string{labels.ManagedBy: labels.Kipper}
	address := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: "db", Namespace: "shop-prod", Labels: kippers, OwnerReferences: owned,
	}}
	m := &Manager{Client: fake.NewSimpleClientset(address, dataClaim("db", 0))} //nolint:staticcheck

	err := m.Delete(context.Background(), "shop-prod", "db", true)

	if err == nil {
		t.Fatal("a new service's address was deleted as though it were leftovers")
	}
	if _, err := m.Client.CoreV1().Services("shop-prod").
		Get(context.Background(), "db", metav1.GetOptions{}); err != nil {
		t.Error("the new service's address was deleted")
	}
	if !claimExists(t, m, "data-db-0") {
		t.Error("the new service's volume was destroyed")
	}
}

// The refusal names what it refused, and a Secret's kind is not its name.
func TestDelete_NamesTheKindOfObjectItRefused(t *testing.T) {
	theirs := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "db-credentials", Namespace: "shop-prod", Labels: map[string]string{"app": "db"},
	}}
	m := &Manager{Client: fake.NewSimpleClientset(theirs)} //nolint:staticcheck

	err := m.Delete(context.Background(), "shop-prod", "db", true)

	if err == nil {
		t.Fatal("a secret that is not Kipper's was taken")
	}
	if !strings.Contains(err.Error(), "credentials named db-credentials") {
		t.Errorf("the message does not say what kind of object it refused: %v", err)
	}
}
