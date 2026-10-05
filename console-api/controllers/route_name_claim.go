package controllers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/controller/pkg/routename"
)

// ConfigMaps reserve route-name keys cluster-wide against new collisions.
// Existing collisions are recorded as shared claims. Namespace UIDs keep
// recreated namespaces from inheriting claims or traffic history.
const (
	routeNameClaimPrefix = "route-name-"
	routeNameClaimLabel  = "kipper.run/route-name-claim"

	// attributionRetention covers the default three-day Prometheus retention
	// plus a one-hour margin.
	attributionRetention = 73 * time.Hour
	// maxAttributionIntervals caps retained history by dropping the oldest entries.
	maxAttributionIntervals = 50
)

// attributionInterval records a span attributed to the key's sole observed
// producer. To is nil while the interval is open.
type attributionInterval struct {
	From       time.Time  `json:"from"`
	To         *time.Time `json:"to,omitempty"`
	Generation string     `json:"generation"`
}

type routeNameClaim struct {
	Key       string
	Namespace string
	UID       string
	// Shared lists the namespaces that produce the key alongside the owner
	// while a grandfathered collision lasts, and SharedUIDs the UID each had,
	// so a namespace recreated under the same name has no right to it.
	Shared     []string
	SharedUIDs map[string]string

	// Occupied marks a tenant backend using a reserved platform key.
	Occupied  bool
	Intervals []attributionInterval

	configMap *corev1.ConfigMap
}

// routeNameClaimName derives the reservation's object name from its key. The
// hash keeps it DNS-safe and fixed-length.
func routeNameClaimName(key string) string {
	sum := sha256.Sum256([]byte(key))
	return routeNameClaimPrefix + hex.EncodeToString(sum[:])[:48]
}

// routeNameClaimConfigMap renders a reservation as the ConfigMap that stores
// it, carrying over the stored object's metadata when there is one.
func routeNameClaimConfigMap(c routeNameClaim) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{}
	if c.configMap != nil {
		cm = c.configMap.DeepCopy()
	}
	cm.Name = routeNameClaimName(c.Key)
	cm.Namespace = routeClaimNamespace
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	cm.Labels["app.kubernetes.io/managed-by"] = "kipper"
	cm.Labels[routeNameClaimLabel] = "true"
	cm.Labels[routeOwnerNamespaceLabel] = c.Namespace

	intervals, _ := json.Marshal(c.Intervals)
	shared, _ := json.Marshal(c.Shared)
	sharedUIDs, _ := json.Marshal(c.SharedUIDs)
	cm.Data = map[string]string{
		"key":        c.Key,
		"namespace":  c.Namespace,
		"uid":        c.UID,
		"intervals":  string(intervals),
		"shared":     string(shared),
		"sharedUIDs": string(sharedUIDs),
	}

	if c.Occupied {
		cm.Data["occupied"] = "true"
	}
	return cm
}

// parseRouteNameClaim reads a reservation from its ConfigMap.
func parseRouteNameClaim(cm *corev1.ConfigMap) (routeNameClaim, error) {
	c := routeNameClaim{
		Key:       cm.Data["key"],
		Namespace: cm.Data["namespace"],
		UID:       cm.Data["uid"],
		Occupied:  cm.Data["occupied"] == "true",
		configMap: cm,
	}
	if raw := cm.Data["intervals"]; raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &c.Intervals); err != nil {
			return c, fmt.Errorf("reading intervals of route name claim %s: %w", cm.Name, err)
		}
	}
	if raw := cm.Data["shared"]; raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &c.Shared); err != nil {
			return c, fmt.Errorf("reading shared namespaces of route name claim %s: %w", cm.Name, err)
		}
	}
	if raw := cm.Data["sharedUIDs"]; raw != "" && raw != "null" {
		if err := json.Unmarshal([]byte(raw), &c.SharedUIDs); err != nil {
			return c, fmt.Errorf("reading shared namespace UIDs of route name claim %s: %w", cm.Name, err)
		}
	}
	return c, nil
}

// setShared records the sharing namespaces with their UIDs, sorted.
func (c *routeNameClaim) setShared(names []string, uids map[string]types.UID) {
	c.Shared, c.SharedUIDs = nil, nil
	sorted := append([]string(nil), names...)
	slices.Sort(sorted)
	for _, n := range slices.Compact(sorted) {
		if c.SharedUIDs == nil {
			c.SharedUIDs = map[string]string{}
		}
		c.Shared = append(c.Shared, n)
		c.SharedUIDs[n] = string(uids[n])
	}
}

// sharedBy reports whether namespace, with uid, is one of the grandfathered
// sharers.
func (c routeNameClaim) sharedBy(namespace string, uid types.UID) bool {
	return slices.Contains(c.Shared, namespace) && uid != "" && c.SharedUIDs[namespace] == string(uid)
}

// reserveRouteName creates or reuses a namespace's claim, or takes over an
// unshared claim whose namespace UID is gone. Takeover clears attribution
// history. Use an uncached reader for ownership checks; read failures abort
// admission.
func reserveRouteName(ctx context.Context, reader client.Reader, writer client.Client, namespace, key string) (bool, error) {
	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		return false, fmt.Errorf("reading namespace %s: %w", namespace, err)
	}
	mine := routeNameClaim{Key: key, Namespace: namespace, UID: string(ns.UID)}

	var cm corev1.ConfigMap
	err := reader.Get(ctx, types.NamespacedName{Name: routeNameClaimName(key), Namespace: routeClaimNamespace}, &cm)
	if apierrors.IsNotFound(err) {
		createErr := writer.Create(ctx, routeNameClaimConfigMap(mine))
		if createErr == nil {
			return true, nil
		}
		if !apierrors.IsAlreadyExists(createErr) {
			return false, fmt.Errorf("creating route name claim for %s: %w", key, createErr)
		}
		err = reader.Get(ctx, types.NamespacedName{Name: routeNameClaimName(key), Namespace: routeClaimNamespace}, &cm)
	}
	if err != nil {
		return false, fmt.Errorf("reading route name claim for %s: %w", key, err)
	}

	held, err := parseRouteNameClaim(&cm)
	if err != nil {
		return false, err
	}
	if held.UID != "" && held.UID == mine.UID {
		return true, nil
	}
	// A shared key has producers besides its holder, so it changes hands only
	// at a bootstrap, once no earlier reconcile can still be publishing.
	if len(held.Shared) > 0 {
		return false, nil
	}
	live, err := routeNameOwnerLive(ctx, reader, held)
	if err != nil || live {
		return false, err
	}

	mine.configMap = &cm
	if err := writer.Update(ctx, routeNameClaimConfigMap(mine)); err != nil {
		if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
			return false, errRouteNameRace
		}
		return false, fmt.Errorf("taking over route name claim for %s: %w", key, err)
	}
	return true, nil
}

// routeNameOwnerLive reports whether the namespace that holds a reservation
// still exists as the same namespace. A namespace recreated under the same
// name has a new UID and does not count.
func routeNameOwnerLive(ctx context.Context, reader client.Reader, c routeNameClaim) (bool, error) {
	if c.Namespace == "" {
		return false, nil
	}
	var ns corev1.Namespace
	err := reader.Get(ctx, types.NamespacedName{Name: c.Namespace}, &ns)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading namespace %s: %w", c.Namespace, err)
	}
	return string(ns.UID) == c.UID, nil
}

// pruneIntervals drops intervals that ended before the retention window or
// never covered any time, then keeps the newest maxAttributionIntervals.
func pruneIntervals(in []attributionInterval, now time.Time) []attributionInterval {
	cutoff := now.Add(-attributionRetention)
	var out []attributionInterval
	for _, iv := range in {
		if iv.To != nil && (!iv.To.After(iv.From) || iv.To.Before(cutoff)) {
			continue
		}
		out = append(out, iv)
	}
	if len(out) > maxAttributionIntervals {
		out = out[len(out)-maxAttributionIntervals:]
	}
	return out
}

// RouteNameRefusal checks current claims before app creation, without reserving
// the name. Refusal messages omit other projects; the reconciler makes the
// authoritative admission decision.
func RouteNameRefusal(ctx context.Context, reader client.Reader, namespace, app string) (bool, string, error) {
	key := routename.Key(namespace, app)
	if _, ok := routename.Platform(key); ok {
		return true, routeNamePlatformReserved, nil
	}
	conflict, err := sameNamespaceConflict(ctx, reader, namespace, app, key)
	if err != nil || conflict {
		return conflict, routeNameIndistinguishable, err
	}
	var cm corev1.ConfigMap
	err = reader.Get(ctx, types.NamespacedName{Name: routeNameClaimName(key), Namespace: routeClaimNamespace}, &cm)
	if apierrors.IsNotFound(err) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("reading route name claim for %s: %w", key, err)
	}
	held, err := parseRouteNameClaim(&cm)
	if err != nil {
		return false, "", err
	}
	var ns corev1.Namespace
	if err := reader.Get(ctx, types.NamespacedName{Name: namespace}, &ns); err != nil {
		return false, "", fmt.Errorf("reading namespace %s: %w", namespace, err)
	}
	if held.sharedBy(namespace, ns.UID) || (held.Namespace == namespace && held.UID != "" && string(ns.UID) == held.UID) {
		return false, "", nil
	}
	if len(held.Shared) > 0 {
		return true, routeNameIndistinguishable, nil
	}
	live, err := routeNameOwnerLive(ctx, reader, held)
	if err != nil {
		return false, "", err
	}
	if live {
		return true, routeNameIndistinguishable, nil
	}
	return false, "", nil
}

// RouteNameClaimObject returns the stored form of a reservation of key held by
// a namespace, for seeding one outside the controllers.
func RouteNameClaimObject(key, namespace string, uid types.UID) *corev1.ConfigMap {
	return routeNameClaimConfigMap(routeNameClaim{Key: key, Namespace: namespace, UID: string(uid)})
}
