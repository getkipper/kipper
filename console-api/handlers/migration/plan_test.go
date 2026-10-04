package migration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
	"github.com/getkipper/kipper/console-api/middleware"
)

// fakeClientWithProject builds a source cluster holding one project
// namespace.
func fakeClientWithProject(t *testing.T) kubernetes.Interface {
	t.Helper()
	return fake.NewSimpleClientset(projectNamespace("shop-prod", "shop"))
}

// crFakeWithWorkloads populates the shop project with a git app.
func crFakeWithWorkloads(t *testing.T) crclient.Client {
	t.Helper()
	app := &kipperv1.App{
		ObjectMeta: metav1.ObjectMeta{Name: "storefront", Namespace: "shop-prod"},
		Spec: kipperv1.AppSpec{
			Image: "registry.example.com/storefront:latest",
			Git:   &kipperv1.AppGitSource{URL: "https://github.com/example/storefront.git"},
		},
	}
	return crfake.NewClientBuilder().WithScheme(migrationScheme()).WithStatusSubresource(&kipperv1.App{}).
		WithObjects(app, ownerOf("shop-prod")).Build()
}

func planClaims(email string) *middleware.Claims {
	return &middleware.Claims{
		RegisteredClaims: jwt.RegisteredClaims{Issuer: "https://dex.example.com/dex", Subject: "sub-" + email},
		Email:            email,
	}
}

func testToken(t *testing.T, endpoint string) (*Token, string) {
	t.Helper()
	tok := &Token{
		Endpoint:   endpoint,
		Secret:     "valid-secret",
		Cluster:    "target.example.com",
		BaseDomain: "target.example.com",
		Expires:    time.Now().Add(time.Hour),
	}
	payload, err := json.Marshal(tok) //nolint:gosec // minting a test migration token is the point
	if err != nil {
		t.Fatal(err)
	}
	return tok, base64.StdEncoding.EncodeToString(payload)
}

func TestReceiptValidationAndBinding(t *testing.T) {
	h := &Handler{}
	claims := planClaims("admin@example.com")
	token := &Token{Endpoint: "https://api.target.example.com", Secret: "s1"}

	id, err := h.issueReceipt(planReceipt{
		User:      receiptUser(claims),
		TokenFP:   tokenFingerprint(token),
		Projects:  canonicalProjects([]string{"shop", "blog"}),
		ExpiresAt: time.Now().Add(planReceiptTTL),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wrong user, wrong token, wrong projects, smuggled overwrites — each is
	// refused, and a failed validation leaves the receipt intact.
	other := planClaims("other@example.com")
	if err := h.validateReceipt(id, other, token, []string{"shop", "blog"}, nil); err == nil {
		t.Fatal("a receipt must not validate for another user")
	}
	rogue := &Token{Endpoint: "https://api.attacker.example.com", Secret: "s2"}
	if err := h.validateReceipt(id, claims, rogue, []string{"shop", "blog"}, nil); err == nil {
		t.Fatal("a receipt must not validate for a different target token")
	}
	if err := h.validateReceipt(id, claims, token, []string{"shop"}, nil); err == nil {
		t.Fatal("a receipt must not validate for a changed project set")
	}
	if err := h.validateReceipt(id, claims, token, []string{"shop", "blog"}, []string{"shop"}); err == nil {
		t.Fatal("a start must not smuggle in overwrite consent the plan never showed")
	}

	// Project order must not matter, and validation is repeatable while
	// consumption works exactly once.
	if err := h.validateReceipt(id, claims, token, []string{"blog", "shop"}, nil); err != nil {
		t.Fatalf("valid receipt refused: %v", err)
	}
	if err := h.validateReceipt(id, claims, token, []string{"blog", "shop"}, nil); err != nil {
		t.Fatalf("validation must be repeatable, got: %v", err)
	}
	if err := h.consumeReceipt(id); err != nil {
		t.Fatalf("consume refused: %v", err)
	}
	if err := h.consumeReceipt(id); err == nil {
		t.Fatal("a consumed receipt must not be consumable again")
	}
	if err := h.validateReceipt(id, claims, token, []string{"blog", "shop"}, nil); err == nil {
		t.Fatal("a consumed receipt must not validate")
	}
}

func TestReceiptBindsConfirmedOverwrites(t *testing.T) {
	h := &Handler{}
	claims := planClaims("admin@example.com")
	token := &Token{Endpoint: "https://api.target.example.com", Secret: "s1"}
	id, _ := h.issueReceipt(planReceipt{
		User:       receiptUser(claims),
		TokenFP:    tokenFingerprint(token),
		Projects:   canonicalProjects([]string{"shop"}),
		Overwrites: canonicalProjects([]string{"shop"}),
		ExpiresAt:  time.Now().Add(planReceiptTTL),
	})
	if err := h.validateReceipt(id, claims, token, []string{"shop"}, []string{"shop"}); err != nil {
		t.Fatalf("matching overwrites refused: %v", err)
	}
	if err := h.validateReceipt(id, claims, token, []string{"shop"}, nil); err == nil {
		t.Fatal("dropping a confirmed overwrite must invalidate the receipt")
	}
}

func TestReceiptExpires(t *testing.T) {
	h := &Handler{}
	claims := planClaims("admin@example.com")
	token := &Token{Endpoint: "https://api.target.example.com", Secret: "s1"}
	id, _ := h.issueReceipt(planReceipt{
		User:      receiptUser(claims),
		TokenFP:   tokenFingerprint(token),
		Projects:  canonicalProjects([]string{"shop"}),
		ExpiresAt: time.Now().Add(-time.Second),
	})
	if err := h.validateReceipt(id, claims, token, []string{"shop"}, nil); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired receipt must be refused, got: %v", err)
	}
}

// TestPlanHandlerReportsInventoryAndBlockers drives the full plan against a
// fake source cluster and an httptest target.
func TestPlanHandlerReportsInventoryAndBlockers(t *testing.T) {
	// Target: valid capacity, one existing project ("shop") for the
	// conflict path.
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/capacity"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"allocatable_cpu_millis": 8000, "allocatable_memory_bytes": 16 * 1024 * 1024 * 1024,
				"allocatable_storage_bytes": 100 * 1024 * 1024 * 1024,
				"requested_cpu_millis":      1000, "requested_memory_bytes": 1024 * 1024 * 1024,
				"requested_storage_bytes": 10 * 1024 * 1024 * 1024,
				"target_version":          "dev",
			})
		case strings.HasSuffix(r.URL.Path, "/projects"):
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{{"name": "shop"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer target.Close()

	h := &Handler{
		Client:   fakeClientWithProject(t),
		CRClient: crFakeWithWorkloads(t),
		Sessions: NewSessionStore(),
		Domain:   "source.example.com",
	}
	_, tokenStr := testToken(t, target.URL)

	body, _ := json.Marshal(map[string]interface{}{
		"token":    tokenStr,
		"projects": []string{"shop"},
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migration/plan", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), middleware.UserContextKey, planClaims("admin@example.com")))
	rec := httptest.NewRecorder()
	h.PlanHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("plan returned %d: %s", rec.Code, rec.Body.String())
	}
	var resp planResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}

	if resp.Receipt == "" {
		t.Fatal("plan must issue a receipt")
	}
	if resp.TargetCluster != "target.example.com" || resp.TargetEndpoint == "" {
		t.Fatal("plan must repeat the consent line data (target cluster and endpoint)")
	}
	if resp.Capacity == nil || resp.Capacity.FreeCPUMillis != 7000 {
		t.Fatalf("plan must carry capacity numbers, got %+v", resp.Capacity)
	}
	if len(resp.NotMigrated) == 0 {
		t.Fatal("plan must list what never migrates")
	}
	// The unconfirmed conflict with the existing target project is a blocker.
	foundConflict := false
	for _, b := range resp.Blockers {
		if strings.Contains(b, "already exists on the target") {
			foundConflict = true
		}
	}
	if !foundConflict {
		t.Fatalf("unconfirmed overwrite must be a blocker, got blockers: %v", resp.Blockers)
	}
	// Inventory: the app row exists and the git app carries its rebuild note.
	foundApp := false
	for _, item := range resp.WillMigrate {
		if item.Kind == "app" && item.Name == "storefront" && strings.Contains(item.Detail, "rebuilt") {
			foundApp = true
		}
	}
	if !foundApp {
		t.Fatalf("git app must appear with its rebuild note, got: %+v", resp.WillMigrate)
	}
}

// TestPlanDigestBindsMaterialFacts pins what re-consent requires: the target
// identity and version, the migrate/skip inventory, warnings, conflicts, and
// the demand being sent all change the digest; the target's free-capacity
// figures (live-rechecked at start) and per-item size details do not.
// The volume mount mapping is behavior the operator consents to, so it must
// render into the plan row (and thus the digest) in a stable order, and a
// change to which app mounts a volume or where must alter the rendering.
func TestVolumeMountSummary(t *testing.T) {
	vol := &kipperv1.Volume{Spec: kipperv1.VolumeSpec{Mounts: []kipperv1.VolumeMountTarget{
		{App: "web", MountPath: "/data/uploads"},
		{App: "api", MountPath: "/srv/files"},
	}}}
	got := volumeMountSummary(vol)
	if got != "api@/srv/files, web@/data/uploads" {
		t.Fatalf("summary = %q, want stable-sorted app@path pairs", got)
	}
	// Reordering the same mounts must not change the rendering.
	rev := &kipperv1.Volume{Spec: kipperv1.VolumeSpec{Mounts: []kipperv1.VolumeMountTarget{
		{App: "api", MountPath: "/srv/files"},
		{App: "web", MountPath: "/data/uploads"},
	}}}
	if volumeMountSummary(rev) != got {
		t.Error("summary must be order-independent")
	}
	// A changed mount path must change the rendering, so the digest moves.
	moved := &kipperv1.Volume{Spec: kipperv1.VolumeSpec{Mounts: []kipperv1.VolumeMountTarget{
		{App: "web", MountPath: "/data/elsewhere"},
		{App: "api", MountPath: "/srv/files"},
	}}}
	if volumeMountSummary(moved) == got {
		t.Error("a changed mount path must change the summary")
	}
	if volumeMountSummary(&kipperv1.Volume{}) != "" {
		t.Error("no mounts renders empty")
	}
}

// A volume's mount binding must move the digest even when it differs only in
// digits, since digit runs are normalised out of the display Detail.
func TestPlanDigestBindsVolumeMountsVerbatim(t *testing.T) {
	base := func() *planResponse {
		return &planResponse{
			TargetCluster: "target.example.com",
			WillMigrate: []planItem{{
				Kind: "volume", Namespace: "shop-prod", Name: "uploads", Status: "ok",
				Detail: "5Gi claimed", Binding: "web1@/data/uploads",
			}},
		}
	}
	reference := planDigest(base())

	numericAppChange := base()
	numericAppChange.WillMigrate[0].Binding = "web2@/data/uploads"
	if planDigest(numericAppChange) == reference {
		t.Error("a numeric-only app change in the mount binding must move the digest")
	}

	numericPathChange := base()
	numericPathChange.WillMigrate[0].Binding = "web1@/data/uploads2"
	if planDigest(numericPathChange) == reference {
		t.Error("a numeric-only mount-path change must move the digest")
	}
}

// A changed per-app disposition or the Mode B flag must move the digest, so a
// start request cannot smuggle a different keep/move choice past the reviewed
// plan's 2FA consent.
func TestPlanDigestBindsDomainDisposition(t *testing.T) {
	base := func() *planResponse {
		return &planResponse{
			TargetCluster: "target.example.com",
			WillMigrate: []planItem{{
				Kind: "app", Namespace: "hrportal-prod", Name: "backend", Status: "ok",
				Host: "app.hrportal.example", DomainClass: "custom", Disposition: "move",
				Binding: "route=app.hrportal.example;disp=move",
			}},
		}
	}
	reference := planDigest(base())

	flipped := base()
	flipped.WillMigrate[0].Disposition = "coexist"
	flipped.WillMigrate[0].Binding = "route=app.hrportal.example;disp=coexist"
	if planDigest(flipped) == reference {
		t.Error("a changed app disposition must move the digest")
	}

	modeB := base()
	modeB.MoveBaseDomain = true
	if planDigest(modeB) == reference {
		t.Error("the Mode B flag must move the digest")
	}

	// The target base domain drives the coexist URLs and the env/secret rewrite
	// destinations, so re-encoding the token with a different (but target-valid)
	// base domain after the plan must invalidate the receipt via the digest.
	rebased := base()
	rebased.TargetBaseDomain = "example.com"
	if planDigest(rebased) == reference {
		t.Error("the target base domain must move the digest")
	}
}

func TestPlanDigestBindsMaterialFacts(t *testing.T) {
	base := func() *planResponse {
		return &planResponse{
			TargetCluster:  "target.example.com",
			TargetEndpoint: "https://api.target.example.com",
			TargetVersion:  "1.0.0",
			WillMigrate:    []planItem{{Kind: "app", Namespace: "shop-prod", Name: "storefront", Status: "ok", Detail: "postgres, ~120MB of data"}},
			WillSkip:       []planItem{{Kind: "service", Namespace: "shop-prod", Name: "reports-db"}},
			Warnings:       []string{"autoscaled app keeps serving"},
			Conflicts:      []string{"shop"},
			NotMigrated:    notMigratedList,
			Capacity:       &planCapacity{NeedCPUMillis: 500, NeedMemoryBytes: 1 << 30, FreeCPUMillis: 4000, FreeMemoryBytes: 1 << 33},
		}
	}
	reference := planDigest(base())

	mutations := map[string]func(*planResponse){
		"target version": func(p *planResponse) { p.TargetVersion = "2.0.0" },
		"target cluster": func(p *planResponse) { p.TargetCluster = "other.example.com" },
		"added workload": func(p *planResponse) {
			p.WillMigrate = append(p.WillMigrate, planItem{Kind: "app", Namespace: "shop-prod", Name: "worker"})
		},
		"added skip": func(p *planResponse) {
			p.WillSkip = append(p.WillSkip, planItem{Kind: "volume", Namespace: "shop-prod", Name: "uploads"})
		},
		"changed warning":    func(p *planResponse) { p.Warnings = []string{"different warning"} },
		"new conflict":       func(p *planResponse) { p.Conflicts = append(p.Conflicts, "blog") },
		"changed demand":     func(p *planResponse) { p.Capacity.NeedCPUMillis = 900 },
		"row status changed": func(p *planResponse) { p.WillMigrate[0].Status = "warn" },
		"row semantics changed": func(p *planResponse) {
			p.WillMigrate[0].Detail = "size could not be measured (no pod found); the transfer decides against the #MB cap at run time"
		},
		"rebuild note dropped": func(p *planResponse) { p.WillMigrate[0].Detail = "" },
	}
	for name, mutate := range mutations {
		p := base()
		mutate(p)
		if planDigest(p) == reference {
			t.Errorf("%s must change the digest", name)
		}
	}

	insensitive := map[string]func(*planResponse){
		"free capacity moved": func(p *planResponse) { p.Capacity.FreeCPUMillis = 100 },
		"size detail moved":   func(p *planResponse) { p.WillMigrate[0].Detail = "postgres, ~121MB of data" },
	}
	for name, mutate := range insensitive {
		p := base()
		mutate(p)
		if planDigest(p) != reference {
			t.Errorf("%s must not force a re-plan", name)
		}
	}
}

// TestPlanProjectBlocksInvalidAutoscaling runs the shared capacity rule over
// each source app, because the target creates the App fresh and a create gets
// no ratcheting from the CRD rule.
func TestPlanProjectBlocksInvalidAutoscaling(t *testing.T) {
	replicas := func(n int32) *int32 { return &n }
	cases := []struct {
		name      string
		autoscale *kipperv1.AppAutoscale
		replicas  *int32
		stopped   bool
		want      []string
		// removal is set where the bounds of a disabled block are invalid.
		removal bool
		// countOnly is set where the block is valid and only the replica count
		// is outside the bounds, which the target's CRD rules do not check.
		countOnly bool
		// resave is set where autoscaling is on and only the stored count is
		// outside the bounds, which kip app scale refuses to change.
		resave bool
	}{
		{name: "no block"},
		{name: "opt-out without bounds", autoscale: &kipperv1.AppAutoscale{Enabled: false}, replicas: replicas(7)},
		{name: "valid policy within bounds", autoscale: &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)}, replicas: replicas(3)},
		{
			name:      "enabled without a maximum",
			autoscale: &kipperv1.AppAutoscale{Enabled: true, CPUTarget: ptr.To[int32](70)},
			want:      []string{"maxReplicas is required when autoscaling is enabled"},
		},
		{
			name:      "minimum above maximum",
			autoscale: &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](5), MaxReplicas: ptr.To[int32](3), CPUTarget: ptr.To[int32](70)},
			want:      []string{"minReplicas (5) must not exceed maxReplicas (3)"},
		},
		{
			name:      "enabled without a target",
			autoscale: &kipperv1.AppAutoscale{Enabled: true, MaxReplicas: ptr.To[int32](3)},
			want:      []string{"set cpuTarget or memoryTarget when autoscaling is enabled"},
		},
		{
			name:      "every problem on one line",
			autoscale: &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](5), MaxReplicas: ptr.To[int32](3)},
			want:      []string{"minReplicas (5) must not exceed maxReplicas (3); set cpuTarget or memoryTarget"},
		},
		{
			name:      "policy off with replicas above the maximum",
			autoscale: &kipperv1.AppAutoscale{Enabled: false, MaxReplicas: ptr.To[int32](3)},
			replicas:  replicas(5),
			want:      []string{"replicas (5) must be between 1 and 3"},
			countOnly: true,
		},
		{
			name:      "absent replicas below the minimum",
			autoscale: &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](5), CPUTarget: ptr.To[int32](70)},
			want:      []string{"replicas (1) must be between 2 and 5"},
			countOnly: true,
			resave:    true,
		},
		{
			name:      "stopped app outside its bounds",
			autoscale: &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](2), MaxReplicas: ptr.To[int32](4)},
			replicas:  replicas(0),
			stopped:   true,
			want:      []string{"replicas (0) must be between 2 and 4"},
			countOnly: true,
		},
		{
			name:      "explicit zero minimum",
			autoscale: &kipperv1.AppAutoscale{Enabled: true, MinReplicas: ptr.To[int32](0), MaxReplicas: ptr.To[int32](3), CPUTarget: ptr.To[int32](70)},
			want:      []string{"minReplicas must be at least 1"},
		},
		{
			name:      "policy off with an explicit zero minimum",
			autoscale: &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](0), MaxReplicas: ptr.To[int32](5)},
			replicas:  replicas(8),
			want:      []string{"minReplicas must be at least 1"},
			removal:   true,
		},
		{
			name:      "policy off with an explicit zero maximum",
			autoscale: &kipperv1.AppAutoscale{Enabled: false, MaxReplicas: ptr.To[int32](0)},
			want:      []string{"maxReplicas must be at least 1"},
			removal:   true,
		},
		{
			name:      "policy off with a minimum and no maximum",
			autoscale: &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](3)},
			replicas:  replicas(3),
			want:      []string{"set maxReplicas with minReplicas, or leave both out to remove the bounds"},
			removal:   true,
		},
		{
			name:      "policy off with the default minimum and no maximum",
			autoscale: &kipperv1.AppAutoscale{Enabled: false, MinReplicas: ptr.To[int32](1)},
			replicas:  replicas(7),
		},
		{
			name:      "explicit zero cpu target beside a memory target",
			autoscale: &kipperv1.AppAutoscale{Enabled: true, MaxReplicas: ptr.To[int32](3), CPUTarget: ptr.To[int32](0), MemoryTarget: ptr.To[int32](60)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := &kipperv1.App{
				ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "shop-prod"},
				Spec: kipperv1.AppSpec{
					Image:     "registry.example.com/web:1",
					Replicas:  tc.replicas,
					Autoscale: tc.autoscale,
				},
			}
			if tc.stopped {
				app.Spec.Stopped = &kipperv1.AppStopped{Reason: "maintenance"}
			}
			h := &Handler{
				Client: fakeClientWithProject(t),
				CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).
					WithObjects(app, ownerOf("shop-prod")).Build(),
				Domain: "source.example.com",
			}
			token, _ := testToken(t, "https://api.target.example.com")
			resp := &planResponse{}
			if err := h.planProject(context.Background(), "shop", token, nil, resp); err != nil {
				t.Fatal(err)
			}
			var blockers []string
			for _, b := range resp.Blockers {
				if strings.Contains(b, "autoscaling") {
					blockers = append(blockers, b)
				}
			}
			if len(blockers) != len(tc.want) {
				t.Fatalf("autoscaling blockers = %q, want %d", blockers, len(tc.want))
			}
			for i, want := range tc.want {
				if !strings.Contains(blockers[i], "shop-prod/web") || !strings.Contains(blockers[i], want) {
					t.Errorf("blocker %q must name shop-prod/web and say %q", blockers[i], want)
				}
				if strings.Contains(blockers[i], "\n") {
					t.Errorf("blocker %q must be one line", blockers[i])
				}
				const remove = "'kip app autoscale web --project shop --remove'"
				if got := strings.Contains(blockers[i], remove); got != tc.removal {
					t.Errorf("blocker %q naming %s = %v, want %v", blockers[i], remove, got, tc.removal)
				}
				const sameRules = "the target checks the same rules"
				if got := strings.Contains(blockers[i], sameRules); got == tc.countOnly {
					t.Errorf("blocker %q saying %q = %v, want %v", blockers[i], sameRules, got, !tc.countOnly)
				}
				for _, cmd := range []string{"'kip app autoscale web --project shop'", "'kip app autoscale web --project shop --off'"} {
					if got := strings.Contains(blockers[i], cmd); got != tc.resave {
						t.Errorf("blocker %q naming %s = %v, want %v", blockers[i], cmd, got, tc.resave)
					}
				}
				if tc.resave && strings.Contains(blockers[i], "kip app scale") {
					t.Errorf("blocker %q must not suggest kip app scale, which autoscaling refuses", blockers[i])
				}
			}
		})
	}
}
