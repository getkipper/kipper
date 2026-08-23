package handlers

import (
	"context"
	"encoding/json"
	goerrors "errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	apimeta "k8s.io/apimachinery/pkg/api/meta"

	"github.com/go-chi/chi/v5"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/kubernetes"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/domain"
	"github.com/getkipper/kipper/console-api/handlers/copyenv"
	"github.com/getkipper/kipper/console-api/internal/nsowner"
	"github.com/getkipper/kipper/console-api/internal/workloadname"
	"github.com/getkipper/kipper/console-api/middleware"
	kipperlabels "github.com/getkipper/kipper/controller/pkg/labels"
)

// projectMemberRole returns email's role in the project, or "" if not a member.
func projectMemberRole(project kipperv1.Project, email string) string {
	for _, m := range project.Spec.Members {
		if m.Email == email {
			return string(m.Role)
		}
	}
	return ""
}

// Projects provides handlers for project (namespace) management.
type Projects struct {
	Client   kubernetes.Interface
	CRClient crclient.Client
	// Domain is the cluster's CLUSTER_DOMAIN. Used when copy-from
	// auto-assigns per-env hostnames to the apps it copies.
	Domain string
}

const kipperLabel = "app.kubernetes.io/managed-by"
const kipperValue = "kipper"

type environmentResponse struct {
	Name      string       `json:"name"`
	Namespace string       `json:"namespace"`
	Status    string       `json:"status"`
	Apps      []appSummary `json:"apps"`
	Order     string       `json:"order"`
	// Owned says nothing live contradicts this project's claim on the
	// namespace: either it holds it, or no such namespace exists yet.
	//
	// A declaration is not ownership. Two projects can resolve to one namespace
	// — project "shop" with an environment "prod" beside a project "shop-prod"
	// with a default one — and only the one that got there first has it. The
	// reconciler records the other as a conflict and leaves both CRs listable,
	// so a client comparing declarations cannot tell them apart. This reports
	// what the shared owner lookup answers, which is the same thing
	// authorization asks.
	Owned bool `json:"owned"`
}

type appSummary struct {
	Name     string `json:"name"`
	Image    string `json:"image"`
	Status   string `json:"status"`
	Ready    string `json:"ready"`
	Replicas int32  `json:"replicas"`
	URL      string `json:"url,omitempty"`
}

type projectResponse struct {
	Name         string                `json:"name"`
	DisplayName  string                `json:"display_name,omitempty"`
	Org          string                `json:"org,omitempty"`
	Role         string                `json:"role"`
	Environments []environmentResponse `json:"environments"`
	// EnvLimit is the effective environment cap so the console can show
	// "N of M environments" and stop offering Add at the limit.
	EnvLimit int `json:"env_limit"`
}

type createProjectRequest struct {
	Name         string   `json:"name"`
	DisplayName  string   `json:"display_name"`
	Environments []string `json:"environments"`
	// Tier selects the default resource quota (small/medium/large).
	// Empty keeps the CRD default (small).
	Tier string `json:"tier,omitempty"`
}

type updateProjectRequest struct {
	DisplayName  *string  `json:"display_name,omitempty"`
	Environments []string `json:"environments"`
}

// addEnvironmentRequest is the body of POST /environments. Phase 1
// honoured Name, CopyFrom and AssignDefaultRoutes; Phase 2 adds per-app
// Apps overrides for the wizard. Apps is a map keyed by source-app name —
// any app not listed copies verbatim with the auto-route default.
type addEnvironmentRequest struct {
	Name                string                         `json:"name"`
	CopyFrom            string                         `json:"copy_from,omitempty"`
	AssignDefaultRoutes *bool                          `json:"assign_default_routes,omitempty"`
	Apps                map[string]copyenv.AppOverride `json:"apps,omitempty"`
}

// List returns all Kipper-managed projects grouped by environment.
// GET /api/v1/projects
func (p *Projects) List(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var projectList kipperv1.ProjectList
	if err := p.CRClient.List(ctx, &projectList); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list projects")
		return
	}

	// Non-admins only see projects they are a member of. Admins see all.
	isAdmin := middleware.RoleFromContext(ctx) == middleware.RoleAdmin
	var email string
	if claims := middleware.UserFromContext(ctx); claims != nil {
		email = claims.Email
	}

	owners, ownersKnown := p.namespaceOwners(ctx, projectList.Items)

	projects := make([]projectResponse, 0, len(projectList.Items))
	for _, proj := range projectList.Items {
		role := projectMemberRole(proj, email)
		if isAdmin {
			role = middleware.ProjectRoleOwner
		} else if !validProjectRole(role) {
			// Not just the empty string. A member holding a role this build
			// does not know holds nothing: the projection binds them nowhere
			// and the resolver refuses them. This index is the one
			// authorization decision made straight from spec.members rather
			// than through the resolver, so treating any non-empty role as
			// membership would hand project, app and route names to somebody
			// no other path admits.
			continue
		}
		// The effective list, not the declared one. A Project that declares no
		// environments still gets one from the reconciler, and reading the spec
		// directly reported that project as owning no namespace at all — so the
		// console could not tell who owned the namespace it was looking at.
		effective := controllers.ProjectEnvironments(&proj)
		envs := make([]environmentResponse, 0, len(effective))
		for i, env := range effective {
			nsName := controllers.ResolveNamespace(proj.Name, env.Name)

			// A namespace nothing has taken yet leaves the claim standing, so a
			// project whose reconcile has not run is reported as it was before
			// this field existed.
			owned := true
			if owner, live := owners[nsName]; live {
				owned = owner == proj.Name
			}

			// Reporting that a claim is contradicted does not authorise reading
			// what is in the namespace. These summaries carry app names, images,
			// replica counts, readiness and route hosts, so a project declaring
			// an environment whose namespace another project holds would have
			// been shown that project's workloads.
			//
			// A namespace nobody has taken yet reads as empty anyway, so the
			// only cost here is the case that should cost something. When the
			// ownership list could not be read at all, nothing is established
			// and nothing is read.
			var apps []appSummary
			if owned && ownersKnown {
				apps = p.getAppSummaries(ctx, nsName, env.Name)
			}

			// Derive status from Project CR phase
			status := strings.ToLower(proj.Status.Phase)
			if status == "" {
				status = "active"
			}

			envs = append(envs, environmentResponse{
				Name:      env.Name,
				Namespace: nsName,
				Status:    status,
				Apps:      apps,
				Order:     fmt.Sprintf("%d", i),
				Owned:     owned,
			})
		}

		projects = append(projects, projectResponse{
			Name:         proj.Name,
			DisplayName:  proj.Spec.DisplayName,
			Role:         role,
			Environments: envs,
			EnvLimit:     proj.EffectiveEnvLimit(),
		})
	}

	sort.Slice(projects, func(i, j int) bool {
		return projects[i].Name < projects[j].Name
	})

	respondJSON(w, http.StatusOK, projects)
}

// getAppSummaries lists the apps in one project environment. The environment
// name comes from the Project spec the caller is already iterating: the
// reconciler and the Routes handler read the same value back off the
// namespace's kipper.run/environment label (which the project reconciler
// stamps from this spec), so passing it through keeps implicit-host
// derivation in agreement without a namespace read per environment on every
// Projects poll.
// namespaceOwners maps each live namespace to the project that owns it.
//
// Through the shared owner lookup, because what this gates is reading what is
// inside the namespace: app names, images, replica counts, readiness and route
// hosts. Reading the label here answered that from a value anyone who can write
// a namespace can set. A namespace nothing owns maps to the empty string, which
// no project name matches.
//
// The projects it resolves against are the ones the caller has already listed,
// so the whole map costs one namespace list and no project reads at all. What
// the lookup requires, and the release it starts requiring the claim, is stated
// once in nsowner.Of.
//
// Every namespace, not only the Kipper-managed ones. A namespace that exists
// without Kipper's managed-by label still occupies the name, the reconciler
// refuses to adopt it, and ProjectAccessResolver resolves a request to it
// regardless of that label — so selecting on managed-by would leave the one
// case this exists for looking like a free name.
//
// The second return says whether the answer is known at all. A failed list
// leaves every claim reported as standing, which is the pre-existing behaviour
// and only affects which tab the console offers; it must not be read as
// permission to list what is inside those namespaces, so the caller stops
// reading them instead.
func (p *Projects) namespaceOwners(ctx context.Context, projects []kipperv1.Project) (map[string]string, bool) {
	list, err := p.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, false
	}
	listed := make(listedProjects, len(projects))
	for i := range projects {
		listed[projects[i].Name] = &projects[i]
	}
	owners := make(map[string]string, len(list.Items))
	for i := range list.Items {
		owner, ok, err := nsowner.OfNamespace(ctx, listed, &list.Items[i])
		if err != nil {
			return nil, false
		}
		if ok {
			owners[list.Items[i].Name] = owner
		} else {
			owners[list.Items[i].Name] = ""
		}
	}
	return owners, true
}

// listedProjects answers the owner lookup's project reads from a page the
// caller has already fetched.
//
// Resolving every namespace on a cluster would otherwise be one project read
// per namespace on an endpoint that already holds all of them.
type listedProjects map[string]*kipperv1.Project

func (l listedProjects) Get(_ context.Context, key crclient.ObjectKey, obj crclient.Object, _ ...crclient.GetOption) error {
	found, ok := l[key.Name]
	if !ok {
		return errors.NewNotFound(kipperv1.GroupVersion.WithResource("projects").GroupResource(), key.Name)
	}
	target, ok := obj.(*kipperv1.Project)
	if !ok {
		return fmt.Errorf("listedProjects can only answer for a Project, not %T", obj)
	}
	found.DeepCopyInto(target)
	return nil
}

func (p *Projects) getAppSummaries(ctx context.Context, namespace, environment string) []appSummary {
	var appList kipperv1.AppList
	if err := p.CRClient.List(ctx, &appList, crclient.InNamespace(namespace)); err != nil {
		return nil
	}

	apps := make([]appSummary, 0, len(appList.Items))
	for _, app := range appList.Items {
		replicas := app.Status.Replicas
		if replicas == 0 && app.Spec.Replicas != nil {
			replicas = *app.Spec.Replicas
		}

		status := strings.ToLower(app.Status.Phase)
		if status == "" {
			status = "pending"
		}
		// While a build is running, show it as the app's status — this takes
		// precedence over the workload phase, so both a first build (placeholder
		// pod "pending") and a rebuild of a running app read as "building".
		if app.Status.Build != nil && (app.Status.Build.Phase == "Building" || app.Status.Build.Phase == "Pending") {
			status = "building"
		}

		apps = append(apps, appSummary{
			Name:     app.Name,
			Image:    app.Spec.Image,
			Status:   status,
			Ready:    fmt.Sprintf("%d/%d", app.Status.ReadyReplicas, replicas),
			Replicas: replicas,
			URL:      p.appPublicURL(&app, environment),
		})
	}

	return apps
}

// appPublicURL derives the app's serving URL from its route. An explicit
// Spec.Route.Host wins; an implicit one is derived from the cluster domain
// and the environment name, exactly like the App reconciler and the Routes
// handler (which read the same value off the namespace's environment label),
// so routed apps get a link on the Projects screen too. A route path of "/"
// collapses to the bare host so links stay clean. Apps without a route have
// no public URL.
func (p *Projects) appPublicURL(app *kipperv1.App, environment string) string {
	route := app.Spec.Route
	if route == nil {
		return ""
	}
	host := route.Host
	if host == "" {
		if p.Domain == "" {
			return ""
		}
		host = domain.SubdomainFor(controllers.AppHostPrefix(app.Name, environment), p.Domain)
	}
	url := "https://" + host
	if route.Path != "" && route.Path != "/" {
		url += route.Path
	}
	return url
}

// Promote copies an app's image from one environment to another.
// POST /api/v1/projects/{name}/promote
func (p *Projects) Promote(w http.ResponseWriter, r *http.Request) {
	project := chi.URLParam(r, "name")

	var req struct {
		App  string `json:"app"`
		From string `json:"from"`
		To   string `json:"to"`
		All  bool   `json:"all"`
	}
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.From == "" || req.To == "" {
		respondError(w, http.StatusBadRequest, "from and to are required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	// Both ends have to be environments this project declares. Composing a
	// namespace from an unchecked name reaches wherever the name lands: a
	// deployer of "shop" promoting to "prod" builds "shop-prod", which belongs
	// to the project called shop-prod whenever shop does not declare that
	// environment — writing an app into their namespace, or copying their app's
	// spec and env back out. The gate in front of this route resolved the
	// caller against the project named in the path, and this is the one place
	// left that then acted somewhere else.
	fromNs, err := p.projectEnvironmentNamespace(ctx, project, req.From)
	if err != nil {
		respondEnvironmentNamespace(w, err)
		return
	}
	toNs, err := p.projectEnvironmentNamespace(ctx, project, req.To)
	if err != nil {
		respondEnvironmentNamespace(w, err)
		return
	}

	var appNames []string
	if req.All {
		var appList kipperv1.AppList
		if err := p.CRClient.List(ctx, &appList, crclient.InNamespace(fromNs)); err != nil {
			respondError(w, http.StatusInternalServerError, "failed to list apps")
			return
		}
		for _, a := range appList.Items {
			appNames = append(appNames, a.Name)
		}
	} else {
		if req.App == "" {
			respondError(w, http.StatusBadRequest, "app name or all=true required")
			return
		}
		appNames = []string{req.App}
	}

	promoted := make([]string, 0)
	for _, appName := range appNames {
		var sourceApp kipperv1.App
		if err := p.CRClient.Get(ctx, crclient.ObjectKey{Namespace: fromNs, Name: appName}, &sourceApp); err != nil {
			continue
		}

		sourceImage := sourceApp.Spec.Image

		// A promotion creates the app in the target, so it reserves a name there
		// like any other create and cannot report an app promoted into a
		// collision it just made.
		release, reserveErr := workloadname.Reserve(ctx, p.CRClient, toNs, appName, "app")
		if reserveErr != nil {
			continue
		}

		var targetApp kipperv1.App
		err := p.CRClient.Get(ctx, crclient.ObjectKey{Namespace: toNs, Name: appName}, &targetApp)
		if err != nil {
			// Target doesn't exist — promote by deep-copying the source spec
			// (Env, SecretRefs, ServiceBindings, Resources, Autoscale, ...)
			// instead of creating an empty stub. Without this an app
			// promoted into a fresh env would have no env vars, no
			// service credentials and no settings, requiring the user to
			// re-enter everything manually.
			//
			// Route is dropped — the source's hostname belongs to the
			// source env's Ingress; the user sets the new env's URL via
			// the route panel (or copies the env wholesale, which assigns
			// fresh hostnames automatically).
			newApp := &kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{
					Name:      appName,
					Namespace: toNs,
					Labels:    sourceApp.Labels,
					Annotations: map[string]string{
						"kipper.run/promoted-from":  req.From,
						"kipper.run/promoted-at":    time.Now().Format(time.RFC3339),
						"kipper.run/promoted-image": sourceImage,
					},
				},
				Spec: *sourceApp.Spec.DeepCopy(),
			}
			newApp.Spec.Route = nil

			recordPromotionHistory(newApp.Annotations, sourceImage, req.From)

			if err := p.CRClient.Create(ctx, newApp); err != nil {
				// AlreadyExists proves the app is there, so the reservation is
				// its own and stands.
				if !errors.IsAlreadyExists(err) {
					release()
				}
				continue
			}

			promoted = append(promoted, appName)
			continue
		}

		// Target exists — update the image
		targetApp.Spec.Image = sourceImage

		if targetApp.Annotations == nil {
			targetApp.Annotations = make(map[string]string)
		}
		targetApp.Annotations["kipper.run/promoted-from"] = req.From
		targetApp.Annotations["kipper.run/promoted-at"] = time.Now().Format(time.RFC3339)
		targetApp.Annotations["kipper.run/promoted-image"] = sourceImage

		recordPromotionHistory(targetApp.Annotations, sourceImage, req.From)

		if err := p.CRClient.Update(ctx, &targetApp); err != nil {
			continue
		}

		promoted = append(promoted, appName)
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"promoted": promoted,
		"from":     req.From,
		"to":       req.To,
	})
}

// checkEnvironmentLimit enforces the per-tier environment cap the same way the
// Project CRD's CEL rule does, so the console API and a direct GitOps/kubectl
// write reject the same proposals. A count within the effective limit (admin
// MaxEnvironments override, else the tier default) is always fine, and a
// proposal that does not grow the count past its current value is allowed too,
// so an already-over-limit project (e.g. after a forced tier downgrade) stays
// editable and can be worked back down. Pass currentCount 0 for a create.
//
// The cap is exact only because the callers do a plain Get-then-Update on the
// Project: two racing writes that both pass this check collide on
// resourceVersion, and the loser fails rather than silently retrying a stale
// list. If a caller is ever wrapped in retry.RetryOnConflict, re-run this
// check inside the retry against the freshly loaded project.
func checkEnvironmentLimit(project *kipperv1.Project, proposedCount, currentCount int) error {
	if proposedCount < 1 {
		proposedCount = 1
	}
	limit := project.EffectiveEnvLimit()
	if proposedCount <= limit || proposedCount <= currentCount {
		return nil
	}
	return fmt.Errorf("project %q is at its environment limit (%d); a cluster admin can raise it by setting maxEnvironments, once the cluster has the capacity to back more environments", project.Name, limit)
}

// Create creates a new Kipper-managed project.
// POST /api/v1/projects
func (p *Projects) Create(w http.ResponseWriter, r *http.Request) {
	var req createProjectRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if req.Name == "" {
		respondError(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := validateTier(req.Tier); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	envs := req.Environments
	if len(envs) == 0 {
		envs = []string{"default"}
	}
	if err := validateEnvironmentNames(req.Name, envs); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	// The one place a collision can still be prevented rather than contained.
	if err := p.refuseNamespaceCollision(ctx, req.Name, envs); err != nil {
		respondNamespaceCollision(w, err)
		return
	}

	environments := make([]kipperv1.ProjectEnvironment, 0, len(envs))
	for _, env := range envs {
		environments = append(environments, kipperv1.ProjectEnvironment{Name: env})
	}

	// The creator becomes the project's first owner so they can manage it and
	// add other members without needing a cluster admin.
	var members []kipperv1.ProjectMember
	if claims := middleware.UserFromContext(ctx); claims != nil && claims.Email != "" {
		members = []kipperv1.ProjectMember{{Email: claims.Email, Role: kipperv1.ProjectRoleOwner}}
	}

	project := &kipperv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.Name,
			Labels: map[string]string{
				kipperLabel: kipperValue,
			},
		},
		Spec: kipperv1.ProjectSpec{
			DisplayName:  req.DisplayName,
			Environments: environments,
			Members:      members,
			Tier:         req.Tier,
		},
	}

	if err := checkEnvironmentLimit(project, len(project.Spec.Environments), 0); err != nil {
		respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	if err := p.CRClient.Create(ctx, project); err != nil {
		if errors.IsAlreadyExists(err) {
			respondError(w, http.StatusConflict, fmt.Sprintf("project %q already exists", req.Name))
			return
		}
		log.Printf("projects: creating %s: %v", req.Name, err)
		respondError(w, http.StatusInternalServerError, "failed to create project")
		return
	}

	// Derive expected namespace names for the response
	namespaces := make([]string, 0, len(envs))
	for _, env := range envs {
		nsName := controllers.ResolveNamespace(req.Name, env)
		namespaces = append(namespaces, nsName)
	}

	respondJSON(w, http.StatusCreated, map[string]any{
		"name":         req.Name,
		"environments": envs,
		"namespaces":   namespaces,
	})
}

// Update mutates the project's editable spec fields. Today that means the
// environment list (add, remove, reorder) and the display name. Removing an
// environment cascades to its namespace and everything inside it via the
// project reconciler — callers are responsible for confirming destructive
// edits with the user before sending them.
// PUT /api/v1/projects/{name}
func (p *Projects) Update(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	var req updateProjectRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.Environments) == 0 {
		respondError(w, http.StatusBadRequest, "environments must contain at least one entry")
		return
	}
	if err := validateEnvironmentNames(name, req.Environments); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := p.refuseNamespaceCollision(r.Context(), name, req.Environments); err != nil {
		respondNamespaceCollision(w, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var project kipperv1.Project
	if err := p.CRClient.Get(ctx, crclient.ObjectKey{Name: name}, &project); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", name))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to load project")
		return
	}

	// Rebuild the environment list from the request but keep each surviving
	// environment's quota override; the request only carries names.
	existingQuota := make(map[string]*kipperv1.EnvQuota, len(project.Spec.Environments))
	for _, env := range project.Spec.Environments {
		existingQuota[env.Name] = env.Quota
	}
	envs := make([]kipperv1.ProjectEnvironment, 0, len(req.Environments))
	for _, env := range req.Environments {
		envs = append(envs, kipperv1.ProjectEnvironment{Name: env, Quota: existingQuota[env]})
	}

	// The wholesale env-list rewrite is the bypass a cap on AddEnvironment alone
	// would miss: an owner could PUT many names at once. Ratchet against the
	// current count so a reduction on an over-limit project still goes through.
	if err := checkEnvironmentLimit(&project, len(envs), len(project.Spec.Environments)); err != nil {
		respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	project.Spec.Environments = envs
	if req.DisplayName != nil {
		project.Spec.DisplayName = *req.DisplayName
	}

	if err := p.CRClient.Update(ctx, &project); err != nil {
		if errors.IsConflict(err) {
			respondError(w, http.StatusConflict, "project was modified concurrently; reload and retry")
			return
		}
		log.Printf("projects: updating %s: %v", chi.URLParam(r, "name"), err)
		respondError(w, http.StatusInternalServerError, "failed to update project")
		return
	}

	namespaces := make([]string, 0, len(req.Environments))
	for _, env := range req.Environments {
		nsName := controllers.ResolveNamespace(name, env)
		namespaces = append(namespaces, nsName)
	}

	respondJSON(w, http.StatusOK, map[string]any{
		"name":         name,
		"environments": req.Environments,
		"namespaces":   namespaces,
	})
}

// copyPreviewApp is what the wizard renders the "routes", "env vars" and
// "resources" steps from. We only echo what the wizard needs so the
// payload stays compact for projects with many apps.
type copyPreviewApp struct {
	Name      string                `json:"name"`
	Image     string                `json:"image"`
	Port      int32                 `json:"port"`
	Replicas  int32                 `json:"replicas"`
	Env       map[string]string     `json:"env"`
	Route     *copyPreviewRoute     `json:"route,omitempty"`
	Resources kipperv1.AppResources `json:"resources"`
}

type copyPreviewRoute struct {
	Host string `json:"host"`
	Path string `json:"path"`
}

type copyPreviewService struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	Version string `json:"version,omitempty"`
	Storage string `json:"storage,omitempty"`
}

type copyPreviewVolume struct {
	Name string `json:"name"`
	Size string `json:"size"`
}

type copyPreviewSecret struct {
	Name string   `json:"name"`
	Keys []string `json:"keys"`
}

type copyPreviewResponse struct {
	Source          string               `json:"source"`
	SourceNamespace string               `json:"source_namespace"`
	Target          string               `json:"target"`
	TargetNamespace string               `json:"target_namespace"`
	ClusterDomain   string               `json:"cluster_domain"`
	DefaultHosts    map[string]string    `json:"default_hosts"`
	Apps            []copyPreviewApp     `json:"apps"`
	Services        []copyPreviewService `json:"services"`
	Volumes         []copyPreviewVolume  `json:"volumes"`
	Functions       []string             `json:"functions"`
	Jobs            []string             `json:"jobs"`
	Secrets         []copyPreviewSecret  `json:"secrets"`
}

// CopyPreview returns the structure of a source environment in the shape
// the copy-wizard renders forms over. Secret VALUES are never returned —
// the wizard only needs to know which secrets exist and what keys they
// hold; rotation lives in the secrets handler.
//
// GET /api/v1/projects/{name}/copy-preview?from=<env>&target=<env>
func (p *Projects) CopyPreview(w http.ResponseWriter, r *http.Request) {
	projectName := chi.URLParam(r, "name")
	source := r.URL.Query().Get("from")
	target := r.URL.Query().Get("target")
	if source == "" {
		respondError(w, http.StatusBadRequest, "from query parameter is required")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	sourceNs := controllers.ResolveNamespace(projectName, source)
	targetNs := controllers.ResolveNamespace(projectName, target)

	// This lists the source's apps, services, volumes, functions, jobs and the
	// name and key names of every Secret in it. Both names are guesses built
	// from a project and an environment the caller supplied, so ownership is
	// established before anything is read.
	if err := namespaceBelongsTo(ctx, p.Client, p.CRClient, sourceNs, projectName); err != nil {
		respondForeignNamespace(w, err)
		return
	}
	// The target is normally the environment about to be created, so its
	// absence is expected and only a namespace somebody else holds is refused.
	if err := namespaceBelongsTo(ctx, p.Client, p.CRClient, targetNs, projectName); err != nil {
		var foreign *foreignNamespaceError
		if !goerrors.As(err, &foreign) || !foreign.absent {
			respondForeignNamespace(w, err)
			return
		}
	}

	resp := copyPreviewResponse{
		Source:          source,
		SourceNamespace: sourceNs,
		Target:          target,
		TargetNamespace: targetNs,
		ClusterDomain:   p.Domain,
		DefaultHosts:    map[string]string{},
		Apps:            []copyPreviewApp{},
		Services:        []copyPreviewService{},
		Volumes:         []copyPreviewVolume{},
		Functions:       []string{},
		Jobs:            []string{},
		Secrets:         []copyPreviewSecret{},
	}

	var apps kipperv1.AppList
	if err := p.CRClient.List(ctx, &apps, crclient.InNamespace(sourceNs)); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list apps")
		return
	}
	for i := range apps.Items {
		a := &apps.Items[i]
		entry := copyPreviewApp{
			Name:      a.Name,
			Image:     a.Spec.Image,
			Port:      a.Spec.Port,
			Env:       a.Spec.Env,
			Resources: a.Spec.Resources,
		}
		if a.Spec.Replicas != nil {
			entry.Replicas = *a.Spec.Replicas
		} else {
			entry.Replicas = 1
		}
		if a.Spec.Route != nil {
			entry.Route = &copyPreviewRoute{Host: a.Spec.Route.Host, Path: a.Spec.Route.Path}
			if entry.Route.Path == "" {
				entry.Route.Path = "/"
			}
			// Pre-compute the auto-hostname for the prospective target env
			// so the wizard can pre-fill the input without having to
			// re-derive the kipper.run hyphen vs. dot rule.
			if p.Domain != "" {
				resp.DefaultHosts[a.Name] = defaultHostFor(a.Name, target, p.Domain)
			}
		}
		resp.Apps = append(resp.Apps, entry)
	}

	var services kipperv1.ServiceList
	if err := p.CRClient.List(ctx, &services, crclient.InNamespace(sourceNs)); err == nil {
		for i := range services.Items {
			s := &services.Items[i]
			resp.Services = append(resp.Services, copyPreviewService{
				Name: s.Name, Type: s.Spec.Type, Version: s.Spec.Version, Storage: s.Spec.Storage,
			})
		}
	}

	var volumes kipperv1.VolumeList
	if err := p.CRClient.List(ctx, &volumes, crclient.InNamespace(sourceNs)); err == nil {
		for i := range volumes.Items {
			v := &volumes.Items[i]
			resp.Volumes = append(resp.Volumes, copyPreviewVolume{Name: v.Name, Size: v.Spec.Size})
		}
	}

	var fns kipperv1.FunctionList
	if err := p.CRClient.List(ctx, &fns, crclient.InNamespace(sourceNs)); err == nil {
		for i := range fns.Items {
			resp.Functions = append(resp.Functions, fns.Items[i].Name)
		}
	}

	var jobs kipperv1.JobList
	if err := p.CRClient.List(ctx, &jobs, crclient.InNamespace(sourceNs)); err == nil {
		for i := range jobs.Items {
			resp.Jobs = append(resp.Jobs, jobs.Items[i].Name)
		}
	}

	secrets, err := p.Client.CoreV1().Secrets(sourceNs).List(ctx, metav1.ListOptions{
		LabelSelector: "app.kubernetes.io/managed-by=kipper",
	})
	if err == nil {
		for i := range secrets.Items {
			s := &secrets.Items[i]
			if shouldOmitSecretFromPreview(s) {
				continue
			}
			keys := make([]string, 0, len(s.Data))
			for k := range s.Data {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			resp.Secrets = append(resp.Secrets, copyPreviewSecret{Name: s.Name, Keys: keys})
		}
	}

	respondJSON(w, http.StatusOK, resp)
}

// shouldOmitSecretFromPreview hides reconciler-managed secrets from the
// wizard. Showing them would be misleading — the user can't act on them
// (they regenerate automatically) and they clutter the secrets step.
func shouldOmitSecretFromPreview(s *corev1.Secret) bool {
	if _, ok := s.Labels["kipper.run/service-type"]; ok {
		return true
	}
	if s.Labels[kipperlabels.Binding] == "true" {
		return true
	}
	if s.Labels["kipper.run/registry"] == "true" {
		return true
	}
	for _, ref := range s.OwnerReferences {
		if ref.Controller != nil && *ref.Controller {
			return true
		}
	}
	return false
}

// defaultHostFor builds the per-env hostname the way copyenv does. Kept
// here as a thin wrapper so the preview endpoint and the copier agree on
// the rule without the handler needing to import the copyenv internals.
func defaultHostFor(appName, envName, clusterDomain string) string {
	if clusterDomain == "" {
		return ""
	}
	prefix := appName
	if envName != "" && envName != "default" {
		prefix = appName + "-" + envName
	}
	return domain.SubdomainFor(prefix, strings.TrimPrefix(clusterDomain, "."))
}

// AddEnvironment appends a new environment to an existing project. When
// copy_from is provided, every Kipper-owned resource in the source env's
// namespace (apps, services, volumes, functions, jobs, user secrets) is
// deep-copied into the new namespace. Service credentials are regenerated
// by the service reconciler; volume PVCs start empty (data migration is a
// separate flow). Apps with a route in the source get a fresh per-env
// hostname under the cluster wildcard, so the new env is reachable in a
// browser as soon as pods come up.
//
// POST /api/v1/projects/{name}/environments
func (p *Projects) AddEnvironment(w http.ResponseWriter, r *http.Request) {
	projectName := chi.URLParam(r, "name")

	var req addEnvironmentRequest
	if err := decodeJSON(r, &req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		respondError(w, http.StatusBadRequest, "environment name is required")
		return
	}
	if req.Name == req.CopyFrom {
		respondError(w, http.StatusBadRequest, "copy_from must differ from the new environment name")
		return
	}
	if err := validateEnvironmentNames(projectName, []string{req.Name}); err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := p.refuseNamespaceCollision(r.Context(), projectName, []string{req.Name}); err != nil {
		respondNamespaceCollision(w, err)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var project kipperv1.Project
	if err := p.CRClient.Get(ctx, crclient.ObjectKey{Name: projectName}, &project); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", projectName))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to load project")
		return
	}

	// Write down the environment a project has but never declared, before
	// appending to the list. A project that declares none still gets one from
	// the reconciler, and appending to the empty spec replaced it: the next
	// reconcile read a list that no longer mentioned it, so its namespace was
	// not on the keep-list and was deleted with everything in it.
	//
	// It also makes the checks below answer for the environments that exist
	// rather than the ones that were typed, so adding "test" to a project that
	// already has one collides, copying from it is allowed, and the limit counts
	// it.
	project.Spec.Environments = controllers.ProjectEnvironments(&project)

	for _, env := range project.Spec.Environments {
		if env.Name == req.Name {
			respondError(w, http.StatusConflict, fmt.Sprintf("environment %q already exists in project %q", req.Name, projectName))
			return
		}
	}

	if req.CopyFrom != "" {
		found := false
		for _, env := range project.Spec.Environments {
			if env.Name == req.CopyFrom {
				found = true
				break
			}
		}
		if !found {
			respondError(w, http.StatusBadRequest, fmt.Sprintf("source environment %q does not exist in project %q", req.CopyFrom, projectName))
			return
		}

		// Declared, and now established as this project's, before the project is
		// changed. Checking only before the copy left a refused request with the
		// environment durably added and its namespace reconciled. The check
		// before Copier.Run stays as the race guard.
		//
		// After the declaration check rather than before it, so naming an
		// environment that does not exist stays the plain answer it was instead
		// of becoming a refusal about a namespace.
		sourceNs := controllers.ResolveNamespace(projectName, req.CopyFrom)
		if err := namespaceBelongsTo(ctx, p.Client, p.CRClient, sourceNs, projectName); err != nil {
			respondForeignNamespace(w, err)
			return
		}
	}

	if err := checkEnvironmentLimit(&project, len(project.Spec.Environments)+1, len(project.Spec.Environments)); err != nil {
		respondError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	// Append the new env to the project spec. The project reconciler then
	// creates the namespace. We wait briefly for the namespace to appear
	// before invoking the copier — services and apps need it to exist as
	// their target.
	project.Spec.Environments = append(project.Spec.Environments,
		kipperv1.ProjectEnvironment{Name: req.Name})
	if err := p.CRClient.Update(ctx, &project); err != nil {
		if errors.IsConflict(err) {
			respondError(w, http.StatusConflict, "project was modified concurrently; reload and retry")
			return
		}
		log.Printf("projects: updating %s: %v", chi.URLParam(r, "name"), err)
		respondError(w, http.StatusInternalServerError, "failed to update project")
		return
	}

	targetNs := projectName
	if req.Name != "default" {
		targetNs = fmt.Sprintf("%s-%s", projectName, req.Name)
	}

	// Waits for the namespace to resolve to this project, not merely for it to
	// exist.
	//
	// The reconciler creates a namespace before it decides whether the project
	// may hold it, and refuses one another project claims. Waiting on existence
	// alone returned as soon as the object appeared and left this handler
	// writing into a namespace the reconcile was about to refuse. The claim is
	// recorded once the namespace is proven this project's, so it is the thing
	// worth waiting for.
	if err := waitForNamespaceClaim(ctx, p.CRClient, p.Client, projectName, targetNs); err != nil {
		log.Printf("projects: waiting for project %s to claim namespace %s: %v", projectName, targetNs, err)
		if goerrors.Is(err, errNamespaceRefused) {
			respondError(w, http.StatusConflict, err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError,
			fmt.Sprintf("namespace %s did not become this project's", targetNs))
		return
	}

	// The wait answers whether the namespace exists, which is not the same
	// question as whether it is ours. refuseNamespaceCollision closed the
	// ordinary case before the CR was written; this closes the window between
	// that check and now, and it sits here because what follows writes into the
	// namespace under the console's own service account rather than the
	// caller's.
	if err := namespaceBelongsTo(ctx, p.Client, p.CRClient, targetNs, projectName); err != nil {
		respondForeignNamespace(w, err)
		return
	}

	resp := map[string]any{
		"name":      req.Name,
		"namespace": targetNs,
	}

	if req.CopyFrom != "" {
		sourceNs := projectName
		if req.CopyFrom != "default" {
			sourceNs = fmt.Sprintf("%s-%s", projectName, req.CopyFrom)
		}
		// The source is read, not written, and it is derived from a name the
		// caller supplied. Reading a namespace this project does not own would
		// copy its apps, services and secrets into one the caller does.
		if err := namespaceBelongsTo(ctx, p.Client, p.CRClient, sourceNs, projectName); err != nil {
			respondForeignNamespace(w, err)
			return
		}

		assign := true
		if req.AssignDefaultRoutes != nil {
			assign = *req.AssignDefaultRoutes
		}
		c := &copyenv.Copier{CRClient: p.CRClient, Client: p.Client}
		summary, copyErr := c.Run(ctx, copyenv.Options{
			Source:              sourceNs,
			Target:              targetNs,
			TargetEnv:           req.Name,
			ClusterDomain:       p.Domain,
			AssignDefaultRoutes: assign,
			AppOverrides:        req.Apps,
		})
		resp["copy"] = summary
		if copyErr != nil {
			log.Printf("projects: copying %s into %s: %v", sourceNs, targetNs, copyErr)
			respondJSON(w, http.StatusPartialContent, map[string]any{
				"name":      req.Name,
				"namespace": targetNs,
				"copy":      summary,
				"error":     "copying the environment did not finish; the summary says how far it got",
			})
			return
		}
	}

	respondJSON(w, http.StatusCreated, resp)
}

// errNamespaceRefused says the reconcile examined the namespace and would not
// give it to this project, so waiting longer changes nothing.
var errNamespaceRefused = goerrors.New("the namespace is not this project's")

// waitForNamespaceClaim polls until the namespace resolves to this project, or
// the context expires.
//
// It asks the shared owner lookup, so what it waits for is whatever that
// requires. In this release the label still answers when neither record covers
// the namespace, which means it can return before the claim is published;
// release 2 is where the name becomes literal. Existence
// says only that an object appeared, which is also true of a namespace the
// reconcile is about to refuse because another project holds it.
func waitForNamespaceClaim(ctx context.Context, cr crclient.Client, client kubernetes.Interface, project, namespace string) error {
	for {
		live, liveErr := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
		if liveErr != nil && !errors.IsNotFound(liveErr) {
			return liveErr
		}

		var p kipperv1.Project
		err := cr.Get(ctx, crclient.ObjectKey{Name: project}, &p)
		if err != nil && !errors.IsNotFound(err) {
			return err
		}
		if err == nil && liveErr == nil {
			// Through the shared owner lookup rather than a second copy of the
			// rule. This used to carry its own claim check and its own label
			// fallback, so the release that stops accepting a label had two
			// places to change and one of them was not the one anybody would
			// look at. What the lookup requires, and the release it starts
			// requiring the claim, is stated once in nsowner.Of.
			// A read that failed is not an answer, and this is a poll: the
			// two reads either side of it retry the same class of failure, so
			// giving up here fails an AddEnvironment on one blip.
			owns, ownErr := nsowner.OwnsNamespace(ctx, cr, project, live)
			if ownErr == nil && owns {
				return nil
			}
		}

		// The reconcile has looked and refused. Checked whether or not the
		// namespace is readable, because a refusal is a decision about a name
		// and the namespace may be somebody else's or not there at all.
		// Waiting out the timeout would turn a decision already made into a
		// generic failure a minute later.
		if err == nil {
			if refusal := namespaceRefusal(&p, namespace); refusal != "" {
				return fmt.Errorf("%w: %s", errNamespaceRefused, refusal)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// namespaceRefusal returns the reconcile's reason for refusing this namespace,
// or empty when it has not refused this one.
//
// A condition from an older generation describes a spec that has since changed
// and is ignored rather than treated as current.
func namespaceRefusal(p *kipperv1.Project, namespace string) string {
	cond := apimeta.FindStatusCondition(p.Status.Conditions, "NamespaceConflict")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		return ""
	}
	if cond.ObservedGeneration != 0 && cond.ObservedGeneration < p.Generation {
		return ""
	}
	for _, refused := range controllers.RefusedNamespaces(cond.Message) {
		if refused == namespace {
			return cond.Message
		}
	}
	return ""
}

// validateEnvironmentNames rejects duplicates and names that would produce an
// invalid namespace when joined with the project name.
func validateEnvironmentNames(projectName string, envs []string) error {
	seen := make(map[string]struct{}, len(envs))
	for _, env := range envs {
		if env == "" {
			return fmt.Errorf("environment name must not be empty")
		}
		if _, dup := seen[env]; dup {
			return fmt.Errorf("environment %q listed more than once", env)
		}
		seen[env] = struct{}{}

		ns := controllers.ResolveNamespace(projectName, env)
		if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
			return fmt.Errorf("environment %q: invalid namespace name %q (%s)", env, ns, strings.Join(errs, "; "))
		}
	}
	return nil
}

// Delete deletes a Kipper-managed namespace.
// DELETE /api/v1/projects/{name}
func (p *Projects) Delete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	// Read before deleting, because what this may delete afterwards is decided
	// from the project's own status and the delete takes that away.
	var project kipperv1.Project
	if err := p.CRClient.Get(ctx, crclient.ObjectKey{Name: name}, &project); err != nil {
		if errors.IsNotFound(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", name))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to delete project")
		return
	}
	held := project.Status

	// Bound to the object that was read. Between the read and here the project
	// can finish deleting and be created again under the same name with
	// different members, and a delete by name alone would take that one instead
	// on the strength of a check made against its predecessor.
	if err := p.CRClient.Delete(ctx, &project, crclient.Preconditions{UID: &project.UID}); err != nil {
		if errors.IsNotFound(err) || errors.IsConflict(err) {
			respondError(w, http.StatusNotFound, fmt.Sprintf("project %q not found", name))
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to delete project")
		return
	}

	// Best-effort cleanup: delete namespaces and keda function ingresses.
	//
	// The label lists the candidates and does not authorise deleting any of
	// them. Anyone who can write a namespace can point its label at another
	// project, and deleting on the label alone made that a way to have somebody
	// else's namespace destroyed: relabel it, delete a project of your own, and
	// the namespace goes with everything in it. The project's own record of
	// having held it is what decides, which is the rule the reconciler's own
	// cleanup deletes by.
	labelled, nsErr := p.Client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: fmt.Sprintf("kipper.run/project=%s", name),
	})
	// Every project, because this project's own records cannot tell whether
	// somebody else holds the live object, and a claim on it outranks any
	// name-only record of the name. A failed read leaves the candidates
	// unchecked, so nothing is deleted rather than deleted unchecked.
	var projects kipperv1.ProjectList
	projectsErr := p.CRClient.List(ctx, &projects)
	//
	// Each delete names the object it was authorised against, for the same
	// reason: a namespace can finish deleting and be recreated under the same
	// name between the list and the delete, and the record covered only the one
	// that was read. A precondition that no longer holds means the object is
	// already gone, and there is nothing to collect.
	namespaces := &corev1.NamespaceList{}
	var stranded []string
	if nsErr == nil && projectsErr == nil {
		for _, ns := range labelled.Items {
			if !nsowner.EverHeld(held, ns.Name, ns.UID) {
				continue
			}
			if nsowner.ClaimedElsewhere(projects.Items, name, ns.Name, ns.UID) {
				continue
			}
			uid := ns.UID
			err := p.Client.CoreV1().Namespaces().Delete(ctx, ns.Name, metav1.DeleteOptions{
				Preconditions: &metav1.Preconditions{UID: &uid},
			})
			switch {
			case err == nil, errors.IsNotFound(err):
				// The keda ingresses below belong to namespaces that are going.
				namespaces.Items = append(namespaces.Items, ns)
			case errors.IsConflict(err):
				// The precondition no longer holds, so the object this was
				// authorised against has already gone and something else
				// carries the name. Nothing to collect, and its ingresses are
				// not this project's to remove.
			default:
				stranded = append(stranded, ns.Name)
			}
		}
	}

	kedaIngresses, kedaErr := p.Client.NetworkingV1().Ingresses("keda").List(ctx, metav1.ListOptions{
		LabelSelector: "kipper.run/fn-namespace",
	})
	if kedaErr == nil {
		for _, ing := range kedaIngresses.Items {
			fnNs := ing.Labels["kipper.run/fn-namespace"]
			if nsErr == nil {
				for _, ns := range namespaces.Items {
					if fnNs == ns.Name {
						_ = p.Client.NetworkingV1().Ingresses("keda").Delete(ctx, ing.Name, metav1.DeleteOptions{})
					}
				}
			}
		}
	}

	// A namespace that could not be deleted for any reason other than already
	// being gone is still there with its workloads, its credentials and its
	// member bindings, and answering 204 says it was collected. Say which ones
	// instead.
	//
	// This is a report about a moment, not a verdict. The delete above only
	// marks the Project, and the reconciler's finalizer runs the same cleanup
	// until it succeeds, so a namespace named here is often collected shortly
	// afterwards. What the caller must not be told is that the work is done
	// when it is not: a project whose finalizer never runs strands these for
	// good, and nothing else would ever mention them.
	if nsErr != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf(
			"project %q is being deleted, but its namespaces could not be listed, so whether any are still on the cluster is unknown", name))
		return
	}
	if projectsErr != nil {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf(
			"project %q is being deleted, but the other projects could not be read, so which of its namespaces are still its own is unknown and none were collected", name))
		return
	}
	if len(stranded) > 0 {
		respondError(w, http.StatusInternalServerError, fmt.Sprintf(
			"project %q is being deleted, but these namespaces could not be collected and are still on the cluster: %s",
			name, strings.Join(stranded, ", ")))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// recordPromotionHistory appends a "promote" entry to the deploy history annotation.
func recordPromotionHistory(annotations map[string]string, image, fromEnv string) {
	history := loadDeployHistory(annotations)

	revision := 1
	if len(history) > 0 {
		revision = history[0].Revision + 1
	}

	entry := deployEntry{
		Revision:  revision,
		Image:     image,
		Trigger:   "promote:" + fromEnv,
		Timestamp: time.Now().Format(time.RFC3339),
	}

	history = append([]deployEntry{entry}, history...)
	if len(history) > 10 {
		history = history[:10]
	}

	data, err := json.Marshal(history)
	if err != nil {
		return
	}
	annotations[historyAnnotation] = string(data)
}

// projectEnvironmentNamespace resolves one of this project's declared
// environments to its namespace, refusing a name the project does not declare.
//
// It resolves through the same rule the reconciler creates namespaces with, so
// a default environment lands on the project's own name rather than on a
// suffixed one that never existed.
func (p *Projects) projectEnvironmentNamespace(ctx context.Context, project, env string) (string, error) {
	var proj kipperv1.Project
	if err := p.CRClient.Get(ctx, crclient.ObjectKey{Name: project}, &proj); err != nil {
		if errors.IsNotFound(err) {
			return "", &missingProjectError{msg: fmt.Sprintf("project %q not found", project)}
		}
		return "", fmt.Errorf("reading project %q: %w", project, err)
	}
	for _, declared := range controllers.ProjectEnvironments(&proj) {
		if declared.Name != env {
			continue
		}
		ns := controllers.ResolveNamespace(project, env)
		// Declared is not the same as owned. In the collision state this whole
		// change exists for, a project declares an environment whose namespace
		// another project holds, and promote would then read one project's app
		// or write into the other's using the console's own credentials.
		if err := namespaceBelongsTo(ctx, p.Client, p.CRClient, ns, project); err != nil {
			return "", err
		}
		return ns, nil
	}
	return "", &undeclaredEnvironmentError{msg: fmt.Sprintf("project %q has no environment %q", project, env)}
}

// namespaceCollisionError is a name that overlaps another project's namespaces.
// It is distinct from a failure to run the check at all, and the two need
// different answers: a collision is the caller's to resolve by choosing another
// name and will not go away on its own, whereas a check that could not run says
// nothing about the name and is worth retrying. Reporting both as a conflict
// tells a client to rename over what was a passing API error.
type namespaceCollisionError struct{ msg string }

func (e *namespaceCollisionError) Error() string { return e.msg }

// respondNamespaceCollision writes the right status for a refuseNamespaceCollision
// error: 409 for an overlap with another project's declaration, 403 for a live
// namespace this project does not own, 500 for a check that could not run.
//
// The three are genuinely different answers. A declared collision is resolved by
// renaming; a namespace somebody else holds is not the caller's to rename; a
// failed check says nothing about the name and is worth retrying. Reporting an
// ownership refusal as a 500 told an operator their cluster was broken when the
// answer was that they had asked for something not theirs.
func respondNamespaceCollision(w http.ResponseWriter, err error) {
	var collision *namespaceCollisionError
	if goerrors.As(err, &collision) {
		respondError(w, http.StatusConflict, collision.Error())
		return
	}
	var foreign *foreignNamespaceError
	if goerrors.As(err, &foreign) {
		respondError(w, http.StatusForbidden, foreign.Error())
		return
	}
	log.Printf("projects: checking for namespace collisions: %v", err)
	respondError(w, http.StatusInternalServerError, "checking for namespace collisions")
}

// foreignNamespaceError says a namespace is not the one this project owns.
//
// Distinct from namespaceCollisionError: that one refuses a name because
// another Project *declares* it, and the remedy is to rename. This refuses a
// name because the live namespace is not this project's, and there is nothing
// to rename — the caller is asking to work inside somebody else's namespace.
type foreignNamespaceError struct {
	msg string
	// absent says there is no such namespace, as opposed to one somebody else
	// holds. A name nothing has taken is free for this project to be given; a
	// name somebody holds is not. The callers want opposite answers for the
	// two, so the difference is carried rather than read back out of the text.
	absent bool
}

func (e *foreignNamespaceError) Error() string { return e.msg }

// namespaceBelongsTo reports whether the live namespace is the one this project
// owns, from the kipper.run/project label.
//
// That label is what ProjectAccessResolver reads to decide who may reach a
// namespace, so asking the same question here keeps a handler from working
// inside a namespace the request gate would have refused. A name derived from a
// project and an environment is a guess about which namespace is meant, and the
// label on the namespace is a guess too: anyone who can write namespace
// metadata can write it. Ownership goes through the shared owner lookup for
// that reason; what it requires, and the release it starts requiring the claim,
// is stated once in nsowner.Of.
//
// Fail closed in every direction. A namespace that does not exist is not this
// project's, one that cannot be read is not assumed to be, and one carrying no
// label at all is refused like one carrying another project's — an unlabelled
// namespace is exactly what the reconciler will not adopt.
func namespaceBelongsTo(ctx context.Context, client kubernetes.Interface, reader crclient.Client, namespace, projectName string) error {
	ns, err := client.CoreV1().Namespaces().Get(ctx, namespace, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return &foreignNamespaceError{absent: true, msg: fmt.Sprintf(
				"namespace %q does not exist, so project %q cannot use it", namespace, projectName)}
		}
		return fmt.Errorf("reading namespace %s: %w", namespace, err)
	}
	owns, err := nsowner.OwnsNamespace(ctx, reader, projectName, ns)
	if err != nil {
		return fmt.Errorf("establishing who owns namespace %s: %w", namespace, err)
	}
	if !owns {
		return &foreignNamespaceError{msg: fmt.Sprintf(
			"namespace %q is not owned by project %q", namespace, projectName)}
	}
	return nil
}

// respondForeignNamespace refuses a request that named a namespace the project
// does not own. 403 rather than 404: the caller may well know the namespace
// exists, and saying so reveals nothing they could not learn by asking the
// namespace directly and being refused there too.
func respondForeignNamespace(w http.ResponseWriter, err error) {
	var foreign *foreignNamespaceError
	if goerrors.As(err, &foreign) {
		respondError(w, http.StatusForbidden, foreign.Error())
		return
	}
	log.Printf("projects: checking namespace ownership: %v", err)
	respondError(w, http.StatusInternalServerError, "checking namespace ownership")
}

// undeclaredEnvironmentError is a project that has no such environment. It is
// the caller's mistake and naming it back is useful; anything else that goes
// wrong while resolving an environment is the server's and is not.
type undeclaredEnvironmentError struct{ msg string }

func (e *undeclaredEnvironmentError) Error() string { return e.msg }

// missingProjectError is a project that does not exist. An administrator
// reaches this handler for any name, so the three answers have to stay apart:
// no such project is a 404, a project without that environment is a malformed
// request, and a check that could not run is the server's failure.
type missingProjectError struct{ msg string }

func (e *missingProjectError) Error() string { return e.msg }

// respondEnvironmentNamespace writes the right status for a
// projectEnvironmentNamespace failure: 403 for a namespace the project does not
// own, 400 for an environment it does not have, 500 for a check that could not
// run.
//
// The third case is why this classifies rather than defaulting. Treating every
// unrecognised error as a bad request told an operator their request was wrong
// when the apiserver had timed out, and echoed the underlying error back to
// them while doing it.
func respondEnvironmentNamespace(w http.ResponseWriter, err error) {
	var foreign *foreignNamespaceError
	if goerrors.As(err, &foreign) {
		respondError(w, http.StatusForbidden, foreign.Error())
		return
	}
	var missing *missingProjectError
	if goerrors.As(err, &missing) {
		respondError(w, http.StatusNotFound, missing.Error())
		return
	}
	var undeclared *undeclaredEnvironmentError
	if goerrors.As(err, &undeclared) {
		respondError(w, http.StatusBadRequest, undeclared.Error())
		return
	}
	log.Printf("projects: resolving the environment's namespace: %v", err)
	respondError(w, http.StatusInternalServerError, "resolving the environment's namespace")
}

// refuseNamespaceCollision rejects a project whose environments would resolve to
// a namespace another project already resolves to.
//
// Namespace names are not unique across projects: project "shop" with an
// environment "prod" and project "shop-prod" with a default environment both
// resolve to "shop-prod". Allowing both to exist means whichever reconciles last
// owns the namespace, relabels it, and installs its own member RoleBindings
// there — so two sets of members end up with access to one namespace and to
// whatever is running in it. The reconciler refuses to adopt a namespace another
// project claims, which contains the damage; this is what stops the collision
// being created in the first place.
func (p *Projects) refuseNamespaceCollision(ctx context.Context, projectName string, envs []string) error {
	wanted := make(map[string]string, len(envs))
	for _, env := range envs {
		wanted[controllers.ResolveNamespace(projectName, env)] = env
	}

	// A namespace that exists and is not this project's stops the request here,
	// before the Project CR is written. Comparing declarations alone misses it
	// entirely: a namespace created outside Kipper, or left behind by a deleted
	// project, is declared by nobody — and the reconciler will refuse to adopt
	// it, so the environment would be created and never work.
	//
	// It is also what stops AddEnvironment copying into it. The assertion before
	// the copy covers the race; this covers the ordinary case, and covers it
	// before anything has been changed.
	for ns := range wanted {
		err := namespaceBelongsTo(ctx, p.Client, p.CRClient, ns, projectName)
		if err == nil {
			continue
		}
		var foreign *foreignNamespaceError
		if !goerrors.As(err, &foreign) {
			return err
		}
		// A name nothing has taken is the normal case here: the reconciler has
		// not created the namespace yet. It is the one shape this must allow.
		if foreign.absent {
			continue
		}
		return foreign
	}

	var projects kipperv1.ProjectList
	if err := p.CRClient.List(ctx, &projects); err != nil {
		return fmt.Errorf("checking for namespace collisions: %w", err)
	}
	for i := range projects.Items {
		other := &projects.Items[i]
		if other.Name == projectName {
			continue
		}
		// The other project's environments as the reconciler will build them,
		// default included. Reading only what it declares approves a name whose
		// namespace a defaulted environment already occupies, and the reconcile
		// then refuses the namespace this check said was free.
		for _, env := range controllers.ProjectEnvironments(other) {
			ns := controllers.ResolveNamespace(other.Name, env.Name)
			if env, clash := wanted[ns]; clash {
				return &namespaceCollisionError{msg: fmt.Sprintf(
					"environment %q would use namespace %q, which project %q already uses; "+
						"rename this project or that environment", env, ns, other.Name)}
			}
		}
	}
	return nil
}
