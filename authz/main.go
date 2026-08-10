// kipper-authz is the data plane for API-key-gated routes. Traefik
// forwardAuth calls /authorize for every request to a gated app; the
// service validates the key against an informer cache, applies the plan's
// token bucket per replica, checks the period quota, and buffers usage
// counters that flush to UsageRollup CRs in batches. It never queries the
// API server on the request path, and it fails closed whenever its view of
// keys and plans cannot be proven fresh.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("authz: %v", err)
	}
}

func run() error {
	// Decision and mutation forensics go out as JSON so Loki can parse the
	// fields (namespace, app, key prefix, reason, client IP).
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	scheme := runtime.NewScheme()
	utilruntime.Must(kipperv1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		// controller-runtime's own metrics endpoint stays off; the
		// service serves its purpose-built metrics itself.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return fmt.Errorf("building manager: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Index keys by prefix and rollups by key so the request path is
	// indexed lookups only.
	if err := mgr.GetFieldIndexer().IndexField(ctx, &kipperv1.ApiKey{}, keyPrefixField,
		func(o client.Object) []string { return []string{o.(*kipperv1.ApiKey).Spec.Prefix} }); err != nil {
		return fmt.Errorf("indexing api keys: %w", err)
	}
	if err := mgr.GetFieldIndexer().IndexField(ctx, &kipperv1.UsageRollup{}, rollupKeyField,
		func(o client.Object) []string { return []string{o.(*kipperv1.UsageRollup).Spec.KeyPrefix} }); err != nil {
		return fmt.Errorf("indexing rollups: %w", err)
	}

	// Warm the cache for the request-path types before serving.
	for _, obj := range []client.Object{&kipperv1.ApiKey{}, &kipperv1.UsagePlan{}, &kipperv1.UsageRollup{}} {
		if _, err := mgr.GetCache().GetInformer(ctx, obj); err != nil {
			return fmt.Errorf("starting informer for %T: %w", obj, err)
		}
	}

	// The initial sync completes once; afterwards freshness is carried by
	// the probe clock alone.
	var synced atomic.Bool
	go func() {
		if mgr.GetCache().WaitForCacheSync(ctx) {
			synced.Store(true)
			log.Printf("authz: informer cache synced")
		}
	}()

	canaryKey := client.ObjectKey{
		Namespace: envString("AUTHZ_CANARY_NAMESPACE", "kipper-system"),
		Name:      envString("AUTHZ_CANARY_NAME", "authz-canary"),
	}
	// One canary per request-path type, so a wedged watch on any of them stalls
	// the clock and fails the replica closed rather than serving stale data.
	canaries := []canaryTarget{
		{key: canaryKey, template: &kipperv1.ApiKey{}},
		{key: canaryKey, template: &kipperv1.UsagePlan{}},
		{key: canaryKey, template: &kipperv1.UsageRollup{}},
	}
	freshness := NewFreshness(
		mgr.GetAPIReader(),
		mgr.GetClient(),
		synced.Load,
		envDuration("AUTHZ_PROBE_INTERVAL", 30*time.Second),
		envDuration("AUTHZ_STALE_BOUND", 90*time.Second),
		canaries,
		envString("HOSTNAME", "authz"),
	)
	globalFreshness = freshness

	usage := NewUsageBuffer()
	flusher := NewFlusher(mgr.GetClient(), mgr.GetAPIReader(), usage, envDuration("AUTHZ_FLUSH_INTERVAL", 30*time.Second))
	authorizer := NewAuthorizer(mgr.GetClient(), freshness, usage)
	server := NewServer(authorizer, freshness)

	mux := http.NewServeMux()
	server.Routes(mux)
	mux.Handle("GET /metrics", promhttp.Handler())

	httpServer := &http.Server{
		Addr:    ":8080",
		Handler: mux,
		// The whole point of forwardAuth is a fast answer; a slow authz
		// must fail, not queue.
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
	}

	go func() {
		if err := mgr.Start(ctx); err != nil {
			log.Fatalf("authz: manager: %v", err)
		}
	}()
	go freshness.Run(ctx)
	go flusher.Run(ctx)

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	log.Printf("authz: serving on %s", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

func envString(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		log.Printf("authz: invalid %s=%q, using %s", name, raw, fallback)
		return fallback
	}
	return d
}
