package migration

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

func twoCopyService() *kipperv1.Service {
	svc := mailhogService()
	svc.Spec.Copies = ptr.To[int32](2)
	return svc
}

func TestPlan_TwoCopiesNeedATargetThatCanCreateThem(t *testing.T) {
	for name, tc := range map[string]struct {
		capacity map[string]interface{}
		refusal  string
	}{
		"an older target":          {map[string]interface{}{}, "--max-copies 2"},
		"a target offering one":    {map[string]interface{}{"copies_offered": 1, "replica_nodes": 2}, "--max-copies 2"},
		"a target with one node":   {map[string]interface{}{"copies_offered": 2, "replica_nodes": 1}, "kip node storage"},
		"a target that can create": {map[string]interface{}{"copies_offered": 2, "replica_nodes": 2}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			resp := planWithCapacity(t, tc.capacity, twoCopyService())
			if tc.refusal == "" {
				if blockerMentioning(resp, "two copies") {
					t.Errorf("refused: %v", resp.Blockers)
				}
				return
			}
			if !blockerMentioning(resp, "shop-prod/mail") || !blockerMentioning(resp, tc.refusal) {
				t.Errorf("want a blocker naming shop-prod/mail and %q, got %v", tc.refusal, resp.Blockers)
			}
		})
	}
}

func TestPlan_OneCopyNeedsNothingFromTheTarget(t *testing.T) {
	if resp := planWithCapacity(t, map[string]interface{}{}, mailhogService()); blockerMentioning(resp, "two copies") {
		t.Errorf("a service without two copies was refused: %v", resp.Blockers)
	}
}

func TestPlan_AVolumeWithTwoCopiesCannotBeCreatedOnTheTarget(t *testing.T) {
	vol := &kipperv1.Volume{
		ObjectMeta: metav1.ObjectMeta{Name: "uploads", Namespace: "shop-prod"},
		Spec:       kipperv1.VolumeSpec{Size: "1Gi", Copies: ptr.To[int32](2)},
	}
	resp := planWithCapacity(t, map[string]interface{}{"copies_offered": 2, "replica_nodes": 2}, vol)
	if !blockerMentioning(resp, "shop-prod/uploads") || !blockerMentioning(resp, "shared volume") {
		t.Errorf("want a blocker naming the shared volume, got %v", resp.Blockers)
	}
}

func TestFetchTargetCapacity_ReadsWhatCopiesTheTargetCanCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"allocatable_cpu_millis":1,"allocatable_memory_bytes":1,"allocatable_storage_bytes":1,` +
			`"requested_cpu_millis":0,"requested_memory_bytes":0,"requested_storage_bytes":0,"copies_offered":2,"replica_nodes":3}`))
	}))
	defer srv.Close()
	got, _, err := (&Handler{}).fetchTargetCapacity(&Token{Endpoint: srv.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.CopiesOffered != 2 || got.ReplicaNodes != 3 {
		t.Errorf("got offered %d, nodes %d", got.CopiesOffered, got.ReplicaNodes)
	}
}

func TestTargetCapacity_ReportsWhatCopiesThisClusterCanCreate(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenSecretName, Namespace: tokenSecretNamespace},
		Data:       map[string][]byte{"secret": []byte("s"), "expires": []byte(time.Now().Add(time.Hour).Format(time.RFC3339))},
	})
	offer := &kipperv1.PlatformConfig{ObjectMeta: metav1.ObjectMeta{Name: "platform"},
		Spec: kipperv1.PlatformConfigSpec{MaxCopies: ptr.To[int32](2)}}
	h := &Handler{Client: client, CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(offer).Build()}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/migrate-target/capacity", nil)
	req.Header.Set("X-Migration-Secret", "s")
	rec := httptest.NewRecorder()
	h.TargetCapacityHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var body map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["copies_offered"] != float64(2) || body["replica_nodes"] != float64(0) {
		t.Errorf("got offered %v, nodes %v; want 2 and 0 on a cluster without Longhorn", body["copies_offered"], body["replica_nodes"])
	}
}
