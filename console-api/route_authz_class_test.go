package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/getkipper/kipper/controller/pkg/capability"
)

// Every route the console serves declares how it is authorized. The table is
// the inventory step 2 of the RBAC plan calls for, and the tests below hold it
// to the router and to the recorded matrix.
//
// It exists because a route added without a gate does not look like anything.
// It answers 200, the reviewer sees a handler, and nothing in the suite objects.
// With this table an unclassified route fails the build, and a route whose
// declaration disagrees with the answers the matrix recorded fails too.
//
// What no test here can check is whether the capability a route names describes
// what the handler actually hands over. Several capabilities are held by exactly
// the same built-in roles, so naming the wrong one of those reproduces every
// column and passes. Four review rounds each found entries of that shape, in
// routes nobody had questioned: a file listing under "read apps and services", a
// diagnosis route that ships pod logs to an external provider, a token that is
// really a deploy. They were found by reading handlers, and the next one will be
// too.
//
// So this table is reviewed rather than proven, and the release that makes it
// the gate has to re-read it rather than trust that the build is green. It will
// have a better tool than a re-read: once a capability is what admits a caller,
// a probe holding exactly one capability separates the ten that share built-in
// membership, the way a column with a lower cluster role would separate the
// global ranks below. Both are fixtures that cannot exist while role ranks are
// still the gate.
type authzClass int

const (
	// classPublic is reachable without credentials, deliberately.
	classPublic authzClass = iota
	// classAuthenticated needs a valid token and nothing more at the router.
	//
	// Many of these act on the caller themselves or expose nothing
	// project-specific. The rest serve project-scoped content and are filtered
	// item by item inside the handler, and their gate is real and is not here.
	//
	// Find that family by its gate rather than by any list, including this one:
	// it is the callers of canAccessNamespace, whether directly or through
	// filterPodsByAccess, accessibleAlerts or filterSeriesByAccess. Today that
	// is the cluster-wide routes, services and jobs lists, resource adjustments,
	// the resource log, the usage summary, both alerts routes, the dashboard and
	// its usage history. An earlier version of this comment counted six, and the
	// count was the wrong thing to write down: the four it missed were found by
	// following the helper's callers, which is what the next reader should do.
	//
	// GET /api/v1/projects is in the family and outside that gate. It filters on
	// projectMemberRole directly and serves app names, images, replica counts,
	// readiness and route hosts, so converting only canAccessNamespace's callers
	// leaves it behind.
	//
	// The gate asks whether the caller is a member and nothing else, with no
	// capability attached, so the release that makes capabilities the gate has
	// to convert it along with this table: a role refused
	// GET /projects/{name}/apps would otherwise read the same summaries off the
	// projects list, and that project's hostnames, services and jobs off the
	// cluster-wide ones. The matrix cannot see any of this, because every column
	// answers 200 and the difference is in the body, which is also why
	// TestAuthenticatedRoutesDoNotDiscriminate passes.
	classAuthenticated
	// classGlobalRole is gated on a cluster-wide role, which is a different axis
	// from project membership and is not a capability. globalRank says which
	// role; admin unless stated.
	classGlobalRole
	// classProjectCapability is gated on the caller's standing in a project,
	// and names the capability that will carry it once the role rank is gone.
	classProjectCapability
	// classHandlerInternal is gated inside the handler rather than by
	// middleware, because the thing being authorized is named in the request
	// body or is a resource the router cannot scope. The probe cannot reach
	// those gates with a generic body, so the matrix records the same answer
	// for every member and proves nothing about them; the reason says which
	// gate is meant to run.
	classHandlerInternal
	// classForeignCredential authorizes something that is not a user: a
	// migration secret, a webhook token, a refresh token. A user identity can
	// never satisfy it, which is why the matrix records 401 for every column.
	classForeignCredential
)

func (c authzClass) String() string {
	switch c {
	case classPublic:
		return "public"
	case classAuthenticated:
		return "authenticated"
	case classGlobalRole:
		return "globalRole"
	case classProjectCapability:
		return "projectCapability"
	case classHandlerInternal:
		return "handlerInternal"
	case classForeignCredential:
		return "foreignCredential"
	}
	return "unknown"
}

// routeScope is which principal a route's {name} segment names, for the routes
// under /api/v1/projects/{name}.
//
// Two projects can put the same string there: a project called shop-prod, and
// project shop's prod environment, whose namespace is also shop-prod. A route
// that resolved the wrong one would hand one tenant's workloads to the other,
// so which resolver a route takes is a security property and not a detail.
type routeScope int

const (
	// scopeNone is every route outside /api/v1/projects/{name}, and the
	// admin-only routes under it, which resolve no project at all.
	scopeNone routeScope = iota
	// scopeProject means {name} is a Project: the route acts on the project
	// itself, its members, its quota, its environments.
	scopeProject
	// scopeNamespace means {name} is an environment namespace: the route acts
	// on what runs inside one, and answers to whoever owns that namespace.
	scopeNamespace
)

func (s routeScope) String() string {
	switch s {
	case scopeProject:
		return "project"
	case scopeNamespace:
		return "namespace"
	}
	return "none"
}

// globalRank is which cluster-wide role a classGlobalRole route needs. It is a
// different axis from project membership: a project owner holds no cluster
// role, and a cluster deployer holds one in every project.
type globalRank int

const (
	// globalAdmin is the default, and the only rank the matrix can prove: the
	// fixture's admin column holds it and no other column does.
	globalAdmin globalRank = iota
	// globalDeployer admits the cluster's deployers as well as its admins.
	//
	// The matrix cannot tell this apart from an ungated route, because every
	// member column in the fixture holds the cluster deployer role, so all of
	// them are admitted whether the wrapper is there or not. Declaring the rank
	// is what records the gate; proving it needs a column with a lower cluster
	// role, which is the fixture change release 2 should make.
	globalDeployer
)

type authzDeclaration struct {
	class authzClass
	// capability is set only for classProjectCapability, and must name a
	// catalogue entry.
	capability capability.Name
	// reason is set only for classForeignCredential, naming the credential.
	reason string
	// scope is which principal {name} names, for routes under
	// /api/v1/projects/{name}.
	scope routeScope
	// globalRank is set only for classGlobalRole, and defaults to admin.
	globalRank globalRank
}

// routeAuthz declares every route in the mux and every raw WebSocket handler.
var routeAuthz = map[string]authzDeclaration{
	"CONNECT /api/v1/migrate-target/{session}/transfer/{transfer}/*":  {class: classForeignCredential, reason: "the per-transfer token derived for this transfer, not a user identity"},
	"DELETE /api/v1/backups/{backup}":                                 {class: classGlobalRole},
	"DELETE /api/v1/invites/{token}":                                  {class: classGlobalRole},
	"DELETE /api/v1/migrate-target/{session}/transfer/{transfer}":     {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"DELETE /api/v1/migrate-target/{session}/transfer/{transfer}/*":   {class: classForeignCredential, reason: "the per-transfer token derived for this transfer, not a user identity"},
	"DELETE /api/v1/projects/{name}/":                                 {class: classProjectCapability, capability: "project.delete", scope: scopeProject},
	"DELETE /api/v1/projects/{name}/api-keys/{key}":                   {class: classProjectCapability, capability: "apikeys.manage", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/apps/{app}/":                      {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/apps/{app}/autoscale":             {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/apps/{app}/basic-auth":            {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/apps/{app}/basic-auth/{username}": {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/apps/{app}/env/conflicts":         {class: classProjectCapability, capability: "env.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/apps/{app}/git":                   {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/apps/{app}/route":                 {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/apps/{app}/secrets/{key}":         {class: classProjectCapability, capability: "env.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/apps/{app}/webhook":               {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/functions/{fn}/":                  {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/functions/{fn}/secrets/{key}":     {class: classProjectCapability, capability: "env.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/members/{email}":                  {class: classProjectCapability, capability: "members.manage", scope: scopeProject},
	"DELETE /api/v1/projects/{name}/route-groups/{host}":              {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/usage-plans/{plan}":               {class: classProjectCapability, capability: "apikeys.manage", scope: scopeNamespace},
	"DELETE /api/v1/projects/{name}/volumes/{vol}":                    {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"DELETE /api/v1/services/{name}":                                  {class: classProjectCapability, capability: "kipper.write"},
	"DELETE /api/v1/services/{name}/db/indexes/{schema}/{indexName}":  {class: classProjectCapability, capability: "database.write"},
	"DELETE /api/v1/services/{name}/db/snippets/{snippetName}":        {class: classProjectCapability, capability: "database.write"},
	"DELETE /api/v1/services/{name}/db/tables/{schema}/{table}/rows":  {class: classProjectCapability, capability: "database.write"},
	"DELETE /api/v1/services/{name}/shares/{id}":                      {class: classGlobalRole},
	"DELETE /api/v1/settings/git-credentials/*":                       {class: classGlobalRole},
	"DELETE /api/v1/settings/registries/*":                            {class: classGlobalRole},
	"DELETE /api/v1/shares":                                           {class: classGlobalRole},
	"DELETE /api/v1/storage/{service}/objects":                        {class: classProjectCapability, capability: "storage.write"},
	"DELETE /api/v1/storage/{service}/public":                         {class: classProjectCapability, capability: "storage.write"},
	"DELETE /api/v1/users/{email}":                                    {class: classGlobalRole},
	"GET /api/v1/alerts":                                              {class: classAuthenticated},
	"GET /api/v1/alerts/unread-count":                                 {class: classAuthenticated},
	"GET /api/v1/auth/2fa/status":                                     {class: classAuthenticated},
	"GET /api/v1/backups":                                             {class: classGlobalRole},
	"GET /api/v1/backups/schedules":                                   {class: classGlobalRole},
	"GET /api/v1/cluster/status":                                      {class: classGlobalRole},
	"GET /api/v1/dashboard":                                           {class: classAuthenticated},
	"GET /api/v1/dashboard/usage-history":                             {class: classAuthenticated},
	"GET /api/v1/invites":                                             {class: classGlobalRole},
	"GET /api/v1/invites/{token}":                                     {class: classPublic},
	"GET /api/v1/jobs":                                                {class: classAuthenticated},
	"GET /api/v1/jobs/{name}/history":                                 {class: classHandlerInternal, reason: "the job's namespace decides, and the fixture has no such job so the probe stops at 404"},
	"GET /api/v1/jobs/{name}/resources":                               {class: classHandlerInternal, reason: "the job's namespace decides, and the fixture has no such job so the probe stops at 404"},
	"GET /api/v1/me":                                                  {class: classAuthenticated},
	"GET /api/v1/migrate-target/capacity":                             {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"GET /api/v1/migrate-target/projects":                             {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"GET /api/v1/migrate-target/{session}/apps":                       {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"GET /api/v1/migrate-target/{session}/status":                     {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"GET /api/v1/migrate-target/{session}/transfer/{transfer}/*":      {class: classForeignCredential, reason: "the per-transfer token derived for this transfer, not a user identity"},
	"GET /api/v1/migration/notification-status":                       {class: classGlobalRole},
	"GET /api/v1/migration/{session}":                                 {class: classGlobalRole},
	"GET /api/v1/migration/{session}/dns":                             {class: classGlobalRole},
	"GET /api/v1/migration/{session}/progress":                        {class: classGlobalRole},
	"GET /api/v1/migration/{session}/verify":                          {class: classGlobalRole},
	"GET /api/v1/nodes":                                               {class: classGlobalRole},
	"GET /api/v1/platform":                                            {class: classAuthenticated},
	"GET /api/v1/platform/components":                                 {class: classAuthenticated},
	"GET /api/v1/projects":                                            {class: classAuthenticated},
	"GET /api/v1/projects/{name}/api-keys":                            {class: classProjectCapability, capability: "project.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/api-keys/{key}/usage":                {class: classProjectCapability, capability: "project.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps":                                {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/autoscale":                {class: classProjectCapability, capability: "workloads.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/basic-auth":               {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/build/logs":               {class: classProjectCapability, capability: "pods.logs.read", scope: scopeNamespace},
	// Wider than the build status it is named for: the handler also reports
	// whether the app's git credential and the cluster's registry credentials
	// authenticate, which is a validity oracle for credentials the caller never
	// sees. workloads.read is the closest capability and understates it; a
	// capability of its own would move who can reach it, which is a decision
	// for the release that replaces the role ranks.
	"GET /api/v1/projects/{name}/apps/{app}/build/status":              {class: classProjectCapability, capability: "workloads.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/env":                       {class: classProjectCapability, capability: "env.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/env/conflicts":             {class: classProjectCapability, capability: "env.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/env/injected":              {class: classProjectCapability, capability: "env.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/env/preview":               {class: classProjectCapability, capability: "env.reveal", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/env/status":                {class: classProjectCapability, capability: "env.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/files":                     {class: classProjectCapability, capability: "files.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/files/content":             {class: classProjectCapability, capability: "files.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/files/download":            {class: classProjectCapability, capability: "files.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/git":                       {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/health":                    {class: classProjectCapability, capability: "workloads.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/history":                   {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/links":                     {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/logs":                      {class: classProjectCapability, capability: "pods.logs.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/pods":                      {class: classProjectCapability, capability: "workloads.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/recommendation":            {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/resources":                 {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/route":                     {class: classProjectCapability, capability: "workloads.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/route/dns-status":          {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/secrets":                   {class: classProjectCapability, capability: "env.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/secrets/{key}":             {class: classProjectCapability, capability: "env.reveal", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/settings":                  {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/apps/{app}/webhook":                   {class: classProjectCapability, capability: "webhook.reveal", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/copy-preview":                         {class: classProjectCapability, capability: "kipper.read", scope: scopeProject},
	"GET /api/v1/projects/{name}/functions":                            {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/functions/{fn}/bindings":              {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/functions/{fn}/dependencies":          {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/functions/{fn}/env":                   {class: classProjectCapability, capability: "env.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/functions/{fn}/resources":             {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/functions/{fn}/secrets":               {class: classProjectCapability, capability: "env.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/functions/{fn}/secrets/{key}":         {class: classProjectCapability, capability: "env.reveal", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/functions/{fn}/settings":              {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/inline-functions/{fn}/code":           {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/link-consent":                         {class: classProjectCapability, capability: "project.read", scope: scopeProject},
	"GET /api/v1/projects/{name}/members":                              {class: classProjectCapability, capability: "members.read", scope: scopeProject},
	"GET /api/v1/projects/{name}/quota":                                {class: classProjectCapability, capability: "project.read", scope: scopeProject},
	"GET /api/v1/projects/{name}/requests":                             {class: classProjectCapability, capability: "project.read", scope: scopeProject},
	"GET /api/v1/projects/{name}/usage-plans":                          {class: classProjectCapability, capability: "project.read", scope: scopeNamespace},
	"GET /api/v1/projects/{name}/volumes":                              {class: classProjectCapability, capability: "kipper.read", scope: scopeNamespace},
	"GET /api/v1/resources/adjustments":                                {class: classAuthenticated},
	"GET /api/v1/resources/usage":                                      {class: classProjectCapability, capability: "workloads.read"},
	"GET /api/v1/resources/usage/summary":                              {class: classAuthenticated},
	"GET /api/v1/routes":                                               {class: classAuthenticated},
	"GET /api/v1/service-types":                                        {class: classAuthenticated},
	"GET /api/v1/services":                                             {class: classAuthenticated},
	"GET /api/v1/services/{name}":                                      {class: classProjectCapability, capability: "kipper.read"},
	"GET /api/v1/services/{name}/db/databases":                         {class: classProjectCapability, capability: "database.read"},
	"GET /api/v1/services/{name}/db/history":                           {class: classProjectCapability, capability: "database.read"},
	"GET /api/v1/services/{name}/db/schema":                            {class: classProjectCapability, capability: "database.read"},
	"GET /api/v1/services/{name}/db/snippets":                          {class: classProjectCapability, capability: "database.read"},
	"GET /api/v1/services/{name}/db/tables/{schema}/{table}/rows":      {class: classProjectCapability, capability: "database.read"},
	"GET /api/v1/services/{name}/db/tables/{schema}/{table}/structure": {class: classProjectCapability, capability: "database.read"},
	"GET /api/v1/services/{name}/logs":                                 {class: classProjectCapability, capability: "pods.logs.read"},
	"GET /api/v1/services/{name}/migrate-data/status":                  {class: classProjectCapability, capability: "pods.logs.read"},
	"GET /api/v1/services/{name}/rabbitmq/vhosts":                      {class: classProjectCapability, capability: "database.read"},
	"GET /api/v1/services/{name}/resources":                            {class: classProjectCapability, capability: "workloads.read"},
	"GET /api/v1/services/{name}/rollout":                              {class: classProjectCapability, capability: "workloads.read"},
	"GET /api/v1/services/{name}/shares":                               {class: classGlobalRole},
	"GET /api/v1/settings/ai":                                          {class: classGlobalRole},
	"GET /api/v1/settings/ai/bundle-status":                            {class: classGlobalRole},
	"GET /api/v1/settings/auth":                                        {class: classGlobalRole},
	"GET /api/v1/settings/git-credentials":                             {class: classGlobalRole},
	"GET /api/v1/settings/git-credentials/health":                      {class: classGlobalRole},
	"GET /api/v1/settings/mode":                                        {class: classAuthenticated},
	"GET /api/v1/settings/registries":                                  {class: classGlobalRole},
	"GET /api/v1/settings/registries/health":                           {class: classGlobalRole},
	"GET /api/v1/settings/resource-log":                                {class: classAuthenticated},
	"GET /api/v1/settings/slack":                                       {class: classGlobalRole},
	"GET /api/v1/settings/smtp":                                        {class: classGlobalRole},
	"GET /api/v1/storage/{service}/buckets":                            {class: classProjectCapability, capability: "storage.read"},
	"GET /api/v1/storage/{service}/download":                           {class: classProjectCapability, capability: "storage.read"},
	"GET /api/v1/storage/{service}/objects":                            {class: classProjectCapability, capability: "storage.read"},
	"GET /api/v1/storage/{service}/public":                             {class: classProjectCapability, capability: "storage.read"},
	"GET /api/v1/storage/{service}/public/{bucket}/*":                  {class: classPublic},
	"GET /api/v1/storage/{service}/shared":                             {class: classPublic},
	"GET /api/v1/users/":                                               {class: classGlobalRole},
	"GET /auth/check":                                                  {class: classForeignCredential, reason: "the forward-auth cookie the gateway presents"},
	"GET /auth/login":                                                  {class: classPublic},
	"GET /health":                                                      {class: classPublic},
	"GET /health/controllers":                                          {class: classPublic},
	"GET ws /api/v1/migration/":                                        {class: classGlobalRole},
	// The log streamer, serving the same pod logs as the REST log routes and
	// so declaring the same capability. Every built-in that holds kipper.read
	// also holds pods.logs.read, so the matrix agrees either way and cannot
	// catch a divergence here; a custom role that reads resources without
	// reading logs would have been refused the REST routes and streamed the
	// same logs from this one.
	"GET ws /api/v1/projects/":                                         {class: classProjectCapability, capability: "pods.logs.read"},
	"GET ws /api/v1/terminal/":                                         {class: classProjectCapability, capability: "terminal.open"},
	"HEAD /api/v1/migrate-target/{session}/transfer/{transfer}/*":      {class: classForeignCredential, reason: "the per-transfer token derived for this transfer, not a user identity"},
	"OPTIONS /api/v1/migrate-target/{session}/transfer/{transfer}/*":   {class: classForeignCredential, reason: "the per-transfer token derived for this transfer, not a user identity"},
	"PATCH /api/v1/migrate-target/{session}/transfer/{transfer}/*":     {class: classForeignCredential, reason: "the per-transfer token derived for this transfer, not a user identity"},
	"PATCH /api/v1/platform/components/{name}":                         {class: classGlobalRole},
	"PATCH /api/v1/projects/{name}/api-keys/{key}":                     {class: classProjectCapability, capability: "apikeys.manage", scope: scopeNamespace},
	"PATCH /api/v1/services/{name}/db/tables/{schema}/{table}":         {class: classProjectCapability, capability: "database.write"},
	"PATCH /api/v1/services/{name}/db/tables/{schema}/{table}/rows":    {class: classProjectCapability, capability: "database.write"},
	"POST /api/v1/ai/analyse-logs":                                     {class: classGlobalRole, globalRank: globalDeployer},
	"POST /api/v1/ai/chat":                                             {class: classGlobalRole, globalRank: globalDeployer},
	"POST /api/v1/alerts/dismiss":                                      {class: classAuthenticated},
	"POST /api/v1/auth/2fa/confirm":                                    {class: classAuthenticated},
	"POST /api/v1/auth/2fa/enroll":                                     {class: classAuthenticated},
	"POST /api/v1/auth/2fa/reset":                                      {class: classAuthenticated},
	"POST /api/v1/backups":                                             {class: classGlobalRole},
	"POST /api/v1/backups/{backup}/restore":                            {class: classGlobalRole},
	"POST /api/v1/bind":                                                {class: classHandlerInternal, reason: "the namespace comes from the request body"},
	"POST /api/v1/invites":                                             {class: classGlobalRole},
	"POST /api/v1/invites/{token}/accept":                              {class: classPublic},
	"POST /api/v1/jobs":                                                {class: classHandlerInternal, reason: "the namespace comes from the request body"},
	"POST /api/v1/jobs/{name}/trigger":                                 {class: classHandlerInternal, reason: "the job's namespace decides, and the fixture has no such job so the probe stops at 404"},
	"POST /api/v1/link":                                                {class: classHandlerInternal, reason: "the caller's namespace comes from the request body"},
	"POST /api/v1/migrate-target/accept":                               {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"POST /api/v1/migrate-target/{session}/abort":                      {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"POST /api/v1/migrate-target/{session}/commit":                     {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"POST /api/v1/migrate-target/{session}/db-import":                  {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"POST /api/v1/migrate-target/{session}/db-prune":                   {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"POST /api/v1/migrate-target/{session}/resource":                   {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"POST /api/v1/migrate-target/{session}/secret":                     {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"POST /api/v1/migrate-target/{session}/transfer":                   {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"POST /api/v1/migrate-target/{session}/transfer/{transfer}/*":      {class: classForeignCredential, reason: "the per-transfer token derived for this transfer, not a user identity"},
	"POST /api/v1/migrate-target/{session}/transfer/{transfer}/ensure": {class: classForeignCredential, reason: "the migration secret the source cluster presents, not a user identity"},
	"POST /api/v1/migration/plan":                                      {class: classGlobalRole},
	"POST /api/v1/migration/start":                                     {class: classGlobalRole},
	"POST /api/v1/migration/token":                                     {class: classGlobalRole},
	"POST /api/v1/migration/{session}/cancel":                          {class: classGlobalRole},
	"POST /api/v1/migration/{session}/cutover":                         {class: classGlobalRole},
	"POST /api/v1/projects":                                            {class: classGlobalRole},
	"POST /api/v1/projects/{name}/api-keys":                            {class: classProjectCapability, capability: "apikeys.manage", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/apps":                                {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/apps/{app}/build/cancel":             {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	// The three diagnose routes run one code path and carry two declarations,
	// because their gates differ: the function one is reachable by a viewer and
	// these two are not. kipper.write declares the log read it grants here.
	"POST /api/v1/projects/{name}/apps/{app}/diagnose":               {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/apps/{app}/files/upload":           {class: classProjectCapability, capability: "files.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/apps/{app}/git/reveal":             {class: classProjectCapability, capability: "env.reveal", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/apps/{app}/optimise":               {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/apps/{app}/rebuild":                {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/apps/{app}/recommendation/apply":   {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/apps/{app}/recommendation/dismiss": {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/apps/{app}/restart":                {class: classProjectCapability, capability: "workloads.restart", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/apps/{app}/rollback":               {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/apps/{app}/webhook":                {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/environments":                      {class: classProjectCapability, capability: "project.settings", scope: scopeProject},
	"POST /api/v1/projects/{name}/functions":                         {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/functions/{fn}/diagnose":           {class: classProjectCapability, capability: "pods.logs.read", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/functions/{fn}/test":               {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/inline-functions":                  {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/promote":                           {class: classProjectCapability, capability: "kipper.write", scope: scopeProject},
	"POST /api/v1/projects/{name}/route-groups":                      {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/volumes":                           {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/volumes/mount":                     {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/projects/{name}/volumes/unmount":                   {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"POST /api/v1/services":                                          {class: classHandlerInternal, reason: "the namespace comes from the request body"},
	"POST /api/v1/services/{name}/db/ddl/preview":                    {class: classProjectCapability, capability: "database.write"},
	"POST /api/v1/services/{name}/db/indexes":                        {class: classProjectCapability, capability: "database.write"},
	"POST /api/v1/services/{name}/db/query":                          {class: classProjectCapability, capability: "database.write"},
	"POST /api/v1/services/{name}/db/snippets":                       {class: classProjectCapability, capability: "database.write"},
	"POST /api/v1/services/{name}/db/tables":                         {class: classProjectCapability, capability: "database.write"},
	"POST /api/v1/services/{name}/db/tables/{schema}/{table}/rows":   {class: classProjectCapability, capability: "database.write"},
	"POST /api/v1/services/{name}/diagnose":                          {class: classProjectCapability, capability: "kipper.write"},
	"POST /api/v1/services/{name}/migrate-data":                      {class: classProjectCapability, capability: "kipper.write"},
	"POST /api/v1/services/{name}/shares":                            {class: classGlobalRole},
	"POST /api/v1/sessions/revoke-all":                               {class: classGlobalRole},
	"POST /api/v1/settings/git-credentials":                          {class: classGlobalRole},
	"POST /api/v1/settings/git-credentials/{name}/reveal":            {class: classGlobalRole},
	"POST /api/v1/settings/registries":                               {class: classGlobalRole},
	"POST /api/v1/settings/registries/{name}/reveal":                 {class: classGlobalRole},
	"POST /api/v1/settings/smtp/test":                                {class: classGlobalRole},
	"POST /api/v1/shares/rotate-key":                                 {class: classGlobalRole},
	"POST /api/v1/storage/{service}/buckets":                         {class: classProjectCapability, capability: "storage.write"},
	"POST /api/v1/storage/{service}/folder":                          {class: classProjectCapability, capability: "storage.write"},
	"POST /api/v1/storage/{service}/share":                           {class: classProjectCapability, capability: "storage.write"},
	"POST /api/v1/storage/{service}/upload":                          {class: classProjectCapability, capability: "storage.write"},
	"POST /api/v1/unbind":                                            {class: classHandlerInternal, reason: "the namespace comes from the request body"},
	"POST /api/v1/unlink":                                            {class: classHandlerInternal, reason: "the caller's namespace comes from the request body"},
	"POST /api/v1/users/":                                            {class: classGlobalRole},
	"POST /api/v1/users/{email}/reset-password":                      {class: classGlobalRole},
	"POST /api/v1/webhook/{namespace}/{app}":                         {class: classForeignCredential, reason: "the webhook token in the request, not a user identity"},
	"POST /auth/callback":                                            {class: classPublic},
	"POST /auth/logout":                                              {class: classPublic},
	"POST /auth/refresh":                                             {class: classForeignCredential, reason: "the refresh token in the request, not an access token"},
	"POST /auth/ui-code":                                             {class: classAuthenticated},
	"PUT /api/v1/backups/schedules/{schedule}":                       {class: classGlobalRole},
	"PUT /api/v1/jobs/{name}/resources":                              {class: classHandlerInternal, reason: "the job's namespace decides, and the fixture has no such job so the probe stops at 404"},
	"PUT /api/v1/migrate-target/{session}/transfer/{transfer}/*":     {class: classForeignCredential, reason: "the per-transfer token derived for this transfer, not a user identity"},
	"PUT /api/v1/projects/{name}/":                                   {class: classProjectCapability, capability: "project.settings", scope: scopeProject},
	"PUT /api/v1/projects/{name}/apps/{app}/autoscale":               {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/apps/{app}/basic-auth":              {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/apps/{app}/env":                     {class: classProjectCapability, capability: "env.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/apps/{app}/files/content":           {class: classProjectCapability, capability: "files.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/apps/{app}/git":                     {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/apps/{app}/image":                   {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/apps/{app}/resources":               {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/apps/{app}/route":                   {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/apps/{app}/scale":                   {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/apps/{app}/secrets":                 {class: classProjectCapability, capability: "env.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/apps/{app}/settings":                {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/functions/{fn}/dependencies":        {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/functions/{fn}/env":                 {class: classProjectCapability, capability: "env.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/functions/{fn}/resources":           {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/functions/{fn}/secrets":             {class: classProjectCapability, capability: "env.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/functions/{fn}/settings":            {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/inline-functions/{fn}/code":         {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/link-consent":                       {class: classProjectCapability, capability: "project.settings", scope: scopeProject},
	"PUT /api/v1/projects/{name}/members":                            {class: classProjectCapability, capability: "members.manage", scope: scopeProject},
	"PUT /api/v1/projects/{name}/quota":                              {class: classGlobalRole},
	"PUT /api/v1/projects/{name}/route-groups":                       {class: classProjectCapability, capability: "kipper.write", scope: scopeNamespace},
	"PUT /api/v1/projects/{name}/usage-plans":                        {class: classProjectCapability, capability: "apikeys.manage", scope: scopeNamespace},
	"PUT /api/v1/services/{name}/resources":                          {class: classProjectCapability, capability: "kipper.write"},
	"PUT /api/v1/settings/ai":                                        {class: classGlobalRole},
	"PUT /api/v1/settings/auth":                                      {class: classGlobalRole},
	"PUT /api/v1/settings/mode":                                      {class: classGlobalRole},
	"PUT /api/v1/settings/slack":                                     {class: classGlobalRole},
	"PUT /api/v1/settings/smtp":                                      {class: classGlobalRole},
	"PUT /api/v1/storage/{service}/public":                           {class: classProjectCapability, capability: "storage.write"},
	"PUT /api/v1/users/{email}/role":                                 {class: classGlobalRole},
	"TRACE /api/v1/migrate-target/{session}/transfer/{transfer}/*":   {class: classForeignCredential, reason: "the per-transfer token derived for this transfer, not a user identity"},
}

// TestEveryRouteDeclaresHowItIsAuthorized holds the table to the router in both
// directions.
//
// A route added without a declaration fails here rather than shipping with
// whatever gate it happened to inherit, and a declaration whose route is gone
// fails too, so the table cannot rot into a description of a router that no
// longer exists.
func TestEveryRouteDeclaresHowItIsAuthorized(t *testing.T) {
	router, _, _ := matrixRouter(t, "", "", "")

	served := map[string]bool{}
	for _, r := range matrixRoutes(t, router) {
		served[r.method+" "+r.pattern] = true
	}

	for route := range served {
		if _, declared := routeAuthz[route]; !declared {
			t.Errorf("%s is served and declares no authorization class; add it to routeAuthz", route)
		}
	}
	for route := range routeAuthz {
		if !served[route] {
			t.Errorf("%s is declared and no longer served; take it out of routeAuthz", route)
		}
	}
}

// TestEveryDeclarationIsWellFormed checks the fields each class requires.
func TestEveryDeclarationIsWellFormed(t *testing.T) {
	for route, d := range routeAuthz {
		switch d.class {
		case classProjectCapability:
			if d.capability == "" {
				t.Errorf("%s is gated on a project capability and names none", route)
				continue
			}
			if _, ok := capability.Lookup(d.capability); !ok {
				t.Errorf("%s names capability %q, which is not in the catalogue", route, d.capability)
			}
			if d.reason != "" {
				t.Errorf("%s is a project capability and carries a foreign-credential reason", route)
			}
		case classHandlerInternal:
			if d.reason == "" {
				t.Errorf("%s is gated inside its handler and does not say which gate", route)
			}
			if d.capability != "" {
				t.Errorf("%s names a capability but is not gated by middleware on one", route)
			}
		case classForeignCredential:
			if d.reason == "" {
				t.Errorf("%s authorizes something other than a user and does not say what", route)
			}
			if d.capability != "" {
				t.Errorf("%s names a capability but is not gated on one", route)
			}
		default:
			if d.capability != "" {
				t.Errorf("%s is %s and names a capability, which only a project route may do", route, d.class)
			}
			if d.reason != "" {
				t.Errorf("%s is %s and carries a reason, which only a handler-internal or foreign-credential route may do", route, d.class)
			}
		}
	}
}

// TestEachRouteBehavesLikeItsClass compares the declaration against what the
// recorded matrix says the route actually answers.
//
// This is what makes the table more than documentation. A route declared as a
// project capability that lets an outsider through, or one declared public that
// answers 401, is either mis-declared or has changed behaviour, and either is
// worth stopping for.
func TestEachRouteBehavesLikeItsClass(t *testing.T) {
	denied := func(status string) bool { return status == "401" || status == "403" }

	for route, cells := range matrixCells(t) {
		d, ok := routeAuthz[route]
		if !ok {
			continue // the totality test above reports this
		}
		switch d.class {
		case classPublic:
			if cells["anon"] == "401" {
				t.Errorf("%s is declared public and refuses an anonymous caller", route)
			}
		case classForeignCredential:
			for _, column := range []string{"anon", "outsider", "viewer", "deployer", "owner", "admin"} {
				if cells[column] != "401" {
					t.Errorf("%s is declared to need a credential that is not a user identity, but admits %s (%s)",
						route, column, cells[column])
				}
			}
		case classGlobalRole:
			if cells["anon"] != "401" {
				t.Errorf("%s is gated on a cluster role and admits an anonymous caller (%s)", route, cells["anon"])
			}
			if denied(cells["admin"]) {
				t.Errorf("%s is gated on a cluster role and refuses an admin (%s)", route, cells["admin"])
			}
			// Only the admin rank is provable here. Every member column in the
			// fixture holds the cluster deployer role, so a deployer-ranked
			// gate admits all of them and looks exactly like no gate at all.
			if d.globalRank == globalAdmin && !denied(cells["owner"]) {
				t.Errorf("%s is declared admin-only and admits a project owner (%s)", route, cells["owner"])
			}
		case classProjectCapability:
			if cells["anon"] != "401" {
				t.Errorf("%s is gated on project standing and admits an anonymous caller (%s)", route, cells["anon"])
			}
			if !denied(cells["outsider"]) {
				t.Errorf("%s is gated on project standing and admits a non-member (%s)", route, cells["outsider"])
			}
		case classAuthenticated, classHandlerInternal:
			// The member columns are deliberately not asserted for a
			// handler-internal route: the probe cannot reach the gate, which
			// is the whole reason the class exists. Authentication is still
			// checked, because that gate is middleware and does run.
			if cells["anon"] != "401" {
				t.Errorf("%s needs a token and admits an anonymous caller (%s)", route, cells["anon"])
			}
		}
	}
}

// TestDeclaredCapabilitiesMatchTodaysAccess is what makes the capability
// mapping worth having.
//
// For every project route it compares three independent things: the capability
// the route declares, whether each built-in role holds that capability, and
// whether the recorded matrix says that role reaches the route today. All three
// must agree.
//
// Without it the declarations are an opinion. With it they are a claim that the
// capability model reproduces the role model exactly, which is the property the
// migration needs and the one that is easy to believe and wrong. Writing this
// test found 49 disagreements in the first draft: viewers reach environment
// variables and project settings that the viewer capability set did not carry,
// deployers write app secrets and open terminals that the deployer set did not,
// and project updates were declared kipper.write, which deployers hold and
// which would have widened them.
func TestDeclaredCapabilitiesMatchTodaysAccess(t *testing.T) {
	cells := matrixCells(t)
	roles := map[string]capability.Role{
		"viewer":   capability.RoleViewer,
		"deployer": capability.RoleDeployer,
		"owner":    capability.RoleOwner,
	}

	for route, d := range routeAuthz {
		if d.class != classProjectCapability {
			continue
		}
		recorded, ok := cells[route]
		if !ok {
			t.Errorf("%s is declared and the matrix has no row for it", route)
			continue
		}
		for column, role := range roles {
			holds := slices.Contains(capability.BuiltIn(role), d.capability)
			status := recorded[column]
			admitted := status != "401" && status != "403"
			if holds == admitted {
				continue
			}
			if holds {
				t.Errorf("%s declares %s, which %s holds, but %s is refused today (%s): the migration would widen access",
					route, d.capability, column, column, status)
				continue
			}
			t.Errorf("%s declares %s, which %s does not hold, but %s reaches it today (%s): the migration would take access away",
				route, d.capability, column, column, status)
		}
	}
}

// TestHandlerInternalRoutesAreActuallyUnreachedByTheProbe separates the two
// classes the matrix cannot tell apart.
//
// classAuthenticated and classHandlerInternal both answer the same thing to
// every member, so the behaviour test asserts the same weak property of both
// and one could quietly become the other. That is not hypothetical: deriving
// the table from the matrix classified POST /api/v1/jobs/{name}/trigger as
// authenticated, because the fixture has no such job and the 404 lands before
// its project gate. A route that needs project standing declared as needing
// only a token is a hole nobody would see.
//
// So the two are held apart from the other side: a handler-internal route must
// in fact answer every member identically, because that is the symptom of a
// gate the probe never reached. One that discriminates has a reachable gate and
// belongs in classProjectCapability, where the capability cross-check applies.
func TestHandlerInternalRoutesAreActuallyUnreachedByTheProbe(t *testing.T) {
	cells := matrixCells(t)
	for route, d := range routeAuthz {
		if d.class != classHandlerInternal {
			continue
		}
		recorded, ok := cells[route]
		if !ok {
			t.Errorf("%s is declared and the matrix has no row for it", route)
			continue
		}
		answers := map[string]bool{}
		for _, column := range []string{"outsider", "viewer", "deployer", "owner"} {
			answers[recorded[column]] = true
		}
		if len(answers) > 1 {
			t.Errorf("%s is declared handler-internal but answers members differently (%v), so its gate is reachable: declare it as a project capability instead",
				route, recorded)
		}
	}
}

// TestAuthenticatedRoutesDoNotDiscriminate holds classAuthenticated to what it
// declares: a route that needs a token and nothing more.
//
// It is the mirror of the handler-internal check, and both exist because the
// matrix cannot tell those two classes apart on its own. A route that does
// discriminate between members is gated on project standing whatever it says,
// and belongs in classProjectCapability where the capability cross-check
// applies. Without this, the weakest class in the table is the easiest place
// for a project route to hide.
func TestAuthenticatedRoutesDoNotDiscriminate(t *testing.T) {
	cells := matrixCells(t)
	for route, d := range routeAuthz {
		if d.class != classAuthenticated {
			continue
		}
		recorded, ok := cells[route]
		if !ok {
			t.Errorf("%s is declared and the matrix has no row for it", route)
			continue
		}
		answers := map[string]bool{}
		for _, column := range []string{"outsider", "viewer", "deployer", "owner"} {
			answers[recorded[column]] = true
		}
		if len(answers) > 1 {
			t.Errorf("%s is declared to need only a token but answers members differently (%v): it is gated on project standing and should say so",
				route, recorded)
		}
	}
}

// TestOnlyAdminRoutesUnderAProjectResolveNoPrincipal keeps scopeNone from
// becoming a place to put a route nobody classified.
//
// Under /api/v1/projects/{name} the segment names a Project or a namespace, and
// a route that resolves neither is doing something else entirely. Today exactly
// one does: PUT .../quota, which is admin-only. The direction test infers
// scopeNone whenever neither tenant is admitted, so without this a project
// route that started refusing both owners for an unrelated reason would slide
// into scopeNone and stop being checked.
func TestOnlyAdminRoutesUnderAProjectResolveNoPrincipal(t *testing.T) {
	for route, d := range routeAuthz {
		if !strings.HasPrefix(routePattern(route), "/api/v1/projects/{name}") {
			if d.scope != scopeNone {
				t.Errorf("%s is outside /projects/{name} and declares a scope; the segment it would apply to does not exist there", route)
			}
			continue
		}
		if d.scope != scopeNone {
			continue
		}
		if d.class != classGlobalRole {
			t.Errorf("%s is under /projects/{name} and resolves neither a Project nor a namespace, but is declared %s rather than admin-only",
				route, d.class)
		}
	}
}

// routePattern strips the method from a route key.
func routePattern(route string) string {
	_, pattern, found := strings.Cut(route, " ")
	if !found {
		return route
	}
	return pattern
}
