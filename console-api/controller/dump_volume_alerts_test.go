package controller

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/handlers"
	"github.com/getkipper/kipper/controller/pkg/servicecatalog"
)

const faultedDumpMessage = "The dump volume is faulted, so consistent dumps fail and the database pod cannot start until the dump volume is replaced."

func serviceWithDumpVolume(faulted bool, at time.Time) *kipperv1.Service {
	svc := &kipperv1.Service{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop", UID: "db-1"}}
	if faulted {
		svc.Status.Conditions = []metav1.Condition{{
			Type: servicecatalog.ConditionDumpVolumeReady, Status: metav1.ConditionFalse, Reason: "Faulted",
			Message: faultedDumpMessage, LastTransitionTime: metav1.Time{Time: at},
		}}
	}
	return svc
}

func dumpServiceClient(objs ...crclient.Object) crclient.Client {
	return crfake.NewClientBuilder().WithScheme(testScheme()).WithObjects(objs...).WithStatusSubresource(&kipperv1.Service{}).Build()
}

func setService(t *testing.T, rc *ResourceController, svc *kipperv1.Service) {
	t.Helper()
	var current kipperv1.Service
	if err := rc.crClient.Get(context.Background(), crclient.ObjectKeyFromObject(svc), &current); err != nil {
		t.Fatal(err)
	}
	current.Status = svc.Status
	if err := rc.crClient.Status().Update(context.Background(), &current); err != nil {
		t.Fatal(err)
	}
}

func dumpFaultEntries(batches []alertBatch) []ResourceLogEntry {
	var out []ResourceLogEntry
	for _, e := range entriesOf(batches) {
		if e.Action == "dump volume faulted" {
			out = append(out, e)
		}
	}
	return out
}

func storedAlerts(t *testing.T, client *fake.Clientset) []handlers.Alert {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps("kipper-system").Get(context.Background(), "kipper-alerts", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected the alert store to be written: %v", err)
	}
	var alerts []handlers.Alert
	if err := json.Unmarshal([]byte(cm.Data["alerts"]), &alerts); err != nil {
		t.Fatal(err)
	}
	return alerts
}

// TestTickAlertsWhenADumpVolumeIsFaulted checks that faults reach the alert store.
// Operators need a warning while the database pod is still running because a
// faulted dump volume prevents it from starting again after a restart.
func TestTickAlertsWhenADumpVolumeIsFaulted(t *testing.T) {
	client := fake.NewClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "kipper-mode", Namespace: "kipper-system"},
		Data:       map[string]string{"mode": "expert"},
	})
	rc := NewResourceController(client, dumpServiceClient(serviceWithDumpVolume(true, time.Now().Add(-time.Minute))))

	rc.tick(context.Background())

	alerts := storedAlerts(t, client)
	if len(alerts) != 1 {
		t.Fatalf("expected one alert for the faulted dump volume, got %+v", alerts)
	}
	if a := alerts[0]; a.Action != "dump volume faulted" || a.Severity != "warning" || a.App != "db" || a.Namespace != "shop" ||
		a.Reason != faultedDumpMessage {
		t.Fatalf("unexpected alert %+v", a)
	}
	rc.tick(context.Background())
	if again := storedAlerts(t, client); len(again) != 1 {
		t.Fatalf("the next tick adds nothing for the same episode, got %+v", again)
	}
}

func TestCheckDumpVolumesWarnsOncePerEpisode(t *testing.T) {
	at := time.Now().Add(-time.Minute)
	rc := NewResourceController(fake.NewClientset(), dumpServiceClient(serviceWithDumpVolume(true, at)))
	ctx := context.Background()

	batches := rc.checkDumpVolumes(ctx)
	if entries := dumpFaultEntries(batches); len(entries) != 1 {
		t.Fatalf("expected one warning, got %+v", entriesOf(batches))
	}
	if again := dumpFaultEntries(rc.checkDumpVolumes(ctx)); len(again) != 1 {
		t.Fatalf("an alert that was never stored stays due, got %+v", again)
	}
	rc.commitBatches(batches)
	if again := dumpFaultEntries(rc.checkDumpVolumes(ctx)); len(again) != 0 {
		t.Fatalf("one episode warns once, got %+v", again)
	}

	setService(t, rc, serviceWithDumpVolume(false, at))
	if again := dumpFaultEntries(rc.checkDumpVolumes(ctx)); len(again) != 0 {
		t.Fatalf("a healthy dump volume warns about nothing, got %+v", again)
	}
	setService(t, rc, serviceWithDumpVolume(true, time.Now()))
	if again := dumpFaultEntries(rc.checkDumpVolumes(ctx)); len(again) != 1 {
		t.Fatalf("recovery ends the episode, so a new fault warns again, got %+v", again)
	}
}

// TestCheckDumpVolumesWarnsAgainForAFaultAfterAnUnseenRecovery covers recovery
// between checks. The service reconciler removes the condition on recovery and
// sets it on the next fault, giving the new episode its own transition time.
func TestCheckDumpVolumesWarnsAgainForAFaultAfterAnUnseenRecovery(t *testing.T) {
	rc := NewResourceController(fake.NewClientset(), dumpServiceClient(serviceWithDumpVolume(true, time.Now().Add(-time.Hour))))
	ctx := context.Background()
	rc.commitBatches(rc.checkDumpVolumes(ctx))

	setService(t, rc, serviceWithDumpVolume(true, time.Now()))
	if entries := dumpFaultEntries(rc.checkDumpVolumes(ctx)); len(entries) != 1 {
		t.Fatalf("a fault with a new transition time is a new episode, got %+v", entries)
	}
}

func TestCheckDumpVolumesTreatsAReplacedServiceAsANewEpisode(t *testing.T) {
	first := serviceWithDumpVolume(true, time.Now().Add(-time.Hour))
	rc := NewResourceController(fake.NewClientset(), dumpServiceClient(first))
	ctx := context.Background()
	rc.commitBatches(rc.checkDumpVolumes(ctx))

	if err := rc.crClient.Delete(ctx, first); err != nil {
		t.Fatal(err)
	}
	second := serviceWithDumpVolume(true, time.Now())
	second.UID = "db-2"
	if err := rc.crClient.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	setService(t, rc, second)
	if entries := dumpFaultEntries(rc.checkDumpVolumes(ctx)); len(entries) != 1 {
		t.Fatalf("a replaced Service is a new episode, got %+v", entries)
	}
}

// TestCheckDumpVolumesDoesNotRepeatAStoredAlert covers deduplication after a
// restart, when stored alerts identify episodes reported by the previous leader
// and the in-memory marks are empty.
func TestCheckDumpVolumesDoesNotRepeatAStoredAlert(t *testing.T) {
	at := time.Now().Add(-10 * time.Minute)
	stored := handlers.Alert{
		Time: at.Add(time.Minute).UTC().Format(time.RFC3339), App: "db", Namespace: "shop",
		Action: "dump volume faulted", Severity: "warning", Reason: faultedDumpMessage,
	}
	rc := NewResourceController(fake.NewClientset(alertStore(t, stored)), dumpServiceClient(serviceWithDumpVolume(true, at)))
	if entries := dumpFaultEntries(rc.checkDumpVolumes(context.Background())); len(entries) != 0 {
		t.Fatalf("an episode the store already holds is not reported again, got %+v", entries)
	}

	rc = NewResourceController(fake.NewClientset(alertStore(t, stored)), dumpServiceClient(serviceWithDumpVolume(true, at.Add(5*time.Minute))))
	if entries := dumpFaultEntries(rc.checkDumpVolumes(context.Background())); len(entries) != 1 {
		t.Fatalf("an alert from before the episode began does not cover it, got %+v", entries)
	}
}
