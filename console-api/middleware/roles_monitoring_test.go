package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func usersConfigMap(monitoringJSON string) *corev1.ConfigMap {
	cm := roleConfigMap(monitoringUsers)
	if monitoringJSON != "" {
		cm.Data["monitoring"] = monitoringJSON
	}
	return cm
}

const monitoringUsers = `{"admin@test.com":"admin","dev@test.com":"deployer","viewer@test.com":"viewer"}`

func TestRoleStore_MonitoringAccess(t *testing.T) {
	cm := usersConfigMap(`["dev@test.com","gone@test.com"]`)
	store := NewRoleStore(fake.NewClientset(kipperNamespace(), cm))

	tests := []struct {
		email       string
		wantRole    string
		wantAllowed bool
	}{
		{"admin@test.com", RoleAdmin, true},
		{"dev@test.com", RoleDeployer, true},
		{"viewer@test.com", "", false},
		{"gone@test.com", "", false},
		{"stranger@test.com", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.email, func(t *testing.T) {
			role, allowed := store.MonitoringAccess(tt.email)
			if role != tt.wantRole || allowed != tt.wantAllowed {
				t.Errorf("MonitoringAccess(%q) = (%q, %v), want (%q, %v)", tt.email, role, allowed, tt.wantRole, tt.wantAllowed)
			}
		})
	}
}

func TestRoleStore_MonitoringAccess_MissingKeyMeansNoGrants(t *testing.T) {
	store := NewRoleStore(fake.NewClientset(kipperNamespace(), usersConfigMap("")))

	if _, allowed := store.MonitoringAccess("admin@test.com"); !allowed {
		t.Error("admin should hold monitoring without a grant list")
	}
	if _, allowed := store.MonitoringAccess("dev@test.com"); allowed {
		t.Error("deployer without a grant should be refused")
	}
}

func TestRoleStore_MonitoringAccess_MalformedGrantsRefuseEveryone(t *testing.T) {
	store := NewRoleStore(fake.NewClientset(kipperNamespace(), usersConfigMap(`{"not":"a list"}`)))

	for _, email := range []string{"admin@test.com", "dev@test.com"} {
		if _, allowed := store.MonitoringAccess(email); allowed {
			t.Errorf("%s allowed despite a malformed grant list", email)
		}
	}
	if got := store.GetRole("dev@test.com"); got != RoleDeployer {
		t.Errorf("console roles should survive a malformed grant list, got %q", got)
	}
}

func TestRoleStore_MonitoringAccess_RefusesWhenSnapshotIsStale(t *testing.T) {
	client := fake.NewClientset(kipperNamespace(), usersConfigMap(`["dev@test.com"]`))
	store := NewRoleStore(client)
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	if _, allowed := store.MonitoringAccess("dev@test.com"); !allowed {
		t.Fatal("grant holder refused on a fresh snapshot")
	}

	client.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("api server unavailable")
	})

	now = now.Add(45 * time.Second)
	if _, allowed := store.MonitoringAccess("dev@test.com"); !allowed {
		t.Error("refused within the trust window although the last read succeeded 45s ago")
	}

	now = now.Add(20 * time.Second)
	if _, allowed := store.MonitoringAccess("dev@test.com"); allowed {
		t.Error("allowed on a snapshot older than the trust window")
	}
	if _, allowed := store.MonitoringAccess("admin@test.com"); allowed {
		t.Error("admin allowed on a snapshot older than the trust window")
	}
}

func storedGrants(t *testing.T, client *fake.Clientset) []string {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps(roleConfigMapNamespace).Get(context.Background(), roleConfigMapName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("reading ConfigMap: %v", err)
	}
	var grants []string
	if raw, ok := cm.Data["monitoring"]; ok {
		if err := json.Unmarshal([]byte(raw), &grants); err != nil {
			t.Fatalf("stored grant list is not a JSON array: %v", err)
		}
	}
	sort.Strings(grants)
	return grants
}

func TestRoleStore_SetMonitoring_GrantsAndRevokes(t *testing.T) {
	client := fake.NewClientset(kipperNamespace(), usersConfigMap(""))
	store := NewRoleStore(client)
	ctx := context.Background()

	if err := store.SetMonitoring(ctx, "dev@test.com", true); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := store.SetMonitoring(ctx, "viewer@test.com", true); err != nil {
		t.Fatalf("second grant: %v", err)
	}
	if err := store.SetMonitoring(ctx, "dev@test.com", true); err != nil {
		t.Fatalf("repeated grant: %v", err)
	}
	if got := storedGrants(t, client); len(got) != 2 || got[0] != "dev@test.com" || got[1] != "viewer@test.com" {
		t.Fatalf("stored grants = %v, want [dev@test.com viewer@test.com]", got)
	}
	if _, allowed := store.MonitoringAccess("viewer@test.com"); !allowed {
		t.Error("grant not visible to MonitoringAccess right after the write")
	}

	if err := store.SetMonitoring(ctx, "viewer@test.com", false); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := storedGrants(t, client); len(got) != 1 || got[0] != "dev@test.com" {
		t.Fatalf("stored grants after revoke = %v, want [dev@test.com]", got)
	}
	if _, allowed := store.MonitoringAccess("viewer@test.com"); allowed {
		t.Error("revoked grant still honoured right after the write")
	}

	cm, _ := client.CoreV1().ConfigMaps(roleConfigMapNamespace).Get(ctx, roleConfigMapName, metav1.GetOptions{})
	if cm.Data["users"] != monitoringUsers {
		t.Errorf("users key changed by a grant write: %s", cm.Data["users"])
	}
}

func TestRoleStore_SetMonitoring_RefusesUnknownUser(t *testing.T) {
	client := fake.NewClientset(kipperNamespace(), usersConfigMap(""))
	store := NewRoleStore(client)

	err := store.SetMonitoring(context.Background(), "stranger@test.com", true)
	if !errors.Is(err, ErrUnknownUser) {
		t.Fatalf("err = %v, want ErrUnknownUser", err)
	}
	if got := storedGrants(t, client); len(got) != 0 {
		t.Errorf("stored grants = %v, want none", got)
	}
}

func TestRoleStore_SetMonitoring_RefusesMalformedList(t *testing.T) {
	client := fake.NewClientset(kipperNamespace(), usersConfigMap(`"oops"`))
	store := NewRoleStore(client)

	if err := store.SetMonitoring(context.Background(), "dev@test.com", true); err == nil {
		t.Fatal("grant written over a malformed list")
	}
}

func TestRoleStore_SetMonitoring_RetriesOnConflict(t *testing.T) {
	client := fake.NewClientset(kipperNamespace(), usersConfigMap(""))
	store := NewRoleStore(client)
	conflicts := 1
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts > 0 {
			conflicts--
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, roleConfigMapName, errors.New("stale"))
		}
		return false, nil, nil
	})

	if err := store.SetMonitoring(context.Background(), "dev@test.com", true); err != nil {
		t.Fatalf("grant after one conflict: %v", err)
	}
	if got := storedGrants(t, client); len(got) != 1 || got[0] != "dev@test.com" {
		t.Errorf("stored grants = %v, want [dev@test.com]", got)
	}
}

func TestRoleStore_RemoveUser_DropsMonitoringGrant(t *testing.T) {
	client := fake.NewClientset(kipperNamespace(), usersConfigMap(`["dev@test.com","viewer@test.com"]`))
	store := NewRoleStore(client)

	if err := store.RemoveUser(context.Background(), "dev@test.com"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if got := storedGrants(t, client); len(got) != 1 || got[0] != "viewer@test.com" {
		t.Errorf("stored grants = %v, want [viewer@test.com]", got)
	}
}

func TestRoleStore_SetRole_NewUserStartsWithoutStaleGrant(t *testing.T) {
	// An email removed by a kip release that only rewrites "users" can linger
	// in the grant list; adding it again must not bring the grant back.
	client := fake.NewClientset(kipperNamespace(), usersConfigMap(`["old@test.com","dev@test.com"]`))
	store := NewRoleStore(client)
	ctx := context.Background()

	if err := store.SetRole(ctx, "old@test.com", RoleViewer); err != nil {
		t.Fatalf("add user: %v", err)
	}
	if err := store.SetRole(ctx, "dev@test.com", RoleViewer); err != nil {
		t.Fatalf("change role: %v", err)
	}
	if got := storedGrants(t, client); len(got) != 1 || got[0] != "dev@test.com" {
		t.Errorf("stored grants = %v, want [dev@test.com]: a role change keeps the grant, a new user starts without one", got)
	}
}

func TestRoleStore_MonitoringHolders(t *testing.T) {
	store := NewRoleStore(fake.NewClientset(kipperNamespace(), usersConfigMap(`["dev@test.com","gone@test.com"]`)))

	holders := store.MonitoringHolders()
	if !holders["dev@test.com"] || !holders["admin@test.com"] {
		t.Errorf("holders = %v, want dev and admin", holders)
	}
	if holders["viewer@test.com"] || holders["gone@test.com"] {
		t.Errorf("holders = %v, must not include users without a grant or a role", holders)
	}
}

func TestRoleStore_FailedPromotionGrantsNothing(t *testing.T) {
	client := fake.NewClientset(kipperNamespace(), usersConfigMap(""))
	store := NewRoleStore(client)
	if _, allowed := store.MonitoringAccess("viewer@test.com"); allowed {
		t.Fatal("setup: viewer should not hold monitoring")
	}
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("write refused")
	})

	if err := store.SetRole(context.Background(), "viewer@test.com", RoleAdmin); err == nil {
		t.Fatal("promotion reported success although the write failed")
	}
	if role, allowed := store.MonitoringAccess("viewer@test.com"); allowed || role != "" {
		t.Errorf("a failed promotion granted monitoring: (%q, %v)", role, allowed)
	}
	if got := store.GetRole("viewer@test.com"); got != RoleViewer {
		t.Errorf("GetRole after a failed promotion = %q, want viewer", got)
	}
}

func countUpdates(client *fake.Clientset) *int {
	n := 0
	client.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		n++
		return false, nil, nil
	})
	return &n
}

func TestRoleStore_NewUserAndGrantCleanupAreOneWrite(t *testing.T) {
	// Two writes would leave a window, or on failure a permanent state, where
	// the re-added user holds the old grant.
	client := fake.NewClientset(kipperNamespace(), usersConfigMap(`["old@test.com"]`))
	store := NewRoleStore(client)
	updates := countUpdates(client)

	if err := store.SetRole(context.Background(), "old@test.com", RoleViewer); err != nil {
		t.Fatal(err)
	}
	if *updates != 1 {
		t.Errorf("adding a user took %d ConfigMap updates, want 1", *updates)
	}
	if got := storedGrants(t, client); len(got) != 0 {
		t.Errorf("stored grants = %v, want none", got)
	}
}

func TestRoleStore_RemoveUserIsOneWrite(t *testing.T) {
	client := fake.NewClientset(kipperNamespace(), usersConfigMap(`["dev@test.com"]`))
	store := NewRoleStore(client)
	updates := countUpdates(client)

	if err := store.RemoveUser(context.Background(), "dev@test.com"); err != nil {
		t.Fatal(err)
	}
	if *updates != 1 {
		t.Errorf("removing a user took %d ConfigMap updates, want 1", *updates)
	}
}
