package sharedcred

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func listOf(entries ...Entry) []Entry { return entries }

func TestGrantAddsAProjectToAnEmptyList(t *testing.T) {
	entries, err := Grant(listOf(Entry{Name: "forge", Server: "git.example.com"}), "forge", []string{"shop"})

	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !entries[0].AllowsProject("shop") {
		t.Errorf("allowed projects = %v", entries[0].AllowedProjects)
	}
}

// Granting is additive. A grant that replaced the list would revoke every other
// project as a side effect of allowing one.
func TestGrantKeepsTheProjectsAlreadyAllowed(t *testing.T) {
	entries, err := Grant(listOf(Entry{Name: "forge", AllowedProjects: []string{"shop"}}), "forge", []string{"blog"})

	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if !entries[0].AllowsProject("shop") || !entries[0].AllowsProject("blog") {
		t.Errorf("allowed projects = %v", entries[0].AllowedProjects)
	}
}

func TestGrantIsIdempotent(t *testing.T) {
	entries, err := Grant(listOf(Entry{Name: "forge", AllowedProjects: []string{"shop"}}), "forge", []string{"shop"})

	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	if len(entries[0].AllowedProjects) != 1 {
		t.Errorf("allowed projects = %v", entries[0].AllowedProjects)
	}
}

// A grant against a name that is not there has to fail. Reporting success for a
// credential nobody has would leave the operator believing a build will work.
func TestGrantRefusesACredentialThatIsNotThere(t *testing.T) {
	if _, err := Grant(listOf(Entry{Name: "forge"}), "other", []string{"shop"}); err == nil {
		t.Fatal("granted on a credential that does not exist")
	}
}

func TestGrantRefusesABlankProject(t *testing.T) {
	if _, err := Grant(listOf(Entry{Name: "forge"}), "forge", []string{""}); err == nil {
		t.Fatal("granted a blank project, which AllowsProject can never match")
	}
}

func TestRevokeRemovesOnlyTheProjectNamed(t *testing.T) {
	entries, err := Revoke(listOf(Entry{Name: "forge", AllowedProjects: []string{"shop", "blog"}}), "forge", []string{"shop"})

	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if entries[0].AllowsProject("shop") || !entries[0].AllowsProject("blog") {
		t.Errorf("allowed projects = %v", entries[0].AllowedProjects)
	}
}

func TestRevokeRefusesACredentialThatIsNotThere(t *testing.T) {
	if _, err := Revoke(listOf(Entry{Name: "forge"}), "other", []string{"shop"}); err == nil {
		t.Fatal("revoked on a credential that does not exist")
	}
}

// The whole point of the seeding: a credential written before allow-lists
// existed denies every project, so the first build after an upgrade fails on a
// cluster that was working.
func TestSeedFillsAnEmptyListFromTheProjectsAlreadyBuildingWithIt(t *testing.T) {
	entries, seeded, _ := Seed(listOf(Entry{Name: "forge"}), map[string][]string{"forge": {"shop", "blog"}})

	if !entries[0].AllowsProject("shop") || !entries[0].AllowsProject("blog") {
		t.Errorf("allowed projects = %v", entries[0].AllowedProjects)
	}
	if len(seeded) != 1 || seeded[0] != "forge" {
		t.Errorf("seeded = %v", seeded)
	}
}

// A list an admin has curated is their decision. An upgrade that widened it
// would hand a project a token somebody had deliberately kept from it.
func TestSeedNeverWidensAListSomebodyCurated(t *testing.T) {
	entries, seeded, _ := Seed(listOf(Entry{Name: "forge", AllowedProjects: []string{"shop"}}),
		map[string][]string{"forge": {"shop", "blog"}})

	if entries[0].AllowsProject("blog") {
		t.Error("an upgrade widened an allow-list an admin had already set")
	}
	if len(seeded) != 0 {
		t.Errorf("seeded = %v", seeded)
	}
}

func TestClosingLeavesACredentialNothingBuildsWithDenyingEverybody(t *testing.T) {
	entries, changed := CloseUndecided(listOf(Entry{Name: "forge"}))

	if entries[0].AllowedProjects == nil || len(entries[0].AllowedProjects) != 0 {
		t.Errorf("allowed projects = %v", entries[0].AllowedProjects)
	}
	if !changed {
		t.Error("closing an undecided credential reported no change")
	}
}

// Save built a fresh Secret and replaced the object, so anything else stored
// alongside the list went with it.
func TestUpdateKeepsWhatElseIsInTheSecret(t *testing.T) {
	listed, _ := json.Marshal([]Entry{{Name: "forge"}})
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: ConfigSecretName, Namespace: Namespace,
			Annotations: map[string]string{"kipper.run/note": "kept"},
		},
		Data: map[string][]byte{dataKey: listed, "unrelated": []byte("kept")},
	})

	err := Update(context.Background(), client, func(entries []Entry) ([]Entry, error) {
		return Grant(entries, "forge", []string{"shop"})
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	live, getErr := client.CoreV1().Secrets(Namespace).Get(context.Background(), ConfigSecretName, metav1.GetOptions{})
	if getErr != nil {
		t.Fatalf("get: %v", getErr)
	}
	if string(live.Data["unrelated"]) != "kept" {
		t.Error("the write replaced the Secret and took another key with it")
	}
	if live.Annotations["kipper.run/note"] != "kept" {
		t.Error("the write replaced the Secret and took its annotations with it")
	}
}

// A mutate that refuses must leave the stored list alone, or a failed grant
// still rewrites the control it failed to change.
func TestUpdateWritesNothingWhenTheChangeIsRefused(t *testing.T) {
	listed, _ := json.Marshal([]Entry{{Name: "forge", AllowedProjects: []string{"shop"}}})
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigSecretName, Namespace: Namespace},
		Data:       map[string][]byte{dataKey: listed},
	})

	err := Update(context.Background(), client, func(entries []Entry) ([]Entry, error) {
		return Grant(entries, "missing", []string{"blog"})
	})
	if err == nil {
		t.Fatal("a refused change reported success")
	}

	after, loadErr := Load(context.Background(), client)
	if loadErr != nil {
		t.Fatalf("load: %v", loadErr)
	}
	if len(after) != 1 || !after[0].AllowsProject("shop") {
		t.Errorf("the stored list changed after a refused write: %+v", after)
	}
}

func TestUpdateCreatesTheSecretWhenThereIsNone(t *testing.T) {
	client := fake.NewClientset()

	err := Update(context.Background(), client, func(entries []Entry) ([]Entry, error) {
		return append(entries, Entry{Name: "forge", Server: "git.example.com"}), nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	after, loadErr := Load(context.Background(), client)
	if loadErr != nil || len(after) != 1 {
		t.Fatalf("stored list = %+v (%v)", after, loadErr)
	}
}

// The seeding runs on every upgrade, and an empty list is not only the state a
// cluster installed before allow-lists is in. An admin who revokes the last
// project reaches it too, and an upgrade that read that as "never decided"
// would hand the project back what was taken from it.
func TestSeedLeavesADeliberatelyEmptyListClosed(t *testing.T) {
	entries, seeded, changed := Seed(listOf(Entry{Name: "forge", AllowedProjects: []string{}}),
		map[string][]string{"forge": {"shop"}})

	if entries[0].AllowsProject("shop") {
		t.Error("an upgrade undid a revoke")
	}
	if len(seeded) != 0 || changed {
		t.Errorf("seeded = %v, changed = %v", seeded, changed)
	}
}

// Closing records a decision for every credential still undecided, including
// the ones nothing builds with, so the inference runs once on a cluster rather
// than on every upgrade for the rest of its life.
func TestClosingUndecidedRunsTheMigrationOnce(t *testing.T) {
	first, _, changed := Seed(listOf(Entry{Name: "forge"}, Entry{Name: "unused"}),
		map[string][]string{"forge": {"shop"}})
	if !changed {
		t.Fatal("the first pass reported nothing to do")
	}
	first, closed := CloseUndecided(first)
	if !closed {
		t.Fatal("nothing was closed, so the credential nothing uses stays a candidate")
	}

	_, seeded, changed := Seed(first, map[string][]string{"forge": {"shop"}, "unused": {"blog"}})

	if len(seeded) != 0 || changed {
		t.Errorf("a later upgrade seeded again: seeded = %v, changed = %v", seeded, changed)
	}
}

// Seeding is what runs while builds can still start under the old rules, so it
// must not decide anything it has not been asked to. A credential nothing
// references yet is left undecided for the pass that runs after the rollout.
func TestSeedLeavesACredentialNothingReferencesUndecided(t *testing.T) {
	entries, seeded, changed := Seed(listOf(Entry{Name: "forge"}), map[string][]string{})

	if entries[0].AllowedProjects != nil {
		t.Errorf("a first pass decided a credential nothing had referenced yet: %v", entries[0].AllowedProjects)
	}
	if len(seeded) != 0 || changed {
		t.Errorf("seeded = %v, changed = %v", seeded, changed)
	}
}

// Revoking the last project is a decision, and it has to survive a round trip
// through the stored JSON as one. A list that marshalled away to nothing would
// read back as a credential nobody had ever decided about.
func TestRevokingTheLastProjectIsRecordedAsADecision(t *testing.T) {
	entries, err := Revoke(listOf(Entry{Name: "forge", AllowedProjects: []string{"shop"}}), "forge", []string{"shop"})
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}

	stored, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var reloaded []Entry
	if err := json.Unmarshal(stored, &reloaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if reloaded[0].AllowedProjects == nil {
		t.Errorf("a revoke read back as no decision at all: %s", stored)
	}
}

func TestGrantOnACredentialThatIsNotThereIsRecognisable(t *testing.T) {
	_, err := Grant(listOf(Entry{Name: "forge"}), "other", []string{"shop"})

	var unknown *UnknownCredentialError
	if !errors.As(err, &unknown) {
		t.Fatalf("a caller cannot tell this from any other failure: %v", err)
	}
}

// An upgrade with nothing to change must not rewrite the Secret. Every write is
// a new resourceVersion, an audit entry and a conflict for whoever else is
// writing.
func TestUpdateWritesNothingWhenTheListIsUnchanged(t *testing.T) {
	listed, _ := json.Marshal([]Entry{{Name: "forge", AllowedProjects: []string{"shop"}}})
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigSecretName, Namespace: Namespace},
		Data:       map[string][]byte{dataKey: listed},
	})
	client.ClearActions()

	err := Update(context.Background(), client, func(entries []Entry) ([]Entry, error) { return entries, nil })
	if err != nil {
		t.Fatalf("update: %v", err)
	}

	for _, action := range client.Actions() {
		if action.GetVerb() == "update" || action.GetVerb() == "create" {
			t.Errorf("a no-op rewrote the Secret: %s", action.GetVerb())
		}
	}
}

// Two writers on a cluster with no list yet both find it missing, and the one
// that loses the race meets AlreadyExists, which is not a conflict and so is
// not retried by the usual helper.
func TestUpdateRetriesWhenAnotherWriterCreatedTheListFirst(t *testing.T) {
	client := fake.NewClientset()
	raced := false
	client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if raced {
			return false, nil, nil
		}
		raced = true
		// The other writer's object, which the retry has to find on its
		// re-read rather than give up on.
		listed, _ := json.Marshal([]Entry{{Name: "theirs"}})
		if err := client.Tracker().Add(&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: ConfigSecretName, Namespace: Namespace},
			Data:       map[string][]byte{dataKey: listed},
		}); err != nil {
			return true, nil, err
		}
		return true, nil, k8serrors.NewAlreadyExists(
			schema.GroupResource{Resource: "secrets"}, ConfigSecretName)
	})

	err := Update(context.Background(), client, func(entries []Entry) ([]Entry, error) {
		return append(entries, Entry{Name: "ours"}), nil
	})
	if err != nil {
		t.Fatalf("update gave up on a list another writer had just created: %v", err)
	}

	after, loadErr := Load(context.Background(), client)
	if loadErr != nil {
		t.Fatalf("load: %v", loadErr)
	}
	if len(after) != 2 {
		t.Errorf("the retry lost one of the two writers: %+v", after)
	}
}

// The retry is the whole reason Update exists rather than a plain write, and
// nothing else in the suite makes the apiserver refuse one.
func TestUpdateRetriesOnConflict(t *testing.T) {
	listed, _ := json.Marshal([]Entry{{Name: "forge"}})
	client := fake.NewClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: ConfigSecretName, Namespace: Namespace},
		Data:       map[string][]byte{dataKey: listed},
	})
	refused := false
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if refused {
			return false, nil, nil
		}
		refused = true
		return true, nil, k8serrors.NewConflict(
			schema.GroupResource{Resource: "secrets"}, ConfigSecretName, errors.New("modified"))
	})

	err := Update(context.Background(), client, func(entries []Entry) ([]Entry, error) {
		return Grant(entries, "forge", []string{"shop"})
	})
	if err != nil {
		t.Fatalf("update gave up on a conflict instead of retrying: %v", err)
	}

	after, _ := Load(context.Background(), client)
	if !after[0].AllowsProject("shop") {
		t.Errorf("the retry lost the change: %+v", after)
	}
}
