package uisession

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func newTestStore() (*RecordStore, *fake.Clientset, *time.Time) {
	client := fake.NewSimpleClientset()
	clock := epoch
	s := NewRecordStore(client, "kipper-system")
	s.now = func() time.Time { return clock }
	return s, client, &clock
}

func TestRecordCreateThenLive(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()
	if err := s.Create(ctx, "sid1", "sub-a", "a@example.com", "mailhog.apps.example.com", epoch, epoch.Add(12*time.Hour)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !s.Live(ctx, "sid1") {
		t.Fatal("record not live after create")
	}
}

func TestRecordCreateReplay(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()
	if err := s.Create(ctx, "sid1", "sub-a", "a@example.com", "h", epoch, epoch.Add(time.Hour)); err != nil {
		t.Fatalf("first Create: %v", err)
	}
	err := s.Create(ctx, "sid1", "sub-a", "a@example.com", "h", epoch, epoch.Add(time.Hour))
	if !errors.Is(err, ErrReplay) {
		t.Fatalf("expected ErrReplay on duplicate sid, got %v", err)
	}
}

func TestLiveFailsClosedOnAPIError(t *testing.T) {
	s, client, _ := newTestStore()
	ctx := context.Background()
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("down")
	})
	if s.Live(ctx, "whatever") {
		t.Fatal("Live returned true on API error (should fail closed)")
	}
}

func TestLiveNotFoundCached(t *testing.T) {
	s, client, _ := newTestStore()
	ctx := context.Background()
	var gets int
	client.PrependReactor("get", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "secrets"}, "x")
	})
	// Two reads: the first caches the NotFound, the second must be served
	// from that negative cache without a second Get.
	first := s.Live(ctx, "sid-missing")
	second := s.Live(ctx, "sid-missing")
	if first || second {
		t.Fatal("missing record read as live")
	}
	if gets != 1 {
		t.Fatalf("expected NotFound cached (1 Get), got %d", gets)
	}
}

func TestDeleteBySubjectRevokes(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()
	_ = s.Create(ctx, "sid1", "sub-a", "a@example.com", "h", epoch, epoch.Add(time.Hour))
	_ = s.Create(ctx, "sid2", "sub-a", "a@example.com", "h", epoch, epoch.Add(time.Hour))
	_ = s.Create(ctx, "sid3", "sub-b", "b@example.com", "h", epoch, epoch.Add(time.Hour))

	if err := s.DeleteBySubject(ctx, "sub-a"); err != nil {
		t.Fatalf("DeleteBySubject: %v", err)
	}
	if s.Live(ctx, "sid1") || s.Live(ctx, "sid2") {
		t.Fatal("sub-a sessions still live after revoke")
	}
	if !s.Live(ctx, "sid3") {
		t.Fatal("unrelated session sub-b revoked")
	}
}

func TestDeleteBySubjectByEmail(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()
	_ = s.Create(ctx, "sid1", "sub-a", "a@example.com", "h", epoch, epoch.Add(time.Hour))
	// Removing a user is keyed by email, not Dex subject.
	if err := s.DeleteBySubject(ctx, "a@example.com"); err != nil {
		t.Fatalf("DeleteBySubject by email: %v", err)
	}
	if s.Live(ctx, "sid1") {
		t.Fatal("session not revoked when keyed by email")
	}
}

func TestDeleteAll(t *testing.T) {
	s, _, _ := newTestStore()
	ctx := context.Background()
	_ = s.Create(ctx, "sid1", "sub-a", "a@example.com", "h", epoch, epoch.Add(time.Hour))
	_ = s.Create(ctx, "sid2", "sub-b", "b@example.com", "h", epoch, epoch.Add(time.Hour))
	if err := s.DeleteAll(ctx); err != nil {
		t.Fatalf("DeleteAll: %v", err)
	}
	if s.Live(ctx, "sid1") || s.Live(ctx, "sid2") {
		t.Fatal("sessions survived DeleteAll")
	}
}

func TestPositiveCacheExpires(t *testing.T) {
	s, client, clock := newTestStore()
	ctx := context.Background()
	_ = s.Create(ctx, "sid1", "sub-a", "a@example.com", "h", epoch, epoch.Add(time.Hour))
	// Delete the underlying record straight through the API, bypassing the store
	// so the positive cache is untouched.
	_ = client.CoreV1().Secrets("kipper-system").Delete(ctx, recordName("sid1"), metav1.DeleteOptions{})
	if !s.Live(ctx, "sid1") {
		t.Fatal("expected still-cached-live within TTL")
	}
	*clock = clock.Add(recordCacheTTL + time.Second)
	if s.Live(ctx, "sid1") {
		t.Fatal("stale positive cache outlived its TTL")
	}
}

func TestRecordCarriesBothHashes(t *testing.T) {
	s, client, _ := newTestStore()
	ctx := context.Background()
	_ = s.Create(ctx, "sid1", "sub-a", "a@example.com", "h", epoch, epoch.Add(time.Hour))
	sec, err := client.CoreV1().Secrets("kipper-system").Get(ctx, recordName("sid1"), metav1.GetOptions{})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if sec.Labels[subjectHashLabel] != hashLabel("sub-a") {
		t.Fatal("subject hash label missing/wrong")
	}
	if sec.Labels[emailHashLabel] != hashLabel("a@example.com") {
		t.Fatal("email hash label missing/wrong")
	}
	if _, ok := sec.Data["email"]; !ok {
		t.Fatal("record missing email payload")
	}
}
