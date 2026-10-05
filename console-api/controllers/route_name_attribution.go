package controllers

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/controller/pkg/routename"
)

// AttributionState says whether an app's traffic can be told apart from
// everyone else's.
type AttributionState string

const (
	// AttributionHeld: the app's namespace holds its route name alone; Spans
	// lists when.
	AttributionHeld AttributionState = "held"
	// AttributionShared: another workload produces the same name.
	AttributionShared AttributionState = "shared"
	// AttributionOther: another namespace, or an earlier namespace of the same
	// name, holds it, or a platform route may produce it.
	AttributionOther AttributionState = "other"
	// AttributionNone: no claim is recorded for the name.
	AttributionNone AttributionState = "none"
)

// AttributionSpan is a time range in which the app was the only producer of
// its route name.
type AttributionSpan struct {
	From time.Time
	To   time.Time
}

// Attribution is what RouteNameAttribution found. Spans is empty unless the
// state is AttributionHeld.
type Attribution struct {
	State AttributionState
	Spans []AttributionSpan
}

// RouteNameAttribution returns exclusive ownership spans for the current namespace
// UID. Open spans end at the sweeper's last heartbeat and require a heartbeat.
func RouteNameAttribution(ctx context.Context, reader client.Reader, namespace, app string) (Attribution, error) {
	key := routename.Key(namespace, app)
	// Read in the reverse of the sweeper's write order to avoid pairing an
	// older claim with a newer heartbeat. This requires an uncached reader.
	marker, found, err := readRouteNameMarker(ctx, reader)
	if err != nil {
		return Attribution{}, err
	}
	var cm corev1.ConfigMap
	err = reader.Get(ctx, types.NamespacedName{Name: routeNameClaimName(key), Namespace: routeClaimNamespace}, &cm)
	if apierrors.IsNotFound(err) {
		return Attribution{State: AttributionNone}, nil
	}
	if err != nil {
		return Attribution{}, fmt.Errorf("reading route name claim for %s: %w", key, err)
	}
	claim, err := parseRouteNameClaim(&cm)
	if err != nil {
		return Attribution{}, err
	}
	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		return Attribution{}, fmt.Errorf("reading namespace %s: %w", namespace, err)
	}
	holder := claim.Namespace == namespace && claim.UID != "" && claim.UID == string(ns.UID)
	switch {
	case holder && len(claim.Shared) > 0, claim.sharedBy(namespace, ns.UID):
		return Attribution{State: AttributionShared}, nil
	case !holder, claim.Occupied:
		return Attribution{State: AttributionOther}, nil
	}

	var openEnd time.Time
	if found {
		openEnd = marker.Heartbeat
	}
	out := Attribution{State: AttributionHeld}
	for _, iv := range claim.Intervals {
		span := AttributionSpan{From: iv.From}
		switch {
		case iv.To != nil:
			span.To = *iv.To
		case openEnd.IsZero():
			continue
		default:
			span.To = openEnd
		}
		if span.To.After(span.From) {
			out.Spans = append(out.Spans, span)
		}
	}
	return out, nil
}
