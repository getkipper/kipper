package cmd

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestCheckKipperRunHost(t *testing.T) {
	assert.NoError(t, checkKipperRunHost("shop.example.com", "lab.kipper.run"))
	assert.NoError(t, checkKipperRunHost("lab.kipper.run", "lab.kipper.run"))
	assert.NoError(t, checkKipperRunHost("api--lab.kipper.run", "lab.kipper.run"))
	assert.ErrorContains(t, checkKipperRunHost("api--other.kipper.run", "lab.kipper.run"), "lab.kipper.run")
	assert.ErrorContains(t, checkKipperRunHost("api--lab.kipper.run", "example.com"), "kipper.run")
	assert.Error(t, checkKipperRunHost("api.foo--lab.kipper.run", "lab.kipper.run"), "the gateway certificate covers one label under kipper.run")
}

func TestDerivedRouteHost(t *testing.T) {
	assert.Equal(t, "api--lab.kipper.run", derivedRouteHost("api", "", "lab.kipper.run"))
	assert.Equal(t, "api--lab.kipper.run", derivedRouteHost("api", "default", "lab.kipper.run"))
	assert.Equal(t, "api-prod--lab.kipper.run", derivedRouteHost("api", "prod", "lab.kipper.run"))
	assert.Equal(t, "api-prod.example.com", derivedRouteHost("api", "prod", "example.com"))
}

func TestWriteRouteShowsADerivedHost(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "api"},
		"spec":     map[string]interface{}{"route": map[string]interface{}{"path": "/"}},
	}}
	var out bytes.Buffer
	writeRouteWithHost(&out, app, "api--lab.kipper.run")

	assert.Contains(t, out.String(), "  Host:   api--lab.kipper.run (derived from the app name)\n")
	assert.Contains(t, out.String(), "  URL:    https://api--lab.kipper.run/\n")
}

func TestRouteOutcomeForADerivedHost(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "api", "generation": int64(2)},
		"spec":     map[string]interface{}{"route": map[string]interface{}{"path": "/"}},
		"status": map[string]interface{}{"conditions": []interface{}{
			map[string]interface{}{"type": "ChildrenAdopted", "status": "True", "observedGeneration": int64(2)},
		}},
	}}
	line, done := routeOutcome(routeView{app: app, derivedHost: "api--lab.kipper.run", ingressHosts: []string{"api--lab.kipper.run"}})

	assert.True(t, done)
	assert.NotContains(t, line, "without a public URL")
	assert.Contains(t, line, "https://api--lab.kipper.run/")
}

func TestWaitForRouteWithdrawn(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	gone := func(context.Context, string) bool { return true }
	assert.True(t, waitForRouteWithdrawn(ctx, gone, "api", time.Millisecond))

	short, cancelShort := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancelShort()
	stays := func(_ context.Context, name string) bool { return name != "api" }
	assert.False(t, waitForRouteWithdrawn(short, stays, "api", time.Millisecond))
}

func TestWaitForRouteWithdrawnEndsAStalledRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	stalled := func(ctx context.Context, _ string) bool {
		<-ctx.Done()
		return false
	}
	done := make(chan bool)
	go func() { done <- waitForRouteWithdrawn(ctx, stalled, "api", time.Millisecond) }()

	select {
	case got := <-done:
		assert.False(t, got)
	case <-time.After(time.Second):
		t.Fatal("the wait outlived its deadline")
	}
}

func TestWriteRoute(t *testing.T) {
	app := &unstructured.Unstructured{Object: map[string]interface{}{
		"metadata": map[string]interface{}{"name": "api"},
		"spec": map[string]interface{}{"route": map[string]interface{}{
			"host": "api.example.com", "path": "/v1", "basicAuth": true, "internalPaths": []interface{}{"/actuator"},
		}},
		"status": map[string]interface{}{"conditions": []interface{}{
			map[string]interface{}{"type": "RouteReady", "status": "False", "reason": "HostClaimed", "message": "another project holds api.example.com"},
		}},
	}}
	var out bytes.Buffer
	writeRoute(&out, app)
	got := out.String()

	assert.Contains(t, got, "  Host:   api.example.com\n")
	assert.Contains(t, got, "  Path:   /v1\n")
	assert.Contains(t, got, "  URL:    https://api.example.com/v1\n")
	assert.Contains(t, got, "  Gates:  basic auth; internal paths /actuator\n")
	assert.Contains(t, got, "  ✘  RouteReady: HostClaimed\n           another project holds api.example.com\n")
}

func TestWriteRouteForAnAppWithoutOne(t *testing.T) {
	var out bytes.Buffer
	writeRoute(&out, &unstructured.Unstructured{Object: map[string]interface{}{"metadata": map[string]interface{}{"name": "worker"}, "spec": map[string]interface{}{}}})

	assert.Contains(t, out.String(), "worker has no route")
}

func TestConfirmRouteRemoval(t *testing.T) {
	assert.NoError(t, confirmRouteRemoval(strings.NewReader(""), false, true))
	assert.ErrorContains(t, confirmRouteRemoval(strings.NewReader(""), false, false), "--yes")
	assert.NoError(t, confirmRouteRemoval(strings.NewReader("y\n"), true, false))
	assert.Error(t, confirmRouteRemoval(strings.NewReader("n\n"), true, false))
}

func TestWriteRouteSaysTheRouteIsClosed(t *testing.T) {
	route := func(extra map[string]interface{}) *unstructured.Unstructured {
		r := map[string]interface{}{"host": "pay.example.com", "closed": true}
		for k, v := range extra {
			r[k] = v
		}
		return &unstructured.Unstructured{Object: map[string]interface{}{
			"metadata": map[string]interface{}{"name": "payments"},
			"spec":     map[string]interface{}{"route": r},
		}}
	}
	var out bytes.Buffer
	writeRoute(&out, route(map[string]interface{}{"publicPaths": []interface{}{"/webhooks/stripe"}}))
	assert.Contains(t, out.String(), "  Closed: configured to publish only /webhooks/stripe\n")

	out.Reset()
	writeRoute(&out, route(nil))
	assert.Contains(t, out.String(), "  Closed: configured with no public paths\n")
	assert.NotContains(t, out.String(), "nothing is published", "the spec alone cannot say the old route is gone")

	out.Reset()
	writeRoute(&out, route(map[string]interface{}{"path": "/pay/", "publicPaths": []interface{}{"/webhooks/stripe", "/webhooks/paypal"}}))
	assert.Contains(t, out.String(), "  Closed: configured to publish only /pay/webhooks/stripe, /pay/webhooks/paypal\n", "public paths sit under the route's path")

	rejected := route(map[string]interface{}{"path": "/pay//v1", "publicPaths": []interface{}{"/hook"}})
	rejected.Object["status"] = map[string]interface{}{"conditions": []interface{}{map[string]interface{}{
		"type": "RouteReady", "status": "False", "reason": "ClosedWithoutPublicPaths", "message": "closed route has no usable public paths; removing its Ingresses",
	}}}
	out.Reset()
	writeRoute(&out, rejected)
	assert.Contains(t, out.String(), "ClosedWithoutPublicPaths")
	assert.NotContains(t, out.String(), " is published")
}
