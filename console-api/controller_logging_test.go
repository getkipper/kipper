package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// A reconcile error must reach the same handler everything else in console-api
// logs through. Until controller-runtime is given a logger its root sink is a
// NullLogSink, so a reconciler failing on every pass leaves no record anywhere.
//
// This drives controllerLogger rather than the global that configureControllerLogging
// sets, because SetLogger is a one-way latch: the first call in a process wins,
// so a test that asserted through the global would pass once and fail on the
// second run of the same binary. What the latch itself does is not covered here.
func TestControllerLogger_ReconcileErrorReachesTheHandler(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})))

	controllerLogger().Error(errors.New("was not created by Kipper"), "reconciling middleware", "app", "billing-api")

	logged := out.String()
	if !strings.Contains(logged, "reconciling middleware") {
		t.Fatalf("the controller's error never reached the handler; handler saw %q", logged)
	}
	if !strings.Contains(logged, "was not created by Kipper") {
		t.Fatalf("the error text was dropped on the way; handler saw %q", logged)
	}
	if !strings.Contains(logged, "billing-api") {
		t.Fatalf("the structured field naming the workload was dropped; handler saw %q", logged)
	}
}

// The workload a reconcile error names must stay a queryable field rather than
// being flattened into the message, which is what makes the log useful in Loki.
//
// This also pins the adapter to whatever handler is installed when it runs. A
// handler captured at init still reaches the same place, because slog.SetDefault
// redirects the legacy log package into the new handler, but it arrives having
// been formatted as text — so the structure, not the destination, is what
// distinguishes the two.
func TestControllerLogger_KeepsStructuredFieldsQueryable(t *testing.T) {
	var out bytes.Buffer
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	slog.SetDefault(slog.New(slog.NewJSONHandler(&out, &slog.HandlerOptions{Level: slog.LevelInfo})))

	controllerLogger().Error(errors.New("was not created by Kipper"), "reconciling middleware", "app", "billing-api")

	var record map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(out.Bytes()), &record); err != nil {
		t.Fatalf("the handler received something other than one JSON record: %v; it saw %q", err, out.String())
	}
	if record["app"] != "billing-api" {
		t.Fatalf("app should be a top-level field, queryable on its own; the record was %v", record)
	}
}

// The manager's metrics are what make a reconcile that fails on every pass
// countable, and therefore alertable. Disabled, the counter exists in the
// process and reaches nobody.
func TestMetricsOptions_AreServedRatherThanDisabled(t *testing.T) {
	got := metricsOptions().BindAddress
	if got == "0" || got == "" {
		t.Fatalf("the controller manager's metrics must be served for anything to alert on them; BindAddress is %q", got)
	}
	// The installer renders a container port and a Service that target this
	// number from another module, where the compiler cannot check it.
	if got != ":8081" {
		t.Fatalf("the manifest in kip/internal/installer targets 8081; BindAddress is %q", got)
	}
}
