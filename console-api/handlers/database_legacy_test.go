package handlers

import (
	"context"
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

const (
	legacyHistory = `[{"sql":"select 1","user":"dev@example.com"}]`
	legacySnippet = `{"name":"daily","sql":"select now()"}`
)

func legacyConfigMap(name string, data map[string]string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "shop-test", UID: "cm-uid", ResourceVersion: "7"}, Data: data}
}

func servicesNamed(names ...string) ServiceExistsFunc {
	return func(_ context.Context, namespace, name string) (bool, error) {
		for _, n := range names {
			if namespace == "shop-test" && name == n {
				return true, nil
			}
		}
		return false, nil
	}
}

func configMapGone(t *testing.T, client *fake.Clientset, name string) bool {
	t.Helper()
	_, err := client.CoreV1().ConfigMaps("shop-test").Get(context.Background(), name, metav1.GetOptions{})
	return k8serrors.IsNotFound(err)
}

// Raw SQL in query history can contain credentials, such as CREATE USER passwords.
func TestLegacyQueryHistoryMovesIntoASecret(t *testing.T) {
	client := fake.NewClientset(legacyConfigMap("kipper-db-history-orders", map[string]string{"dev_example.com": legacyHistory}))

	RetireLegacyDBConfigMaps(context.Background(), client, servicesNamed("orders"))

	secret, err := client.CoreV1().Secrets("shop-test").Get(context.Background(), "kipper-db-history-orders", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("the history was not moved into a Secret: %v", err)
	}
	if string(secret.Data["dev_example.com"]) != legacyHistory {
		t.Errorf("history not carried over: %q", secret.Data["dev_example.com"])
	}
	if !configMapGone(t, client, "kipper-db-history-orders") {
		t.Error("the readable ConfigMap must be deleted once its contents are safe")
	}
}

func TestLegacySnippetsMergeWithoutOverwritingNewerOnes(t *testing.T) {
	newer := `{"name":"daily","sql":"select 2"}`
	client := fake.NewClientset(
		legacyConfigMap("kipper-db-snippets-orders", map[string]string{"daily": legacySnippet, "weekly": `{"name":"weekly","sql":"select 3"}`}),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kipper-db-snippets-orders", Namespace: "shop-test"},
			Data: map[string][]byte{"daily": []byte(newer)}},
	)

	RetireLegacyDBConfigMaps(context.Background(), client, servicesNamed("orders"))

	secret, _ := client.CoreV1().Secrets("shop-test").Get(context.Background(), "kipper-db-snippets-orders", metav1.GetOptions{})
	if string(secret.Data["daily"]) != newer {
		t.Errorf("a snippet saved since the move was overwritten: %q", secret.Data["daily"])
	}
	if len(secret.Data["weekly"]) == 0 {
		t.Error("a snippet only the ConfigMap had was lost")
	}
	if !configMapGone(t, client, "kipper-db-snippets-orders") {
		t.Error("the ConfigMap must be deleted after the merge")
	}
}

func TestLegacyConfigMapStaysWhenItsContentsCouldNotBeSaved(t *testing.T) {
	client := fake.NewClientset(legacyConfigMap("kipper-db-history-orders", map[string]string{"k": legacyHistory}))
	client.PrependReactor("create", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, errors.New("quota exceeded")
	})

	RetireLegacyDBConfigMaps(context.Background(), client, servicesNamed("orders"))

	if configMapGone(t, client, "kipper-db-history-orders") {
		t.Error("the only copy of the history was deleted")
	}
}

// Application ConfigMaps can share the legacy prefix, so migration also checks
// for an existing service and the expected payload shape.
func TestConfigMapsThatAreNotLegacyDatabaseStateAreLeftAlone(t *testing.T) {
	client := fake.NewClientset(
		legacyConfigMap("orders-settings", map[string]string{"k": "v"}),
		legacyConfigMap("kipper-db-history-settings", map[string]string{"k": legacyHistory}),
		legacyConfigMap("kipper-db-snippets-orders", map[string]string{"defaults": "max_connections=10"}),
	)

	RetireLegacyDBConfigMaps(context.Background(), client, servicesNamed("orders"))

	for _, name := range []string{"orders-settings", "kipper-db-history-settings", "kipper-db-snippets-orders"} {
		if configMapGone(t, client, name) {
			t.Errorf("%s is not legacy database state and was deleted", name)
		}
	}
}

// Concurrent history writes can cause Secret update conflicts during migration.
func TestLegacyMoveRetriesAConflict(t *testing.T) {
	client := fake.NewClientset(
		legacyConfigMap("kipper-db-history-orders", map[string]string{"a": legacyHistory}),
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "kipper-db-history-orders", Namespace: "shop-test"}, Data: map[string][]byte{"b": []byte(legacyHistory)}},
	)
	conflicted := false
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicted {
			return false, nil, nil
		}
		conflicted = true
		return true, nil, k8serrors.NewConflict(schema.GroupResource{Resource: "secrets"}, "kipper-db-history-orders", errors.New("changed"))
	})

	RetireLegacyDBConfigMaps(context.Background(), client, servicesNamed("orders"))

	if !configMapGone(t, client, "kipper-db-history-orders") {
		t.Error("a single conflict left the readable ConfigMap in place")
	}
}

// Conditional deletion protects ConfigMap writes made after the migration read.
func TestLegacyDeleteNamesTheRevisionItCopied(t *testing.T) {
	client := fake.NewClientset(legacyConfigMap("kipper-db-history-orders", map[string]string{"a": legacyHistory}))
	var preconditions *metav1.Preconditions
	client.PrependReactor("delete", "configmaps", func(a k8stesting.Action) (bool, runtime.Object, error) {
		preconditions = a.(k8stesting.DeleteActionImpl).DeleteOptions.Preconditions
		return false, nil, nil
	})

	RetireLegacyDBConfigMaps(context.Background(), client, servicesNamed("orders"))

	if preconditions == nil || preconditions.ResourceVersion == nil || *preconditions.ResourceVersion != "7" {
		t.Fatalf("the delete must be conditional on the revision that was copied, got %+v", preconditions)
	}
}

// A later sweep refreshes an unchanged migration copy after a ConfigMap write
// prevented deletion in the first sweep.
func TestASecondSweepCarriesOverAChangeMadeAfterTheFirstCopy(t *testing.T) {
	client := fake.NewClientset(legacyConfigMap("kipper-db-history-orders", map[string]string{"alice": `[{"sql":"select 1"}]`}))
	client.PrependReactor("delete", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, k8serrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "kipper-db-history-orders", errors.New("changed"))
	})
	RetireLegacyDBConfigMaps(context.Background(), client, servicesNamed("orders"))

	cm, _ := client.CoreV1().ConfigMaps("shop-test").Get(context.Background(), "kipper-db-history-orders", metav1.GetOptions{})
	cm.Data["alice"] = `[{"sql":"select 2"},{"sql":"select 1"}]`
	_, err := client.CoreV1().ConfigMaps("shop-test").Update(context.Background(), cm, metav1.UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	client.ReactionChain = client.ReactionChain[1:]
	RetireLegacyDBConfigMaps(context.Background(), client, servicesNamed("orders"))

	secret, _ := client.CoreV1().Secrets("shop-test").Get(context.Background(), "kipper-db-history-orders", metav1.GetOptions{})
	if string(secret.Data["alice"]) != `[{"sql":"select 2"},{"sql":"select 1"}]` {
		t.Errorf("the change made after the first copy was lost: %q", secret.Data["alice"])
	}
}

func TestHistoryWithoutQueriesIsLeftAlone(t *testing.T) {
	for _, value := range []string{`[{"action":"allow","resource":"reports"}]`, `[{}]`, `[null]`, `null`, `[]`} {
		client := fake.NewClientset(legacyConfigMap("kipper-db-history-orders", map[string]string{"rules": value}))

		RetireLegacyDBConfigMaps(context.Background(), client, servicesNamed("orders"))

		if configMapGone(t, client, "kipper-db-history-orders") {
			t.Errorf("a ConfigMap holding %s is not query history and was deleted", value)
		}
	}
}

func TestASnippetDeletedAfterItWasCopiedStaysDeleted(t *testing.T) {
	client := fake.NewClientset(
		legacyConfigMap("kipper-db-snippets-orders", map[string]string{"daily": legacySnippet}),
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "kipper-db-snippets-orders", Namespace: "shop-test",
				Annotations: map[string]string{migratedAnnotation: `{"daily":"` + digest([]byte(legacySnippet)) + `"}`}},
			Data: map[string][]byte{},
		},
	)

	RetireLegacyDBConfigMaps(context.Background(), client, servicesNamed("orders"))

	secret, _ := client.CoreV1().Secrets("shop-test").Get(context.Background(), "kipper-db-snippets-orders", metav1.GetOptions{})
	if _, back := secret.Data["daily"]; back {
		t.Error("a snippet its user deleted came back")
	}
}
