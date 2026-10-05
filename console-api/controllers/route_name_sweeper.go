package controllers

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/internal/leader"
	"github.com/getkipper/kipper/controller/pkg/routename"
)

const (
	// routeNameMarkerName is the ConfigMap that records the enforcement
	// generation, the binary it belongs to, and its heartbeat.
	routeNameMarkerName = "route-name-claims"
	routeNameLease      = "kipper-route-names"
	// routeNameScanInterval is how often the sweeper scans routes and
	// refreshes the heartbeat.
	routeNameScanInterval = 30 * time.Second
	// routeNameScanGap is the longest gap between scans that preserves
	// continuous attribution.
	routeNameScanGap    = 2*routeNameScanInterval + 15*time.Second
	consoleAPIPodLabel  = "console-api"
	consoleAPIContainer = "console-api"
)

// routeNameMarker is the current enforcement generation. A generation belongs
// to one console-api binary, identified by the image digest its pods run.
type routeNameMarker struct {
	Generation   string
	ImageID      string
	Bootstrapped bool
	Heartbeat    time.Time

	configMap *corev1.ConfigMap
}

func routeNameMarkerConfigMap(m routeNameMarker, existing *corev1.ConfigMap) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{}
	if existing != nil {
		cm = existing.DeepCopy()
	}
	cm.Name = routeNameMarkerName
	cm.Namespace = routeClaimNamespace
	if cm.Labels == nil {
		cm.Labels = map[string]string{}
	}
	cm.Labels["app.kubernetes.io/managed-by"] = "kipper"
	cm.Data = map[string]string{
		"generation":   m.Generation,
		"imageID":      m.ImageID,
		"bootstrapped": fmt.Sprintf("%t", m.Bootstrapped),
	}
	if !m.Heartbeat.IsZero() {
		cm.Data["heartbeat"] = m.Heartbeat.UTC().Format(time.RFC3339Nano)
	}
	return cm
}

// readRouteNameMarker reads the marker, reporting false when there is none.
func readRouteNameMarker(ctx context.Context, reader client.Reader) (routeNameMarker, bool, error) {
	var cm corev1.ConfigMap
	err := reader.Get(ctx, types.NamespacedName{Name: routeNameMarkerName, Namespace: routeClaimNamespace}, &cm)
	if apierrors.IsNotFound(err) {
		return routeNameMarker{}, false, nil
	}
	if err != nil {
		return routeNameMarker{}, false, fmt.Errorf("reading route name marker: %w", err)
	}
	m := routeNameMarker{
		Generation:   cm.Data["generation"],
		ImageID:      cm.Data["imageID"],
		Bootstrapped: cm.Data["bootstrapped"] == "true",
		configMap:    &cm,
	}
	if raw := cm.Data["heartbeat"]; raw != "" {
		t, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			// A zero heartbeat makes bootstrap use its start time as the
			// fallback closing boundary.
			log.Printf("route names: ignoring an unreadable heartbeat %q: %v", raw, err)
		} else {
			m.Heartbeat = t
		}
	}
	return m, true, nil
}

func writeRouteNameMarker(ctx context.Context, c client.Client, m routeNameMarker) error {
	if m.configMap == nil {
		return c.Create(ctx, routeNameMarkerConfigMap(m, nil))
	}
	return c.Update(ctx, routeNameMarkerConfigMap(m, m.configMap))
}

// podImageID returns the image digest the console-api container of a pod
// runs, or "" before the container has started.
func podImageID(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == consoleAPIContainer {
			return cs.ImageID
		}
	}
	return ""
}

// podLive treats every nonterminal pod as a possible controller writer.
func podLive(pod *corev1.Pod) bool {
	return pod.Status.Phase != corev1.PodFailed && pod.Status.Phase != corev1.PodSucceeded
}

func ownImageID(ctx context.Context, reader client.Reader, podName string) (string, error) {
	var pod corev1.Pod
	if err := reader.Get(ctx, types.NamespacedName{Name: podName, Namespace: routeClaimNamespace}, &pod); err != nil {
		return "", fmt.Errorf("reading console-api pod %s: %w", podName, err)
	}
	return podImageID(&pod), nil
}

// RouteNameGate admits new routes in this process only while it holds the
// sweeper's Lease and its generation is bootstrapped. Closing it waits for
// every admitted write to finish, which is how a bootstrap drains the writes
// this process already allowed; other processes are drained by waiting for
// their pods to stop.
type RouteNameGate struct {
	mu     sync.RWMutex
	isOpen bool
}

// NewRouteNameGate returns a closed gate.
func NewRouteNameGate() *RouteNameGate {
	return &RouteNameGate{}
}

// Admit reports whether a route may be published now. Defer release after
// admission so both successful and failed writes unblock the sweeper.
func (g *RouteNameGate) Admit() (release func(), ok bool) {
	g.mu.RLock()
	if !g.isOpen {
		g.mu.RUnlock()
		return nil, false
	}
	var once sync.Once
	return func() { once.Do(g.mu.RUnlock) }, true
}

func (g *RouteNameGate) open() {
	g.mu.Lock()
	g.isOpen = true
	g.mu.Unlock()
}

// exclusive runs f while no route write this process admitted is in flight
// and none can start.
func (g *RouteNameGate) exclusive(f func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f()
}

func (g *RouteNameGate) close() {
	g.mu.Lock()
	g.isOpen = false
	g.mu.Unlock()
}

// RouteNamesReady reads the persisted bootstrap marker for API prechecks.
// The reconciler's local gate still controls publication.
func RouteNamesReady(ctx context.Context, reader client.Reader) (bool, error) {
	m, found, err := readRouteNameMarker(ctx, reader)
	return found && m.Bootstrapped, err
}

// RouteNameSweeper keeps the route name claims and their attribution
// intervals, in one console-api pod at a time.
type RouteNameSweeper struct {
	Client    client.Client
	Reader    client.Reader
	Clientset kubernetes.Interface
	PodName   string
	Gate      *RouteNameGate
	Now       func() time.Time

	worker     leader.Worker
	retryDelay time.Duration
}

// Run holds the sweeper's Lease and runs a generation while it does. It
// blocks until ctx is cancelled.
func (s *RouteNameSweeper) Run(ctx context.Context) {
	leader.Run(ctx, s.Clientset, routeClaimNamespace, routeNameLease, s.PodName, leader.DefaultTiming, &s.worker, s.lead)
}

func (s *RouteNameSweeper) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// lead runs one leadership term: it waits for this pod's image, for every
// earlier console-api pod to stop and for no other binary to be live, starts a
// generation, opens the gate, then scans. The gate closes when the term ends.
func (s *RouteNameSweeper) lead(ctx context.Context) {
	s.Gate.close()
	defer s.Gate.close()
	image, err := s.waitReady(ctx)
	if err != nil {
		return
	}
	if err := s.bootstrapWithRetry(ctx, image); err != nil {
		return
	}
	ticker := time.NewTicker(routeNameScanInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.scan(ctx, image); err != nil {
				log.Printf("route names: scan: %v", err)
			}
		}
	}
}

// bootstrapWithRetry opens the gate after a successful bootstrap, retrying
// failures until the leadership term ends.
func (s *RouteNameSweeper) bootstrapWithRetry(ctx context.Context, image string) error {
	delay := s.retryDelay
	if delay == 0 {
		delay = 5 * time.Second
	}
	for {
		// Checked before every attempt: another binary can appear while a
		// failed attempt waits to retry.
		ready, err := s.readyToStart(ctx, image)
		if err == nil && ready {
			err = s.startGeneration(ctx, image)
			if err == nil {
				s.Gate.open()
				return nil
			}
		}
		if err != nil {
			log.Printf("route names: starting a generation: %v", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

// waitReady returns this pod's image digest once a generation may start.
func (s *RouteNameSweeper) waitReady(ctx context.Context) (string, error) {
	lastLog := time.Now()
	for {
		image, err := ownImageID(ctx, s.Reader, s.PodName)
		reason := "this pod's image is not known yet"
		if err == nil && image != "" {
			ready, readyErr := s.readyToStart(ctx, image)
			if readyErr == nil && ready {
				return image, nil
			}
			reason, err = "another console-api pod is still live", readyErr
		}
		if time.Since(lastLog) > time.Minute {
			log.Printf("route names: not checking route names yet: %s (%v)", reason, err)
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// readyToStart reports whether no live console-api pod created before this one
// remains, so every write it allowed has ended, and no live pod runs another
// binary, which could publish routes this generation never checked.
func (s *RouteNameSweeper) readyToStart(ctx context.Context, image string) (bool, error) {
	earlier, err := s.earlierPodsLive(ctx)
	if err != nil || earlier {
		return false, err
	}
	_, foreign, err := s.foreignBinarySince(ctx, image)
	return !foreign, err
}

func (s *RouteNameSweeper) consoleAPIPods(ctx context.Context) ([]corev1.Pod, error) {
	var pods corev1.PodList
	if err := s.Reader.List(ctx, &pods, client.InNamespace(routeClaimNamespace), client.MatchingLabels{"app": consoleAPIPodLabel}); err != nil {
		return nil, fmt.Errorf("listing console-api pods: %w", err)
	}
	return pods.Items, nil
}

// earlierPodsLive checks for older live console-api pods, breaking creation-time
// ties by pod name.
func (s *RouteNameSweeper) earlierPodsLive(ctx context.Context) (bool, error) {
	pods, err := s.consoleAPIPods(ctx)
	if err != nil {
		return false, err
	}
	var own *corev1.Pod
	for i := range pods {
		if pods[i].Name == s.PodName {
			own = &pods[i]
		}
	}
	if own == nil {
		return false, fmt.Errorf("console-api pod %s not found", s.PodName)
	}
	for i := range pods {
		p := &pods[i]
		if p.Name == s.PodName || !podLive(p) {
			continue
		}
		created, mine := p.CreationTimestamp.Time, own.CreationTimestamp.Time
		if created.Before(mine) || (created.Equal(mine) && p.Name < own.Name) {
			return true, nil
		}
	}
	return false, nil
}

// foreignBinarySince returns the earliest creation time of a live console-api
// pod that does not run image. A pod whose image is not known yet counts, so
// an unknown binary never extends attribution.
func (s *RouteNameSweeper) foreignBinarySince(ctx context.Context, image string) (time.Time, bool, error) {
	pods, err := s.consoleAPIPods(ctx)
	if err != nil {
		return time.Time{}, false, err
	}
	var at time.Time
	found := false
	for i := range pods {
		p := &pods[i]
		if !podLive(p) || podImageID(p) == image {
			continue
		}
		if created := p.CreationTimestamp.UTC(); !found || created.Before(at) {
			at, found = created, true
		}
	}
	return at, found, nil
}

// startGeneration begins a generation for image: it writes a new marker,
// bootstraps every claim, and marks the generation bootstrapped.
func (s *RouteNameSweeper) startGeneration(ctx context.Context, image string) error {
	previous, _, err := readRouteNameMarker(ctx, s.Reader)
	if err != nil {
		return err
	}
	// The previous heartbeat stays until this generation is bootstrapped, so a
	// failed attempt leaves the boundary it would close intervals at.
	m := routeNameMarker{Generation: newGenerationID(), ImageID: image, Heartbeat: previous.Heartbeat, configMap: previous.configMap}
	if err := writeRouteNameMarker(ctx, s.Client, m); err != nil {
		return fmt.Errorf("writing route name marker: %w", err)
	}

	listedAt := s.now()
	snap, err := s.snapshot(ctx, listedAt, m.Generation)
	if err != nil {
		return err
	}
	snap.Bootstrap = true
	snap.Heartbeat = previous.Heartbeat
	if err := s.apply(ctx, snap, planRouteNames(snap)); err != nil {
		return err
	}

	m, _, err = readRouteNameMarker(ctx, s.Reader)
	if err != nil {
		return err
	}
	m.Bootstrapped = true
	m.Heartbeat = listedAt
	return writeRouteNameMarker(ctx, s.Client, m)
}

// errClaimWriteDropped reports that a claim changed between the snapshot and
// its write, so the pass must not refresh the heartbeat.
var errClaimWriteDropped = errors.New("a route name claim changed during the scan")

// scan applies one pass of the current generation. While a pod of another
// binary exists it writes no heartbeat and opens nothing, and open intervals
// close at that pod's creation or the last heartbeat, whichever came first.
func (s *RouteNameSweeper) scan(ctx context.Context, image string) error {
	m, found, err := readRouteNameMarker(ctx, s.Reader)
	if err != nil {
		return err
	}
	if !found || m.ImageID != image {
		return fmt.Errorf("the route name marker belongs to another binary")
	}
	foreignAt, foreign, err := s.foreignBinarySince(ctx, image)
	if err != nil {
		return err
	}

	listedAt := s.now()
	snap, err := s.snapshot(ctx, listedAt, m.Generation)
	if err != nil {
		return err
	}
	switch {
	case foreign:
		snap.Suspended = true
		snap.SuspendedAt = foreignAt
		if !m.Heartbeat.IsZero() && m.Heartbeat.Before(foreignAt) {
			snap.SuspendedAt = m.Heartbeat
		}
	case !m.Heartbeat.IsZero() && listedAt.Sub(m.Heartbeat) > routeNameScanGap:
		// Another binary may have run during the gap. Close intervals at the
		// last heartbeat; a later successful scan can open new ones.
		snap.Suspended = true
		snap.SuspendedAt = m.Heartbeat
	}
	if err := s.apply(ctx, snap, planRouteNames(snap)); errors.Is(err, errClaimWriteDropped) {
		log.Printf("route names: %v; the heartbeat waits for the next scan", err)
		return nil
	} else if err != nil {
		return err
	}
	if foreign {
		return nil
	}
	m.Heartbeat = listedAt
	return writeRouteNameMarker(ctx, s.Client, m)
}

// snapshot reads everything planRouteNames needs.
func (s *RouteNameSweeper) snapshot(ctx context.Context, now time.Time, generation string) (routeNameSnapshot, error) {
	snap := routeNameSnapshot{
		Now:        now,
		Generation: generation,
		Claims:     map[string]routeNameClaim{},
		Namespaces: map[string]types.UID{},
		Workloads:  map[string]map[string]bool{},
	}

	var ingresses networkingv1.IngressList
	if err := s.Reader.List(ctx, &ingresses); err != nil {
		return snap, fmt.Errorf("listing Ingresses: %w", err)
	}
	for _, ing := range ingresses.Items {
		for _, b := range routename.Backends(ing) {
			snap.Producers = append(snap.Producers, routeNameProducer{
				Namespace: b.Namespace, Service: b.Service, Port: b.Port, Created: ing.CreationTimestamp.UTC(),
			})
		}
	}

	var claims corev1.ConfigMapList
	if err := s.Reader.List(ctx, &claims, client.InNamespace(routeClaimNamespace), client.MatchingLabels{routeNameClaimLabel: "true"}); err != nil {
		return snap, fmt.Errorf("listing route name claims: %w", err)
	}
	for i := range claims.Items {
		c, err := parseRouteNameClaim(&claims.Items[i])
		if err != nil {
			// Left alone: its key stays held by the unreadable object, because
			// creating a claim over it fails and reservations refuse on it.
			log.Printf("route names: skipping %s: %v", claims.Items[i].Name, err)
			snap.Unreadable = append(snap.Unreadable, claims.Items[i].Data["key"])
			continue
		}
		snap.Claims[c.Key] = c
	}

	var namespaces corev1.NamespaceList
	if err := s.Reader.List(ctx, &namespaces); err != nil {
		return snap, fmt.Errorf("listing namespaces: %w", err)
	}
	for _, ns := range namespaces.Items {
		snap.Namespaces[ns.Name] = ns.UID
	}

	var apps kipperv1.AppList
	if err := s.Reader.List(ctx, &apps); err != nil {
		return snap, fmt.Errorf("listing apps: %w", err)
	}
	for _, a := range apps.Items {
		snap.addWorkload(a.Namespace, a.Name)
	}
	var services kipperv1.ServiceList
	if err := s.Reader.List(ctx, &services); err != nil {
		return snap, fmt.Errorf("listing services: %w", err)
	}
	for _, svc := range services.Items {
		snap.addWorkload(svc.Namespace, svc.Name)
	}
	return snap, nil
}

func (s *routeNameSnapshot) addWorkload(namespace, name string) {
	if s.Workloads[namespace] == nil {
		s.Workloads[namespace] = map[string]bool{}
	}
	s.Workloads[namespace][name] = true
}

// apply persists the plan. A concurrent claim change defers writes or deletes
// to the next pass; a dropped write also blocks the heartbeat update.
func (s *RouteNameSweeper) apply(ctx context.Context, snap routeNameSnapshot, plan routeNamePlan) error {
	dropped, droppedKey := false, ""
	for _, c := range plan.Write {
		cm := routeNameClaimConfigMap(c)
		var err error
		if c.configMap == nil {
			err = s.Client.Create(ctx, cm)
		} else {
			err = s.Client.Update(ctx, cm)
		}
		switch {
		case apierrors.IsConflict(err), apierrors.IsAlreadyExists(err), apierrors.IsNotFound(err):
			if !dropped {
				droppedKey = c.Key
			}
			dropped = true
		case err != nil:
			return fmt.Errorf("writing route name claim for %s: %w", c.Key, err)
		}
	}
	if dropped {
		// Refresh the heartbeat only after every planned claim write succeeds.
		return fmt.Errorf("%w: %s", errClaimWriteDropped, droppedKey)
	}
	if len(plan.Delete) == 0 {
		return nil
	}
	// Deleted with the gate held exclusively, so no write this process
	// admitted is in flight, and only after a fresh look shows nothing that
	// could use the key: a reservation that finds its own claim writes
	// nothing, so the resourceVersion alone would not notice it.
	var err error
	s.Gate.exclusive(func() {
		for _, key := range plan.Delete {
			if err = s.deleteIdleClaim(ctx, snap.Claims[key]); err != nil {
				return
			}
		}
	})
	return err
}

func (s *RouteNameSweeper) deleteIdleClaim(ctx context.Context, c routeNameClaim) error {
	old := c.configMap
	if old == nil {
		return nil
	}
	used, err := s.keyInUse(ctx, c)
	if err != nil || used {
		return err
	}
	uid, version := old.UID, old.ResourceVersion
	err = s.Client.Delete(ctx, old, client.Preconditions{UID: &uid, ResourceVersion: &version})
	if err != nil && !apierrors.IsConflict(err) && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting route name claim for %s: %w", c.Key, err)
	}
	return nil
}

// keyInUse reads, uncached, whether a workload in the claim's namespace could
// publish its key or any Ingress routes under it.
func (s *RouteNameSweeper) keyInUse(ctx context.Context, c routeNameClaim) (bool, error) {
	if c.Namespace != "" {
		probe := routeNameSnapshot{Workloads: map[string]map[string]bool{}}
		var apps kipperv1.AppList
		if err := s.Reader.List(ctx, &apps, client.InNamespace(c.Namespace)); err != nil {
			return false, err
		}
		for _, a := range apps.Items {
			probe.addWorkload(a.Namespace, a.Name)
		}
		var services kipperv1.ServiceList
		if err := s.Reader.List(ctx, &services, client.InNamespace(c.Namespace)); err != nil {
			return false, err
		}
		for _, svc := range services.Items {
			probe.addWorkload(svc.Namespace, svc.Name)
		}
		if probe.canPublish(c.Namespace, c.Key) {
			return true, nil
		}
	}
	var ingresses networkingv1.IngressList
	if err := s.Reader.List(ctx, &ingresses); err != nil {
		return false, err
	}
	for _, ing := range ingresses.Items {
		for _, b := range routename.Backends(ing) {
			if b.Key() == c.Key {
				return true, nil
			}
		}
	}
	return false, nil
}

func newGenerationID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
