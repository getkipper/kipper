package controllers

import (
	"context"
	"errors"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/controller/pkg/routename"
)

type routeNameDecision int

const (
	routeNameAdmitted routeNameDecision = iota
	routeNamePending
	routeNameTaken
	routeNameReservedForPlatform
)

// errRouteNameRace is returned when another writer changed a reservation
// while this one was taking it over; the caller looks again shortly.
var errRouteNameRace = errors.New("the route name reservation changed while it was being taken over")

// admitNewRoute decides whether a route that does not exist yet may be
// published for the workload namespace/name. When it is admitted through the
// gate, defer release until publication finishes, including failures. A nil gate
// leaves tenant names unchecked.
func admitNewRoute(ctx context.Context, gate *RouteNameGate, reader client.Reader, writer client.Client, namespace, name string) (routeNameDecision, func(), error) {
	key := routename.Key(namespace, name)
	if _, ok := routename.Platform(key); ok {
		return routeNameReservedForPlatform, nil, nil
	}
	if gate == nil {
		return routeNameAdmitted, nil, nil
	}
	release, ok := gate.Admit()
	if !ok {
		return routeNamePending, nil, nil
	}
	decision, err := decideNewRoute(ctx, reader, writer, namespace, name, key)
	if err != nil || decision != routeNameAdmitted {
		release()
		return decision, nil, err
	}
	return routeNameAdmitted, release, nil
}

func decideNewRoute(ctx context.Context, reader client.Reader, writer client.Client, namespace, name, key string) (routeNameDecision, error) {
	conflict, err := sameNamespaceConflict(ctx, reader, namespace, name, key)
	if err != nil {
		return routeNameTaken, err
	}
	if conflict {
		return routeNameTaken, nil
	}
	owned, err := reserveRouteName(ctx, reader, writer, namespace, key)
	if errors.Is(err, errRouteNameRace) {
		return routeNamePending, nil
	}
	if err != nil {
		return routeNameTaken, err
	}
	if owned {
		return routeNameAdmitted, nil
	}
	shared, err := sharesRouteName(ctx, reader, namespace, key)
	if err != nil {
		return routeNameTaken, err
	}
	if shared {
		return routeNameAdmitted, nil
	}
	return routeNameTaken, nil
}

// sameNamespaceConflict reports whether another App or stateful service in the
// namespace has a name that produces the same key, such as "prod-web" next to
// "prod--web". Traefik would route both under one name.
func sameNamespaceConflict(ctx context.Context, reader client.Reader, namespace, name, key string) (bool, error) {
	var apps kipperv1.AppList
	if err := reader.List(ctx, &apps, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for _, a := range apps.Items {
		if a.Name != name && routename.Key(namespace, a.Name) == key {
			return true, nil
		}
	}
	var services kipperv1.ServiceList
	if err := reader.List(ctx, &services, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for _, s := range services.Items {
		if s.Name != name && routename.Key(namespace, s.Name) == key {
			return true, nil
		}
	}
	return false, nil
}

// sharesRouteName reports whether namespace is a grandfathered participant in
// a shared key: its holder, or a sharer whose namespace still has the UID it
// had when it was recorded.
func sharesRouteName(ctx context.Context, reader client.Reader, namespace, key string) (bool, error) {
	var cm corev1.ConfigMap
	err := reader.Get(ctx, types.NamespacedName{Name: routeNameClaimName(key), Namespace: routeClaimNamespace}, &cm)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	claim, err := parseRouteNameClaim(&cm)
	if err != nil || len(claim.Shared) == 0 {
		return false, err
	}
	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		return false, err
	}
	holder := claim.Namespace == namespace && claim.UID != "" && claim.UID == string(ns.UID)
	return holder || claim.sharedBy(namespace, ns.UID), nil
}
