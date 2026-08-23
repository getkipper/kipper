package migration

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	crfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	kipperv1 "github.com/getkipper/kipper/console-api/api/v1alpha1"
)

// receivedResource is one /resource payload as the target saw it.
type receivedResource struct {
	Kind        string                  `json:"kind"`
	Name        string                  `json:"name"`
	Namespace   string                  `json:"namespace"`
	Credentials *transferredCredentials `json:"credentials"`
}

// recordingTarget answers the endpoints migrateServices and transferSecrets
// drive, and keeps what it was sent.
func recordingTarget(t *testing.T, resources *[]receivedResource, secrets *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/status"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"namespace_ready": true, "statefulsets_ready": true, "deployments_ready": true,
			})
		case strings.HasSuffix(r.URL.Path, "/resource"):
			var got receivedResource
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decoding resource payload: %v", err)
			}
			*resources = append(*resources, got)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "created"})
		case strings.HasSuffix(r.URL.Path, "/secret"):
			var got struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Errorf("decoding secret payload: %v", err)
			}
			*secrets = append(*secrets, got.Name)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "created"})
		default:
			t.Errorf("unexpected target call: %s %s", r.Method, r.URL.Path)
		}
	}))
}

func handoverSession(targetURL string) (*Session, *Token) {
	session := &Session{ID: "sess-handover", Projects: []string{"shop"}, Status: SessionRunning,
		Secret: "s", TargetAPI: targetURL, ExpiresAt: time.Now().Add(time.Hour)}
	return session, &Token{Endpoint: targetURL, Secret: "s"}
}

// mailhog is the fixture type throughout: it is in neither hasExportableData nor
// needsManualDataTransfer, so migrateServices does the handover and stops,
// without needing a pod to exec into.
func mailhogService() *kipperv1.Service {
	return &kipperv1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "mail", Namespace: "shop-prod", UID: types.UID("uid-mail")},
		Spec:       kipperv1.ServiceSpec{Type: "mailhog"},
	}
}

// projectionOf builds a derived per-binding Secret as the workload's controller
// renders it: labelled, and controlled by the App it belongs to.
func projectionOf(name, app string) *corev1.Secret {
	controller := true
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "shop-prod",
		Labels: map[string]string{"kipper.run/binding": "true"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: kipperv1.GroupVersion.String(), Kind: "App",
			Name: app, UID: types.UID("uid-" + app), Controller: &controller,
		}},
	}}
}

// The credentials a service's engine initialised with have to arrive on the
// target before its StatefulSet starts, or the new engine makes up its own
// password and the migrated data no longer opens with what the apps are given.
// Carrying them in the Service's own handover is what guarantees that ordering,
// and what lets the receiver own them without inferring anything.
func TestMigrateServices_CarriesCredentialsWithTheServiceCR(t *testing.T) {
	var resources []receivedResource
	var secrets []string
	target := recordingTarget(t, &resources, &secrets)
	defer target.Close()

	svc := mailhogService()
	creds := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mail-credentials", Namespace: "shop-prod",
			Labels: map[string]string{"app": "mail", "kipper.run/service-type": "mailhog"}},
		Data: map[string][]byte{
			"HOST": []byte("mail.shop-prod.svc"), "PORT": []byte("1025"),
			"PASSWORD": []byte("what-the-engine-knows"),
		},
	}
	h := &Handler{
		Client:   fake.NewSimpleClientset(creds),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(svc).Build(),
	}
	session, token := handoverSession(target.URL)

	mustSucceed(t, h.migrateServices(context.Background(), session, token, "shop-prod"))

	if len(resources) != 1 {
		t.Fatalf("sent %d resources, want the one Service", len(resources))
	}
	sent := resources[0]
	if sent.Credentials == nil {
		t.Fatal("the Service handover must carry its credentials")
	}
	password, err := base64.StdEncoding.DecodeString(sent.Credentials.Data["PASSWORD"])
	if err != nil {
		t.Fatalf("credentials must travel base64 encoded: %v", err)
	}
	if string(password) != "what-the-engine-knows" {
		t.Fatalf("password = %q, want the source engine's own", password)
	}
	if sent.Credentials.Labels["kipper.run/service-type"] != "mailhog" {
		t.Fatalf("labels must travel with the credentials, got %v", sent.Credentials.Labels)
	}
}

// A service whose credentials are missing cannot be migrated: the target would
// generate its own and the data would arrive locked. Failing here names the
// service, rather than leaving it to surface as an authentication error inside
// somebody's application weeks later.
func TestMigrateServices_FailsWhenSourceCredentialsAreMissing(t *testing.T) {
	var resources []receivedResource
	var secrets []string
	target := recordingTarget(t, &resources, &secrets)
	defer target.Close()

	h := &Handler{
		Client:   fake.NewSimpleClientset(),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(mailhogService()).Build(),
	}
	session, token := handoverSession(target.URL)

	err := h.migrateServices(context.Background(), session, token, "shop-prod")
	if err == nil {
		t.Fatal("a service with no credentials must fail the migration")
	}
	if !strings.Contains(err.Error(), "mail") {
		t.Fatalf("the error must name the service, got %v", err)
	}
	if len(resources) != 0 {
		t.Fatalf("nothing may be sent for a service that cannot be handed over, got %d", len(resources))
	}
}

// The bulk Secret phase must leave behind exactly what something else carries:
// a service's shared credentials ride with their Service, and derived binding
// Secrets are projections the target's own controllers render. Sending a
// projection is worse than pointless, because the receiving controller refuses
// to write through an object it does not own and its workload stops
// reconciling.
func TestTransferSecrets_LeavesCredentialsAndProjectionsBehind(t *testing.T) {
	var resources []receivedResource
	var sent []string
	target := recordingTarget(t, &resources, &sent)
	defer target.Close()

	objects := []runtime.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mail-credentials", Namespace: "shop-prod"}},
		projectionOf("mail-app-api-credentials", "api"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "app-api-secrets", Namespace: "shop-prod"}},
		// No Service called "legacy", so this is nobody's shared credentials
		// and travels as the ordinary Secret it is.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "legacy-credentials", Namespace: "shop-prod"}},
		// Labelled a projection but owned by nobody, so nothing on the target
		// renders it again. Dropping it would lose it outright.
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "orphaned-app-gone-credentials", Namespace: "shop-prod",
			Labels: map[string]string{"kipper.run/binding": "true"}}},
	}
	h := &Handler{
		Client:   fake.NewSimpleClientset(objects...),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(mailhogService()).Build(),
	}
	session, token := handoverSession(target.URL)

	mustSucceed(t, h.transferSecrets(context.Background(), session, token, "shop-prod", nil))

	got := strings.Join(sent, ",")
	for _, want := range []string{"app-api-secrets", "legacy-credentials", "orphaned-app-gone-credentials"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s must still transfer, sent: %s", want, got)
		}
	}
	for _, unwanted := range []string{"mail-credentials", "mail-app-api-credentials"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("%s must not transfer in the bulk phase, sent: %s", unwanted, got)
		}
	}
	if len(sent) != 3 {
		t.Fatalf("sent %d secrets, want 3: %s", len(sent), got)
	}
}

// The plan is what the operator consents to, and the digest binds that consent
// to a number. A count built from a different predicate than the transfer is
// consent to a run that does not happen.
func TestPlanSecretCountMatchesWhatTransfers(t *testing.T) {
	var resources []receivedResource
	var sent []string
	target := recordingTarget(t, &resources, &sent)
	defer target.Close()

	objects := []runtime.Object{
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mail-credentials", Namespace: "shop-prod"}},
		projectionOf("mail-app-api-credentials", "api"),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "app-api-secrets", Namespace: "shop-prod"}},
	}
	objects = append(objects, projectNamespace("shop-prod", "shop"))
	h := &Handler{
		Client: fake.NewSimpleClientset(objects...),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).
			WithObjects(mailhogService(), ownerOf("shop-prod")).Build(),
	}
	ctx := context.Background()

	// The real plan builder, not a recomputation of the predicate here: the
	// point is that the two agree, which a test doing its own counting could
	// never show.
	resp := &planResponse{}
	mustSucceed(t, h.planProject(ctx, "shop", nil, map[string]bool{}, resp))
	planned := ""
	for _, item := range resp.WillMigrate {
		if item.Kind == "secrets" {
			planned = item.Name
		}
	}

	session, token := handoverSession(target.URL)
	mustSucceed(t, h.transferSecrets(ctx, session, token, "shop-prod", nil))

	want := fmt.Sprintf("%d secrets", len(sent))
	if planned != want {
		t.Fatalf("the plan says %q and the transfer sent %d: %s", planned, len(sent), strings.Join(sent, ","))
	}
}

// The credentials have to be on the cluster before the CR is, because the
// reconciler creates the StatefulSet as soon as the CR lands and the engine
// reads its password from that Secret when its container starts. Written the
// other way round, the engine initialises against a password the reconciler
// generated and the transferred value arrives too late to matter.
func TestCreateService_WritesCredentialsBeforeTheCR(t *testing.T) {
	ctx := context.Background()
	crClient := crfake.NewClientBuilder().WithScheme(migrationScheme()).Build()
	client := fake.NewSimpleClientset()

	crExistedAtSecretWrite := true
	client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		var svc kipperv1.Service
		err := crClient.Get(ctx, crclient.ObjectKey{Namespace: "shop-prod", Name: "mail"}, &svc)
		crExistedAtSecretWrite = err == nil
		return false, nil, nil
	})

	h := &Handler{Client: client, CRClient: crClient, Sessions: NewSessionStore()}
	h.Sessions.Put(&Session{ID: "sess-order", Projects: []string{"shop"}, Status: SessionRunning,
		Secret: "s", ExpiresAt: time.Now().Add(time.Hour)})

	creds := &transferredCredentials{Data: map[string]string{
		"PASSWORD": base64.StdEncoding.EncodeToString([]byte("what-the-engine-knows")),
	}}
	mustSucceed(t, h.createService(ctx, "sess-order", "mail", "shop-prod", map[string]interface{}{"type": "mailhog"}, creds))

	if crExistedAtSecretWrite {
		t.Fatal("the credentials must be written before the Service CR exists, or the engine can initialise without them")
	}

	got, err := client.CoreV1().Secrets("shop-prod").Get(ctx, "mail-credentials", metav1.GetOptions{})
	mustSucceed(t, err)
	if string(got.Data["PASSWORD"]) != "what-the-engine-knows" {
		t.Fatalf("password = %q, want the transferred one", got.Data["PASSWORD"])
	}
	owner := metav1.GetControllerOf(got)
	if owner == nil || owner.Kind != "Service" || owner.Name != "mail" {
		t.Fatalf("the receiver must claim what it wrote, got %+v", got.OwnerReferences)
	}
}

// A credentials Secret under another controller belongs to that controller,
// whatever its name says. The refusal has to come before the write: discovering
// it afterwards means the data is already gone and only the ownership was
// declined.
func TestCreateService_RefusesAForeignOwnedCredentialsSecretBeforeWriting(t *testing.T) {
	ctx := context.Background()
	controller := true
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mail-credentials", Namespace: "shop-prod",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "StatefulSet", Name: "something-else",
				UID: types.UID("uid-other"), Controller: &controller,
			}}},
		Data: map[string][]byte{"PASSWORD": []byte("not-ours-to-take")},
	}
	client := fake.NewSimpleClientset(foreign)
	h := &Handler{
		Client:   client,
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).Build(),
		Sessions: NewSessionStore(),
	}
	h.Sessions.Put(&Session{ID: "sess-foreign", Projects: []string{"shop"}, Status: SessionRunning,
		Secret: "s", ExpiresAt: time.Now().Add(time.Hour)})

	creds := &transferredCredentials{Data: map[string]string{
		"PASSWORD": base64.StdEncoding.EncodeToString([]byte("from-the-source")),
	}}
	err := h.createService(ctx, "sess-foreign", "mail", "shop-prod", map[string]interface{}{"type": "mailhog"}, creds)
	if err == nil {
		t.Fatal("a Secret under another controller must not be written over")
	}

	got, getErr := client.CoreV1().Secrets("shop-prod").Get(ctx, "mail-credentials", metav1.GetOptions{})
	mustSucceed(t, getErr)
	if string(got.Data["PASSWORD"]) != "not-ours-to-take" {
		t.Fatalf("the data must be untouched, got %q", got.Data["PASSWORD"])
	}
	if owner := metav1.GetControllerOf(got); owner == nil || owner.Name != "something-else" {
		t.Fatalf("the owner must be untouched, got %+v", got.OwnerReferences)
	}
}

// A service already running here keeps the password its engine initialised
// with: no service StatefulSet carries a credential digest, so nothing restarts
// the pod when the Secret changes. Publishing the source's password would roll
// the bound workloads onto a credential the running database refuses, and the
// migration would report success over it.
func TestCreateService_RefusesToPublishOverARunningService(t *testing.T) {
	ctx := context.Background()
	// postgres rather than mailhog: the guard protects the password an engine
	// initialised with, and mailhog has none to protect. Naming a type without
	// one made this pass for a reason unrelated to what it tests.
	existing := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "db-credentials", Namespace: "shop-prod"},
		Data:       map[string][]byte{"PASSWORD": []byte("what-this-cluster-initialised-with")},
	}
	running := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "shop-prod"}}
	client := fake.NewSimpleClientset(existing, running)
	h := &Handler{
		Client:   client,
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).Build(),
		Sessions: NewSessionStore(),
	}
	h.Sessions.Put(&Session{ID: "sess-running", Projects: []string{"shop"}, Status: SessionRunning,
		Secret: "s", ExpiresAt: time.Now().Add(time.Hour)})

	incoming := &transferredCredentials{Data: map[string]string{
		"PASSWORD": base64.StdEncoding.EncodeToString([]byte("from-the-source")),
	}}
	err := h.createService(ctx, "sess-running", "db", "shop-prod", map[string]interface{}{"type": "postgres"}, incoming)
	if err == nil {
		t.Fatal("a running service's password must not be replaced under it")
	}

	got, getErr := client.CoreV1().Secrets("shop-prod").Get(ctx, "db-credentials", metav1.GetOptions{})
	mustSucceed(t, getErr)
	if string(got.Data["PASSWORD"]) != "what-this-cluster-initialised-with" {
		t.Fatalf("the running service's password must survive, got %q", got.Data["PASSWORD"])
	}

	// The same handover replaying its own value is the ordinary case and must
	// still pass: the engine already holds it. Against the running db, so the
	// comparison actually runs — naming a service the fixture does not have
	// takes the fresh-service path and would pass however the guard behaved.
	same := &transferredCredentials{Data: map[string]string{
		"PASSWORD": base64.StdEncoding.EncodeToString([]byte("what-this-cluster-initialised-with")),
	}}
	mustSucceed(t, h.createService(ctx, "sess-running", "db", "shop-prod", map[string]interface{}{"type": "postgres"}, same))
}

func mustSucceed(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Readiness only inspects the workloads that exist, so a CR whose reconcile
// fails before it creates one is invisible to it and the namespace reports
// healthy. That is what a refused service binding does to an App, and it is why
// every ownership bug in this area surfaced in somebody's application rather
// than in the migration that caused it.
func TestStatusHandler_ReportsACRThatProducedNoWorkload(t *testing.T) {
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"}}
	app := &kipperv1.App{ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-prod"}}

	h := &Handler{
		Client:   fake.NewSimpleClientset(ns),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(app).Build(),
	}

	missing := statusMissingWorkloads(t, h)
	if len(missing) != 1 || !strings.Contains(missing[0], "api") {
		t.Fatalf("an App with no Deployment must be reported, got %v", missing)
	}

	// With its Deployment present the namespace is genuinely complete.
	ready := int32(1)
	h.Client = fake.NewSimpleClientset(ns, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "shop-prod",
			Labels: map[string]string{"app.kubernetes.io/managed-by": "kipper"}},
		Spec:   appsv1.DeploymentSpec{Replicas: &ready},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	})
	if missing := statusMissingWorkloads(t, h); len(missing) != 0 {
		t.Fatalf("a complete namespace must report nothing missing, got %v", missing)
	}
}

// A list that could not be read is not an empty list. Reporting ready over one
// is the same failure as counting only what exists.
func TestStatusHandler_FailsOnAListError(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-prod"}})
	client.PrependReactor("list", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("the apiserver said no")
	})
	h := &Handler{
		Client:   client,
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).Build(),
	}

	rec := httptest.NewRecorder()
	h.StatusHandler(rec, httptest.NewRequest(http.MethodGet, "/status?namespace=shop-prod", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 when a list fails: %s", rec.Code, rec.Body.String())
	}
}

func statusMissingWorkloads(t *testing.T, h *Handler) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.StatusHandler(rec, httptest.NewRequest(http.MethodGet, "/status?namespace=shop-prod", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Missing []string `json:"missing_workloads"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&got); err != nil {
		t.Fatalf("decoding status: %v", err)
	}
	return got.Missing
}

// An owner reference naming a Service of this name whose UID has moved on is a
// stale reference, not a permission. Matching on the name alone let that shape
// through the preflight, so the data was overwritten and only the ownership was
// refused afterwards, which is the damage the preflight exists to prevent.
func TestCreateService_RefusesAStaleOwnerBeforeWriting(t *testing.T) {
	ctx := context.Background()
	controller := true
	stale := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mail-credentials", Namespace: "shop-prod",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: kipperv1.GroupVersion.String(), Kind: "Service",
				Name: "mail", UID: types.UID("uid-mail-before-the-restore"), Controller: &controller,
			}}},
		Data: map[string][]byte{"PASSWORD": []byte("what-the-restored-data-knows")},
	}
	client := fake.NewSimpleClientset(stale)
	h := &Handler{
		Client:   client,
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).Build(),
		Sessions: NewSessionStore(),
	}
	h.Sessions.Put(&Session{ID: "sess-stale", Projects: []string{"shop"}, Status: SessionRunning,
		Secret: "s", ExpiresAt: time.Now().Add(time.Hour)})

	creds := &transferredCredentials{Data: map[string]string{
		"HOST":     base64.StdEncoding.EncodeToString([]byte("mail.shop-prod.svc")),
		"PORT":     base64.StdEncoding.EncodeToString([]byte("1025")),
		"PASSWORD": base64.StdEncoding.EncodeToString([]byte("from-the-source")),
	}}
	err := h.createService(ctx, "sess-stale", "mail", "shop-prod", map[string]interface{}{"type": "mailhog"}, creds)
	if err == nil {
		t.Fatal("a credentials Secret under a stale owner must not be written over")
	}

	got, getErr := client.CoreV1().Secrets("shop-prod").Get(ctx, "mail-credentials", metav1.GetOptions{})
	mustSucceed(t, getErr)
	if string(got.Data["PASSWORD"]) != "what-the-restored-data-knows" {
		t.Fatalf("the refusal must come before the write, got %q", got.Data["PASSWORD"])
	}
}

// The shared credentials reach bound workloads through envFrom, so a key added
// between the write and the claim would arrive in their environment carrying
// the Service's own provenance. Only what the reconciler legitimately adds in
// that window is acceptable.
func TestClaimableAfterConflict(t *testing.T) {
	written := map[string][]byte{"PASSWORD": []byte("s3cret"), "HOST": []byte("db.shop-prod.svc")}

	for _, tc := range []struct {
		name  string
		fresh map[string][]byte
		want  bool
	}{
		{"untouched", map[string][]byte{"PASSWORD": []byte("s3cret"), "HOST": []byte("db.shop-prod.svc")}, true},
		{"the reconciler added its type default", map[string][]byte{
			"PASSWORD": []byte("s3cret"), "HOST": []byte("db.shop-prod.svc"), "NAME": []byte("app")}, true},
		{"a key we never wrote", map[string][]byte{
			"PASSWORD": []byte("s3cret"), "HOST": []byte("db.shop-prod.svc"), "INJECTED": []byte("x")}, false},
		{"a default with a value that is not the default", map[string][]byte{
			"PASSWORD": []byte("s3cret"), "HOST": []byte("db.shop-prod.svc"), "NAME": []byte("somebody_elses_db")}, false},
		{"one of ours changed", map[string][]byte{
			"PASSWORD": []byte("changed"), "HOST": []byte("db.shop-prod.svc")}, false},
		{"one of ours removed", map[string][]byte{"PASSWORD": []byte("s3cret")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := claimableAfterConflict(written, tc.fresh, "postgres"); got != tc.want {
				t.Fatalf("claimable = %v, want %v", got, tc.want)
			}
		})
	}
}

// A partial credential is worse than none: on a replay it would replace the
// target's complete set, and on a fresh install it stands an engine up with no
// password at all.
func TestMigrateServices_RefusesIncompleteCredentials(t *testing.T) {
	var resources []receivedResource
	var secrets []string
	target := recordingTarget(t, &resources, &secrets)
	defer target.Close()

	// A mailhog credential needs HOST and PORT. It carries no password: the
	// image has no authentication, so one was never a key its pod reads.
	partial := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "mail-credentials", Namespace: "shop-prod"},
		Data:       map[string][]byte{"HOST": []byte("mail.shop-prod.svc")},
	}
	h := &Handler{
		Client:   fake.NewSimpleClientset(partial),
		CRClient: crfake.NewClientBuilder().WithScheme(migrationScheme()).WithObjects(mailhogService()).Build(),
	}
	session, token := handoverSession(target.URL)

	err := h.migrateServices(context.Background(), session, token, "shop-prod")
	if err == nil {
		t.Fatal("incomplete credentials must fail the handover")
	}
	if !strings.Contains(err.Error(), "PORT") {
		t.Fatalf("the error must name the missing key, got %v", err)
	}
	if len(resources) != 0 {
		t.Fatalf("nothing may be sent, got %d", len(resources))
	}
}

// The catalog starts mongod with MONGO_INITDB_ROOT_USERNAME and _PASSWORD set,
// and the official image turns on --auth when both are present, so a dump with
// no credentials is refused and the mongodb migration cannot work at all. The
// values are read from the pod's own environment, so they never appear in the
// command line.
func TestMongoCommandsAuthenticate(t *testing.T) {
	dump, _ := buildDumpCommand("mongodb", "orders")
	restore, _ := buildImportCommand("mongodb", "orders")

	for _, tc := range []struct {
		what string
		cmd  []string
	}{{"dump", dump}, {"restore", restore}} {
		if len(tc.cmd) == 0 {
			t.Fatalf("%s command is missing", tc.what)
		}
		script := tc.cmd[len(tc.cmd)-1]
		for _, want := range []string{
			`-u "$MONGO_INITDB_ROOT_USERNAME"`,
			`-p "$MONGO_INITDB_ROOT_PASSWORD"`,
			"--authenticationDatabase admin",
		} {
			if !strings.Contains(script, want) {
				t.Errorf("mongo %s command must pass %s, got: %s", tc.what, want, script)
			}
		}
	}
}

// rabbitmqctl import_definitions takes a path and nothing else. Given "-" it
// looks for a file of that name and fails with "File - does not exist", so the
// streamed definitions have to reach disk before the import runs. Checked
// against rabbitmq:3-management-alpine (3.13.7), the tag the catalog pins:
// export_definitions does accept "-" and writes to stdout, which is why only
// one half of the pair needed changing.
func TestRabbitMQImportReadsAFileRatherThanStdin(t *testing.T) {
	importCmd, _ := buildImportCommand("rabbitmq", "queue")
	if len(importCmd) == 0 {
		t.Fatal("rabbitmq import command is missing")
	}
	script := importCmd[len(importCmd)-1]
	if strings.Contains(script, "import_definitions -") {
		t.Fatalf("import_definitions cannot read stdin, got: %s", script)
	}
	if !strings.Contains(script, "cat > ") {
		t.Fatalf("the streamed definitions must be written to a file first, got: %s", script)
	}

	dumpCmd, _ := buildDumpCommand("rabbitmq", "queue")
	if !strings.Contains(dumpCmd[len(dumpCmd)-1], "export_definitions -") {
		t.Fatalf("export_definitions writes to stdout with a dash and stays as it is, got: %s", dumpCmd)
	}
}
