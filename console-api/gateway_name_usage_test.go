package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func ingressFor(host string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "route", Namespace: "shop"},
		Spec:       networkingv1.IngressSpec{Rules: []networkingv1.IngressRule{{Host: host}}},
	}
}

func unusedRecord(t *testing.T, client *fake.Clientset) string {
	t.Helper()
	secret, err := client.CoreV1().Secrets(gatewayCredentialsNamespace).Get(context.Background(), gatewayCredentialsSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return secret.Annotations[nameUnusedAnnotation]
}

func setUnusedRecord(t *testing.T, client *fake.Clientset, value string) {
	t.Helper()
	secrets := client.CoreV1().Secrets(gatewayCredentialsNamespace)
	secret, err := secrets.Get(context.Background(), gatewayCredentialsSecret, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	secret.Annotations = map[string]string{nameUnusedAnnotation: value}
	if _, err := secrets.Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func addIngress(t *testing.T, client *fake.Clientset, host string) {
	t.Helper()
	if _, err := client.NetworkingV1().Ingresses("shop").Create(context.Background(), ingressFor(host), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestHostUnderName(t *testing.T) {
	for host, want := range map[string]bool{
		"myapp.kipper.run":          true,
		"api--myapp.kipper.run":     true,
		"API--MyApp.kipper.run.":    true,
		"api--other.kipper.run":     false,
		"xmyapp.kipper.run":         false,
		"api.myapp.example.com":     false,
		"myapp.kipper.run.evil.com": false,
	} {
		if got := hostUnderName(host, "myapp"); got != want {
			t.Errorf("hostUnderName(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestBeatRenewsWhileAnIngressServesTheName(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"pin": "active"}}
	h, client := heartbeatUnderTest(t, gw)
	addIngress(t, client, "api--myapp.kipper.run")

	h.beat(context.Background())

	if len(gw.requests) != 1 {
		t.Fatalf("a name in use must be renewed, got %d requests", len(gw.requests))
	}
	if got := unusedRecord(t, client); got != "" {
		t.Errorf("a name in use carries no unused record, got %q", got)
	}
}

func TestBeatStopsRenewingANameUnusedForADay(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"pin": "active"}}
	h, client := heartbeatUnderTest(t, gw)
	addIngress(t, client, "api.example.com")
	start := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	now := start
	h.now = func() time.Time { return now }

	h.beat(context.Background())
	if len(gw.requests) != 1 {
		t.Fatalf("the first unused beat still renews, got %d requests", len(gw.requests))
	}
	if got := unusedRecord(t, client); got != "myapp@"+start.Format(time.RFC3339) {
		t.Errorf("the unused record must name the label and the time, got %q", got)
	}

	now = start.Add(nameUnusedGrace + time.Minute)
	next := h.beat(context.Background())

	if len(gw.requests) != 1 {
		t.Errorf("a name unused for longer than the grace must not be renewed, got %d requests", len(gw.requests))
	}
	if next != nameUnusedRecheck {
		t.Errorf("an unused name is rechecked every %v, got %v", nameUnusedRecheck, next)
	}
}

func TestBeatResumesRenewingWhenAHostComesBack(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"pin": "active"}}
	h, client := heartbeatUnderTest(t, gw)
	setUnusedRecord(t, client, "myapp@2026-09-01T00:00:00Z")
	addIngress(t, client, "shop--myapp.kipper.run")

	h.beat(context.Background())

	if len(gw.requests) != 1 {
		t.Errorf("a name in use again must be renewed, got %d requests", len(gw.requests))
	}
	if got := unusedRecord(t, client); got != "" {
		t.Errorf("the unused record must be cleared, got %q", got)
	}
}

func TestBeatKeepsRenewingWhenIngressesCannotBeRead(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"pin": "active"}}
	h, client := heartbeatUnderTest(t, gw)
	setUnusedRecord(t, client, "myapp@2026-09-01T00:00:00Z")
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	failing := true
	client.PrependReactor("list", "ingresses", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing {
			return true, nil, errors.New("api server unavailable")
		}
		return false, nil, nil
	})

	h.beat(context.Background())
	if len(gw.requests) != 1 {
		t.Fatalf("an unreadable cluster must keep renewing, got %d requests", len(gw.requests))
	}

	failing = false
	now = now.Add(5 * time.Minute)
	h.beat(context.Background())

	if len(gw.requests) != 2 {
		t.Errorf("the first readable beat after a failure must still renew, got %d requests", len(gw.requests))
	}
	if got := unusedRecord(t, client); got != "myapp@"+now.Format(time.RFC3339) {
		t.Errorf("the grace must restart after the failure, got %q", got)
	}
}

func TestBeatRestartsTheGraceWhenClearingTheRecordFails(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"pin": "active"}}
	h, client := heartbeatUnderTest(t, gw)
	setUnusedRecord(t, client, "myapp@2026-09-01T00:00:00Z")
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	addIngress(t, client, "api--myapp.kipper.run")
	failUpdate := true
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failUpdate {
			return true, nil, errors.New("conflict")
		}
		return false, nil, nil
	})

	h.beat(context.Background())

	failUpdate = false
	if err := client.NetworkingV1().Ingresses("shop").Delete(context.Background(), "route", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * time.Minute)
	h.beat(context.Background())

	if len(gw.requests) != 2 {
		t.Errorf("a stale record must not stop renewal right after the name was seen in use, got %d requests", len(gw.requests))
	}
	if got := unusedRecord(t, client); got != "myapp@"+now.Format(time.RFC3339) {
		t.Errorf("the grace must restart, got %q", got)
	}
}

func TestBeatIgnoresAnUnusedRecordForAnotherName(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"pin": "active"}}
	h, client := heartbeatUnderTest(t, gw)
	setUnusedRecord(t, client, "oldname@2026-09-01T00:00:00Z")
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }

	h.beat(context.Background())

	if len(gw.requests) != 1 {
		t.Errorf("a record for another name must not stop renewal, got %d requests", len(gw.requests))
	}
	if got := unusedRecord(t, client); got != "myapp@"+now.Format(time.RFC3339) {
		t.Errorf("the record must restart for this name, got %q", got)
	}
}

// Restarting loses in-memory invalidation state, so a stored record may be stale.
func TestBeatDoesNotTrustARecordFromBeforeARestart(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"pin": "active"}}
	h, client := heartbeatUnderTest(t, gw)
	setUnusedRecord(t, client, "myapp@2026-09-01T00:00:00Z")
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }

	h.beat(context.Background())

	if len(gw.requests) != 1 {
		t.Errorf("a new process must renew rather than trust an old record, got %d requests", len(gw.requests))
	}
	if got := unusedRecord(t, client); got != "myapp@"+now.Format(time.RFC3339) {
		t.Errorf("the grace must restart, got %q", got)
	}
}

// Only the exact record this process wrote is trusted, so an older one put back
// in its place, by a restore for example, restarts the grace.
func TestBeatDoesNotTrustARecordReplacedSinceItWasWritten(t *testing.T) {
	gw := &fakeGateway{t: t, status: http.StatusCreated, body: map[string]string{"pin": "active"}}
	h, client := heartbeatUnderTest(t, gw)
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	h.now = func() time.Time { return now }
	h.beat(context.Background())

	setUnusedRecord(t, client, "myapp@2026-09-01T00:00:00Z")
	now = now.Add(5 * time.Minute)
	h.beat(context.Background())

	if len(gw.requests) != 2 {
		t.Errorf("a replaced record must not stop renewal, got %d requests", len(gw.requests))
	}
	if got := unusedRecord(t, client); got != "myapp@"+now.Format(time.RFC3339) {
		t.Errorf("the grace must restart, got %q", got)
	}
}
