package dexsecrets

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const (
	oldKey   = "KIPPER_GITHUB_CLIENT_SECRET_ABCDEFGHIJKL"
	usedKey  = "KIPPER_GITHUB_CLIENT_SECRET_MNOPQRSTUVWX"
	otherKey = "KIPPER_GOOGLE_CLIENT_SECRET_234567ABCDEF"
)

var now = time.Date(2026, 11, 1, 18, 0, 0, 0, time.UTC)

func dexConfig(text string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "dex-config", Namespace: Namespace},
		Data:       map[string]string{"config.yaml": text},
	}
}

func connectorSecret(marks map[string]time.Time, keys ...string) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: SecretName, Namespace: Namespace},
		Data:       map[string][]byte{},
	}
	for _, k := range keys {
		s.Data[k] = []byte("value-of-" + k)
	}
	if marks != nil {
		encoded := map[string]string{}
		for k, t := range marks {
			encoded[k] = t.Format(time.RFC3339)
		}
		raw, _ := json.Marshal(encoded)
		s.Annotations = map[string]string{UnusedSinceAnnotation: string(raw)}
	}
	return s
}

func readSecret(t *testing.T, client *fake.Clientset) *corev1.Secret {
	t.Helper()
	s, err := client.CoreV1().Secrets(Namespace).Get(context.Background(), SecretName, metav1.GetOptions{})
	require.NoError(t, err)
	return s
}

func marksOf(t *testing.T, s *corev1.Secret) map[string]string {
	t.Helper()
	marks := map[string]string{}
	if raw := s.Annotations[UnusedSinceAnnotation]; raw != "" {
		require.NoError(t, json.Unmarshal([]byte(raw), &marks))
	}
	return marks
}

const usingConfig = `connectors:
- id: github
  type: github
  config:
    clientID: abc
    clientSecret: $` + usedKey + `
`

func TestNewKeyHasTheFormRetireConsiders(t *testing.T) {
	key := NewKey("github")

	assert.Regexp(t, `^KIPPER_GITHUB_CLIENT_SECRET_[A-Z2-7]{12}$`, key)
	assert.True(t, generated(key))
	assert.NotEqual(t, key, NewKey("github"), "each stored secret gets a key of its own")
}

func TestRetireMarksAnUnusedKeyWithoutDeletingIt(t *testing.T) {
	client := fake.NewClientset(dexConfig(usingConfig), connectorSecret(nil, oldKey, usedKey))

	deleted, err := Retire(context.Background(), client, now)

	require.NoError(t, err)
	assert.Empty(t, deleted)
	s := readSecret(t, client)
	assert.Contains(t, s.Data, oldKey, "a writer may have stored it and not yet saved the config that uses it")
	assert.Equal(t, map[string]string{oldKey: now.Format(time.RFC3339)}, marksOf(t, s))
}

func TestRetireDeletesAKeyUnusedForTheGracePeriod(t *testing.T) {
	client := fake.NewClientset(dexConfig(usingConfig), connectorSecret(map[string]time.Time{oldKey: now.Add(-UnusedGrace)}, oldKey, usedKey))

	deleted, err := Retire(context.Background(), client, now)

	require.NoError(t, err)
	assert.Equal(t, []string{oldKey}, deleted)
	s := readSecret(t, client)
	assert.NotContains(t, s.Data, oldKey)
	assert.Contains(t, s.Data, usedKey)
	assert.Empty(t, marksOf(t, s))
}

func TestRetireKeepsAKeyMarkedLessThanTheGracePeriodAgo(t *testing.T) {
	client := fake.NewClientset(dexConfig(usingConfig), connectorSecret(map[string]time.Time{oldKey: now.Add(-UnusedGrace + time.Minute)}, oldKey, usedKey))

	deleted, err := Retire(context.Background(), client, now)

	require.NoError(t, err)
	assert.Empty(t, deleted)
	assert.Contains(t, readSecret(t, client).Data, oldKey)
}

func TestRetireKeepsKeysUsedAnywhereInTheConfig(t *testing.T) {
	for name, config := range map[string]string{
		"a connector kip does not manage": `connectors:
- id: corp
  type: oidc
  config:
    clientSecret: $` + oldKey + `
`,
		"another field":     "staticClients:\n- id: x\n  secret: ${" + oldKey + "}\n",
		"a second data key": "",
	} {
		t.Run(name, func(t *testing.T) {
			cm := dexConfig(config)
			if config == "" {
				cm.Data["extra.yaml"] = "uses " + oldKey
			}
			client := fake.NewClientset(cm, connectorSecret(map[string]time.Time{oldKey: now.Add(-2 * UnusedGrace)}, oldKey))

			deleted, err := Retire(context.Background(), client, now)

			require.NoError(t, err)
			assert.Empty(t, deleted)
			s := readSecret(t, client)
			assert.Contains(t, s.Data, oldKey)
			assert.Empty(t, marksOf(t, s), "a key in use loses its mark")
		})
	}
}

func TestRetireLeavesKeysKipDidNotGenerate(t *testing.T) {
	client := fake.NewClientset(dexConfig(usingConfig), connectorSecret(nil, "MY_OWN_SECRET", "KIPPER_GITHUB_CLIENT_SECRET", usedKey))

	deleted, err := Retire(context.Background(), client, now)

	require.NoError(t, err)
	assert.Empty(t, deleted)
	s := readSecret(t, client)
	assert.Contains(t, s.Data, "MY_OWN_SECRET")
	assert.Empty(t, marksOf(t, s))
}

func TestRetireChangesNothingWithoutTheConfig(t *testing.T) {
	for name, react := range map[string]func(*fake.Clientset){
		"missing": func(*fake.Clientset) {},
		"read fails": func(c *fake.Clientset) {
			c.PrependReactor("get", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, errors.New("connection refused")
			})
		},
	} {
		t.Run(name, func(t *testing.T) {
			objects := []runtime.Object{connectorSecret(map[string]time.Time{oldKey: now.Add(-2 * UnusedGrace)}, oldKey)}
			if name != "missing" {
				objects = append(objects, dexConfig(usingConfig))
			}
			client := fake.NewClientset(objects...)
			react(client)

			deleted, err := Retire(context.Background(), client, now)

			require.Error(t, err)
			assert.Empty(t, deleted)
			assert.Contains(t, readSecret(t, client).Data, oldKey)
		})
	}
}

func TestRetireDeletesNothingWhenTheSecretChangedMeanwhile(t *testing.T) {
	client := fake.NewClientset(dexConfig(usingConfig), connectorSecret(map[string]time.Time{oldKey: now.Add(-2 * UnusedGrace)}, oldKey))
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, SecretName, errors.New("modified"))
	})

	deleted, err := Retire(context.Background(), client, now)

	require.Error(t, err, "the next pass starts again from fresh reads")
	assert.Empty(t, deleted)
	assert.Contains(t, readSecret(t, client).Data, oldKey)
}

func TestRetireWithoutTheSecretDoesNothing(t *testing.T) {
	client := fake.NewClientset(dexConfig(usingConfig))

	deleted, err := Retire(context.Background(), client, now)

	require.NoError(t, err)
	assert.Empty(t, deleted)
}

func TestRetireStartsAgainFromAnUnreadableMark(t *testing.T) {
	s := connectorSecret(nil, oldKey)
	s.Annotations = map[string]string{UnusedSinceAnnotation: "not json"}
	client := fake.NewClientset(dexConfig(usingConfig), s)

	deleted, err := Retire(context.Background(), client, now)

	require.NoError(t, err)
	assert.Empty(t, deleted)
	assert.Equal(t, map[string]string{oldKey: now.Format(time.RFC3339)}, marksOf(t, readSecret(t, client)))
}

func TestRetireKeepsAKeyMarkedInTheFuture(t *testing.T) {
	client := fake.NewClientset(dexConfig(usingConfig), connectorSecret(map[string]time.Time{oldKey: now.Add(2 * UnusedGrace)}, oldKey))

	deleted, err := Retire(context.Background(), client, now)

	require.NoError(t, err)
	assert.Empty(t, deleted, "a replica whose clock runs ahead marked it")
	assert.Contains(t, readSecret(t, client).Data, oldKey)
}

func TestRetireDropsTheMarkOfAKeyThatIsGone(t *testing.T) {
	client := fake.NewClientset(dexConfig(usingConfig), connectorSecret(map[string]time.Time{oldKey: now.Add(-time.Minute)}, usedKey))

	deleted, err := Retire(context.Background(), client, now)

	require.NoError(t, err)
	assert.Empty(t, deleted)
	assert.Empty(t, marksOf(t, readSecret(t, client)))
}

func TestStoreCreatesTheSecret(t *testing.T) {
	client := fake.NewClientset()

	require.NoError(t, Store(context.Background(), client, map[string]string{usedKey: "s3cret"}))

	assert.Equal(t, "s3cret", string(readSecret(t, client).Data[usedKey]))
}

func TestStoreClearsTheMarkOfTheKeysItWrites(t *testing.T) {
	marks := map[string]time.Time{oldKey: now.Add(-2 * UnusedGrace), otherKey: now.Add(-2 * UnusedGrace)}
	client := fake.NewClientset(connectorSecret(marks, oldKey, otherKey))

	require.NoError(t, Store(context.Background(), client, map[string]string{oldKey: "again"}))

	s := readSecret(t, client)
	assert.Equal(t, "again", string(s.Data[oldKey]))
	assert.Contains(t, s.Data, otherKey)
	assert.Equal(t, []string{otherKey}, keysOf(marksOf(t, s)), "a stored key may be referenced next, so it starts unmarked")
}

func TestStoreLeavesAnUnreadableMarkAlone(t *testing.T) {
	s := connectorSecret(nil, otherKey)
	s.Annotations = map[string]string{UnusedSinceAnnotation: "not json"}
	client := fake.NewClientset(s)

	require.NoError(t, Store(context.Background(), client, map[string]string{usedKey: "s3cret"}))

	got := readSecret(t, client)
	assert.Equal(t, "s3cret", string(got.Data[usedKey]))
	assert.Equal(t, "not json", got.Annotations[UnusedSinceAnnotation], "Retire replaces it with fresh marks")
}

func TestStoreRetriesAWriteThatLostARace(t *testing.T) {
	client := fake.NewClientset(connectorSecret(nil, otherKey))
	conflicts := 1
	client.PrependReactor("update", "secrets", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflicts == 0 {
			return false, nil, nil
		}
		conflicts--
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, SecretName, errors.New("modified"))
	})

	require.NoError(t, Store(context.Background(), client, map[string]string{usedKey: "s3cret"}))

	s := readSecret(t, client)
	assert.Equal(t, "s3cret", string(s.Data[usedKey]))
	assert.Contains(t, s.Data, otherKey)
}

func TestEnsureDexReadsAddsTheSecretOnce(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "dex", Namespace: Namespace},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "dex"}},
		}}},
	}
	client := fake.NewClientset(deploy)

	require.NoError(t, EnsureDexReads(context.Background(), client))
	require.NoError(t, EnsureDexReads(context.Background(), client))

	got, err := client.AppsV1().Deployments(Namespace).Get(context.Background(), "dex", metav1.GetOptions{})
	require.NoError(t, err)
	envFrom := got.Spec.Template.Spec.Containers[0].EnvFrom
	require.Len(t, envFrom, 1)
	assert.Equal(t, SecretName, envFrom[0].SecretRef.Name)
	require.NotNil(t, envFrom[0].SecretRef.Optional)
	assert.True(t, *envFrom[0].SecretRef.Optional, "Dex must start before the Secret exists")
}

func keysOf(m map[string]string) []string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
