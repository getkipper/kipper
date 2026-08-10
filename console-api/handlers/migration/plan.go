package migration

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/builder"
	"github.com/getkipper/kipper/console-api/domain"
	"github.com/getkipper/kipper/console-api/middleware"
)

// The plan screen is the mandatory report every migration starts from: what
// will move, what will be skipped, what never migrates, capacity numbers,
// conflicts, and warnings — all computed with token-authenticated,
// non-consuming calls. It issues a single-use receipt the start endpoint
// requires, which proves the report was produced for exactly this user,
// token, and project set. The receipt is consent, never a reservation:
// the start endpoint re-runs every blocker check against live state.

// planItem is one row of the report.
type planItem struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	// Status is ok (green, will migrate), warn (amber, skipped or needs
	// attention), or blocked (red).
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Binding carries behavior-bearing identifiers that must enter the
	// receipt digest verbatim, not through the digit-normalised Detail — a
	// volume's app@path mounts, where a change of "web1" to "web2" or a
	// numeric path segment would otherwise be normalised to the same string.
	Binding string `json:"binding,omitempty"`

	// Domain disposition for an app with a public route. Host is the current
	// route host, DomainClass is custom|platform|gateway, Disposition is
	// move|coexist|gateway, and TargetURL is where the app is reachable after
	// cutover. Display fields; the behaviour is bound through Binding.
	Host        string `json:"host,omitempty"`
	DomainClass string `json:"domain_class,omitempty"`
	Disposition string `json:"disposition,omitempty"`
	TargetURL   string `json:"target_url,omitempty"`
}

// planCapacity carries the numbers behind the capacity verdict. Operators
// trust numbers over a bare pass/fail.
type planCapacity struct {
	NeedCPUMillis    int64 `json:"need_cpu_millis"`
	NeedMemoryBytes  int64 `json:"need_memory_bytes"`
	NeedStorageBytes int64 `json:"need_storage_bytes"`
	FreeCPUMillis    int64 `json:"free_cpu_millis"`
	FreeMemoryBytes  int64 `json:"free_memory_bytes"`
	FreeStorageBytes int64 `json:"free_storage_bytes"`
	// StorageKnown is false when the target could not measure its disk, in
	// which case the storage comparison is skipped rather than failed.
	StorageKnown bool `json:"storage_known"`
}

// planResponse is the full report.
type planResponse struct {
	TargetCluster  string        `json:"target_cluster"`
	TargetEndpoint string        `json:"target_endpoint"`
	TargetVersion  string        `json:"target_version,omitempty"`
	Blockers       []string      `json:"blockers"`
	Warnings       []string      `json:"warnings"`
	WillMigrate    []planItem    `json:"will_migrate"`
	WillSkip       []planItem    `json:"will_skip"`
	NotMigrated    []string      `json:"not_migrated"`
	Conflicts      []string      `json:"conflicts"`
	Capacity       *planCapacity `json:"capacity,omitempty"`
	Receipt        string        `json:"receipt,omitempty"`
	ReceiptExpires *time.Time    `json:"receipt_expires,omitempty"`
	// OutOfBand reports whether any security-notification channel leaves the
	// box; the console warns before start when none does.
	OutOfBand bool `json:"out_of_band_notifications"`
	// TargetBaseDomain is the target cluster's base domain, for the console to
	// render coexist URLs. MoveBaseDomain echoes the consented Mode B choice and
	// is bound into the digest.
	TargetBaseDomain string `json:"target_base_domain,omitempty"`
	MoveBaseDomain   bool   `json:"move_base_domain,omitempty"`
}

// notMigratedList is the static "what does NOT move" block, mirrored from
// docs/en/migration.md. Nobody reads docs mid-migration, so the report
// repeats it.
var notMigratedList = []string{
	"System components (Traefik, cert-manager, Longhorn, Dex) — they already exist on the target",
	"User accounts (Dex) — the target starts with only its bootstrap admin; import them with kip user import",
	"TLS certificates — cert-manager issues new ones on the target once DNS lands",
	"Postgres globals — extra roles, role passwords, and ALTER DATABASE settings stay behind",
	"Service share links — grants and signing key stay on the source; mint fresh links after cutover",
	"Build history — git apps are rebuilt fresh on the target",
	"Pod logs",
}

// planReceiptTTL bounds how long an issued plan stays startable before the
// operator has to look at a fresh report.
const planReceiptTTL = 15 * time.Minute

// digitRuns matches the numeric figures normalised out of digest-bound
// details.
var digitRuns = regexp.MustCompile(`[0-9]+`)

// planReceipt binds one issued report to the exact migration it authorises:
// the operator, the target token, the project set, the overwrites the
// operator confirmed, and a digest of the material facts the report showed.
type planReceipt struct {
	User       string
	TokenFP    string
	Projects   string
	Overwrites string
	Digest     string
	ExpiresAt  time.Time
}

// canonicalProjects renders a project list order-independently.
func canonicalProjects(projects []string) string {
	sorted := append([]string(nil), projects...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

// tokenFingerprint identifies a migration token without holding its secret.
func tokenFingerprint(token *Token) string {
	sum := sha256.Sum256([]byte(token.Endpoint + "\x00" + token.Secret))
	return hex.EncodeToString(sum[:8])
}

// receiptUser identifies the operator a receipt was issued to, by the same
// (issuer, subject) pair the 2FA factor is keyed on.
func receiptUser(claims *middleware.Claims) string {
	if claims == nil {
		return ""
	}
	return claims.Issuer + "\x00" + claims.Subject
}

// planDigest fingerprints the material facts of the displayed report, so a
// receipt stops matching when what the operator saw no longer describes what
// would happen: the target's identity and version, everything that migrates
// or gets skipped — identity, status, and semantic detail — the demand being
// sent, warnings, conflicts, and the notification posture.
//
// Two exclusions, both live-checked at start instead: blockers, because the
// start refuses on any current blocker with its own message, and the
// target's free-capacity figures, because unrelated scheduling on a live
// target moves them constantly — the shortfall check re-runs against the
// fresh numbers, so a fit that still holds needs no re-consent.
func planDigest(resp *planResponse) string {
	// A row's status and detail carry material semantics — the rebuild note
	// on a git app, a size that could not be measured, the service type —
	// so both are bound. Only the numeric figures inside the detail are
	// normalised out: a database drifting from ~120MB to ~121MB changes
	// nothing the operator consented to, while crossing into "skipped"
	// moves the row between lists and changes the digest regardless.
	items := func(list []planItem) []string {
		out := make([]string, 0, len(list))
		for _, item := range list {
			detail := digitRuns.ReplaceAllString(item.Detail, "#")
			// Binding is appended verbatim: its identifiers are exact, so
			// digit normalisation must not collapse "web1" and "web2".
			out = append(out, item.Kind+"/"+item.Namespace+"/"+item.Name+"/"+item.Status+"/"+detail+"/"+item.Binding)
		}
		sort.Strings(out)
		return out
	}
	warnings := append([]string(nil), resp.Warnings...)
	sort.Strings(warnings)
	conflicts := append([]string(nil), resp.Conflicts...)
	sort.Strings(conflicts)

	var demandCPU, demandMemory, demandStorage int64
	if resp.Capacity != nil {
		demandCPU = resp.Capacity.NeedCPUMillis
		demandMemory = resp.Capacity.NeedMemoryBytes
		demandStorage = resp.Capacity.NeedStorageBytes
	}

	payload, _ := json.Marshal(struct {
		TargetCluster    string
		TargetEndpoint   string
		TargetVersion    string
		Migrates         []string
		Skips            []string
		Warnings         []string
		Conflicts        []string
		NotMigrated      []string
		DemandCPU        int64
		DemandMemory     int64
		DemandStorage    int64
		OutOfBand        bool
		MoveBaseDomain   bool
		TargetBaseDomain string
	}{
		resp.TargetCluster, resp.TargetEndpoint, resp.TargetVersion,
		items(resp.WillMigrate), items(resp.WillSkip), warnings, conflicts,
		resp.NotMigrated, demandCPU, demandMemory, demandStorage, resp.OutOfBand,
		resp.MoveBaseDomain,
		resp.TargetBaseDomain,
	})
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

// issueReceipt stores a new receipt and returns its ID.
func (h *Handler) issueReceipt(rc planReceipt) (string, error) {
	idBytes := make([]byte, 16)
	if _, err := rand.Read(idBytes); err != nil {
		return "", fmt.Errorf("generating plan receipt: %w", err)
	}
	id := hex.EncodeToString(idBytes)

	h.receiptsMu.Lock()
	defer h.receiptsMu.Unlock()
	if h.receipts == nil {
		h.receipts = make(map[string]planReceipt)
	}
	// Expired receipts die here rather than on a timer; the map only ever
	// holds what recent plan views created.
	for old, r := range h.receipts {
		if time.Now().After(r.ExpiresAt) {
			delete(h.receipts, old)
		}
	}
	h.receipts[id] = rc
	return id, nil
}

// validateReceipt checks a receipt against the start request without
// spending it, so precheck failures after a valid receipt never burn it.
// Every binding must hold: operator, token, project set, and the exact
// overwrites the operator confirmed on the report — a start request cannot
// smuggle in overwrite consent the plan never showed.
func (h *Handler) validateReceipt(id string, claims *middleware.Claims, token *Token, projects, overwrites []string) error {
	h.receiptsMu.Lock()
	defer h.receiptsMu.Unlock()
	rc, ok := h.receipts[id]
	if !ok {
		return fmt.Errorf("no valid migration plan found — review the plan first")
	}
	if time.Now().After(rc.ExpiresAt) {
		delete(h.receipts, id)
		return fmt.Errorf("the migration plan has expired — review a fresh plan")
	}
	if rc.User != receiptUser(claims) {
		return fmt.Errorf("the migration plan was issued to a different user — review the plan yourself")
	}
	if rc.TokenFP != tokenFingerprint(token) {
		return fmt.Errorf("the migration plan was issued for a different target token — review the plan again")
	}
	if rc.Projects != canonicalProjects(projects) {
		return fmt.Errorf("the project selection changed since the plan — review the plan again")
	}
	if rc.Overwrites != canonicalProjects(overwrites) {
		return fmt.Errorf("the confirmed overwrites differ from the plan — review the plan again")
	}
	return nil
}

// receiptDigest returns the digest a receipt was issued with.
func (h *Handler) receiptDigest(id string) string {
	h.receiptsMu.Lock()
	defer h.receiptsMu.Unlock()
	return h.receipts[id].Digest
}

// consumeReceipt spends a validated receipt. Atomic and single-use: of two
// concurrent starts holding the same receipt, exactly one gets past this.
func (h *Handler) consumeReceipt(id string) error {
	h.receiptsMu.Lock()
	defer h.receiptsMu.Unlock()
	if _, ok := h.receipts[id]; !ok {
		return fmt.Errorf("the migration plan was already used — review a fresh plan")
	}
	delete(h.receipts, id)
	return nil
}

// outboundMigrationDisabled reports whether the host-level kill switch is
// set. Any value except empty, "0", and "false" disables outbound migration —
// fail closed on typos like "yes please".
func outboundMigrationDisabled() bool {
	v := strings.TrimSpace(os.Getenv("KIPPER_DISABLE_OUTBOUND_MIGRATION"))
	return v != "" && v != "0" && !strings.EqualFold(v, "false")
}

// buildPlan computes the full migration report. Shared verbatim by the plan
// endpoint and the start recomputation, so what the report showed and what
// the start enforces can never drift.
func (h *Handler) buildPlan(ctx context.Context, claims *middleware.Claims, token *Token, projects, confirmedOverwrites, keepDomains []string, moveBaseDomain bool) *planResponse {
	resp := &planResponse{
		TargetCluster:    token.Cluster,
		TargetEndpoint:   token.Endpoint,
		TargetBaseDomain: token.BaseDomain,
		MoveBaseDomain:   moveBaseDomain,
		NotMigrated:      notMigratedList,
		Blockers:         []string{},
		Warnings:         []string{},
		WillSkip:         []planItem{},
		Conflicts:        []string{},
	}
	keep := make(map[string]bool, len(keepDomains))
	for _, k := range keepDomains {
		keep[k] = true
	}

	// Mode B (adopting the source base domain on the target) is not implemented
	// yet: without the ClusterIdentity adoption, setting this only strips the
	// platform hosts while leaving env/secret references on the source, so the
	// flag is refused here — for the console and any direct API caller alike.
	// The digest still binds it, so a start cannot slip it past the plan.
	if moveBaseDomain {
		resp.Blockers = append(resp.Blockers,
			"Moving the whole base domain (full-cluster adoption) is not available yet — leave it off.")
	}
	if h.Security != nil {
		resp.OutOfBand = h.Security.OutOfBandConfigured(ctx)
	}
	if !resp.OutOfBand {
		resp.Warnings = append(resp.Warnings,
			"No out-of-band notification channel is configured (SMTP, Slack, or a host-pinned security channel). Only the console bell would record this migration.")
	}

	// Kill switch surfaces at plan time, not after the operator has filled
	// in the whole flow.
	if outboundMigrationDisabled() {
		resp.Blockers = append(resp.Blockers,
			"Outbound migration is disabled on this cluster (KIPPER_DISABLE_OUTBOUND_MIGRATION). A host operator can lift this.")
	}

	// Step-up factor: the report names the eligibility date instead of
	// letting the start button discover it.
	if h.StepUpStatus != nil {
		state, eligibleAt, eligible, err := h.StepUpStatus(ctx, claims)
		switch {
		case err != nil:
			resp.Blockers = append(resp.Blockers, fmt.Sprintf("2FA status unavailable: %v", err))
		case state != "active":
			resp.Blockers = append(resp.Blockers,
				"Starting a migration requires 2FA. Enroll a factor in Settings → Two-factor authentication (a host operator issues the enrollment code with: kip 2fa bootstrap <your email>).")
		case !eligible:
			resp.Blockers = append(resp.Blockers, fmt.Sprintf(
				"Your 2FA factor is too new to authorise a migration — it becomes eligible on %s.",
				eligibleAt.Format("2 January 2006 15:04 MST")))
		}
	}

	// Everything target-side runs off the validating, non-consuming
	// endpoints. An unreachable target is a blocker, not an error page.
	demand, err := h.computeMigrationDemand(ctx, projects)
	if err != nil {
		resp.Blockers = append(resp.Blockers, fmt.Sprintf("Could not size the selected projects: %v", err))
		return resp
	}
	target, targetVersion, err := h.fetchTargetCapacity(token, projects)
	if err != nil {
		resp.Blockers = append(resp.Blockers, fmt.Sprintf("The target cluster is unreachable or refused the token: %v", err))
	} else {
		resp.TargetVersion = targetVersion
		resp.Capacity = &planCapacity{
			NeedCPUMillis:    demand.CPU,
			NeedMemoryBytes:  demand.Memory,
			NeedStorageBytes: demand.Storage,
			FreeCPUMillis:    target.FreeCPU(),
			FreeMemoryBytes:  target.FreeMemory(),
			FreeStorageBytes: target.FreeStorage(),
			StorageKnown:     target.AllocatableStorage > 0,
		}
		if msg := capacityShortfall(demand, target); msg != "" {
			resp.Blockers = append(resp.Blockers, msg)
		}
		if srcMajor, srcOK := majorVersion(BuildVersion); srcOK {
			if tgtMajor, tgtOK := majorVersion(targetVersion); tgtOK && srcMajor != tgtMajor {
				resp.Blockers = append(resp.Blockers, fmt.Sprintf(
					"Version mismatch: this cluster runs %s, the target runs %s. Upgrade the older cluster to the same major version first.",
					BuildVersion, targetVersion))
			}
		}

		// Conflicts come from the same listing the accept-time 409 uses,
		// surfaced before anything moves. Confirmed overwrites downgrade to
		// a warning row.
		if conflicts, err := h.targetConflicts(token, projects); err != nil {
			resp.Blockers = append(resp.Blockers, fmt.Sprintf("Could not check the target for project conflicts: %v", err))
		} else {
			confirmed := make(map[string]bool, len(confirmedOverwrites))
			for _, name := range confirmedOverwrites {
				confirmed[name] = true
			}
			for _, name := range conflicts {
				resp.Conflicts = append(resp.Conflicts, name)
				if confirmed[name] {
					resp.Warnings = append(resp.Warnings, fmt.Sprintf(
						"Project %s already exists on the target and will be overwritten.", name))
				} else {
					resp.Blockers = append(resp.Blockers, fmt.Sprintf(
						"Project %s already exists on the target — confirm the overwrite to proceed.", name))
				}
			}
		}
	}

	// Inventory. Failing to enumerate a project is a blocker: a partial
	// report must never read as the whole picture.
	for _, project := range projects {
		if err := h.planProject(ctx, project, token, keep, resp); err != nil {
			resp.Blockers = append(resp.Blockers, fmt.Sprintf("Could not inventory project %s: %v", project, err))
		}
	}

	if warning := h.autoscaledAppsWarning(ctx, projects); warning != nil {
		resp.Warnings = append(resp.Warnings, warning.Detail)
	}

	return resp
}

// PlanHandler produces the migration report and its receipt.
// POST /api/v1/migration/plan
func (h *Handler) PlanHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Token               string   `json:"token"`
		Projects            []string `json:"projects"`
		ConfirmedOverwrites []string `json:"confirmed_overwrites,omitempty"`
		KeepDomains         []string `json:"keep_domains,omitempty"`
		MoveBaseDomain      bool     `json:"move_base_domain,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Token == "" || len(req.Projects) == 0 {
		respondError(w, http.StatusBadRequest, "token and projects are required")
		return
	}

	claims := middleware.UserFromContext(r.Context())

	// Local decode only — the plan never consumes the token and never
	// pre-authorises anything.
	token, err := DecodeToken(req.Token)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	resp := h.buildPlan(ctx, claims, token, req.Projects, req.ConfirmedOverwrites, req.KeepDomains, req.MoveBaseDomain)

	// The receipt is issued regardless of blockers — it proves the report
	// was seen, and start re-checks everything anyway.
	expires := time.Now().Add(planReceiptTTL)
	receiptID, err := h.issueReceipt(planReceipt{
		User:       receiptUser(claims),
		TokenFP:    tokenFingerprint(token),
		Projects:   canonicalProjects(req.Projects),
		Overwrites: canonicalProjects(req.ConfirmedOverwrites),
		Digest:     planDigest(resp),
		ExpiresAt:  expires,
	})
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	resp.Receipt = receiptID
	resp.ReceiptExpires = &expires

	respondJSON(w, http.StatusOK, resp)
}

// targetConflicts lists the selected projects that already exist on the
// target, using the token-authenticated project listing.
func (h *Handler) targetConflicts(token *Token, projects []string) ([]string, error) {
	status, body, err := h.callTargetRaw(token, "GET", "/api/v1/migrate-target/projects", nil)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("target returned %d", status)
	}
	var targetProjects []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &targetProjects); err != nil {
		return nil, fmt.Errorf("unreadable project list from target")
	}
	existing := make(map[string]bool, len(targetProjects))
	for _, p := range targetProjects {
		existing[p.Name] = true
	}
	var conflicts []string
	for _, name := range projects {
		if existing[name] {
			conflicts = append(conflicts, name)
		}
	}
	return conflicts, nil
}

// planProject adds one project's inventory to the report.
func (h *Handler) planProject(ctx context.Context, project string, token *Token, keep map[string]bool, resp *planResponse) error {
	namespaces, err := h.getProjectNamespaces(ctx, project)
	if err != nil {
		return err
	}

	var runningApps []string
	var gitApps []string
	for _, ns := range namespaces {
		// Fail closed on a namespace read error: guessing env="" would derive
		// the wrong host and misclassify a platform subdomain as a mover.
		nsObj, nsErr := h.Client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
		if nsErr != nil {
			return fmt.Errorf("reading namespace %s: %w", ns, nsErr)
		}
		env := nsObj.Labels["kipper.run/environment"]
		var appList kipperv1.AppList
		if err := h.CRClient.List(ctx, &appList, crclient.InNamespace(ns)); err != nil {
			return fmt.Errorf("listing apps in %s: %w", ns, err)
		}
		for i := range appList.Items {
			app := &appList.Items[i]
			item := planItem{Kind: "app", Name: app.Name, Namespace: ns, Status: "ok"}
			if app.Spec.Git != nil {
				item.Detail = "rebuilt from the branch head on the target"
				gitApps = append(gitApps, ns+"/"+app.Name)
			}
			h.planAppDomain(app, ns, env, token, keep, resp.MoveBaseDomain, &item)
			resp.WillMigrate = append(resp.WillMigrate, item)
			// Each volume is copied once, so writes from still-running apps
			// after their volume's transfer would stay behind silently.
			if app.Spec.Replicas == nil || *app.Spec.Replicas > 0 {
				runningApps = append(runningApps, ns+"/"+app.Name)
			}
		}

		var svcList kipperv1.ServiceList
		if err := h.CRClient.List(ctx, &svcList, crclient.InNamespace(ns)); err != nil {
			return fmt.Errorf("listing services in %s: %w", ns, err)
		}
		for i := range svcList.Items {
			svc := &svcList.Items[i]
			h.planService(ctx, svc, ns, resp)
		}

		var fnList kipperv1.FunctionList
		if err := h.CRClient.List(ctx, &fnList, crclient.InNamespace(ns)); err != nil {
			return fmt.Errorf("listing functions in %s: %w", ns, err)
		}
		for i := range fnList.Items {
			resp.WillMigrate = append(resp.WillMigrate, planItem{Kind: "function", Name: fnList.Items[i].Name, Namespace: ns, Status: "ok"})
		}

		var jobList kipperv1.JobList
		if err := h.CRClient.List(ctx, &jobList, crclient.InNamespace(ns)); err != nil {
			return fmt.Errorf("listing jobs in %s: %w", ns, err)
		}
		for i := range jobList.Items {
			resp.WillMigrate = append(resp.WillMigrate, planItem{Kind: "job", Name: jobList.Items[i].Name, Namespace: ns, Status: "ok"})
		}

		if err := h.planVolumes(ctx, ns, resp); err != nil {
			return err
		}

		secrets, err := h.Client.CoreV1().Secrets(ns).List(ctx, metav1.ListOptions{})
		if err != nil {
			return fmt.Errorf("listing secrets in %s: %w", ns, err)
		}
		// The same predicate the transfer uses. The operator consents to a plan
		// digest, so a count built any other way is consent to a number that
		// does not describe the run.
		riding, err := h.credentialsRidingWithServices(ctx, ns)
		if err != nil {
			return err
		}
		count := 0
		for i := range secrets.Items {
			if transferableSecret(&secrets.Items[i], riding) {
				count++
			}
		}
		if count > 0 {
			resp.WillMigrate = append(resp.WillMigrate, planItem{
				Kind: "secrets", Name: fmt.Sprintf("%d secrets", count), Namespace: ns, Status: "ok",
			})
		}
	}
	if len(runningApps) > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf(
			"Source apps are still running (%s). Data is copied once per volume, so scale them to 0 before starting — writes after a transfer stay on this cluster.",
			strings.Join(runningApps, ", ")))
	}
	if cpu, memory := builder.ClusterBuildDefaults(); len(gitApps) > 0 && (cpu != "" || memory != "") {
		limits := memory
		if limits == "" {
			limits = cpu + " CPU"
		} else if cpu != "" {
			limits += " and " + cpu + " CPU"
		}
		resp.Warnings = append(resp.Warnings, fmt.Sprintf(
			"This cluster raises the build limit to %s for apps that set none of their own. That is a setting on this cluster, not part of an app, so it is written into each migrated git app to keep its build working on the target.",
			limits))
	}
	if len(gitApps) > 0 {
		resp.Warnings = append(resp.Warnings, fmt.Sprintf(
			"Git apps (%s) are rebuilt from the branch head on the target, not copied byte-for-byte. If a branch moved since its last deploy, the target runs the newer code. Confirm each branch is where you want it, or copy the running image by hand before cutover.",
			strings.Join(gitApps, ", ")))
	}
	return nil
}

// planAppDomain fills the domain-disposition fields on an app's plan item and
// its digest Binding, so what the operator consents to matches what the run
// does. An app with no public route contributes nothing.
func (h *Handler) planAppDomain(app *kipperv1.App, ns, env string, token *Token, keep map[string]bool, moveBaseDomain bool, item *planItem) {
	if app.Spec.Route == nil {
		return
	}
	prefix := domain.AppRoutePrefix(app.Name, env)
	derived := domain.SubdomainFor(prefix, h.Domain)
	effective := app.Spec.Route.Host
	if effective == "" {
		effective = derived
	}
	key := ns + "/" + app.Name
	class, disp := classifyAppRoute(effective, derived, key, keep)

	item.Host = effective
	item.DomainClass = string(class)
	item.Disposition = string(disp)
	switch {
	case disp == dispositionMove:
		// The custom domain follows the app to the new cluster.
		item.TargetURL = "https://" + effective
	case moveBaseDomain && class == domain.DomainClassPlatform:
		// Mode B adopts the base domain on the target, so the app keeps its host.
		item.TargetURL = "https://" + effective
	default:
		if tgt := domain.TargetEquivalent(prefix, token.BaseDomain); tgt != "" {
			item.TargetURL = "https://" + tgt
		}
	}

	// Bind host + disposition into the digest verbatim, so a tampered keep/move
	// choice changes the digest and fails consent.
	item.Binding = fmt.Sprintf("route=%s;disp=%s", effective, disp)
}

// planService classifies one service for the report, sizing its database
// against the automatic-transfer cap.
func (h *Handler) planService(ctx context.Context, svc *kipperv1.Service, ns string, resp *planResponse) {
	switch {
	case needsManualDataTransfer(svc.Spec.Type):
		resp.WillMigrate = append(resp.WillMigrate, planItem{
			Kind: "service", Name: svc.Name, Namespace: ns, Status: "ok",
			Detail: fmt.Sprintf("%s — storage moves as raw bytes; the service pauses during the transfer", svc.Spec.Type),
		})
	case hasExportableData(svc.Spec.Type):
		size, err := h.estimateDatabaseSize(ctx, ns, svc.Name, svc.Spec.Type)
		switch {
		case err != nil:
			resp.WillMigrate = append(resp.WillMigrate, planItem{
				Kind: "service", Name: svc.Name, Namespace: ns, Status: "warn",
				Detail: fmt.Sprintf("%s — size could not be measured (%v); the transfer decides against the %dMB cap at run time", svc.Spec.Type, err, maxAutoTransferBytes/(1024*1024)),
			})
		case size > maxAutoTransferBytes:
			resp.WillSkip = append(resp.WillSkip, planItem{
				Kind: "service", Name: svc.Name, Namespace: ns, Status: "warn",
				Detail: fmt.Sprintf("%s database is ~%dMB — over the %dMB automatic-transfer cap, data stays behind with manual steps", svc.Spec.Type, size/(1024*1024), maxAutoTransferBytes/(1024*1024)),
			})
		default:
			resp.WillMigrate = append(resp.WillMigrate, planItem{
				Kind: "service", Name: svc.Name, Namespace: ns, Status: "ok",
				Detail: fmt.Sprintf("%s, ~%dMB of data", svc.Spec.Type, size/(1024*1024)),
			})
		}
	default:
		resp.WillMigrate = append(resp.WillMigrate, planItem{
			Kind: "service", Name: svc.Name, Namespace: ns, Status: "ok", Detail: svc.Spec.Type,
		})
	}
}

// planVolumes lists the namespace's shared volumes. Volume data moves
// through the chunked resumable transfer, so the claim size is information,
// never a cap. Service-owned PVCs are covered by their service's entry and
// stay out of this list.
func (h *Handler) planVolumes(ctx context.Context, ns string, resp *planResponse) error {
	volumes, err := h.listCRVolumes(ctx, ns)
	if err != nil {
		return fmt.Errorf("listing volumes in %s: %w", ns, err)
	}
	for i := range volumes {
		vol := &volumes[i]
		mounts := volumeMountSummary(vol)
		detail := fmt.Sprintf("%s claimed — chunked transfer, resumes on failure", vol.Spec.Size)
		if mounts != "" {
			detail += ", mounted by " + mounts
		}
		// The mount mapping also rides Binding so it enters the receipt
		// digest verbatim: migration re-applies the whole volume spec on the
		// target, so which app mounts it and where is consented behavior, and
		// a numeric-only change (web1 vs web2) must still move the digest.
		resp.WillMigrate = append(resp.WillMigrate, planItem{
			Kind: "volume", Name: vol.Name, Namespace: ns, Status: "ok",
			Detail: detail, Binding: mounts,
		})
	}
	return nil
}

// volumeMountSummary renders a volume's app/mount-path mapping in a stable
// order, so a change to which app mounts a volume or at what path alters the
// plan digest the 2FA step approves.
func volumeMountSummary(vol *kipperv1.Volume) string {
	if len(vol.Spec.Mounts) == 0 {
		return ""
	}
	pairs := make([]string, 0, len(vol.Spec.Mounts))
	for _, m := range vol.Spec.Mounts {
		pairs = append(pairs, m.App+"@"+m.MountPath)
	}
	sort.Strings(pairs)
	return strings.Join(pairs, ", ")
}
