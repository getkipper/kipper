package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	"github.com/getkipper/kipper/controller/pkg/labels"
)

const newWriter = "console-api-new_new-uid"

// serviceSet is a service StatefulSet last written by writer, ready on its
// latest revision unless restarting.
func serviceSet(namespace, name, writer string, restarting bool) appsv1.StatefulSet {
	sts := appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Generation: 2,
			Labels:      map[string]string{serviceTypeLabel: "postgres"},
			Annotations: map[string]string{labels.AnnoWrittenBy: writer}},
		Spec:   appsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 2, UpdatedReplicas: 1, ReadyReplicas: 1},
	}
	if restarting {
		sts.Status.UpdatedReplicas, sts.Status.ReadyReplicas = 0, 0
	}
	return sts
}

// scriptedWait serves one listing per poll from steps, repeating the last, on
// a clock that only moves when the wait sleeps. The new console-api runs the
// controllers as newWriter.
func scriptedWait(steps ...[]appsv1.StatefulSet) (*serviceWait, *int) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	polls := 0
	w := &serviceWait{
		List: func(context.Context) ([]appsv1.StatefulSet, error) {
			i := min(polls, len(steps)-1)
			polls++
			if steps[i] == nil {
				return nil, errors.New("connection refused")
			}
			return steps[i], nil
		},
		Writer:   func(context.Context) (string, error) { return newWriter, nil },
		Now:      func() time.Time { return now },
		Sleep:    func(_ context.Context, d time.Duration) { now = now.Add(d) },
		Interval: 10 * time.Second,
		Timeout:  5 * time.Minute,
	}
	return w, &polls
}

var shopDB = map[string]bool{"shop/db": true}

// A ready service the new console-api has not written yet is still waited for,
// however long it stays quiet.
func TestServiceWaitWaitsForTheNewConsoleAPIToWriteEachService(t *testing.T) {
	old := []appsv1.StatefulSet{serviceSet("shop", "db", "console-api-old_old-uid", false)}
	steps := [][]appsv1.StatefulSet{old, old, old, old, old, old, old, old}
	steps = append(steps,
		[]appsv1.StatefulSet{serviceSet("shop", "db", newWriter, true)},
		[]appsv1.StatefulSet{serviceSet("shop", "db", newWriter, false)})
	w, polls := scriptedWait(steps...)
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Equal(t, "\n  ...  Services\n  ✔  1 service ready\n", out.String())
	assert.Equal(t, 10, *polls, "80 seconds on the old template, a restart, then ready")
}

// A new template the StatefulSet controller has not acted on yet is not ready.
func TestServiceWaitWaitsForTheStatefulSetToCatchUp(t *testing.T) {
	behind := serviceSet("shop", "db", newWriter, false)
	behind.Generation = 3
	w, polls := scriptedWait([]appsv1.StatefulSet{behind}, []appsv1.StatefulSet{serviceSet("shop", "db", newWriter, false)})
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Equal(t, "\n  ...  Services\n  ✔  1 service ready\n", out.String())
	assert.Equal(t, 2, *polls)
}

func TestServiceWaitNamesAServiceThatStaysDown(t *testing.T) {
	w, _ := scriptedWait([]appsv1.StatefulSet{serviceSet("shop", "db", newWriter, true), serviceSet("blog", "cache", newWriter, false)})
	var out bytes.Buffer

	w.Wait(context.Background(), &out, map[string]bool{"shop/db": true, "blog/cache": true})

	assert.Contains(t, out.String(), "  ⚠  Not updated and ready 5m0s after the upgrade: shop/db\n     Check them with 'kip status'\n")
}

// A service that was down before the upgrade is not the upgrade's doing.
func TestServiceWaitIgnoresServicesThatWereNotReadyBefore(t *testing.T) {
	w, _ := scriptedWait([]appsv1.StatefulSet{serviceSet("shop", "db", newWriter, false), serviceSet("blog", "cache", "console-api-old_old-uid", true)})
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Equal(t, "\n  ...  Services\n  ✔  1 service ready\n", out.String())
}

// A service stopped or fenced while the upgrade waits is down on purpose.
func TestServiceWaitSkipsAServiceStoppedDuringTheWait(t *testing.T) {
	stopped := serviceSet("shop", "db", "console-api-old_old-uid", true)
	stopped.Spec.Replicas = ptr.To(int32(0))
	w, _ := scriptedWait([]appsv1.StatefulSet{stopped, serviceSet("blog", "cache", newWriter, false)})
	var out bytes.Buffer

	w.Wait(context.Background(), &out, map[string]bool{"shop/db": true, "blog/cache": true})

	assert.Equal(t, "\n  ...  Services\n  ✔  1 service ready\n", out.String())
}

// An OnDelete StatefulSet keeps its old current revision after every pod is
// replaced, so readiness rests on the updated and ready counts.
func TestServiceWaitAcceptsAnUpdatedOnDeleteService(t *testing.T) {
	sts := serviceSet("shop", "db", newWriter, false)
	sts.Spec.UpdateStrategy.Type = appsv1.OnDeleteStatefulSetStrategyType
	sts.Status.CurrentRevision, sts.Status.UpdateRevision = "r1", "r2"
	w, _ := scriptedWait([]appsv1.StatefulSet{sts})
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Equal(t, "\n  ...  Services\n  ✔  1 service ready\n", out.String())
}

func TestServiceWaitRecoversFromAFailedListing(t *testing.T) {
	w, polls := scriptedWait(nil, []appsv1.StatefulSet{serviceSet("shop", "db", newWriter, false)})
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Equal(t, "\n  ...  Services\n  ✔  1 service ready\n", out.String())
	assert.Equal(t, 2, *polls)
}

func TestServiceWaitSaysWhenServicesCannotBeListed(t *testing.T) {
	w, _ := scriptedWait(nil)
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Equal(t, "\n  ...  Services\n  ⚠  Services not checked (connection refused)\n", out.String())
}

// The console-api being replaced holds the controllers for up to a minute
// after the rollout, and what it writes does not count.
func TestServiceWaitStartsOnceTheNewConsoleAPIRunsTheControllers(t *testing.T) {
	w, polls := scriptedWait([]appsv1.StatefulSet{serviceSet("shop", "db", newWriter, false)})
	asked := 0
	w.Writer = func(context.Context) (string, error) {
		asked++
		if asked < 7 {
			return "", nil
		}
		return newWriter, nil
	}
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Equal(t, "\n  ...  Services\n  ✔  1 service ready\n", out.String())
	assert.Equal(t, 1, *polls, "services are listed once the new console-api runs the controllers")
}

func TestServiceWaitSaysWhenTheNewConsoleAPINeverTakesOver(t *testing.T) {
	w, polls := scriptedWait([]appsv1.StatefulSet{serviceSet("shop", "db", "console-api-old_old-uid", false)})
	w.Writer = func(context.Context) (string, error) { return "", nil }
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Equal(t, "\n  ...  Services\n  ⚠  Services not checked: the new console-api was not running its controllers after 5m0s\n     Check them with 'kip status'\n", out.String())
	assert.Zero(t, *polls, "nothing to compare the services with")
}

// Two console-api builds can share a version string, and an old pod can
// restart during the rollout. Only services written by the new controller
// count as updated.
func TestServiceWaitWantsTheRunningControllerItself(t *testing.T) {
	sameBuildOtherPod := serviceSet("shop", "db", "console-api-old_old-uid", false)
	w, _ := scriptedWait([]appsv1.StatefulSet{sameBuildOtherPod})
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Contains(t, out.String(), "  ⚠  Not updated and ready 5m0s after the upgrade: shop/db\n")
}

// A service deleted during the wait is neither counted nor waited for.
func TestServiceWaitForgetsADeletedService(t *testing.T) {
	w, _ := scriptedWait([]appsv1.StatefulSet{serviceSet("blog", "cache", newWriter, false)})
	var out bytes.Buffer

	w.Wait(context.Background(), &out, map[string]bool{"shop/db": true, "blog/cache": true})

	assert.Equal(t, "\n  ...  Services\n  ✔  1 service ready\n", out.String())
}

// A request that hangs ends with the wait's own budget.
func TestServiceWaitEndsAHangingRequestAtTheTimeout(t *testing.T) {
	w, _ := scriptedWait()
	w.List = func(ctx context.Context) ([]appsv1.StatefulSet, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	w.Timeout = 50 * time.Millisecond
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Equal(t, "\n  ...  Services\n  ⚠  Services not checked (context deadline exceeded)\n", out.String())
}

func TestServiceWaitIsSilentWithoutServices(t *testing.T) {
	w, polls := scriptedWait([]appsv1.StatefulSet{})
	var out bytes.Buffer

	w.Wait(context.Background(), &out, map[string]bool{})

	assert.Empty(t, out.String())
	assert.Zero(t, *polls)
}

func TestServiceReadyListsRolledOutServices(t *testing.T) {
	behind := serviceSet("shop", "queue", newWriter, false)
	behind.Status.ObservedGeneration = 1
	stopped := serviceSet("shop", "cache", newWriter, false)
	stopped.Spec.Replicas = ptr.To(int32(0))
	clientset := k8sfake.NewClientset()
	for _, sts := range []appsv1.StatefulSet{serviceSet("shop", "db", "console-api-old_old-uid", false), serviceSet("blog", "db", newWriter, true), behind, stopped} {
		_, err := clientset.AppsV1().StatefulSets(sts.Namespace).Create(context.Background(), &sts, metav1.CreateOptions{})
		require.NoError(t, err)
	}

	ready, err := newServiceWait(clientset, "").Ready(context.Background())

	require.NoError(t, err)
	assert.Equal(t, shopDB, ready)
}

func TestConsoleAPIWriter(t *testing.T) {
	lease := func(holder string) *coordinationv1.Lease {
		return &coordinationv1.Lease{ObjectMeta: metav1.ObjectMeta{Name: consoleAPILeaseName, Namespace: kipperSystemNS},
			Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder)}}
	}
	pod := func(name, hash string, going bool) *corev1.Pod {
		p := consoleAPIPodOfRevision(name, hash, going)
		p.UID = types.UID(name + "-uid")
		return p
	}
	cases := map[string]struct {
		objects []runtime.Object
		want    string
	}{
		"the rolled pod holds it":       {[]runtime.Object{lease("new_new-uid"), pod("new", currentRevision, false)}, "new_new-uid"},
		"the old pod still holds it":    {[]runtime.Object{lease("old_old-uid"), pod("old", "old123", false), pod("new", currentRevision, false)}, ""},
		"a later rollout holds it":      {[]runtime.Object{lease("back_back-uid"), pod("back", "old123", false)}, ""},
		"the holder is shutting down":   {[]runtime.Object{lease("new_new-uid"), pod("new", currentRevision, true)}, ""},
		"a pod of that name was reborn": {[]runtime.Object{lease("new_other-uid"), pod("new", currentRevision, false)}, ""},
		"the holder is gone":            {[]runtime.Object{lease("old_old-uid"), pod("new", currentRevision, false)}, ""},
		"nobody holds it":               {[]runtime.Object{lease(""), pod("new", currentRevision, false)}, ""},
		"there is no lease yet":         {[]runtime.Object{pod("new", currentRevision, false)}, ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := consoleAPIWriter(context.Background(), k8sfake.NewClientset(tc.objects...), currentRevision)

			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// After leadership moves to another pod in the rollout, each service must
// be written by the new leader before it counts as updated.
func TestServiceWaitFollowsTheControllersToAnotherPod(t *testing.T) {
	const other = "console-api-other_other-uid"
	w, polls := scriptedWait(
		[]appsv1.StatefulSet{serviceSet("shop", "db", newWriter, true)},
		[]appsv1.StatefulSet{serviceSet("shop", "db", newWriter, true)},
		[]appsv1.StatefulSet{serviceSet("shop", "db", newWriter, false)},
		[]appsv1.StatefulSet{serviceSet("shop", "db", newWriter, false)},
		[]appsv1.StatefulSet{serviceSet("shop", "db", other, false)})
	w.Writer = func(context.Context) (string, error) {
		if *polls < 2 {
			return newWriter, nil
		}
		return other, nil
	}
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Equal(t, "\n  ...  Services\n  ✔  1 service ready\n", out.String())
	assert.Equal(t, 5, *polls, "ready under the first writer's stamp after the handover does not count")
}

func TestServiceWaitSaysWhenTheControllersCannotBeRead(t *testing.T) {
	w, _ := scriptedWait([]appsv1.StatefulSet{serviceSet("shop", "db", newWriter, false)})
	w.Writer = func(context.Context) (string, error) { return "", errors.New("leases is forbidden") }
	var out bytes.Buffer

	w.Wait(context.Background(), &out, shopDB)

	assert.Equal(t, "\n  ...  Services\n  ⚠  Services not checked (leases is forbidden)\n", out.String())
}
