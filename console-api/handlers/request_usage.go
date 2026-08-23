package handlers

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/getkipper/kipper/console-api/internal/nsowner"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// requestUsageWindow is how far back the endpoint counts requests. It matches
// the Prometheus retention (3d), so the numbers are operational, never
// billing-grade: anything older has already been dropped by the metrics
// store. A durable rollup is the prerequisite for longer periods.
const requestUsageWindow = 72 * time.Hour

// RequestUsage serves aggregate HTTP request counts per project, read from
// Traefik's Prometheus metrics.
type RequestUsage struct {
	Client            kubernetes.Interface
	CRClient          crclient.Client
	PrometheusBaseURL string

	// PromQueryVec is injectable for tests; when nil a real client against
	// PrometheusBaseURL is used.
	PromQueryVec PromQueryInstantVecFunc
}

// RequestUsageResponse is the JSON shape for GET /projects/{name}/requests.
type RequestUsageResponse struct {
	WindowHours         int                       `json:"window_hours"`
	PrometheusAvailable bool                      `json:"prometheus_available"`
	TotalRequests       float64                   `json:"total_requests"`
	Environments        []EnvironmentRequestUsage `json:"environments"`
}

// EnvironmentRequestUsage aggregates one environment namespace.
type EnvironmentRequestUsage struct {
	Environment   string            `json:"environment"`
	Namespace     string            `json:"namespace"`
	TotalRequests float64           `json:"total_requests"`
	Apps          []AppRequestUsage `json:"apps"`
}

// AppRequestUsage is the per-app request count within one environment.
type AppRequestUsage struct {
	App      string  `json:"app"`
	Requests float64 `json:"requests"`
}

// Get returns request counts for every app in every environment of the
// project over the last 72 hours. When Prometheus is unreachable or disabled
// the response carries prometheus_available=false and empty counts instead
// of an error, matching how the resource usage endpoints degrade.
func (h *RequestUsage) Get(w http.ResponseWriter, r *http.Request) {
	projectName := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	labelled, err := h.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("%s=%s", kipperlabels.Project, projectName),
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("listing project namespaces: %v", err))
		return
	}
	// The label gathers the candidates and does not decide. What is summed here
	// is one project's traffic, and a namespace pointed at this project by
	// somebody who can write namespace metadata would have its request counts
	// reported as this project's. The owner lookup decides, the same way every
	// other project-scoped read does.
	nsList := &corev1.NamespaceList{}
	for i := range labelled.Items {
		owns, err := nsowner.OwnsNamespace(ctx, h.CRClient, projectName, &labelled.Items[i])
		if err != nil {
			respondError(w, http.StatusInternalServerError, fmt.Sprintf("establishing which namespaces are project %s's: %v", projectName, err))
			return
		}
		if owns {
			nsList.Items = append(nsList.Items, labelled.Items[i])
		}
	}

	resp := RequestUsageResponse{
		WindowHours:  int(requestUsageWindow.Hours()),
		Environments: []EnvironmentRequestUsage{},
	}

	namespaces := make([]string, 0, len(nsList.Items))
	envByNamespace := map[string]string{}
	for _, ns := range nsList.Items {
		namespaces = append(namespaces, ns.Name)
		envByNamespace[ns.Name] = ns.Labels[kipperlabels.Environment]
	}
	if len(namespaces) == 0 {
		respondJSON(w, http.StatusOK, resp)
		return
	}

	// Resolve Traefik service labels against every namespace, not just this
	// project's. A Traefik label flattens "<namespace>-<service>", so a service
	// in "billing-prod-test" looks like it could belong to "billing-prod" if we
	// only match this project's prefixes. Matching against the full set lets the
	// genuinely-longest namespace win, and anything resolving outside the
	// project is dropped below.
	allNS, err := h.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf("listing namespaces: %v", err))
		return
	}
	resolveNamespaces := make([]string, 0, len(allNS.Items))
	for _, ns := range allNS.Items {
		resolveNamespaces = append(resolveNamespaces, ns.Name)
	}

	samples, err := h.queryRequestTotals(ctx, namespaces)
	if err != nil {
		// Monitoring off or unreachable: return the project shape with
		// empty counts so the console can render "metrics unavailable".
		for _, ns := range namespaces {
			resp.Environments = append(resp.Environments, EnvironmentRequestUsage{
				Environment: envByNamespace[ns],
				Namespace:   ns,
				Apps:        []AppRequestUsage{},
			})
		}
		sortEnvironments(resp.Environments)
		respondJSON(w, http.StatusOK, resp)
		return
	}
	resp.PrometheusAvailable = true

	byNamespace := map[string][]AppRequestUsage{}
	for _, s := range samples {
		ns, app, ok := splitTraefikService(s.Labels["service"], resolveNamespaces)
		if !ok {
			continue
		}
		// Keep only services that resolve to a namespace in this project; the
		// query's prefix match can pull in a sibling project whose namespace
		// starts with the same string.
		if _, mine := envByNamespace[ns]; !mine {
			continue
		}
		byNamespace[ns] = append(byNamespace[ns], AppRequestUsage{App: app, Requests: s.Value})
	}

	for _, ns := range namespaces {
		env := EnvironmentRequestUsage{
			Environment: envByNamespace[ns],
			Namespace:   ns,
			Apps:        byNamespace[ns],
		}
		if env.Apps == nil {
			env.Apps = []AppRequestUsage{}
		}
		sort.Slice(env.Apps, func(i, j int) bool { return env.Apps[i].Requests > env.Apps[j].Requests })
		for _, app := range env.Apps {
			env.TotalRequests += app.Requests
		}
		resp.TotalRequests += env.TotalRequests
		resp.Environments = append(resp.Environments, env)
	}
	sortEnvironments(resp.Environments)

	respondJSON(w, http.StatusOK, resp)
}

func sortEnvironments(envs []EnvironmentRequestUsage) {
	sort.Slice(envs, func(i, j int) bool { return envs[i].Namespace < envs[j].Namespace })
}

// queryRequestTotals sums request counts per Traefik service over the usage
// window, restricted to services in the project's namespaces.
func (h *RequestUsage) queryRequestTotals(ctx context.Context, namespaces []string) ([]PromVectorSample, error) {
	queryVec := h.PromQueryVec
	if queryVec == nil {
		if h.PrometheusBaseURL == "" {
			return nil, fmt.Errorf("prometheus not configured")
		}
		queryVec = realPromQueryInstantVec(&http.Client{Timeout: 5 * time.Second}, h.PrometheusBaseURL)
	}

	escaped := make([]string, len(namespaces))
	for i, ns := range namespaces {
		escaped[i] = regexp.QuoteMeta(ns)
	}
	query := fmt.Sprintf(
		`sum by (service) (increase(traefik_service_requests_total{service=~"(%s)-.*"}[%dh]))`,
		strings.Join(escaped, "|"), int(requestUsageWindow.Hours()),
	)

	promCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return queryVec(promCtx, query, time.Now())
}

// splitTraefikService maps a Traefik service label back to (namespace, app).
// The label has the shape "<namespace>-<service>-<port>@<provider>", where
// the Kubernetes service carries the app's name. Namespaces are matched
// longest-first so "shop-test-web-80@kubernetes" resolves to namespace
// "shop-test" rather than a hypothetical "shop".
func splitTraefikService(service string, namespaces []string) (namespace, app string, ok bool) {
	name, _, _ := strings.Cut(service, "@")

	sorted := make([]string, len(namespaces))
	copy(sorted, namespaces)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })

	for _, ns := range sorted {
		rest, found := strings.CutPrefix(name, ns+"-")
		if !found {
			continue
		}
		// Drop the trailing port segment; app names can contain dashes, so
		// only the last segment goes.
		if idx := strings.LastIndex(rest, "-"); idx > 0 {
			return ns, rest[:idx], true
		}
		return ns, rest, true
	}
	return "", "", false
}
