package main

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/cors"
	"github.com/go-logr/logr"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
	"github.com/getkipper/kipper/console-api/controller"
	"github.com/getkipper/kipper/console-api/controllers"
	"github.com/getkipper/kipper/console-api/handlers"
	"github.com/getkipper/kipper/console-api/handlers/migration"
	"github.com/getkipper/kipper/console-api/handlers/twofa"
	"github.com/getkipper/kipper/console-api/internal/clusterstamp"
	"github.com/getkipper/kipper/console-api/middleware"
	"github.com/getkipper/kipper/console-api/security"
	"github.com/getkipper/kipper/console-api/share"
	"github.com/getkipper/kipper/console-api/uisession"
	"github.com/getkipper/kipper/console-api/ws"
)

// controllerLogger adapts console-api's slog handler for controller-runtime.
//
// It is separate from configureControllerLogging because SetLogger is a one-way
// latch — the first call wins and later ones are ignored — so a test can drive
// this repeatedly while the global can only be set once per process.
func controllerLogger() logr.Logger {
	return logr.FromSlogHandler(slog.Default().Handler())
}

// configureControllerLogging routes controller-runtime's logging into the same
// slog handler the rest of console-api writes to.
//
// It runs before anything builds a controller-runtime client, so nothing logged
// on the way up is lost. Until SetLogger is called, controller-runtime's root
// sink is a NullLogSink that discards whatever a controller hands it, and thirty
// seconds after start it latches that permanently. Either way every reconcile
// error from every controller goes nowhere, which is how eight apps on one
// cluster stopped reconciling past their middleware step for a day with nothing
// recorded anywhere.
func configureControllerLogging() {
	ctrl.SetLogger(controllerLogger())
}

// version is the console-api build version, injected at build time via
// -ldflags "-X main.version=...". It is surfaced on /health for the CLI's
// version handshake.
var version = "dev"

// datamoverImage resolves the kipper-datamover image for migration data
// transfers. The default covers clusters whose console deployment predates
// the DATAMOVER_IMAGE env var, since `kip upgrade` restarts deployments
// without re-rendering their env.
func datamoverImage() string {
	if img := os.Getenv("DATAMOVER_IMAGE"); img != "" {
		return img
	}
	return "ghcr.io/getkipper/kipper-datamover:latest"
}

func main() {
	// API-key and plan mutations log a JSON audit event so Loki can parse the
	// actor, action, and object.
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))
	configureControllerLogging()

	handlers.BuildVersion = version
	migration.BuildVersion = version

	clientset, dynClient, restConfig, err := buildK8sClients()
	if err != nil {
		log.Fatalf("failed to create kubernetes client: %v", err)
	}

	// Say which build is serving this cluster. An upgrade reads it to tell
	// whether the console-api still running is one that replaces a shared
	// credential's allow-list, which a completed rollout does not say: the image
	// is a moving tag, so a new pod is not necessarily new code. A failure here
	// leaves an upgrade to try again rather than stopping the API from serving.
	if err := clusterstamp.Record(context.Background(), clientset, version); err != nil {
		slog.Warn("recording the console-api build on the namespace failed", "error", err)
	}

	// Create a controller-runtime client for CRD operations
	crScheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(crScheme))
	utilruntime.Must(kipperv1.AddToScheme(crScheme))
	crClient, err := crclient.New(restConfig, crclient.Options{Scheme: crScheme})
	if err != nil {
		log.Fatalf("failed to create controller-runtime client: %v", err)
	}

	// Start the autonomous resource management controller
	resCtrl := controller.NewResourceController(clientset, crClient)
	go resCtrl.Run(context.Background())

	// Age out per-key usage history beyond the retention window.
	go handlers.RunRollupRetention(context.Background(), crClient)

	// Backstop cleanup of build Jobs and their ephemeral credential secrets in
	// kipper-builds: sweeps anything older than 3h, so a hung build or a secret
	// orphaned by a console-api restart never outlives its build.
	go builder.RunBuildJanitor(context.Background(), clientset, 30*time.Minute, 3*time.Hour)

	// Start the CRD controller manager
	go startControllerManager(restConfig)

	// Keep the cluster's kipper.run subdomain alive on the gateway.
	// No-op when KIPPER_RUN_DOMAIN / CLUSTER_HOST aren't set, so this
	// is safe for clusters that don't use the kipper.run gateway.
	startGatewayHeartbeat(context.Background(), clientset, crClient)

	r := buildRouter(context.Background(), clientset, dynClient, restConfig, crClient)

	// Refuse to serve without project enforcement wired — fail closed rather
	// than let the in-handler helpers skip their checks.
	if !handlers.ProjectResolverWired() {
		log.Fatal("project access resolver not wired: refusing to start")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	// ReadHeaderTimeout caps slow-header (slowloris) attacks; IdleTimeout
	// reaps idle keep-alive connections. ReadTimeout and WriteTimeout are left
	// unset on purpose: the API serves long-lived WebSocket log and terminal
	// streams, and a write deadline would sever them.
	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	log.Printf("console-api listening on :%s", port) //nolint:gosec // port from env var, not user input
	if err := srv.ListenAndServe(); err != nil {
		log.Fatalf("server failed: %v", err)
	}
}

// buildRouter assembles the whole HTTP surface. It takes kubernetes.Interface
// rather than the concrete clientset so the assembly itself can be exercised
// against a fake — the routes' authorization gates are decided here, and a test
// that rebuilds the route tree instead of using this one proves only that the
// test agrees with itself.
//
// ctx bounds the background sweepers started below. They belong to the
// migration handler this builds and outlive no process that stops using the
// router, so anything that builds a router and walks away has to be able to
// stop them.
func buildRouter(ctx context.Context, clientset kubernetes.Interface, dynClient dynamic.Interface, restConfig *rest.Config, crClient crclient.Client) http.Handler {
	issuer := os.Getenv("DEX_ISSUER")
	if issuer == "" {
		issuer = "https://dex.localhost/dex"
	}
	audience := os.Getenv("DEX_CLIENT_ID")
	if audience == "" {
		audience = middleware.DefaultAudience
	}
	keyFunc := middleware.JWKSKeyFunc(issuer + "/keys")

	// Shared authz: the WebSocket handlers run on a raw mux that bypasses the
	// Chi auth chain, so they authenticate and resolve project access with the
	// same resolver the REST middleware uses.
	roleStore := middleware.NewRoleStore(clientset)
	projectAccess := middleware.NewProjectAccessResolver(clientset, roleStore, &middleware.CRProjectMembers{Client: crClient}, crClient)
	// Handlers that resolve a namespace from the request body or by looking up
	// a resource (jobs, storage, bind, link, backups, resource usage) enforce
	// membership through this shared resolver.
	handlers.SetProjectResolver(projectAccess)

	// WebSocket handlers on a raw mux — no middleware wrapping the
	// response writer, which would break WebSocket hijacking
	logStreamer := &ws.LogStreamer{Client: clientset, Issuer: issuer, Audience: audience, KeyFunc: keyFunc, Resolver: projectAccess}

	terminal := &ws.Terminal{
		Client:   clientset,
		Config:   restConfig,
		Issuer:   issuer,
		Audience: audience,
		KeyFunc:  keyFunc,
		Resolver: projectAccess,
	}

	wsMux := http.NewServeMux()
	wsMux.HandleFunc("/api/v1/projects/", logStreamer.HandleRaw)
	wsMux.HandleFunc("/api/v1/terminal/", terminal.Handle)

	// Main Chi router with middleware
	r := chi.NewRouter()

	r.Use(middleware.Logging)
	r.Use(cors.Handler(corsOptions()))

	// Health endpoints — unauthenticated. /health is basic liveness;
	// /health/controllers reports the CRD controller manager's state for
	// observability and is not wired to the pod readiness probe.
	r.Get("/health", handlers.Health)
	r.Get("/health/controllers", handlers.ControllerHealthHandler)

	// Unauthenticated storage endpoints
	handlers.InitShareSecret(clientset)
	storageHandler := &handlers.Storage{Client: clientset}
	r.Get("/api/v1/storage/{service}/shared", storageHandler.SharedDownload)
	r.Get("/api/v1/storage/{service}/public/{bucket}/*", storageHandler.PublicDownload)

	auth := &middleware.Auth{
		Issuer:   issuer,
		Audience: audience,
		KeyFunc:  middleware.JWKSKeyFunc(issuer + "/keys"),
	}

	// OAuth callback handler — unauthenticated (exchanges code for token).
	// Also publishes /auth/check, the Traefik forwardAuth target every
	// service UI ingress points at. Domain-scoped cookie issuance happens
	// on the callback so authentication "follows" the user across the
	// per-service UI subdomains under the same cluster.
	authHandler := handlers.NewAuthHandler()
	authHandler.Auth = auth
	// UI_DOMAIN is the cluster base domain: which hosts an SSO code or
	// post-login redirect may target, beyond the console host. The installer
	// leaves it empty on shared *.kipper.run clusters (see UIDomainFor),
	// where a non-empty value would make sibling tenant hosts valid redirect
	// targets, so per-host service-UI SSO is off there for now.
	authHandler.UIDomain = os.Getenv("UI_DOMAIN")
	if consoleDomain := os.Getenv("CONSOLE_DOMAIN"); consoleDomain != "" {
		authHandler.ConsoleURL = "https://" + consoleDomain
	}
	// Share links: the gate reads the keyring, grants, and live Service
	// UIDs through short-lived caches so this public endpoint doesn't
	// hit the API server per request. Revoking a grant or rotating a key
	// takes effect within the cache ttls (the documented ≤30s SLA).
	shareKeys := share.NewKeyCache(clientset, 30*time.Second)
	authHandler.ShareKeyring = shareKeys.Get
	authHandler.ShareGrants = share.NewGrantStore(clientset)
	authHandler.ServiceUID = handlers.NewServiceUIDResolver(crClient, 15*time.Second).Get
	// UI sessions: the gate mints and validates per-host session cookies
	// through the same 30s cache pattern. The keyring cache self-heals a
	// missing signing Secret; the record store is the authoritative
	// liveness switch; RoleOf gates who may mint a code and backs the
	// session role check.
	uiKeys := uisession.NewKeyCache(clientset, 30*time.Second)
	authHandler.UISessionKeyring = uiKeys.Get
	authHandler.UISessions = uisession.NewRecordStore(clientset, uisession.SigningSecretNamespace)
	authHandler.UISessionReset = func(ctx context.Context) error { return uisession.ResetKeyring(ctx, clientset) }
	authHandler.RoleOf = roleStore.GetRole
	r.Get("/auth/login", authHandler.LoginURL)
	r.Post("/auth/callback", authHandler.Callback)
	r.Post("/auth/refresh", authHandler.Refresh)
	r.Get("/auth/check", authHandler.Check)
	r.Post("/auth/ui-code", authHandler.UISessionCode)
	r.Post("/auth/logout", authHandler.Logout)

	// Handlers
	adjustmentsHandler := &handlers.Adjustments{CRClient: crClient}
	cluster := &handlers.Cluster{Client: clientset}
	projects := &handlers.Projects{Client: clientset, CRClient: crClient, Domain: os.Getenv("CLUSTER_DOMAIN")}
	quotaHandler := &handlers.Quota{Client: clientset, CRClient: crClient}
	apps := &handlers.Apps{Client: clientset, CRClient: crClient, Domain: os.Getenv("CLUSTER_DOMAIN")}
	env := &handlers.Env{Client: clientset, CRClient: crClient}
	secrets := &handlers.Secrets{Client: clientset}
	routes := &handlers.Routes{Client: clientset, CRClient: crClient, Domain: os.Getenv("CLUSTER_DOMAIN")}
	svcHandler := &handlers.Services{Client: clientset, CRClient: crClient, RESTConfig: restConfig, Adjustments: adjustmentsHandler, Domain: os.Getenv("CLUSTER_DOMAIN")}
	jobHandler := &handlers.Jobs{Client: clientset, CRClient: crClient}
	fnHandler := &handlers.Functions{Client: clientset, Dynamic: dynClient, CRClient: crClient}
	apiGatewayHandler := &handlers.APIGateway{CRClient: crClient}
	logsHandler := &handlers.Logs{}
	autoscaleHandler := &handlers.Autoscale{Client: clientset, CRClient: crClient}
	recommendationHandler := &handlers.Recommendations{CRClient: crClient}
	resourcesHandler := &handlers.Resources{Client: clientset, CRClient: crClient, Adjustments: adjustmentsHandler}
	volumeHandler := &handlers.Volumes{Client: clientset, CRClient: crClient}
	inlineFnHandler := &handlers.InlineFunctions{Client: clientset, CRClient: crClient, Services: svcHandler, Domain: os.Getenv("CLUSTER_DOMAIN")}
	fnConfig := &handlers.FunctionConfig{Client: clientset, CRClient: crClient}
	dbHandler := &handlers.Database{Client: clientset, CRClient: crClient}
	routeGroupHandler := &handlers.RouteGroups{Client: clientset, CRClient: crClient, Domain: os.Getenv("CLUSTER_DOMAIN")}
	settingsHandler := &handlers.Settings{Client: clientset, CRClient: crClient}
	webhookHandler := &handlers.Webhooks{Client: clientset, CRClient: crClient}
	backupHandler := &handlers.Backups{Client: clientset, Dynamic: dynClient}
	modeHandler := &handlers.Mode{Client: clientset}
	aiSettingsHandler := &handlers.AISettings{Client: clientset}
	aiBundleStatusHandler := &handlers.AIBundleStatus{Client: clientset}
	aiChatHandler := &handlers.AIChat{Settings: aiSettingsHandler}
	aiLogsHandler := &handlers.AILogs{Settings: aiSettingsHandler}
	aiDiagnoseHandler := &handlers.AIDiagnose{Client: clientset, Settings: aiSettingsHandler}
	aiResourcesHandler := &handlers.AIResources{Client: clientset, Settings: aiSettingsHandler}
	alertsHandler := &handlers.Alerts{Client: clientset}
	registryHandler := &handlers.Registry{Client: clientset}
	gitCredentialsHandler := &handlers.GitCredentials{Client: clientset, CRClient: crClient}

	// Security event notifier. The host log and env-pinned channel need no
	// wiring; the console hooks route through the admin-editable alert bell,
	// Slack, and SMTP paths.
	emailService := &handlers.EmailService{Client: clientset}
	securityNotifier := &security.Notifier{Console: security.ConsoleHooks{
		Alert: func(ctx context.Context, kind, reason string) {
			handlers.AddAlert(ctx, clientset, handlers.Alert{
				Time:     time.Now().UTC().Format(time.RFC3339),
				Action:   "security",
				Severity: "critical",
				Reason:   reason,
			})
		},
		Email:           emailService.Send,
		EmailConfigured: emailService.Configured,
		SlackConfigured: func(ctx context.Context) bool {
			return handlers.SlackConfigured(ctx, clientset)
		},
		Admins: func() []string {
			var admins []string
			for email, role := range roleStore.ListUsers() {
				if role == middleware.RoleAdmin {
					admins = append(admins, email)
				}
			}
			return admins
		},
	}}

	slackHandler := &handlers.Slack{Client: clientset, Security: securityNotifier}
	smtpHandler := &handlers.SMTP{Client: clientset, Security: securityNotifier}
	podsHandler := &handlers.Pods{Client: clientset}
	filesHandler := &handlers.Files{Client: clientset, Config: restConfig}
	dashboardHandler := &handlers.Dashboard{Client: clientset}
	usageHistoryHandler := &handlers.UsageHistory{CRClient: crClient}
	platformHandler := &handlers.Platform{CRClient: crClient, Adjustments: adjustmentsHandler}
	users := &handlers.Users{Client: clientset, RoleStore: roleStore, Security: securityNotifier}
	// Removing a user revokes their service-UI sessions: the record store's
	// DeleteBySubject is the authoritative revocation, not role staleness.
	users.UISessions = authHandler.UISessions
	invites := &handlers.Invites{Client: clientset, CRClient: crClient, RoleStore: roleStore, Users: users}
	basicAuthHandler := &handlers.BasicAuth{Client: clientset, CRClient: crClient}
	membersHandler := &handlers.Members{CRClient: crClient, RoleStore: roleStore}

	// Migration handler. Sessions are mirrored into Secrets so a restart
	// mid-migration keeps the session recoverable on both sides.
	migrationHandler := &migration.Handler{
		Client:         clientset,
		CRClient:       crClient,
		RESTConfig:     restConfig,
		Sessions:       migration.NewPersistentSessionStore(clientset, "kipper-system"),
		Domain:         os.Getenv("CLUSTER_DOMAIN"),
		Security:       securityNotifier,
		DatamoverImage: datamoverImage(),
	}
	// Durable recovery for interrupted migrations, on both roles this
	// cluster can play: as a target, expired receivers (and the services
	// they paused) are reaped; as a source, transfers whose session died
	// with a previous process are cleaned up and their services restarted.
	go migrationHandler.RunTransferLeaseSweeper(ctx, 15*time.Minute)
	go migrationHandler.SweepAbandonedTransfers(ctx, 15*time.Minute)

	// Step-up 2FA for destructive operations (migration start and cutover).
	twofaIssuer := os.Getenv("CLUSTER_DOMAIN")
	if twofaIssuer == "" {
		twofaIssuer = "Kipper"
	}
	twofaHandler := twofa.NewHandler(&twofa.Store{Client: clientset}, twofaIssuer, securityNotifier)
	twofa.WarnIfWeakened()
	migrationHandler.StepUp = twofaHandler.VerifyStepUp
	migrationHandler.StepUpStatus = twofaHandler.StatusFor

	// Register migration WebSocket on the raw mux. WebSocket upgrades bypass
	// the Chi auth chain, so authenticate the Dex JWT from the subprotocol and
	// require admin here — migration progress exposes project and cluster state.
	requireAdminWS := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			email, ok := ws.AuthenticatedEmail(req, issuer, audience, keyFunc)
			if !ok {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			if roleStore.GetRole(email) != middleware.RoleAdmin {
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			h(w, req)
		}
	}
	wsMux.HandleFunc("/api/v1/migration/", requireAdminWS(migrationHandler.ProgressHandler))

	// Unauthenticated endpoints
	r.Post("/api/v1/webhook/{namespace}/{app}", webhookHandler.Receive)
	r.Get("/api/v1/invites/{token}", invites.Validate)
	r.Post("/api/v1/invites/{token}/accept", invites.Accept)

	// Migration target endpoints — authenticated by migration token, not Dex JWT.
	// These are called by the source cluster's console-api during migration.
	r.Route("/api/v1/migrate-target", func(r chi.Router) {
		// accept validates the migration token in its body; projects validates
		// the token header itself. Both run before a session exists.
		r.Post("/accept", migrationHandler.AcceptHandler)
		r.Get("/projects", migrationHandler.TargetProjectsHandler)
		r.Get("/capacity", migrationHandler.TargetCapacityHandler)
		// Per-session receive endpoints perform cluster writes, so each must
		// prove it holds the accepted session's migration secret.
		r.Group(func(r chi.Router) {
			r.Use(migrationHandler.RequireMigrationSecret)
			r.Post("/{session}/resource", migrationHandler.ReceiveResourceHandler)
			r.Post("/{session}/secret", migrationHandler.ReceiveSecretHandler)
			r.Get("/{session}/status", migrationHandler.StatusHandler)
			r.Get("/{session}/apps", migrationHandler.TargetAppsHandler)
			r.Post("/{session}/db-import", migrationHandler.ReceiveDBImportHandler)
			r.Post("/{session}/db-prune", migrationHandler.ReceiveDBPruneHandler)
			r.Post("/{session}/transfer", migrationHandler.CreateTransferHandler)
			r.Delete("/{session}/transfer/{transfer}", migrationHandler.DeleteTransferHandler)
			r.Post("/{session}/transfer/{transfer}/ensure", migrationHandler.EnsureReceiverHandler)
			r.Post("/{session}/abort", migrationHandler.AbortHandler)
			r.Post("/{session}/commit", migrationHandler.CommitHandler)
		})
		// Chunk ingest rides outside the session-secret group: the export
		// mover holds only its HKDF-derived per-transfer token, which the
		// import mover verifies, so the master secret never reaches a
		// workload namespace. The proxy still scope-checks the namespace.
		r.HandleFunc("/{session}/transfer/{transfer}/*", migrationHandler.TransferProxyHandler)
	})

	// Authenticated API routes
	r.Route("/api/v1", func(r chi.Router) {
		r.Use(auth.Handler)
		r.Use(middleware.RoleMiddleware(roleStore))

		// Deployer+ middleware — used inline for mutation endpoints
		requireDeployer := middleware.RequireRole(middleware.RoleAdmin, middleware.RoleDeployer)
		deployer := func(h http.HandlerFunc) http.HandlerFunc {
			return requireDeployer(h).ServeHTTP
		}

		// Admin-only middleware for cluster-wide platform mutations (resizing
		// system components, switching profiles). Deployers can ship apps;
		// only admins reshape the platform itself.
		requireAdmin := middleware.RequireRole(middleware.RoleAdmin)
		admin := func(h http.HandlerFunc) http.HandlerFunc {
			return requireAdmin(h).ServeHTTP
		}

		// Project-scoped helpers for namespace-carrying routes outside the
		// /projects/{name} subtree (services, jobs). The namespace comes from
		// ?namespace=; membership replaces the cluster-wide role check so a
		// project member reaches their own project and a non-member does not.
		qscope := middleware.ProjectScopeQuery(projectAccess)
		nsRead := func(h http.HandlerFunc) http.HandlerFunc {
			return qscope(h).ServeHTTP
		}
		nsDeployer := func(h http.HandlerFunc) http.HandlerFunc {
			return qscope(middleware.RequireProjectRole(middleware.ProjectRoleDeployer)(h)).ServeHTTP
		}

		// Raw node inventory carries node names, roles, kubelet versions, and
		// internal/external IPs — platform topology a tenant has no need for and
		// can use to target the cluster. Admin only. Tenants get the IP/version-
		// free health summary from /dashboard instead.
		r.Get("/cluster/status", admin(cluster.GetStatus))
		r.Get("/nodes", admin(cluster.GetNodes))

		// Read-only endpoints — accessible to all authenticated users (viewer+)
		r.Get("/dashboard", dashboardHandler.Status)
		r.Get("/dashboard/usage-history", usageHistoryHandler.Get)

		r.Get("/alerts", alertsHandler.List)
		r.Get("/alerts/unread-count", alertsHandler.UnreadCount)
		r.Post("/alerts/dismiss", alertsHandler.Dismiss)

		r.Get("/platform", platformHandler.Summary)
		r.Get("/platform/components", platformHandler.Components)
		r.Patch("/platform/components/{name}", admin(platformHandler.UpdateComponent))

		// Prometheus URL is overridable via env so nano clusters or
		// future stack repacks can point us elsewhere; empty disables
		// the Prometheus enrichment path entirely.
		promURL := os.Getenv("PROMETHEUS_URL")
		if promURL == "" {
			promURL = "http://prometheus-operated.monitoring.svc.cluster.local:9090"
		}
		usageHistoryHandler.PrometheusBaseURL = promURL
		resourceUsageHandler := &handlers.ResourceUsage{Client: clientset, PrometheusBaseURL: promURL}
		requestUsageHandler := &handlers.RequestUsage{Client: clientset, CRClient: crClient, PrometheusBaseURL: promURL}
		r.Get("/resources/usage", resourceUsageHandler.Get)
		r.Get("/resources/usage/summary", resourceUsageHandler.Summary)
		r.Get("/resources/adjustments", adjustmentsHandler.List)

		r.Get("/routes", routes.List)

		// Backups are cluster-wide Velero operations that can span namespaces
		// and, on restore, reshape the whole cluster. They stay admin-only
		// rather than being scoped to a single project.
		r.Get("/backups", admin(backupHandler.List))
		r.Post("/backups", admin(backupHandler.Create))
		r.Delete("/backups/{backup}", admin(backupHandler.DeleteBackup))
		r.Get("/backups/schedules", admin(backupHandler.Schedules))
		r.Put("/backups/schedules/{schedule}", admin(backupHandler.ToggleSchedule))
		r.Post("/backups/{backup}/restore", admin(backupHandler.Restore))

		// Jobs identify their cronjob by name across namespaces, so each handler
		// resolves the owning project and enforces membership itself. List
		// filters to the caller's projects.
		r.Get("/jobs", jobHandler.List)
		r.Post("/jobs", jobHandler.Create)
		r.Post("/jobs/{name}/trigger", jobHandler.Trigger)
		r.Get("/jobs/{name}/history", jobHandler.History)
		r.Get("/jobs/{name}/resources", jobHandler.GetResources)
		r.Put("/jobs/{name}/resources", jobHandler.UpdateResources)

		// bind/link carry their namespace in the request body, so each handler
		// enforces deploy access on that namespace itself.
		r.Post("/bind", svcHandler.Bind)
		r.Post("/unbind", svcHandler.Unbind)
		r.Post("/link", apps.Link)
		r.Post("/unlink", apps.Unlink)

		// Service list filters to the caller's projects in the handler; create
		// enforces deploy access on the target namespace from the request body.
		// The service catalogue carries no namespace, so it stays open to any
		// signed-in user. Every /services/{name} route below identifies its
		// namespace through ?namespace= and is scoped by nsRead/nsDeployer.
		r.Get("/services", svcHandler.List)
		r.Post("/services", svcHandler.Create)
		r.Get("/service-types", svcHandler.Types)
		r.Get("/services/{name}", nsRead(svcHandler.Info))
		r.Delete("/services/{name}", nsDeployer(svcHandler.Delete))
		r.Get("/services/{name}/logs", nsRead((&handlers.ServiceLogs{Client: clientset}).Query))
		r.Get("/services/{name}/resources", nsRead(svcHandler.GetResources))
		r.Put("/services/{name}/resources", nsDeployer(svcHandler.UpdateResources))
		r.Get("/services/{name}/rollout", nsRead(svcHandler.RolloutStatus))
		r.Post("/services/{name}/migrate-data", nsDeployer(svcHandler.MigrateData))
		r.Get("/services/{name}/migrate-data/status", nsRead(svcHandler.MigrateDataStatus))

		// Share links hand a service UI to someone outside the cluster's
		// user base — minting, listing, and revoking are admin-only.
		sharesHandler := &handlers.Shares{
			Client:   clientset,
			CRClient: crClient,
			Grants:   share.NewGrantStore(clientset),
			Domain:   os.Getenv("CLUSTER_DOMAIN"),
		}
		r.Post("/services/{name}/shares", admin(sharesHandler.Create))
		r.Get("/services/{name}/shares", admin(sharesHandler.List))
		r.Delete("/services/{name}/shares/{id}", admin(sharesHandler.Revoke))
		r.Delete("/shares", admin(sharesHandler.RevokeAll))
		r.Post("/shares/rotate-key", admin(sharesHandler.RotateKey))

		// Bulk UI-session revocation: drops every session record and rotates
		// the UI-session signing key, killing every cookie and outstanding
		// code within the caches' TTLs. Admin-only; accepts either audience so
		// `kip auth sessions revoke-all` can drive it.
		r.Post("/sessions/revoke-all", admin(authHandler.RevokeAllUISessions))
		r.Post("/services/{name}/diagnose", nsDeployer((&handlers.AIDiagnoseService{Client: clientset, Settings: aiSettingsHandler}).Diagnose))
		r.Get("/services/{name}/db/databases", nsRead(dbHandler.ListDatabases))
		r.Get("/services/{name}/rabbitmq/vhosts", nsRead(svcHandler.ListRabbitMQVhosts))
		r.Get("/services/{name}/db/schema", nsRead(dbHandler.Schema))
		r.Post("/services/{name}/db/query", nsDeployer(dbHandler.Query))
		r.Get("/services/{name}/db/tables/{schema}/{table}/rows", nsRead(dbHandler.ListRows))
		r.Post("/services/{name}/db/tables/{schema}/{table}/rows", nsDeployer(dbHandler.InsertRow))
		r.Patch("/services/{name}/db/tables/{schema}/{table}/rows", nsDeployer(dbHandler.UpdateRow))
		r.Delete("/services/{name}/db/tables/{schema}/{table}/rows", nsDeployer(dbHandler.DeleteRows))
		r.Get("/services/{name}/db/tables/{schema}/{table}/structure", nsRead(dbHandler.GetTableStructure))
		r.Post("/services/{name}/db/tables", nsDeployer(dbHandler.CreateTable))
		r.Patch("/services/{name}/db/tables/{schema}/{table}", nsDeployer(dbHandler.AlterTable))
		r.Post("/services/{name}/db/indexes", nsDeployer(dbHandler.CreateIndex))
		r.Delete("/services/{name}/db/indexes/{schema}/{indexName}", nsDeployer(dbHandler.DropIndex))
		r.Post("/services/{name}/db/ddl/preview", nsDeployer(dbHandler.PreviewDDL))
		r.Get("/services/{name}/db/snippets", nsRead(dbHandler.ListSnippets))
		r.Post("/services/{name}/db/snippets", nsDeployer(dbHandler.SaveSnippet))
		r.Delete("/services/{name}/db/snippets/{snippetName}", nsDeployer(dbHandler.DeleteSnippet))
		r.Get("/services/{name}/db/history", nsRead(dbHandler.ListHistory))

		r.Get("/projects", projects.List)
		r.Post("/projects", admin(projects.Create))

		r.Route("/projects/{name}", func(r chi.Router) {
			// Every route below is scoped to the caller's membership of this
			// project. Reads need membership (viewer+), and the shadowed
			// deployer/owner helpers gate mutations. This replaces the
			// cluster-wide role checks for project-scoped routes so a user only
			// reaches their own projects.
			//
			// The {name} segment does not mean the same thing throughout, so the
			// subtree is split into two groups with the matching gate. Routes
			// acting on the Project itself take a project name; routes acting on
			// workloads take one of its environment namespaces. The two can
			// collide — project "shop" with an environment "prod" and project
			// "shop-prod" both answer to "shop-prod" — and resolving one as the
			// other hands whoever owns the namespace authority over the Project,
			// or the reverse.
			deployer := func(h http.HandlerFunc) http.HandlerFunc {
				return middleware.RequireProjectRole(middleware.ProjectRoleDeployer)(h).ServeHTTP
			}
			owner := func(h http.HandlerFunc) http.HandlerFunc {
				return middleware.RequireProjectRole(middleware.ProjectRoleOwner)(h).ServeHTTP
			}

			// {name} is the project's own name here: every handler in this group
			// reads or writes the Project of that name, or derives its
			// namespaces from it.
			r.Group(func(r chi.Router) {
				r.Use(middleware.ProjectScope(projectAccess))

				r.Put("/", owner(projects.Update))
				r.Delete("/", owner(projects.Delete))
				r.Post("/environments", owner(projects.AddEnvironment))
				r.Get("/copy-preview", projects.CopyPreview)
				r.Post("/promote", deployer(projects.Promote))
				r.Get("/requests", requestUsageHandler.Get)
				r.Get("/quota", quotaHandler.Get)
				// Raising a quota grants cluster capacity, so mutation stays
				// with cluster admins rather than project owners.
				r.Put("/quota", admin(quotaHandler.Set))

				// Consent for another project's apps to reach this one's. The
				// owner wrapper is the point: this is the target project's
				// decision, and a project owner has no access to the Project
				// resource directly.
				r.Get("/link-consent", projects.LinkConsent)
				r.Put("/link-consent", owner(projects.SetLinkConsent))

				r.Get("/members", membersHandler.List)
				r.Put("/members", owner(membersHandler.Set))
				r.Delete("/members/{email}", owner(membersHandler.Remove))
			})

			// {name} is an environment namespace from here down. Every handler
			// below acts inside it, so the gate has to be about the project that
			// owns that namespace rather than the project sharing its name.
			r.Group(func(r chi.Router) {
				r.Use(middleware.NamespaceScope(projectAccess))

				// API gateway control plane: plans throttle, keys authenticate.
				// Reads for every member; issuing and revoking is deployer work,
				// like managing the apps the keys protect.
				r.Get("/usage-plans", apiGatewayHandler.ListPlans)
				r.Put("/usage-plans", deployer(apiGatewayHandler.UpsertPlan))
				r.Delete("/usage-plans/{plan}", deployer(apiGatewayHandler.DeletePlan))
				r.Get("/api-keys", apiGatewayHandler.ListKeys)
				r.Post("/api-keys", deployer(apiGatewayHandler.CreateKey))
				r.Patch("/api-keys/{key}", deployer(apiGatewayHandler.UpdateKey))
				r.Delete("/api-keys/{key}", deployer(apiGatewayHandler.DeleteKey))
				r.Get("/api-keys/{key}/usage", apiGatewayHandler.KeyUsage)

				r.Get("/functions", fnHandler.List)
				r.Post("/functions", deployer(fnHandler.Create))
				r.Route("/functions/{fn}", func(r chi.Router) {
					r.Delete("/", deployer(fnHandler.Delete))
					r.Post("/test", deployer(fnHandler.TestRun))
					r.Post("/diagnose", aiDiagnoseHandler.DiagnoseFunction)
					r.Get("/resources", resourcesHandler.GetByParam("fn", handlers.ResourceKindFunction))
					r.Put("/resources", deployer(resourcesHandler.UpdateByParam("fn", handlers.ResourceKindFunction)))
					r.Get("/settings", fnHandler.GetSettings)
					r.Put("/settings", deployer(fnHandler.UpdateSettings))
					r.Get("/env", fnConfig.GetEnv)
					r.Put("/env", deployer(fnConfig.UpdateEnv))
					r.Get("/secrets", fnConfig.ListSecretKeys)
					r.Put("/secrets", deployer(fnConfig.SetSecrets))
					r.Get("/secrets/{key}", deployer(fnConfig.RevealSecret))
					r.Delete("/secrets/{key}", deployer(fnConfig.DeleteSecret))
					r.Get("/dependencies", fnConfig.GetDependencies)
					r.Put("/dependencies", deployer(fnConfig.UpdateDependencies))
					r.Get("/bindings", fnConfig.ListBindings)
				})

				r.Get("/volumes", volumeHandler.List)
				r.Post("/volumes", deployer(volumeHandler.Create))
				r.Delete("/volumes/{vol}", deployer(volumeHandler.Delete))
				r.Post("/volumes/mount", deployer(volumeHandler.Mount))
				r.Post("/volumes/unmount", deployer(volumeHandler.Unmount))

				r.Post("/inline-functions", deployer(inlineFnHandler.Create))
				r.Get("/inline-functions/{fn}/code", inlineFnHandler.GetCode)
				r.Put("/inline-functions/{fn}/code", deployer(inlineFnHandler.UpdateCode))

				r.Post("/route-groups", deployer(routeGroupHandler.Create))
				r.Put("/route-groups", deployer(routeGroupHandler.Update))
				r.Delete("/route-groups/{host}", deployer(routeGroupHandler.Delete))

				r.Get("/apps", apps.List)
				r.Post("/apps", deployer(apps.Create))

				r.Route("/apps/{app}", func(r chi.Router) {
					r.Delete("/", deployer(apps.Delete))
					r.Post("/restart", deployer(apps.Restart))
					r.Get("/pods", podsHandler.List)
					r.Get("/health", podsHandler.Health)
					r.Put("/image", deployer(apps.UpdateImage))
					r.Put("/scale", deployer(apps.Scale))
					r.Get("/history", webhookHandler.History)
					r.Post("/rollback", deployer(webhookHandler.Rollback))
					r.Get("/webhook", webhookHandler.GetConfig)
					r.Post("/webhook", deployer(webhookHandler.GenerateToken))
					r.Delete("/webhook", deployer(webhookHandler.DeleteWebhook))
					r.Post("/rebuild", deployer(webhookHandler.Rebuild))
					r.Get("/build/status", webhookHandler.BuildStatus)
					r.Get("/build/logs", webhookHandler.BuildLogs)
					r.Post("/build/cancel", deployer(webhookHandler.CancelBuild))
					r.Get("/logs", logsHandler.Query)
					r.Post("/diagnose", deployer(aiDiagnoseHandler.Diagnose))
					r.Post("/optimise", deployer(aiResourcesHandler.Optimise))
					r.Get("/resources", resourcesHandler.Get)
					r.Put("/resources", deployer(resourcesHandler.Update))
					r.Get("/autoscale", autoscaleHandler.Get)
					r.Put("/autoscale", deployer(autoscaleHandler.Set))
					r.Delete("/autoscale", deployer(autoscaleHandler.Delete))
					r.Get("/recommendation", recommendationHandler.Get)
					r.Post("/recommendation/dismiss", deployer(recommendationHandler.Dismiss))
					r.Post("/recommendation/apply", deployer(recommendationHandler.Apply))
					r.Get("/settings", settingsHandler.Get)
					r.Put("/settings", deployer(settingsHandler.Update))
					r.Get("/route", apps.GetRoute)
					r.Put("/route", deployer(apps.SetRoute))
					r.Delete("/route", deployer(apps.DeleteRoute))
					r.Get("/route/dns-status", apps.GetRouteDNSStatus)
					r.Get("/git", apps.GetGit)
					r.Put("/git", deployer(apps.SetGit))
					r.Delete("/git", deployer(apps.DeleteGit))
					r.Post("/git/reveal", deployer(apps.RevealGitToken))
					r.Get("/links", apps.Links)
					r.Get("/env", env.Get)
					r.Put("/env", deployer(env.Update))
					// Deployer, unlike the GET above it: that one returns the
					// templates as written and this one returns what they
					// resolve to.
					r.Get("/env/preview", deployer(env.Preview))
					r.Get("/env/status", env.RestartStatus)
					r.Get("/env/conflicts", env.DirectEnvConflicts)
					r.Delete("/env/conflicts", deployer(env.RemoveDirectEnvConflicts))
					r.Get("/env/injected", svcHandler.InjectedEnv)
					r.Get("/secrets", secrets.ListKeys)
					r.Put("/secrets", deployer(secrets.Set))
					r.Get("/secrets/{key}", deployer(secrets.Reveal))
					r.Delete("/secrets/{key}", deployer(secrets.Delete))
					r.Get("/files", filesHandler.List)
					r.Get("/files/content", filesHandler.Content)
					r.Put("/files/content", deployer(filesHandler.Save))
					r.Get("/files/download", filesHandler.Download)
					r.Post("/files/upload", deployer(filesHandler.Upload))
					r.Get("/basic-auth", basicAuthHandler.Get)
					r.Put("/basic-auth", deployer(basicAuthHandler.Set))
					r.Delete("/basic-auth", deployer(basicAuthHandler.Delete))
					r.Delete("/basic-auth/{username}", deployer(basicAuthHandler.DeleteUser))
				})
			})
		})

		// Storage routes carry the service in the path and its namespace in
		// ?namespace=. nsRead / nsDeployer authorize that namespace (project
		// membership for reads, deploy access for writes) before the handler
		// runs, so each handler resolves an already-authorized namespace by an
		// exact lookup — never a cluster-wide name search.
		r.Get("/storage/{service}/buckets", nsRead(storageHandler.ListBuckets))
		r.Post("/storage/{service}/buckets", nsDeployer(storageHandler.CreateBucket))
		r.Get("/storage/{service}/objects", nsRead(storageHandler.ListObjects))
		r.Post("/storage/{service}/upload", nsDeployer(storageHandler.Upload))
		r.Post("/storage/{service}/folder", nsDeployer(storageHandler.CreateFolder))
		r.Get("/storage/{service}/download", nsRead(storageHandler.Download))
		r.Delete("/storage/{service}/objects", nsDeployer(storageHandler.DeleteObject))
		r.Post("/storage/{service}/share", nsDeployer(storageHandler.Share))
		r.Get("/storage/{service}/public", nsRead(storageHandler.IsPublic))
		r.Put("/storage/{service}/public", nsDeployer(storageHandler.MakePublic))
		r.Delete("/storage/{service}/public", nsDeployer(storageHandler.MakePrivate))

		r.Post("/ai/chat", deployer(aiChatHandler.Chat))
		r.Post("/ai/analyse-logs", deployer(aiLogsHandler.AnalyseLogs))

		// Migration source endpoints (authenticated via Dex JWT). Migration
		// exports whole projects to another cluster and reshapes routes on
		// cutover, so the entire source-side flow is admin-only rather than
		// scoped to a single project.
		r.Post("/migration/token", admin(migrationHandler.GenerateTokenHandler))
		r.Get("/migration/notification-status", admin(migrationHandler.NotificationStatusHandler))
		r.Post("/migration/plan", admin(migrationHandler.PlanHandler))
		r.Post("/migration/start", admin(migrationHandler.StartHandler))
		r.Get("/migration/{session}", admin(migrationHandler.SessionHandler))
		r.Get("/migration/{session}/progress", admin(migrationHandler.ProgressHandler))
		r.Get("/migration/{session}/verify", admin(migrationHandler.VerificationHandler))
		r.Get("/migration/{session}/dns", admin(migrationHandler.DNSStatusHandler))
		r.Post("/migration/{session}/cutover", admin(migrationHandler.CutoverHandler))
		r.Post("/migration/{session}/cancel", admin(migrationHandler.CancelHandler))

		r.Get("/me", users.Me)

		// Step-up 2FA lifecycle. Enrollment is authorised by a host-issued
		// bootstrap code, so these stay open to any authenticated user while
		// the destructive operations they gate remain admin-only.
		r.Post("/auth/2fa/enroll", twofaHandler.EnrollHandler)
		r.Post("/auth/2fa/confirm", twofaHandler.ConfirmHandler)
		r.Get("/auth/2fa/status", twofaHandler.StatusHandler)
		r.Post("/auth/2fa/reset", twofaHandler.ResetHandler)

		// Admin-only routes
		r.Route("/users", func(r chi.Router) {
			r.Use(middleware.RequireRole(middleware.RoleAdmin))
			r.Get("/", users.List)
			r.Post("/", users.Create)
			r.Put("/{email}/role", users.UpdateRole)
			r.Post("/{email}/reset-password", users.ResetPassword)
			r.Delete("/{email}", users.Delete)
		})

		r.Get("/settings/mode", modeHandler.Get)
		r.Get("/settings/resource-log", modeHandler.GetResourceLog)

		r.Group(func(r chi.Router) {
			r.Use(middleware.RequireRole(middleware.RoleAdmin))
			r.Put("/settings/mode", modeHandler.Update)
			r.Get("/settings/ai", aiSettingsHandler.Get)
			r.Put("/settings/ai", aiSettingsHandler.Update)
			r.Get("/settings/ai/bundle-status", aiBundleStatusHandler.Get)
			r.Get("/settings/slack", slackHandler.Get)
			r.Put("/settings/slack", slackHandler.Update)
			r.Get("/settings/smtp", smtpHandler.Get)
			r.Put("/settings/smtp", smtpHandler.Update)
			r.Post("/settings/smtp/test", smtpHandler.Test)
			r.Get("/settings/registries", registryHandler.List)
			r.Get("/settings/registries/health", registryHandler.Health)
			r.Post("/settings/registries", registryHandler.Add)
			r.Post("/settings/registries/{name}/reveal", registryHandler.Reveal)
			r.Delete("/settings/registries/*", registryHandler.Remove)
			r.Get("/settings/git-credentials", gitCredentialsHandler.List)
			r.Get("/settings/git-credentials/health", gitCredentialsHandler.Health)
			r.Post("/settings/git-credentials", gitCredentialsHandler.Add)
			r.Post("/settings/git-credentials/{name}/reveal", gitCredentialsHandler.Reveal)
			r.Delete("/settings/git-credentials/*", gitCredentialsHandler.Remove)
			r.Get("/settings/auth", (&handlers.AuthConnectors{Client: clientset}).List)
			r.Put("/settings/auth", (&handlers.AuthConnectors{Client: clientset}).Update)
			r.Get("/invites", invites.List)
			r.Post("/invites", invites.Create)
			r.Delete("/invites/{token}", invites.Revoke)
		})
	})

	// Combine: WebSocket mux handles log streams, Chi handles everything else
	return consoleRouter{api: r, ws: wsMux}
}

// consoleRouter sends WebSocket upgrades to the log-stream mux and everything
// else to the API router.
//
// It names the two halves rather than closing over them so that a test can walk
// the API router's own route table. The authorization-class inventory asserts
// over every route the mux actually serves, which means it has to be able to
// ask the mux what those are.
type consoleRouter struct {
	api *chi.Mux
	ws  http.Handler
}

func (c consoleRouter) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Header.Get("Upgrade") == "websocket" {
		c.ws.ServeHTTP(w, req)
		return
	}
	c.api.ServeHTTP(w, req)
}

// shareGrantStoreFor builds the grant store the service reconciler uses
// to revoke a deleted service's share links. A client build failure is
// fatal: the reconciler fails closed on a nil store (it keeps the
// finalizer and errors), so running without one would wedge every
// service deletion instead. The manager was already built from this same
// cfg, so a failure here is a genuine misconfiguration worth stopping for.
func shareGrantStoreFor(cfg *rest.Config) *share.GrantStore {
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Fatalf("share grant store client: %v", err)
	}
	return share.NewGrantStore(clientset)
}

// servesKind reports whether the API server serves a kind in a group version.
//
// A discovery error counts as absent. The cost of that is one controller sitting
// out until the next restart; the cost of guessing the other way is the whole
// manager failing to start on a cluster that genuinely lacks the CRD.
func servesKind(dc discovery.ServerResourcesInterface, gv schema.GroupVersion, kind string) bool {
	if dc == nil {
		return false
	}
	list, err := dc.ServerResourcesForGroupVersion(gv.String())
	if err != nil {
		log.Printf("checking whether the cluster serves %s: %v", kind, err)
		return false
	}
	return slices.ContainsFunc(list.APIResources, func(r metav1.APIResource) bool {
		return r.Kind == kind
	})
}

// discoveryFor returns the cluster's discovery client, or nil when one cannot be
// built. A nil client reports every kind as absent, which is the same safe
// direction servesKind takes for a failed lookup.
func discoveryFor(cfg *rest.Config) discovery.ServerResourcesInterface {
	dc, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		log.Printf("building a discovery client: %v", err)
		return nil
	}
	return dc
}

func startControllerManager(cfg *rest.Config) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(kipperv1.AddToScheme(scheme))

	// Register networking and autoscaling types for reconciliation
	utilruntime.Must(networkingv1.AddToScheme(scheme))
	utilruntime.Must(autoscalingv2.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: "0", // disable — console-api has its own health endpoint
		Metrics:                metricsOptions(),
		Cache: cache.Options{
			// The OOM watcher is the only controller that watches Pods,
			// and it only cares about monitoring/{prometheus,loki}.
			// Without this scoping the manager would list/watch every Pod
			// in every namespace, adding tens of MB of cache and
			// API-server load on a busy cluster.
			ByObject: map[crclient.Object]cache.ByObject{
				&corev1.Pod{}: {Namespaces: map[string]cache.Config{"monitoring": {}}},
			},
		},
	})
	if err != nil {
		log.Printf("controller manager failed to start: %v (CRD reconcilers disabled)", err)
		return
	}

	domain := os.Getenv("CLUSTER_DOMAIN")
	sidecarImage := os.Getenv("SIDECAR_IMAGE")

	reconcilers := []struct {
		name  string
		setup func(ctrl.Manager) error
	}{
		{"App", (&controllers.AppReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), Domain: domain, SidecarImage: sidecarImage, Recorder: mgr.GetEventRecorderFor("app-controller")}).SetupWithManager}, //nolint:staticcheck // consumes record.EventRecorder; migration to GetEventRecorder/events.EventRecorder is a separate change
		{"Service", (&controllers.ServiceReconciler{
			Client:              mgr.GetClient(),
			Scheme:              mgr.GetScheme(),
			Domain:              os.Getenv("CLUSTER_DOMAIN"),
			ConsoleAuthCheckURL: serviceUIAuthCheckURL(),
			ShareGrants:         shareGrantStoreFor(cfg),
		}).SetupWithManager},
		{"Function", (&controllers.FunctionReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), Domain: domain, Recorder: mgr.GetEventRecorderFor("function-controller")}).SetupWithManager}, //nolint:staticcheck // consumes record.EventRecorder; migration to GetEventRecorder/events.EventRecorder is a separate change
		{"Job", (&controllers.JobReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme(), Recorder: mgr.GetEventRecorderFor("job-controller")}).SetupWithManager},                                //nolint:staticcheck // consumes record.EventRecorder; migration to GetEventRecorder/events.EventRecorder is a separate change
		{"WorkloadName", (&controllers.WorkloadNameReconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Scheme: mgr.GetScheme()}).SetupWithManager},
		{"Volume", (&controllers.VolumeReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager},
		{"Project", (&controllers.ProjectReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(), Recorder: mgr.GetEventRecorderFor("project-controller")}).SetupWithManager}, //nolint:staticcheck // ProjectReconciler consumes record.EventRecorder; migration to GetEventRecorder/events.EventRecorder is a separate change
		{"Build", (&controllers.BuildReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader()}).SetupWithManager},
		{"PlatformConfig", (&controllers.PlatformConfigReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager},
		{"PodOOM", (&controllers.PodOOMReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme()}).SetupWithManager},
		{"ClusterIdentity", (&controllers.ClusterIdentityReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), Metrics: controllers.NewAPIServerMetricsReader(kubernetes.NewForConfigOrDie(cfg))}).SetupWithManager},
		{"DataTransfer", (&controllers.DataTransferReconciler{Client: mgr.GetClient(), Scheme: mgr.GetScheme(), APIReader: mgr.GetAPIReader(), DatamoverImage: datamoverImage()}).SetupWithManager},
	}

	// A cluster that has not been upgraded yet runs this image without the CRDs
	// that came with it: the Deployment pulls :latest on every restart, while the
	// CRDs arrive only when `kip upgrade` applies them. Registering a watch for a
	// kind the API server does not serve leaves that informer unable to sync, and
	// a cache that never syncs makes mgr.Start give up and return, which stops
	// every other reconciler with it. The pod stays Running and serving HTTP the
	// whole time, so nothing deploys and nothing says why.
	//
	// Dropping the one controller keeps the others reconciling until the upgrade
	// lands. A cluster with no WorkloadName CRD also has no reservations to
	// reclaim, so the controller has nothing to do there in any case.
	reconcilers = slices.DeleteFunc(reconcilers, func(rec struct {
		name  string
		setup func(ctrl.Manager) error
	}) bool {
		if rec.name != "WorkloadName" || servesKind(discoveryFor(cfg), kipperv1.GroupVersion, "WorkloadName") {
			return false
		}
		log.Printf("the WorkloadName CRD is not installed; skipping its controller until `kip upgrade` applies it")
		handlers.SetControllerRegistered(rec.name, false)
		return true
	})

	// A single reconciler failing to register must not take down the rest.
	// A partial controller set still reconciles most CRs, which is far better
	// than a silent all-or-nothing abort where nothing deploys.
	setupFailures := 0
	for _, rec := range reconcilers {
		err := rec.setup(mgr)
		handlers.SetControllerRegistered(rec.name, err == nil)
		if err != nil {
			log.Printf("failed to set up %s controller: %v (continuing without it)", rec.name, err)
			setupFailures++
		}
	}
	if setupFailures == len(reconcilers) {
		log.Printf("every controller failed to set up; not starting controller manager")
		return
	}

	// Periodically scan for workloads carrying managed-by=kipper that have
	// no owning Kipper CR and surface them as warnings so the operator
	// can investigate drift between the cluster and the CRs.
	go func() {
		if !mgr.GetCache().WaitForCacheSync(context.Background()) {
			log.Printf("orphan warner: cache sync failed, skipping")
			return
		}
		handlers.SetControllerCacheSynced(true)
		controllers.RunOrphanWarner(
			context.Background(),
			mgr.GetClient(),
			mgr.GetEventRecorderFor("orphan-warner"), //nolint:staticcheck // RunOrphanWarner consumes record.EventRecorder; migration to GetEventRecorder/events.EventRecorder is a separate change
		)
	}()

	log.Printf("CRD controller manager starting")
	handlers.SetControllerManagerStarted(true)
	if err := mgr.Start(context.Background()); err != nil {
		log.Printf("controller manager exited: %v", err)
	}
	handlers.SetControllerManagerStarted(false)
}

// metricsPort is where controller-runtime serves its own metrics, including
// controller_runtime_reconcile_errors_total. It is a second port rather than a
// path on 8080 because controller-runtime's metrics server is its own listener,
// and because the ServiceMonitor selects it by name.
const metricsPort = 8081

// metricsOptions exposes the controller manager's metrics.
//
// They were disabled on the grounds that Prometheus scrapes the cluster
// directly, which covers what Kubernetes reports about the workloads but not
// what the reconcilers report about themselves. Nothing could count a reconcile
// that fails on every pass, so nothing could alert on one.
//
// The endpoint is unauthenticated, as Traefik's is. It carries controller names,
// counts and latencies — aggregate activity per controller, with no object or
// namespace names in it — rather than anything identifying a tenant.
// It is reachable from inside the cluster exactly as port 8080 already is;
// putting a NetworkPolicy in front of console-api is worth doing and is its own
// change, because it has to enumerate every legitimate client of 8080 first.
func metricsOptions() metricsserver.Options {
	return metricsserver.Options{BindAddress: fmt.Sprintf(":%d", metricsPort)}
}

func buildK8sClients() (*kubernetes.Clientset, dynamic.Interface, *rest.Config, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("loading in-cluster config: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating kubernetes client: %w", err)
	}

	dynClient, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("creating dynamic client: %w", err)
	}

	return clientset, dynClient, cfg, nil
}

// serviceUIAuthCheckURL returns the absolute URL Traefik
// forwardAuth middlewares should point at to gate service-UI
// traffic. The endpoint is registered at the chi router root
// (not under /api/v1) because the auth routes live above the
// authenticated API group — keep these aligned, a mismatch
// here silently 404s every UI request through Traefik.
//
// It points at the in-cluster console-api Service, not the public
// console host. Routing the auth sub-request back through the public
// ingress adds a second Traefik hop that overwrites X-Forwarded-Host
// with the console's own hostname, so the gate can't tell which
// service UI the request was for. Share links validate a token bound
// to that host, so preserving the original X-Forwarded-Host is
// required; the in-cluster address has no intermediate hop to clobber
// it. SERVICE_UI_AUTH_CHECK_URL overrides for non-standard installs.
func serviceUIAuthCheckURL() string {
	if override := os.Getenv("SERVICE_UI_AUTH_CHECK_URL"); override != "" {
		return override
	}
	if os.Getenv("CONSOLE_DOMAIN") == "" {
		return ""
	}
	return "http://console-api.kipper-system.svc.cluster.local:8080/auth/check"
}
