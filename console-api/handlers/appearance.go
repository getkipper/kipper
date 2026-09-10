package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	appearanceConfigMap  = "kipper-console-appearance"
	appearanceNamespace  = "kipper-system"
	faviconColourKey     = "faviconColour"
	defaultFaviconColour = "blue"

	// appearanceReadTimeout is short because the read sits on an
	// unauthenticated route: a slow API server must not hold requests open.
	appearanceReadTimeout = 3 * time.Second
	appearanceCacheTTL    = 30 * time.Second

	// appearanceRetryAfterFailure holds off the next attempt when the cluster
	// cannot be reached. Without it every reader during an outage pays for its
	// own failed call, and they queue behind each other on the way.
	appearanceRetryAfterFailure = 5 * time.Second
)

// faviconColours are the tab-icon colours the console knows how to draw. The
// console holds the matching tints; this list is what keeps a typo from
// reaching every browser tab on the cluster, where it would quietly render as
// the default and read as the setting having been ignored.
var faviconColours = map[string]struct{}{
	"blue":   {},
	"green":  {},
	"purple": {},
	"orange": {},
	"yellow": {},
	"pink":   {},
	"red":    {},
	"brown":  {},
	"grey":   {},
}

// Appearance handles how a cluster's console looks to everyone who opens it.
// Today that is the colour of the browser tab icon, which is what tells one
// cluster's tab from another's when several are open.
type Appearance struct {
	Client kubernetes.Interface

	// The read is unauthenticated, so it is cached rather than answered from
	// the API server every time. Without this, anyone who can reach the console
	// can turn request volume into control-plane traffic. A colour changes
	// about never, so a stale answer for the TTL costs nothing.
	mu          sync.Mutex
	cached      string
	nextRefresh time.Time

	// saveMu serialises writers. Two admins saving at once would otherwise be
	// free to publish their colours to the cache in the opposite order to the
	// one their writes landed in, leaving every tab showing a colour the
	// cluster no longer holds.
	//
	// No test covers this. The fake client holds one lock across its whole
	// reaction chain, so the interleaving cannot be produced against it; the
	// window is real only against an API server that serves callers at once.
	saveMu sync.Mutex
}

type appearanceSettings struct {
	FaviconColour string `json:"faviconColour"`
}

// Get returns the cluster's tab-icon colour. It is unauthenticated because the
// colour is not a secret and the login page needs it before anyone has signed
// in, which is exactly when several cluster tabs are hardest to tell apart.
// GET /api/v1/appearance
func (a *Appearance) Get(w http.ResponseWriter, r *http.Request) {
	respondJSON(w, http.StatusOK, appearanceSettings{FaviconColour: a.faviconColour(r.Context())})
}

// faviconColour answers from the cache when it can, and falls back to the
// default whenever the answer is not a colour this build can draw. A missing
// ConfigMap, an unreachable API server and a value written by a newer console
// all leave the tab blue rather than blank.
func (a *Appearance) faviconColour(ctx context.Context) string {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cached != "" && time.Now().Before(a.nextRefresh) {
		return a.cached
	}

	readCtx, cancel := context.WithTimeout(ctx, appearanceReadTimeout)
	defer cancel()

	cm, err := a.Client.CoreV1().ConfigMaps(appearanceNamespace).Get(readCtx, appearanceConfigMap, metav1.GetOptions{})
	switch {
	case err == nil:
		colour := defaultFaviconColour
		if _, ok := faviconColours[cm.Data[faviconColourKey]]; ok {
			colour = cm.Data[faviconColourKey]
		}
		a.cached, a.nextRefresh = colour, time.Now().Add(appearanceCacheTTL)
	case errors.IsNotFound(err):
		// Nobody has chosen one. The default stands and is worth caching.
		a.cached, a.nextRefresh = defaultFaviconColour, time.Now().Add(appearanceCacheTTL)
	case a.cached != "":
		// The cluster is unreachable rather than unset, so keep serving the
		// last answer instead of flipping every tab back to blue.
		a.nextRefresh = time.Now().Add(appearanceRetryAfterFailure)
	default:
		a.cached, a.nextRefresh = defaultFaviconColour, time.Now().Add(appearanceRetryAfterFailure)
	}
	return a.cached
}

// Update sets the cluster's tab-icon colour. Admin-only, because it changes
// what every operator on this cluster sees.
// PUT /api/v1/settings/appearance
func (a *Appearance) Update(w http.ResponseWriter, r *http.Request) {
	var req appearanceSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, ok := faviconColours[req.FaviconColour]; !ok {
		respondError(w, http.StatusBadRequest, "unknown favicon colour")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// The write and the cache publication are one operation. Split them and
	// the later save can publish first, leaving readers on the earlier colour.
	a.saveMu.Lock()
	defer a.saveMu.Unlock()

	if err := a.save(ctx, req.FaviconColour); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to save the appearance settings")
		return
	}

	a.mu.Lock()
	a.cached, a.nextRefresh = req.FaviconColour, time.Now().Add(appearanceCacheTTL)
	a.mu.Unlock()

	respondJSON(w, http.StatusOK, appearanceSettings{FaviconColour: req.FaviconColour})
}

// save writes the colour onto the ConfigMap that is there rather than posting a
// replacement, because Kubernetes refuses an update carrying no
// resourceVersion. Two admins saving at once meet either a conflict or, on the
// very first save, each other's create; both are retried by reading back what
// won and writing onto it.
func (a *Appearance) save(ctx context.Context, colour string) error {
	configMaps := a.Client.CoreV1().ConfigMaps(appearanceNamespace)
	retriable := func(err error) bool { return errors.IsConflict(err) || errors.IsAlreadyExists(err) }

	return retry.OnError(retry.DefaultRetry, retriable, func() error {
		existing, err := configMaps.Get(ctx, appearanceConfigMap, metav1.GetOptions{})
		if errors.IsNotFound(err) {
			_, err = configMaps.Create(ctx, &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{
					Name:      appearanceConfigMap,
					Namespace: appearanceNamespace,
					Labels:    map[string]string{"app.kubernetes.io/managed-by": "kipper"},
				},
				Data: map[string]string{faviconColourKey: colour},
			}, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}

		if existing.Data == nil {
			existing.Data = map[string]string{}
		}
		existing.Data[faviconColourKey] = colour
		_, err = configMaps.Update(ctx, existing, metav1.UpdateOptions{})
		return err
	})
}
