package cmd

import (
	"bytes"
	"context"
	"io"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
	yaml "gopkg.in/yaml.v3"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// A per-operator account listed before the admin, which is what breaks
// first-`hash:`-line surgery.
const dexConfigTwoUsers = `issuer: https://dex.cluster.example/dex
enablePasswordDB: true
staticClients:
- id: kipper-console
  redirectURIs:
  - https://console.cluster.example/callback
  secret: keep-me
staticPasswords:
- email: dana@cluster.example
  hash: HASH_DANA
  username: dana
- email: admin@cluster.example
  hash: HASH_ADMIN
  username: admin
`

func dexFixtures() (*corev1.ConfigMap, *appsv1.Deployment) {
	return &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "dex-config", Namespace: "dex"},
			Data:       map[string]string{"config.yaml": dexConfigTwoUsers},
		}, &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "dex", Namespace: "dex"},
		}
}

// printedPassword extracts the password the command disclosed.
func printedPassword(t *testing.T, out string) string {
	t.Helper()
	m := regexp.MustCompile(`Password:\s+(\S+)`).FindStringSubmatch(out)
	require.Len(t, m, 2, "no password in output:\n%s", out)
	return m[1]
}

func liveDexConfig(t *testing.T, cs *k8sfake.Clientset) string {
	t.Helper()
	cm, err := cs.CoreV1().ConfigMaps("dex").Get(context.Background(), "dex-config", metav1.GetOptions{})
	require.NoError(t, err)
	return cm.Data["config.yaml"]
}

type staticPassword struct {
	Email    string `yaml:"email"`
	Hash     string `yaml:"hash"`
	Username string `yaml:"username"`
}

func staticPasswords(t *testing.T, config string) []staticPassword {
	t.Helper()
	var parsed struct {
		StaticPasswords []staticPassword `yaml:"staticPasswords"`
	}
	require.NoError(t, yaml.Unmarshal([]byte(config), &parsed))
	return parsed.StaticPasswords
}

// requireAdminPassword asserts the live config authenticates the given password
// as the admin.
func requireAdminPassword(t *testing.T, config, password string) {
	t.Helper()
	for _, p := range staticPasswords(t, config) {
		if p.Username == "admin" {
			require.NoError(t, bcrypt.CompareHashAndPassword([]byte(p.Hash), []byte(password)),
				"the stored admin hash does not match the printed password")
			return
		}
	}
	t.Fatalf("no admin entry in the live config:\n%s", config)
}

func TestResetAdminPassword_ResetsOnlyTheAdminEntry(t *testing.T) {
	cm, dep := dexFixtures()
	cs := k8sfake.NewSimpleClientset(cm, dep)

	var out bytes.Buffer
	require.NoError(t, resetAdminPassword(context.Background(), cs, &out))

	config := liveDexConfig(t, cs)
	require.Contains(t, out.String(), "admin@cluster.example")
	requireAdminPassword(t, config, printedPassword(t, out.String()))

	entries := staticPasswords(t, config)
	require.Len(t, entries, 2)
	require.Equal(t, "dana", entries[0].Username)
	require.Equal(t, "HASH_DANA", entries[0].Hash, "the operator account was reset instead of the admin")
	require.Equal(t, "admin@cluster.example", entries[1].Email, "the admin email was rewritten")
	require.Contains(t, config, "secret: keep-me", "the console client secret was lost")

	live, err := cs.AppsV1().Deployments("dex").Get(context.Background(), "dex", metav1.GetOptions{})
	require.NoError(t, err)
	require.NotEmpty(t, live.Spec.Template.Annotations["kipper.run/restartedAt"], "dex was not restarted")
}

// The point of the disclosure order: a failed restart must not cost the
// operator the password, because the ConfigMap already holds its hash.
func TestResetAdminPassword_PrintsPasswordEvenWhenRestartFails(t *testing.T) {
	cm, dep := dexFixtures()
	cs := k8sfake.NewSimpleClientset(cm, dep)
	cs.PrependReactor("patch", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(context.DeadlineExceeded)
	})

	var out bytes.Buffer
	require.Error(t, resetAdminPassword(context.Background(), cs, &out))
	requireAdminPassword(t, liveDexConfig(t, cs), printedPassword(t, out.String()))
}

// The ClusterIdentity reconciler rolls dex itself via its config-hash
// annotation, so a get-modify-update here loses that race. This pins the
// absence of the update rather than the presence of the patch: the reactor
// fails any update on the deployment, so a reset that still passes never
// attempted one.
func TestResetAdminPassword_RestartsWithoutAConflictingUpdate(t *testing.T) {
	cm, dep := dexFixtures()
	cs := k8sfake.NewSimpleClientset(cm, dep)
	cs.PrependReactor("update", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewConflict(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, "dex", context.DeadlineExceeded)
	})

	var out bytes.Buffer
	require.NoError(t, resetAdminPassword(context.Background(), cs, &out))
	requireAdminPassword(t, liveDexConfig(t, cs), printedPassword(t, out.String()))
}

// The reconciler server-side applies the same ConfigMap, so the first write can
// lose the race once and must be retried rather than abandoned.
func TestResetAdminPassword_RetriesConfigMapConflict(t *testing.T) {
	cm, dep := dexFixtures()
	cs := k8sfake.NewSimpleClientset(cm, dep)
	conflictPending := true
	cs.PrependReactor("update", "configmaps", func(k8stesting.Action) (bool, runtime.Object, error) {
		if conflictPending {
			conflictPending = false
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Resource: "configmaps"}, "dex-config", context.DeadlineExceeded)
		}
		return false, nil, nil
	})

	var out bytes.Buffer
	require.NoError(t, resetAdminPassword(context.Background(), cs, &out))
	require.False(t, conflictPending, "the conflicting write was never attempted")
	requireAdminPassword(t, liveDexConfig(t, cs), printedPassword(t, out.String()))
}

// failingWriter accepts nothing, standing in for a full disk or a closed
// destination.
type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// A hash the operator never got to read is the same lockout the disclosure
// order exists to prevent, so the command must not report success.
func TestResetAdminPassword_FailsWhenTheCredentialsCannotBeDisplayed(t *testing.T) {
	cm, dep := dexFixtures()
	cs := k8sfake.NewSimpleClientset(cm, dep)

	err := resetAdminPassword(context.Background(), cs, failingWriter{})
	require.Error(t, err)

	// The error is the operator's last chance at the password, so it has to
	// carry one that actually works.
	m := regexp.MustCompile(`it is "(\S+)"`).FindStringSubmatch(err.Error())
	require.Len(t, m, 2, "the error does not carry the password: %v", err)
	requireAdminPassword(t, liveDexConfig(t, cs), m[1])
}

// Dex matches a static password on its email, so an admin entry without one
// cannot be signed in to whatever hash it carries. Writing a hash there and
// printing an invented address would report a login that does not exist.
func TestResetAdminPassword_FailsClosedWhenTheAdminHasNoEmail(t *testing.T) {
	for name, admin := range map[string]string{
		"no email key": "- hash: HASH_ADMIN\n  username: admin\n",
		"blank email":  "- email: \"\"\n  hash: HASH_ADMIN\n  username: admin\n",
	} {
		t.Run(name, func(t *testing.T) {
			cm, dep := dexFixtures()
			cm.Data["config.yaml"] = "issuer: https://dex.cluster.example/dex\nstaticPasswords:\n" + admin
			cs := k8sfake.NewSimpleClientset(cm, dep)

			var out bytes.Buffer
			err := resetAdminPassword(context.Background(), cs, &out)
			require.ErrorContains(t, err, "no email")
			require.NotContains(t, out.String(), "Password:", "credentials printed for a login that does not exist")
			require.Contains(t, liveDexConfig(t, cs), "HASH_ADMIN", "the config was rewritten anyway")
		})
	}
}

func TestResetAdminPassword_FailsClosedOnAmbiguousAdmin(t *testing.T) {
	cm, dep := dexFixtures()
	cm.Data["config.yaml"] = strings.ReplaceAll(dexConfigTwoUsers, "username: dana", "username: admin")
	cs := k8sfake.NewSimpleClientset(cm, dep)

	var out bytes.Buffer
	require.Error(t, resetAdminPassword(context.Background(), cs, &out))
	require.NotContains(t, out.String(), "Password:", "credentials printed for a config that was never written")
	require.Contains(t, liveDexConfig(t, cs), "HASH_ADMIN", "an ambiguous config was rewritten anyway")
}
